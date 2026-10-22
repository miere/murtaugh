use std::path::Path;

use async_trait::async_trait;
use rax_gateway::store as gateway;
use rusqlite::{OptionalExtension, Row, params};
use time::OffsetDateTime;
use time::format_description::well_known::Rfc3339;

use crate::{
    Conversation, GatewayStore, Grant, LeaderLock, NodeToken, Pin, Result, Store, StoreError,
    ToolMode, UserConfig, UserId, UserToken, slack_ids,
};

const PINS: &str = "
CREATE TABLE IF NOT EXISTS pins (
    channel TEXT NOT NULL,
    thread_ts TEXT NOT NULL,
    node TEXT NOT NULL,
    session_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    pinned_at TEXT NOT NULL,
    PRIMARY KEY (channel, thread_ts)
);
CREATE INDEX IF NOT EXISTS pins_by_node ON pins (node);
";

const PIN_COLUMNS: &str = "channel, thread_ts, node, session_id, user_id, pinned_at";

/// `rax-gateway`'s database, holding Slack ids only, with the pins beside its tables.
#[derive(Clone)]
pub struct SqliteStore {
    inner: gateway::SqliteStore,
}

impl SqliteStore {
    pub fn open(path: &Path) -> Result<Self> {
        let inner = gateway::SqliteStore::open_with(path, slack_ids())?;
        inner.extend(PINS)?;
        Ok(Self { inner })
    }

    pub fn lock_leader(path: &Path) -> Result<LeaderLock> {
        gateway::SqliteStore::lock_leader(path)
    }
}

fn stamp(at: OffsetDateTime) -> Result<String> {
    at.format(&Rfc3339)
        .map_err(|err| StoreError::Corrupt(err.to_string()))
}

fn parse_stamp(raw: &str) -> rusqlite::Result<OffsetDateTime> {
    OffsetDateTime::parse(raw, &Rfc3339).map_err(|err| {
        rusqlite::Error::FromSqlConversionFailure(
            0,
            rusqlite::types::Type::Text,
            format!("{raw:?}: {err}").into(),
        )
    })
}

fn pin_row(store: &gateway::SqliteStore, row: &Row<'_>) -> rusqlite::Result<Pin> {
    Ok(Pin {
        conversation: Conversation {
            channel: row.get(0)?,
            thread_ts: row.get(1)?,
        },
        node: row.get(2)?,
        session_id: row.get(3)?,
        user: store.row_user_id(&row.get::<_, String>(4)?)?,
        pinned_at: parse_stamp(&row.get::<_, String>(5)?)?,
    })
}

#[async_trait]
impl GatewayStore for SqliteStore {
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
impl Store for SqliteStore {
    async fn pin(&self, conversation: &Conversation) -> Result<Option<Pin>> {
        let conversation = conversation.clone();
        let store = self.inner.clone();
        self.inner
            .run(move |db| {
                Ok(db
                    .query_row(
                        &format!(
                            "SELECT {PIN_COLUMNS} FROM pins WHERE channel = ?1 AND thread_ts = ?2"
                        ),
                        params![conversation.channel, conversation.thread_ts],
                        |row| pin_row(&store, row),
                    )
                    .optional()?)
            })
            .await
    }

    async fn set_pin(&self, pin: &Pin) -> Result<()> {
        let pin = pin.clone();
        let at = stamp(pin.pinned_at)?;
        self.inner
            .run(move |db| {
                db.execute(
                    &format!(
                        "INSERT INTO pins ({PIN_COLUMNS}) VALUES (?1, ?2, ?3, ?4, ?5, ?6)
                     ON CONFLICT (channel, thread_ts) DO UPDATE SET node = excluded.node,
                       session_id = excluded.session_id, user_id = excluded.user_id,
                       pinned_at = excluded.pinned_at"
                    ),
                    params![
                        pin.conversation.channel,
                        pin.conversation.thread_ts,
                        pin.node,
                        pin.session_id,
                        pin.user.as_str(),
                        at
                    ],
                )?;
                Ok(())
            })
            .await
    }

    async fn remove_pin(&self, conversation: &Conversation) -> Result<()> {
        let conversation = conversation.clone();
        self.inner
            .run(move |db| {
                db.execute(
                    "DELETE FROM pins WHERE channel = ?1 AND thread_ts = ?2",
                    params![conversation.channel, conversation.thread_ts],
                )?;
                Ok(())
            })
            .await
    }

    async fn remove_pins_on(&self, node: &str) -> Result<Vec<Conversation>> {
        let node = node.to_owned();
        self.inner
            .run(move |db| {
                let tx = db.transaction()?;
                let removed = {
                    let mut query =
                        tx.prepare("SELECT channel, thread_ts FROM pins WHERE node = ?1")?;
                    query
                        .query_map([&node], |row| {
                            Ok(Conversation {
                                channel: row.get(0)?,
                                thread_ts: row.get(1)?,
                            })
                        })?
                        .collect::<rusqlite::Result<Vec<_>>>()?
                };
                tx.execute("DELETE FROM pins WHERE node = ?1", [&node])?;
                tx.commit()?;
                Ok(removed)
            })
            .await
    }

    async fn pins(&self) -> Result<Vec<Pin>> {
        let store = self.inner.clone();
        self.inner
            .run(move |db| {
                let mut query = db.prepare(&format!("SELECT {PIN_COLUMNS} FROM pins"))?;
                let pins = query
                    .query_map([], |row| pin_row(&store, row))?
                    .collect::<rusqlite::Result<_>>()?;
                Ok(pins)
            })
            .await
    }
}
