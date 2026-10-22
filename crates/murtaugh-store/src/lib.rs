//! The gateway's configuration: who administers it, who may join, the nodes they run and which
//! node each conversation is pinned to. All but the pins is `rax-gateway`'s; Murtaugh adds the
//! Slack threads, and holds every id to Slack's shape.

mod firestore;
mod model;
mod sqlite;

use async_trait::async_trait;

pub use firestore::{FirestoreLeader, FirestoreOptions, FirestoreStore};
pub use model::{Conversation, Pin, SlackIds, slack_ids, slack_user};
pub use rax_gateway::store::Store as GatewayStore;
pub use rax_gateway::store::{
    Grant, Leader, LeaderLock, Lease, NodeToken, Result, Scope, ScopeError, SqliteLeader,
    StoreError, ToolMode, ToolModeError, UserConfig, UserId, UserIdError, UserToken,
};
pub use sqlite::SqliteStore;

/// The gateway's store, and the Slack threads pinned to its nodes.
#[async_trait]
pub trait Store: GatewayStore {
    async fn pin(&self, conversation: &Conversation) -> Result<Option<Pin>>;
    async fn set_pin(&self, pin: &Pin) -> Result<()>;
    async fn remove_pin(&self, conversation: &Conversation) -> Result<()>;
    /// Returns the conversations that lost their pin.
    async fn remove_pins_on(&self, node: &str) -> Result<Vec<Conversation>>;
    async fn pins(&self) -> Result<Vec<Pin>>;
}
