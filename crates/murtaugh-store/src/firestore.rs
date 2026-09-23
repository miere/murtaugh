use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use serde_json::{Map, Value, json};
use time::OffsetDateTime;
use time::format_description::well_known::Rfc3339;

use crate::leader::{Leader, Lease};
use crate::{
    Conversation, Grant, NodeToken, Pin, Result, Store, StoreError, ToolMode, UserConfig, UserId,
};

const SCOPE: &str = "https://www.googleapis.com/auth/datastore";
const DEFAULT_COLLECTION: &str = "murtaugh";
const PAGE_SIZE: u32 = 300;

#[derive(Debug, Clone, Default)]
pub struct FirestoreOptions {
    pub project_id: Option<String>,
    pub database_id: Option<String>,
    pub collection: Option<String>,
    pub credentials_file: Option<std::path::PathBuf>,
    /// `host:port` of an emulator, which needs no credentials. Defaults to `FIRESTORE_EMULATOR_HOST`.
    pub emulator_host: Option<String>,
}

enum Auth {
    Emulator,
    Google(Arc<dyn gcp_auth::TokenProvider>),
}

/// Firestore over its REST API. Everything lives under one document, `<collection>/gateway`,
/// so several gateways can share a database by choosing different collections.
#[derive(Clone)]
pub struct FirestoreStore {
    inner: Arc<Inner>,
}

struct Inner {
    http: reqwest::Client,
    api: String,
    database: String,
    root: String,
    auth: Auth,
}

struct Doc {
    fields: Map<String, Value>,
    update_time: String,
}

impl FirestoreStore {
    pub async fn open(options: FirestoreOptions) -> Result<Self> {
        let emulator = options
            .emulator_host
            .clone()
            .or_else(|| std::env::var("FIRESTORE_EMULATOR_HOST").ok())
            .filter(|host| !host.trim().is_empty());
        let (auth, api) = match &emulator {
            Some(host) => (Auth::Emulator, format!("http://{host}/v1")),
            None => {
                let provider: Arc<dyn gcp_auth::TokenProvider> = match &options.credentials_file {
                    Some(path) => Arc::new(
                        gcp_auth::CustomServiceAccount::from_file(path).map_err(auth_error)?,
                    ),
                    None => gcp_auth::provider().await.map_err(auth_error)?,
                };
                (
                    Auth::Google(provider),
                    "https://firestore.googleapis.com/v1".to_owned(),
                )
            }
        };
        let project = match (&options.project_id, &auth) {
            (Some(project), _) => project.clone(),
            (None, Auth::Google(provider)) => {
                provider.project_id().await.map_err(auth_error)?.to_string()
            }
            (None, Auth::Emulator) => "demo-murtaugh".to_owned(),
        };
        let database_id = options
            .database_id
            .clone()
            .unwrap_or_else(|| "(default)".to_owned());
        let collection = options
            .collection
            .clone()
            .unwrap_or_else(|| DEFAULT_COLLECTION.to_owned());
        let http = reqwest::Client::builder()
            .timeout(Duration::from_secs(30))
            .build()
            .map_err(|err| StoreError::Firestore(err.to_string()))?;
        let database = format!("projects/{project}/databases/{database_id}");
        Ok(Self {
            inner: Arc::new(Inner {
                http,
                root: format!("{database}/documents/{collection}/gateway"),
                api,
                database,
                auth,
            }),
        })
    }

    fn name(&self, relative: &str) -> String {
        if relative.is_empty() {
            self.inner.root.clone()
        } else {
            format!("{}/{relative}", self.inner.root)
        }
    }

    async fn request(
        &self,
        method: reqwest::Method,
        url: &str,
        body: Option<Value>,
    ) -> Result<Option<Value>> {
        let inner = &self.inner;
        let mut request = inner.http.request(method, url);
        request = match &inner.auth {
            Auth::Emulator => request.bearer_auth("owner"),
            Auth::Google(provider) => {
                let token = provider.token(&[SCOPE]).await.map_err(auth_error)?;
                request.bearer_auth(token.as_str())
            }
        };
        if let Some(body) = body {
            request = request.json(&body);
        }
        let response = request
            .send()
            .await
            .map_err(|err| StoreError::Firestore(err.to_string()))?;
        let status = response.status();
        if status == reqwest::StatusCode::NOT_FOUND {
            return Ok(None);
        }
        let text = response
            .text()
            .await
            .map_err(|err| StoreError::Firestore(err.to_string()))?;
        if !status.is_success() {
            let reason = serde_json::from_str::<Value>(&text)
                .ok()
                .and_then(|body| body["error"]["status"].as_str().map(str::to_owned))
                .unwrap_or_default();
            if matches!(
                reason.as_str(),
                "ALREADY_EXISTS" | "FAILED_PRECONDITION" | "ABORTED"
            ) || status == reqwest::StatusCode::CONFLICT
            {
                return Err(StoreError::Conflict(format!("{status} {reason}")));
            }
            return Err(StoreError::Firestore(format!("{status}: {text}")));
        }
        if text.trim().is_empty() {
            return Ok(Some(Value::Null));
        }
        serde_json::from_str(&text)
            .map(Some)
            .map_err(|err| StoreError::Firestore(format!("undecodable reply: {err}")))
    }

    async fn get(&self, relative: &str) -> Result<Option<Doc>> {
        let url = format!("{}/{}", self.inner.api, self.name(relative));
        Ok(self
            .request(reqwest::Method::GET, &url, None)
            .await?
            .map(doc))
    }

    async fn list(&self, collection: &str) -> Result<Vec<(String, Doc)>> {
        let mut out = Vec::new();
        let mut page: Option<String> = None;
        loop {
            let mut url = format!(
                "{}/{}/{collection}?pageSize={PAGE_SIZE}",
                self.inner.api, self.inner.root
            );
            if let Some(token) = &page {
                url.push_str("&pageToken=");
                url.push_str(token);
            }
            let Some(body) = self.request(reqwest::Method::GET, &url, None).await? else {
                return Ok(out);
            };
            for document in body["documents"].as_array().cloned().unwrap_or_default() {
                let id = document["name"]
                    .as_str()
                    .and_then(|name| name.rsplit('/').next())
                    .unwrap_or_default()
                    .to_owned();
                out.push((id, doc(document)));
            }
            match body["nextPageToken"].as_str() {
                Some(token) if !token.is_empty() => page = Some(token.to_owned()),
                _ => return Ok(out),
            }
        }
    }

    async fn commit(&self, writes: Vec<Value>) -> Result<Value> {
        let url = format!(
            "{}/{}/documents:commit",
            self.inner.api, self.inner.database
        );
        self.request(
            reqwest::Method::POST,
            &url,
            Some(json!({ "writes": writes })),
        )
        .await?
        .ok_or_else(|| StoreError::Firestore("commit answered 404".into()))
    }

    fn update(&self, relative: &str, fields: Value) -> Value {
        json!({"update": {"name": self.name(relative), "fields": fields}})
    }

    /// Writes one field of a person's settings and leaves the others as they are.
    async fn set_user_field(&self, user: &UserId, field: &str, value: Value) -> Result<()> {
        let mut write = self.update(
            &format!("users/{user}"),
            json!({"user_id": string(user.as_str()), field: value}),
        );
        write["updateMask"] = json!({"fieldPaths": ["user_id", field]});
        self.commit(vec![write]).await.map(drop)
    }

    /// An array transform, so two approvals landing together cannot drop each other's tool.
    async fn change_whitelist(&self, user: &UserId, tool: &str, transform: &str) -> Result<()> {
        let mut write = self.update(
            &format!("users/{user}"),
            json!({"user_id": string(user.as_str())}),
        );
        write["updateMask"] = json!({"fieldPaths": ["user_id"]});
        write["updateTransforms"] = json!([{
            "fieldPath": "tool_whitelist",
            transform: {"values": [string(tool)]},
        }]);
        self.commit(vec![write]).await.map(drop)
    }

    fn delete(&self, relative: &str) -> Value {
        json!({"delete": self.name(relative)})
    }

    async fn query_equal(
        &self,
        collection: &str,
        field: &str,
        value: &str,
    ) -> Result<Vec<(String, Doc)>> {
        let url = format!("{}/{}:runQuery", self.inner.api, self.inner.root);
        let query = json!({"structuredQuery": {
            "from": [{"collectionId": collection}],
            "where": {"fieldFilter": {"field": {"fieldPath": field}, "op": "EQUAL", "value": {"stringValue": value}}},
        }});
        let body = self
            .request(reqwest::Method::POST, &url, Some(query))
            .await?
            .unwrap_or(Value::Null);
        Ok(body
            .as_array()
            .cloned()
            .unwrap_or_default()
            .into_iter()
            .filter_map(|row| row.get("document").cloned())
            .map(|document| {
                let id = document["name"]
                    .as_str()
                    .and_then(|name| name.rsplit('/').next())
                    .unwrap_or_default()
                    .to_owned();
                (id, doc(document))
            })
            .collect())
    }

    async fn read_with_time(&self, relative: &str) -> Result<(Option<Doc>, OffsetDateTime)> {
        let url = format!(
            "{}/{}/documents:batchGet",
            self.inner.api, self.inner.database
        );
        let body = self
            .request(
                reqwest::Method::POST,
                &url,
                Some(json!({"documents": [self.name(relative)]})),
            )
            .await?
            .unwrap_or(Value::Null);
        let row = body
            .as_array()
            .and_then(|rows| rows.first())
            .cloned()
            .unwrap_or(Value::Null);
        let read_time = row["readTime"]
            .as_str()
            .and_then(|raw| OffsetDateTime::parse(raw, &Rfc3339).ok())
            .ok_or_else(|| StoreError::Firestore("batchGet gave no read time".into()))?;
        Ok((row.get("found").cloned().map(doc), read_time))
    }
}

fn auth_error(err: gcp_auth::Error) -> StoreError {
    StoreError::Firestore(format!("Google credentials: {err}"))
}

fn doc(document: Value) -> Doc {
    Doc {
        fields: document["fields"].as_object().cloned().unwrap_or_default(),
        update_time: document["updateTime"]
            .as_str()
            .unwrap_or_default()
            .to_owned(),
    }
}

fn user_of(user: UserId, doc: &Doc) -> Result<UserConfig> {
    let tool_mode = match doc
        .fields
        .get("tool_mode")
        .and_then(|v| v["stringValue"].as_str())
    {
        Some(raw) => raw
            .parse()
            .map_err(|err: crate::ToolModeError| StoreError::Corrupt(err.to_string()))?,
        None => ToolMode::default(),
    };
    let whitelist = doc
        .fields
        .get("tool_whitelist")
        .and_then(|v| v["arrayValue"]["values"].as_array())
        .map(|values| {
            values
                .iter()
                .filter_map(|v| v["stringValue"].as_str().map(str::to_owned))
                .collect()
        })
        .unwrap_or_default();
    Ok(UserConfig {
        user,
        allowed: doc.bool("allowed"),
        tool_mode,
        whitelist,
    })
}

fn string(value: &str) -> Value {
    json!({"stringValue": value})
}

fn timestamp(at: OffsetDateTime) -> Result<Value> {
    Ok(
        json!({"timestampValue": at.format(&Rfc3339).map_err(|err| StoreError::Corrupt(err.to_string()))?}),
    )
}

impl Doc {
    fn string(&self, field: &str) -> Result<String> {
        self.fields
            .get(field)
            .and_then(|value| value["stringValue"].as_str())
            .map(str::to_owned)
            .ok_or_else(|| StoreError::Corrupt(format!("field {field} is missing")))
    }

    fn user(&self, field: &str) -> Result<UserId> {
        UserId::parse(&self.string(field)?).map_err(|err| StoreError::Corrupt(err.to_string()))
    }

    fn bool(&self, field: &str) -> bool {
        self.fields
            .get(field)
            .and_then(|value| value["booleanValue"].as_bool())
            .unwrap_or(false)
    }

    fn time(&self, field: &str) -> Result<Option<OffsetDateTime>> {
        match self
            .fields
            .get(field)
            .and_then(|value| value["timestampValue"].as_str())
        {
            None => Ok(None),
            Some(raw) => OffsetDateTime::parse(raw, &Rfc3339)
                .map(Some)
                .map_err(|err| StoreError::Corrupt(format!("{field}: {err}"))),
        }
    }

    fn required_time(&self, field: &str) -> Result<OffsetDateTime> {
        self.time(field)?
            .ok_or_else(|| StoreError::Corrupt(format!("field {field} is missing")))
    }

    fn integer(&self, field: &str) -> Option<i64> {
        let value = self.fields.get(field)?;
        value["integerValue"]
            .as_str()
            .and_then(|raw| raw.parse().ok())
            .or_else(|| value["integerValue"].as_i64())
    }
}

fn pin_id(conversation: &Conversation) -> String {
    format!("pins/{}-{}", conversation.channel, conversation.thread_ts)
}

fn pin_of(doc: &Doc) -> Result<Pin> {
    Ok(Pin {
        conversation: Conversation {
            channel: doc.string("channel")?,
            thread_ts: doc.string("thread_ts")?,
        },
        node: doc.string("node")?,
        session_id: doc.string("session_id")?,
        user: doc.user("user_id")?,
        pinned_at: doc.required_time("pinned_at")?,
    })
}

fn token_of(doc: &Doc) -> Result<NodeToken> {
    Ok(NodeToken {
        selector: doc.string("selector")?,
        secret_hash: doc.string("secret_hash")?,
        owner: doc.user("owner")?,
        name: doc.string("name")?,
        created_at: doc.required_time("created_at")?,
        revoked_at: doc.time("revoked_at")?,
        disabled_at: doc.time("disabled_at")?,
    })
}

fn grant_of(doc: &Doc) -> Result<Grant> {
    Ok(Grant {
        user: doc.user("user_id")?,
        approved_by: doc.user("approved_by")?,
        approved_at: doc.required_time("approved_at")?,
    })
}

#[async_trait]
impl Store for FirestoreStore {
    async fn admin(&self) -> Result<Option<UserId>> {
        match self.get("").await? {
            Some(doc) if doc.fields.contains_key("admin") => Ok(Some(doc.user("admin")?)),
            _ => Ok(None),
        }
    }

    async fn set_admin(&self, user: &UserId) -> Result<()> {
        let mut write = self.update("", json!({"admin": string(user.as_str())}));
        write["updateMask"] = json!({"fieldPaths": ["admin"]});
        self.commit(vec![write]).await.map(drop)
    }

    async fn grants(&self) -> Result<Vec<Grant>> {
        let mut grants = self
            .list("grants")
            .await?
            .iter()
            .map(|(_, doc)| grant_of(doc))
            .collect::<Result<Vec<_>>>()?;
        grants.sort_by_key(|grant| grant.approved_at);
        Ok(grants)
    }

    async fn approve(&self, user: &UserId, by: &UserId) -> Result<Grant> {
        let relative = format!("grants/{user}");
        let mut write = self.update(
            &relative,
            json!({
                "user_id": string(user.as_str()),
                "approved_by": string(by.as_str()),
                "approved_at": timestamp(OffsetDateTime::now_utc())?,
            }),
        );
        write["currentDocument"] = json!({"exists": false});
        match self.commit(vec![write]).await {
            Ok(_) | Err(StoreError::Conflict(_)) => {}
            Err(err) => return Err(err),
        }
        let doc = self
            .get(&relative)
            .await?
            .ok_or_else(|| StoreError::Corrupt(format!("grant for {user} vanished")))?;
        grant_of(&doc)
    }

    async fn revoke(&self, user: &UserId) -> Result<bool> {
        let relative = format!("grants/{user}");
        let Some(doc) = self.get(&relative).await? else {
            return Ok(false);
        };
        let mut write = self.delete(&relative);
        write["currentDocument"] = json!({"updateTime": doc.update_time});
        match self.commit(vec![write]).await {
            Ok(_) => Ok(true),
            Err(StoreError::Conflict(_)) => Ok(false),
            Err(err) => Err(err),
        }
    }

    async fn users(&self) -> Result<Vec<UserConfig>> {
        let mut users = self
            .list("users")
            .await?
            .iter()
            .map(|(_, doc)| user_of(doc.user("user_id")?, doc))
            .collect::<Result<Vec<_>>>()?;
        users.sort_by(|a, b| a.user.cmp(&b.user));
        Ok(users)
    }

    async fn user(&self, user: &UserId) -> Result<UserConfig> {
        match self.get(&format!("users/{user}")).await? {
            Some(doc) => user_of(user.clone(), &doc),
            None => Ok(UserConfig::new(user.clone())),
        }
    }

    async fn set_allowed(&self, user: &UserId, allowed: bool) -> Result<()> {
        self.set_user_field(user, "allowed", json!({"booleanValue": allowed}))
            .await
    }

    async fn set_tool_mode(&self, user: &UserId, mode: ToolMode) -> Result<()> {
        self.set_user_field(user, "tool_mode", string(mode.as_str()))
            .await
    }

    async fn whitelist_tool(&self, user: &UserId, tool: &str) -> Result<bool> {
        let before = self.user(user).await?.whitelist.contains(tool);
        self.change_whitelist(user, tool, "appendMissingElements")
            .await?;
        Ok(!before)
    }

    async fn unwhitelist_tool(&self, user: &UserId, tool: &str) -> Result<bool> {
        let before = self.user(user).await?.whitelist.contains(tool);
        self.change_whitelist(user, tool, "removeAllFromArray")
            .await?;
        Ok(before)
    }

    async fn node_tokens(&self) -> Result<Vec<NodeToken>> {
        let mut tokens = self
            .list("node_tokens")
            .await?
            .iter()
            .map(|(_, doc)| token_of(doc))
            .collect::<Result<Vec<_>>>()?;
        tokens.sort_by_key(|token| token.created_at);
        Ok(tokens)
    }

    async fn add_node_token(&self, token: &NodeToken) -> Result<()> {
        let mut fields = json!({
            "selector": string(&token.selector),
            "secret_hash": string(&token.secret_hash),
            "owner": string(token.owner.as_str()),
            "name": string(&token.name),
            "created_at": timestamp(token.created_at)?,
        });
        if let Some(revoked_at) = token.revoked_at {
            fields["revoked_at"] = timestamp(revoked_at)?;
        }
        if let Some(disabled_at) = token.disabled_at {
            fields["disabled_at"] = timestamp(disabled_at)?;
        }
        let mut write = self.update(&format!("node_tokens/{}", token.selector), fields);
        write["currentDocument"] = json!({"exists": false});
        self.commit(vec![write]).await.map(drop)
    }

    async fn revoke_node_token(&self, selector: &str) -> Result<bool> {
        let relative = format!("node_tokens/{selector}");
        let Some(doc) = self.get(&relative).await? else {
            return Ok(false);
        };
        if doc.fields.contains_key("revoked_at") {
            return Ok(false);
        }
        let mut write = self.update(
            &relative,
            json!({"revoked_at": timestamp(OffsetDateTime::now_utc())?}),
        );
        write["updateMask"] = json!({"fieldPaths": ["revoked_at"]});
        write["currentDocument"] = json!({"updateTime": doc.update_time});
        match self.commit(vec![write]).await {
            Ok(_) => Ok(true),
            Err(StoreError::Conflict(_)) => Ok(false),
            Err(err) => Err(err),
        }
    }

    async fn set_node_disabled(&self, selector: &str, disabled: bool) -> Result<()> {
        let value = match disabled {
            true => timestamp(OffsetDateTime::now_utc())?,
            false => json!({"nullValue": null}),
        };
        let mut write = self.update(
            &format!("node_tokens/{selector}"),
            json!({"disabled_at": value}),
        );
        write["updateMask"] = json!({"fieldPaths": ["disabled_at"]});
        self.commit(vec![write]).await.map(drop)
    }

    async fn pin(&self, conversation: &Conversation) -> Result<Option<Pin>> {
        self.get(&pin_id(conversation))
            .await?
            .map(|doc| pin_of(&doc))
            .transpose()
    }

    async fn set_pin(&self, pin: &Pin) -> Result<()> {
        let write = self.update(
            &pin_id(&pin.conversation),
            json!({
                "channel": string(&pin.conversation.channel),
                "thread_ts": string(&pin.conversation.thread_ts),
                "node": string(&pin.node),
                "session_id": string(&pin.session_id),
                "user_id": string(pin.user.as_str()),
                "pinned_at": timestamp(pin.pinned_at)?,
            }),
        );
        self.commit(vec![write]).await.map(drop)
    }

    async fn remove_pin(&self, conversation: &Conversation) -> Result<()> {
        self.commit(vec![self.delete(&pin_id(conversation))])
            .await
            .map(drop)
    }

    async fn remove_pins_on(&self, node: &str) -> Result<Vec<Conversation>> {
        let pinned = self.query_equal("pins", "node", node).await?;
        let mut removed = Vec::with_capacity(pinned.len());
        for (_, doc) in pinned {
            let pin = pin_of(&doc)?;
            let mut write = self.delete(&pin_id(&pin.conversation));
            write["currentDocument"] = json!({"updateTime": doc.update_time});
            match self.commit(vec![write]).await {
                Ok(_) => removed.push(pin.conversation),
                Err(StoreError::Conflict(_)) => {}
                Err(err) => return Err(err),
            }
        }
        Ok(removed)
    }

    async fn pins(&self) -> Result<Vec<Pin>> {
        self.list("pins")
            .await?
            .iter()
            .map(|(_, doc)| pin_of(doc))
            .collect()
    }
}

const LEASE: &str = "leases/leader";

/// A renewed lease on one document. Every write is guarded by the update time of the read that
/// justified it, so of two gateways racing for an expired lease exactly one wins.
pub struct FirestoreLeader {
    store: FirestoreStore,
    ttl: Duration,
}

impl FirestoreLeader {
    pub fn new(store: FirestoreStore, ttl: Duration) -> Self {
        Self { store, ttl }
    }

    fn fields(&self, holder: &str) -> Value {
        json!({
            "holder": string(holder),
            "lease_seconds": {"integerValue": self.ttl.as_secs().max(1).to_string()},
            "released": {"booleanValue": false},
        })
    }

    async fn write(&self, holder: &str, precondition: Value) -> Result<Option<Lease>> {
        let mut write = self.store.update(LEASE, self.fields(holder));
        write["currentDocument"] = precondition;
        write["updateTransforms"] =
            json!([{"fieldPath": "acquired_at", "setToServerValue": "REQUEST_TIME"}]);
        match self.store.commit(vec![write]).await {
            Ok(result) => {
                let version = result["writeResults"][0]["updateTime"]
                    .as_str()
                    .unwrap_or_default()
                    .to_owned();
                Ok(Some(Lease {
                    holder: holder.to_owned(),
                    version,
                }))
            }
            Err(StoreError::Conflict(_)) => Ok(None),
            Err(err) => Err(err),
        }
    }
}

#[async_trait]
impl Leader for FirestoreLeader {
    fn ttl(&self) -> Duration {
        self.ttl
    }

    async fn acquire(&self, holder: &str) -> Result<Option<Lease>> {
        let (current, read_time) = self.store.read_with_time(LEASE).await?;
        let Some(doc) = current else {
            return self.write(holder, json!({"exists": false})).await;
        };
        let held_by_me = doc.string("holder").ok().as_deref() == Some(holder);
        let expired = doc.bool("released")
            || match (doc.time("acquired_at")?, doc.integer("lease_seconds")) {
                (Some(acquired), Some(seconds)) => {
                    read_time >= acquired + time::Duration::seconds(seconds)
                }
                _ => true,
            };
        if held_by_me || expired {
            self.write(holder, json!({"updateTime": doc.update_time}))
                .await
        } else {
            Ok(None)
        }
    }

    async fn renew(&self, lease: &Lease) -> Result<Option<Lease>> {
        self.write(&lease.holder, json!({"updateTime": lease.version}))
            .await
    }

    async fn release(&self, lease: Lease) -> Result<()> {
        let mut write = self
            .store
            .update(LEASE, json!({"released": {"booleanValue": true}}));
        write["updateMask"] = json!({"fieldPaths": ["released"]});
        write["currentDocument"] = json!({"updateTime": lease.version});
        match self.store.commit(vec![write]).await {
            Ok(_) | Err(StoreError::Conflict(_)) => Ok(()),
            Err(err) => Err(err),
        }
    }
}
