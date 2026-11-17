use std::time::Duration;

use async_trait::async_trait;
use rax_gateway::store::firestore::{self as gateway, Document, string_value, timestamp_value};
use serde_json::json;

use crate::{
    Conversation, FirestoreLeader, FirestoreOptions, GatewayStore, Grant, NodeToken, Pin, Result,
    Store, StoreError, ToolMode, UserConfig, UserId, UserToken, slack_ids,
};

/// The collection a gateway keeps its documents in unless the config names another.
pub const DEFAULT_COLLECTION: &str = "murtaugh";

/// `rax-gateway`'s Firestore store, holding Slack ids only, with each pin in
/// `pins/<channel>-<thread_ts>` under the same `<collection>/gateway` document.
#[derive(Clone)]
pub struct FirestoreStore {
    inner: gateway::FirestoreStore,
}

impl FirestoreStore {
    pub async fn open(options: FirestoreOptions) -> Result<Self> {
        let inner = gateway::FirestoreStore::open_with(options, slack_ids()).await?;
        Ok(Self { inner })
    }

    /// The lease on this store's `leases/leader`, held across every gateway sharing it.
    pub fn leader(&self, ttl: Duration) -> FirestoreLeader {
        FirestoreLeader::new(self.inner.clone(), ttl)
    }

    fn pin_of(&self, doc: &Document) -> Result<Pin> {
        Ok(Pin {
            conversation: Conversation {
                channel: doc.string("channel")?,
                thread_ts: doc.string("thread_ts")?,
            },
            node: doc.string("node")?,
            session_id: doc.string("session_id")?,
            user: self.inner.user_id(&doc.string("user_id")?)?,
            pinned_at: doc.required_time("pinned_at")?,
        })
    }
}

fn pin_id(conversation: &Conversation) -> String {
    format!("pins/{}-{}", conversation.channel, conversation.thread_ts)
}

#[async_trait]
impl GatewayStore for FirestoreStore {
    async fn admin(&self) -> Result<Option<UserId>> {
        self.inner.admin().await
    }

    async fn set_admin(&self, user: &UserId) -> Result<()> {
        self.inner.set_admin(user).await
    }

    async fn grants(&self) -> Result<Vec<Grant>> {
        self.inner.grants().await
    }

    async fn approve(&self, user: &UserId, by: &UserId) -> Result<Grant> {
        self.inner.approve(user, by).await
    }

    async fn revoke(&self, user: &UserId) -> Result<bool> {
        self.inner.revoke(user).await
    }

    async fn users(&self) -> Result<Vec<UserConfig>> {
        self.inner.users().await
    }

    async fn user(&self, user: &UserId) -> Result<UserConfig> {
        self.inner.user(user).await
    }

    async fn set_allowed(&self, user: &UserId, allowed: bool) -> Result<()> {
        self.inner.set_allowed(user, allowed).await
    }

    async fn set_tool_mode(&self, user: &UserId, mode: ToolMode) -> Result<()> {
        self.inner.set_tool_mode(user, mode).await
    }

    async fn whitelist_tool(&self, user: &UserId, tool: &str) -> Result<bool> {
        self.inner.whitelist_tool(user, tool).await
    }

    async fn unwhitelist_tool(&self, user: &UserId, tool: &str) -> Result<bool> {
        self.inner.unwhitelist_tool(user, tool).await
    }

    async fn deny_tool(&self, user: &UserId, tool: &str) -> Result<bool> {
        self.inner.deny_tool(user, tool).await
    }

    async fn undeny_tool(&self, user: &UserId, tool: &str) -> Result<bool> {
        self.inner.undeny_tool(user, tool).await
    }

    async fn has_tool_rules(&self, user: &UserId) -> Result<bool> {
        self.inner.has_tool_rules(user).await
    }

    async fn node_tokens(&self) -> Result<Vec<NodeToken>> {
        self.inner.node_tokens().await
    }

    async fn add_node_token(&self, token: &NodeToken) -> Result<()> {
        self.inner.add_node_token(token).await
    }

    async fn revoke_node_token(&self, selector: &str) -> Result<bool> {
        self.inner.revoke_node_token(selector).await
    }

    async fn set_node_disabled(&self, selector: &str, disabled: bool) -> Result<()> {
        self.inner.set_node_disabled(selector, disabled).await
    }

    async fn user_tokens(&self) -> Result<Vec<UserToken>> {
        self.inner.user_tokens().await
    }

    async fn add_user_token(&self, token: &UserToken) -> Result<()> {
        self.inner.add_user_token(token).await
    }

    async fn revoke_user_token(&self, selector: &str) -> Result<bool> {
        self.inner.revoke_user_token(selector).await
    }
}

#[async_trait]
impl Store for FirestoreStore {
    async fn pin(&self, conversation: &Conversation) -> Result<Option<Pin>> {
        self.inner
            .get(&pin_id(conversation))
            .await?
            .map(|doc| self.pin_of(&doc))
            .transpose()
    }

    async fn set_pin(&self, pin: &Pin) -> Result<()> {
        let write = self.inner.update(
            &pin_id(&pin.conversation),
            json!({
                "channel": string_value(&pin.conversation.channel),
                "thread_ts": string_value(&pin.conversation.thread_ts),
                "node": string_value(&pin.node),
                "session_id": string_value(&pin.session_id),
                "user_id": string_value(pin.user.as_str()),
                "pinned_at": timestamp_value(pin.pinned_at)?,
            }),
        );
        self.inner.commit(vec![write]).await.map(drop)
    }

    async fn remove_pin(&self, conversation: &Conversation) -> Result<()> {
        self.inner
            .commit(vec![self.inner.delete(&pin_id(conversation))])
            .await
            .map(drop)
    }

    async fn remove_pins_on(&self, node: &str) -> Result<Vec<Conversation>> {
        let pinned = self.inner.query_equal("pins", "node", node).await?;
        let mut removed = Vec::with_capacity(pinned.len());
        for doc in pinned {
            let pin = self.pin_of(&doc)?;
            let write = self
                .inner
                .delete(&pin_id(&pin.conversation))
                .if_unchanged(doc.update_time());
            match self.inner.commit(vec![write]).await {
                Ok(_) => removed.push(pin.conversation),
                Err(StoreError::Conflict(_)) => {}
                Err(err) => return Err(err),
            }
        }
        Ok(removed)
    }

    async fn pins(&self) -> Result<Vec<Pin>> {
        self.inner
            .list("pins")
            .await?
            .iter()
            .map(|doc| self.pin_of(doc))
            .collect()
    }
}
