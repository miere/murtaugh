//! The RAX side, which is `rax-gateway`'s hub served with Murtaugh's Slack files, tools and cards.

use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use murtaugh_store::Store;
use rax_gateway::Embedding;
use tokio_util::sync::CancellationToken;

use crate::access::Access;
use crate::files::Files;
use crate::fleet::Fleet;
use crate::owner::SlackOwner;
use crate::relay::Relay;
use crate::signin::SignIns;
use crate::tools::{Lent, SlackTools, Tools};

pub use rax_gateway::hub::{
    Background, FleetChange, Hub, INITIALIZE_TIMEOUT, REFRESH, RETAIN_FOR, apply,
};

/// What Murtaugh brings to the hub: Slack files, the `slack` group, and the owner's DM.
pub fn embedding(files: &Files, tools: &Tools, lent: &Lent, owner: SlackOwner) -> Embedding {
    Embedding {
        settings: crate::settings().clone(),
        owner: Arc::new(owner),
        files: Arc::new(files.clone()),
        tools: Arc::new(SlackTools::new(tools.clone(), lent.clone(), files.clone())),
    }
}

/// Every collaborator a node's link is served with; bundling them would only move the list.
#[allow(clippy::too_many_arguments)]
pub async fn start(
    listen: SocketAddr,
    retain_for: Duration,
    access: Access,
    fleet: Fleet,
    files: Files,
    tools: Tools,
    lent: Lent,
    sign_ins: SignIns,
    relay: Relay,
    http: axum::Router,
    shutdown: CancellationToken,
) -> std::io::Result<Hub> {
    let embedding = embedding(&files, &tools, &lent, SlackOwner::new(sign_ins, None));
    rax_gateway::hub::start(
        listen,
        retain_for,
        access,
        fleet,
        files.transfers(),
        lent.gateway(),
        relay,
        embedding,
        http,
        shutdown,
    )
    .await
}

/// Reloads access every `every` and closes any node or client that lost it.
pub async fn refresh(
    store: Arc<dyn Store>,
    access: Access,
    fleet: Fleet,
    relay: Relay,
    every: Duration,
    shutdown: CancellationToken,
) {
    rax_gateway::hub::refresh(
        store,
        crate::settings().clone(),
        access,
        fleet,
        relay,
        every,
        shutdown,
    )
    .await;
}
