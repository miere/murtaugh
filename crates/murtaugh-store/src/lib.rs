//! The gateway's configuration: who administers it, who may join, the nodes they run and which
//! node each conversation is pinned to. Backends differ only in how far their leader lock reaches.

mod firestore;
mod leader;
mod model;
mod sqlite;

use async_trait::async_trait;

pub use firestore::{FirestoreLeader, FirestoreOptions, FirestoreStore};
pub use leader::{Leader, Lease, SqliteLeader};
pub use model::{
    Conversation, Grant, NodeToken, Pin, Scope, ScopeError, ToolMode, ToolModeError, UserConfig,
    UserId, UserIdError, UserToken,
};
pub use sqlite::{LeaderLock, SqliteStore};

#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    #[error("store: {0}")]
    Sqlite(#[from] rusqlite::Error),
    #[error("store: {path}: {source}")]
    Io {
        path: std::path::PathBuf,
        #[source]
        source: std::io::Error,
    },
    /// Another gateway holds this database; running two would answer every Slack event twice.
    #[error("store: another gateway already holds {0}")]
    Locked(std::path::PathBuf),
    #[error("store: firestore: {0}")]
    Firestore(String),
    /// A guarded write lost to a concurrent one; callers that race on purpose treat it as a no.
    #[error("store: a concurrent write won: {0}")]
    Conflict(String),
    #[error("store: a stored value is not valid: {0}")]
    Corrupt(String),
    #[error("store: the background task stopped: {0}")]
    Task(String),
}

pub type Result<T> = std::result::Result<T, StoreError>;

/// Every write is visible to the next read by any process on the same database, which is how a
/// CLI change reaches the live gateway.
#[async_trait]
pub trait Store: Send + Sync + 'static {
    async fn admin(&self) -> Result<Option<UserId>>;
    async fn set_admin(&self, user: &UserId) -> Result<()>;

    async fn grants(&self) -> Result<Vec<Grant>>;
    /// Idempotent: approving an approved user keeps the original approval.
    async fn approve(&self, user: &UserId, by: &UserId) -> Result<Grant>;
    /// Returns whether there was a grant to revoke.
    async fn revoke(&self, user: &UserId) -> Result<bool>;

    async fn users(&self) -> Result<Vec<UserConfig>>;
    async fn user(&self, user: &UserId) -> Result<UserConfig>;
    async fn set_allowed(&self, user: &UserId, allowed: bool) -> Result<()>;
    async fn set_tool_mode(&self, user: &UserId, mode: ToolMode) -> Result<()>;
    /// Returns whether the tool was not on the owner's whitelist before.
    async fn whitelist_tool(&self, user: &UserId, tool: &str) -> Result<bool>;
    /// Returns whether the tool was on the owner's whitelist.
    async fn unwhitelist_tool(&self, user: &UserId, tool: &str) -> Result<bool>;

    async fn node_tokens(&self) -> Result<Vec<NodeToken>>;
    async fn add_node_token(&self, token: &NodeToken) -> Result<()>;
    /// Returns whether a live token was revoked.
    async fn revoke_node_token(&self, selector: &str) -> Result<bool>;
    /// Reversible, unlike revoking: a disabled node keeps its credential and its link, it is just
    /// left out of routing until this is called again with `false`.
    async fn set_node_disabled(&self, selector: &str, disabled: bool) -> Result<()>;

    async fn user_tokens(&self) -> Result<Vec<UserToken>>;
    async fn add_user_token(&self, token: &UserToken) -> Result<()>;
    /// Returns whether a live token was revoked.
    async fn revoke_user_token(&self, selector: &str) -> Result<bool>;

    async fn pin(&self, conversation: &Conversation) -> Result<Option<Pin>>;
    async fn set_pin(&self, pin: &Pin) -> Result<()>;
    async fn remove_pin(&self, conversation: &Conversation) -> Result<()>;
    /// Returns the conversations that lost their pin.
    async fn remove_pins_on(&self, node: &str) -> Result<Vec<Conversation>>;
    async fn pins(&self) -> Result<Vec<Pin>>;
}
