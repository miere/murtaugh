//! The RAX side: accepts nodes, introduces itself, keeps the fleet current and closes the links of
//! nodes whose token or owner loses access.

use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use murtaugh_store::Store;
use rax::Open;
use rax::event::BackgroundEvent;
use rax::id::SessionId;
use rax::open::{Subject, UnhandledReason};
use rax::session::{GatewayCapabilities, Initialize, Initialized, PROTOCOL_VERSION};
use rax::{ErrorKind, GatewayCall, GatewayReply, NodeCall, NodeReply, Unhandled};
use rax_tokio::gateway::{
    GatewayConfig, GatewayLink, GatewayServer, LinkEvent, LinkEvents, NewLink, NewLinks,
};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::access::{Access, Snapshot};
use crate::alerts::{Credentials, Reporter};
use crate::files::Files;
use crate::fleet::{Fleet, Node};
use crate::node_access;
use crate::signin::{self, SignIns};
use crate::tools::Tools;

pub const REFRESH: Duration = Duration::from_secs(5);
pub const INITIALIZE_TIMEOUT: Duration = Duration::from_secs(30);

/// What the chat side must hear about: a node that left takes its conversations' pins with it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum FleetChange {
    Attached { selector: String },
    Gone { selector: String },
}

/// Something a node's session did with no turn open, for the chat side to show in its thread:
/// a background task finishing, the answer the agent wrote about it, or a tool one of its
/// sub-agents wants to run.
#[derive(Debug, Clone)]
pub struct Background {
    pub selector: String,
    pub session_id: SessionId,
    pub event: Open<BackgroundEvent>,
}

pub fn capabilities(tools: &Tools) -> GatewayCapabilities {
    GatewayCapabilities {
        question: true,
        plan: true,
        sign_in: true,
        resource_schemes: vec!["chat".into()],
        readable_schemes: vec![crate::files::SCHEME.into()],
        tools: tools.catalogue(),
    }
}

pub struct Hub {
    pub server: GatewayServer,
    pub changes: mpsc::Receiver<FleetChange>,
    pub backgrounds: mpsc::Receiver<Background>,
}

/// Every collaborator a node's link is served with; bundling them would only move the list.
#[allow(clippy::too_many_arguments)]
pub async fn start(
    listen: SocketAddr,
    access: Access,
    fleet: Fleet,
    files: Files,
    tools: Tools,
    credentials: Credentials,
    sign_ins: SignIns,
    shutdown: CancellationToken,
) -> std::io::Result<Hub> {
    let (server, new_links) =
        GatewayServer::bind(listen, access.clone(), GatewayConfig::default()).await?;
    let (changes, receiver) = mpsc::channel(64);
    let (backgrounds, background_receiver) = mpsc::channel(256);
    let serving = Serving {
        access,
        fleet,
        files,
        tools,
        credentials,
        sign_ins,
        changes,
        backgrounds,
    };
    tokio::spawn(accept(new_links, serving, shutdown));
    Ok(Hub {
        server,
        changes: receiver,
        backgrounds: background_receiver,
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
        apply(&access, &fleet, snapshot).await;
    }
}

/// Puts `snapshot` in place and closes the link of every node it no longer admits. Whoever
/// replaces the snapshot must close these links: the next refresh finds nothing lost.
pub async fn apply(access: &Access, fleet: &Fleet, snapshot: Snapshot) {
    for selector in access.replace(snapshot) {
        if let Some(node) = fleet.get(&selector) {
            tracing::info!(node = %node.name, owner = %node.owner, "access revoked; closing the node's link");
            node.link.close().await;
        }
    }
}

/// What every node's link is served with.
#[derive(Clone)]
struct Serving {
    access: Access,
    fleet: Fleet,
    files: Files,
    tools: Tools,
    credentials: Credentials,
    sign_ins: SignIns,
    changes: mpsc::Sender<FleetChange>,
    backgrounds: mpsc::Sender<Background>,
}

async fn accept(mut new_links: NewLinks, serving: Serving, shutdown: CancellationToken) {
    loop {
        let new_link = tokio::select! {
            new_link = new_links.recv() => new_link,
            () = shutdown.cancelled() => return,
        };
        let Some(NewLink { link, events }) = new_link else {
            return;
        };
        tokio::spawn(serve(link, events, serving.clone()));
    }
}

async fn serve(link: GatewayLink, mut events: LinkEvents, serving: Serving) {
    let Serving {
        access,
        fleet,
        files,
        tools,
        credentials,
        sign_ins,
        changes,
        backgrounds,
    } = serving;
    let selector = link.identity().0.clone();
    let snapshot = access.snapshot();
    let (Some(owner), Some(name)) = (
        snapshot.owner(&selector).cloned(),
        snapshot.node_name(&selector).map(str::to_owned),
    ) else {
        link.close().await;
        return;
    };
    let initialized = match initialize(&link, &tools).await {
        Ok(initialized) => initialized,
        Err(reason) => {
            tracing::warn!(node = %name, %reason, "node did not initialize; closing its link");
            link.close().await;
            return;
        }
    };
    // Info, not an alert: only the node's owner can fix their configuration, and the node tells
    // them why it stopped.
    let read = match node_access::read(&initialized.metadata) {
        Ok(read) => read,
        Err(rejection) => {
            tracing::info!(node = %name, %owner, reason = %rejection.message, "rejected the node's metadata; closing its link");
            link.reject(rejection).await;
            return;
        }
    };
    report_ignored(&link, &name, &read.ignored).await;
    let capabilities = initialized.capabilities;
    tracing::info!(node = %name, %owner, tool_gate = ?capabilities.tool_gate, access = ?read.access, "node attached");
    let attachment = fleet.attach(Node {
        selector: selector.clone(),
        owner: owner.clone(),
        name: name.clone(),
        link: link.clone(),
        capabilities,
        connected: true,
        access: read.access,
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
            LinkEvent::Request {
                id,
                call: NodeCall::ReadResource(read),
            } => {
                let (files, link, selector) = (files.clone(), link.clone(), selector.clone());
                tokio::spawn(async move { files.serve(&link, &selector, id, read).await });
            }
            LinkEvent::Request {
                id,
                call: NodeCall::CallTool(call),
            } => {
                let (tools, link, name) = (tools.clone(), link.clone(), name.clone());
                tokio::spawn(async move { tools.serve(&link, &name, id, call).await });
            }
            LinkEvent::Request {
                id,
                call: NodeCall::CredentialHealth(health),
            } => {
                tracing::info!(node = %name, credential = %health.credential, degraded = health.degraded, reason = ?health.reason, "node credential health");
                let from = Reporter {
                    selector: &selector,
                    name: &name,
                    owner: &owner,
                };
                credentials.report(from, &health).await;
                if let Err(err) = link.reply(id, NodeReply::CredentialHealth).await {
                    tracing::debug!(node = %name, error = %err, "could not answer a node call");
                }
            }
            LinkEvent::Request {
                id,
                call: NodeCall::SignIn(request),
            } => {
                let node = signin::Node {
                    selector: selector.clone(),
                    name: name.clone(),
                    owner: owner.clone(),
                    link: link.clone(),
                };
                let sent = match sign_ins.raise(node, request, None).await {
                    Ok(()) => link.reply(id, NodeReply::SignIn).await,
                    Err(error) => link.fault(id, error).await,
                };
                if let Err(err) = sent {
                    tracing::debug!(node = %name, error = %err, "could not answer a node call");
                }
            }
            LinkEvent::Request {
                id,
                call: NodeCall::SignInSettled(settled),
            } => {
                sign_ins.settle(&selector, settled).await;
                if let Err(err) = link.reply(id, NodeReply::SignInSettled).await {
                    tracing::debug!(node = %name, error = %err, "could not answer a node call");
                }
            }
            LinkEvent::Request {
                id,
                call: NodeCall::UpdateMetadata(update),
            } => match node_access::read(&update.metadata) {
                Ok(read) => {
                    report_ignored(&link, &name, &read.ignored).await;
                    tracing::info!(node = %name, access = ?read.access, "node updated its metadata");
                    fleet.set_access(&selector, attachment, read.access);
                    if let Err(err) = link.reply(id, NodeReply::UpdateMetadata).await {
                        tracing::debug!(node = %name, error = %err, "could not answer a node call");
                    }
                }
                // The last good policy is not kept: a node whose owner meant something else must
                // not go on serving under what they no longer want.
                Err(rejection) => {
                    tracing::info!(node = %name, %owner, reason = %rejection.message, "rejected the node's metadata; closing its link");
                    let fault = rax::Error::new(ErrorKind::Rejected, rejection.message.clone());
                    if let Err(err) = link.fault(id, fault).await {
                        tracing::debug!(node = %name, error = %err, "could not answer a node call");
                    }
                    link.reject(rejection).await;
                }
            },
            LinkEvent::Background { session_id, event } => {
                let background = Background {
                    selector: selector.clone(),
                    session_id,
                    event,
                };
                if backgrounds.send(background).await.is_err() {
                    tracing::debug!(node = %name, "no chat side to show a background event");
                }
            }
            LinkEvent::Attachment { transfer_id, bytes } => {
                files.arrived(&selector, transfer_id, Ok(bytes));
            }
            LinkEvent::TransferFailed {
                transfer_id,
                reason,
            } => {
                tracing::debug!(node = %name, %transfer_id, %reason, "transfer failed");
                files.arrived(&selector, transfer_id, Err(reason));
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

/// Tells the node, and so its owner, which keys this gateway did not read. Nobody on this side
/// needs to hear about another gateway's settings.
async fn report_ignored(link: &GatewayLink, name: &str, keys: &[String]) {
    for key in keys {
        let body = Unhandled {
            subject: Subject::MetadataKey { key: key.clone() },
            reason: UnhandledReason::UnsupportedType,
            message: Some(format!("this gateway ignores `{key}`")),
        };
        if let Err(err) = link.unhandled(None, body).await {
            tracing::debug!(node = %name, error = %err, "could not report an ignored metadata key");
        }
    }
}

async fn initialize(link: &GatewayLink, tools: &Tools) -> Result<Initialized, String> {
    let offer = GatewayCall::Initialize(Initialize {
        protocol_version: PROTOCOL_VERSION,
        capabilities: capabilities(tools),
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
            Ok(initialized)
        }
        GatewayReply::Initialize(initialized) => Err(format!(
            "the node speaks RAX {}, this gateway speaks {PROTOCOL_VERSION}",
            initialized.protocol_version
        )),
        other => Err(format!("answered initialize with {other:?}")),
    }
}
