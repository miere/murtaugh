//! The RAX side: accepts nodes, introduces itself, keeps the fleet current and closes the links of
//! nodes whose token or owner loses access.

use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use murtaugh_store::Store;
use rax::session::{GatewayCapabilities, Initialize, PROTOCOL_VERSION};
use rax::{Error, ErrorKind, GatewayCall, GatewayReply, NodeCall, NodeReply};
use rax_tokio::gateway::{
    GatewayConfig, GatewayLink, GatewayServer, LinkEvent, LinkEvents, NewLink, NewLinks,
};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::access::{Access, Snapshot};
use crate::fleet::{Fleet, Node};

pub const REFRESH: Duration = Duration::from_secs(5);
pub const INITIALIZE_TIMEOUT: Duration = Duration::from_secs(30);

/// What the chat side must hear about: a node that left takes its conversations' pins with it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum FleetChange {
    Attached { selector: String },
    Gone { selector: String },
}

pub fn capabilities() -> GatewayCapabilities {
    GatewayCapabilities {
        question: false,
        plan: false,
        sign_in: false,
        resource_schemes: vec!["chat".into()],
        readable_schemes: Vec::new(),
    }
}

pub struct Hub {
    pub server: GatewayServer,
    pub changes: mpsc::Receiver<FleetChange>,
}

pub async fn start(
    listen: SocketAddr,
    access: Access,
    fleet: Fleet,
    shutdown: CancellationToken,
) -> std::io::Result<Hub> {
    let (server, new_links) =
        GatewayServer::bind(listen, access.clone(), GatewayConfig::default()).await?;
    let (changes, receiver) = mpsc::channel(64);
    tokio::spawn(accept(new_links, access, fleet, changes, shutdown));
    Ok(Hub {
        server,
        changes: receiver,
    })
}

/// Reloads access every `every` and closes any node that lost it.
pub async fn refresh(
    store: Arc<dyn Store>,
    access: Access,
    fleet: Fleet,
    every: Duration,
    shutdown: CancellationToken,
) {
    loop {
        tokio::select! {
            () = tokio::time::sleep(every) => {}
            () = shutdown.cancelled() => return,
        }
        let snapshot = match Snapshot::load(&*store).await {
            Ok(snapshot) => snapshot,
            Err(err) => {
                tracing::warn!(error = %err, "could not reload access; keeping the last known rules");
                continue;
            }
        };
        for selector in access.replace(snapshot) {
            if let Some(node) = fleet.get(&selector) {
                tracing::info!(node = %node.name, owner = %node.owner, "access revoked; closing the node's link");
                node.link.close().await;
            }
        }
    }
}

async fn accept(
    mut new_links: NewLinks,
    access: Access,
    fleet: Fleet,
    changes: mpsc::Sender<FleetChange>,
    shutdown: CancellationToken,
) {
    loop {
        let new_link = tokio::select! {
            new_link = new_links.recv() => new_link,
            () = shutdown.cancelled() => return,
        };
        let Some(NewLink { link, events }) = new_link else {
            return;
        };
        tokio::spawn(serve(
            link,
            events,
            access.clone(),
            fleet.clone(),
            changes.clone(),
        ));
    }
}

async fn serve(
    link: GatewayLink,
    mut events: LinkEvents,
    access: Access,
    fleet: Fleet,
    changes: mpsc::Sender<FleetChange>,
) {
    let selector = link.identity().0.clone();
    let snapshot = access.snapshot();
    let (Some(owner), Some(name)) = (
        snapshot.owner(&selector).cloned(),
        snapshot.node_name(&selector).map(str::to_owned),
    ) else {
        link.close().await;
        return;
    };
    let capabilities = match initialize(&link).await {
        Ok(capabilities) => capabilities,
        Err(reason) => {
            tracing::warn!(node = %name, %reason, "node did not initialize; closing its link");
            link.close().await;
            return;
        }
    };
    tracing::info!(node = %name, %owner, tool_gate = ?capabilities.tool_gate, "node attached");
    let attachment = fleet.attach(Node {
        selector: selector.clone(),
        owner,
        name: name.clone(),
        link: link.clone(),
        capabilities,
        connected: true,
    });
    let _ = changes
        .send(FleetChange::Attached {
            selector: selector.clone(),
        })
        .await;
    while let Some(event) = events.recv().await {
        match event {
            LinkEvent::Closed | LinkEvent::Replaced => break,
            LinkEvent::Disconnected { reason } => {
                tracing::info!(node = %name, %reason, "node disconnected; holding its link for a resume");
                fleet.set_connected(&selector, attachment, false);
            }
            LinkEvent::Resumed => {
                tracing::info!(node = %name, "node resumed");
                fleet.set_connected(&selector, attachment, true);
            }
            LinkEvent::Request { id, call } => answer(&link, &name, id, call).await,
            LinkEvent::Background { session_id, .. } => {
                tracing::debug!(node = %name, %session_id, "background event with no turn open");
            }
            LinkEvent::Attachment { transfer_id, .. } => {
                tracing::debug!(node = %name, %transfer_id, "attachment outside a turn dropped");
            }
            LinkEvent::TransferFailed {
                transfer_id,
                reason,
            } => {
                tracing::debug!(node = %name, %transfer_id, %reason, "transfer failed");
            }
            LinkEvent::Unhandled { body, .. } => {
                tracing::warn!(node = %name, subject = ?body.subject, reason = ?body.reason, "node could not handle something the gateway sent");
            }
        }
    }
    if fleet.detach(&selector, attachment) {
        tracing::info!(node = %name, "node detached");
        let _ = changes.send(FleetChange::Gone { selector }).await;
    }
}

async fn initialize(link: &GatewayLink) -> Result<rax::session::NodeCapabilities, String> {
    let offer = GatewayCall::Initialize(Initialize {
        protocol_version: PROTOCOL_VERSION,
        capabilities: capabilities(),
    });
    let pending = link.call(offer).await.map_err(|err| err.to_string())?;
    let reply = tokio::time::timeout(INITIALIZE_TIMEOUT, pending.reply)
        .await
        .map_err(|_| "no answer to initialize".to_owned())?
        .map_err(|err| err.to_string())?;
    match reply {
        GatewayReply::Initialize(initialized)
            if initialized.protocol_version == PROTOCOL_VERSION =>
        {
            Ok(initialized.capabilities)
        }
        GatewayReply::Initialize(initialized) => Err(format!(
            "the node speaks RAX {}, this gateway speaks {PROTOCOL_VERSION}",
            initialized.protocol_version
        )),
        other => Err(format!("answered initialize with {other:?}")),
    }
}

async fn answer(link: &GatewayLink, name: &str, id: rax::id::RequestId, call: NodeCall) {
    let sent = match call {
        NodeCall::CredentialHealth(health) => {
            tracing::info!(node = %name, credential = %health.credential, degraded = health.degraded, reason = ?health.reason, "node credential health");
            link.reply(id, NodeReply::CredentialHealth).await
        }
        NodeCall::SignIn(_) | NodeCall::SignInSettled(_) => {
            let unsupported = Error::new(
                ErrorKind::Unsupported,
                "this gateway cannot show sign-ins yet",
            );
            link.fault(id, unsupported).await
        }
        NodeCall::ReadResource(read) => {
            let gone = Error::new(
                ErrorKind::Forbidden,
                format!("this gateway never sent {}", read.uri),
            );
            link.fault(id, gone).await
        }
    };
    if let Err(err) = sent {
        tracing::debug!(node = %name, error = %err, "could not answer a node call");
    }
}
