//! Who may do what, as `rax-gateway` reads it with Murtaugh's token prefixes.

use std::sync::Arc;

use murtaugh_store::{Store, StoreError};

pub use rax_gateway::access::*;

pub async fn load(store: &dyn Store) -> Result<Snapshot, StoreError> {
    Snapshot::load(store, crate::settings()).await
}

pub fn reloading(snapshot: Snapshot, store: Arc<dyn Store>) -> Access {
    Access::reloading(snapshot, store, crate::settings().clone())
}
