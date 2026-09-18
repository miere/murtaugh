#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use std::sync::Arc;
use std::time::Duration;

use murtaugh_gateway::access::{Access, Snapshot};
use murtaugh_gateway::fleet::Fleet;
use murtaugh_gateway::hub::{self, FleetChange};
use murtaugh_gateway::token;
use murtaugh_store::{NodeToken, SqliteStore, Store, UserId};
use rax::session::{Initialized, NodeCapabilities, ToolGate};
use rax::{GatewayCall, GatewayReply};
use rax_tokio::node::{NodeConfig, NodeEvent, NodeEvents, NodeHandle, NodeLink};
use time::OffsetDateTime;
use tokio_util::sync::CancellationToken;

fn user(raw: &str) -> UserId {
    UserId::parse(raw).unwrap()
}

async fn within<F: std::future::Future>(future: F) -> F::Output {
    tokio::time::timeout(Duration::from_secs(20), future)
        .await
        .expect("timed out")
}

struct Rig {
    _dir: tempfile::TempDir,
    store: Arc<dyn Store>,
    access: Access,
    fleet: Fleet,
    hub: hub::Hub,
    shutdown: CancellationToken,
}

async fn rig() -> Rig {
    let dir = tempfile::tempdir().unwrap();
    let store: Arc<dyn Store> = Arc::new(SqliteStore::open(&dir.path().join("config.db")).unwrap());
    store.set_admin(&user("U0ADMIN01")).await.unwrap();
    store
        .approve(&user("U0PERSON1"), &user("U0ADMIN01"))
        .await
        .unwrap();
    let access = Access::reloading(Snapshot::load(&*store).await.unwrap(), store.clone());
    let fleet = Fleet::default();
    let shutdown = CancellationToken::new();
    let hub = hub::start(
        "127.0.0.1:0".parse().unwrap(),
        access.clone(),
        fleet.clone(),
        shutdown.clone(),
    )
    .await
    .unwrap();
    Rig {
        _dir: dir,
        store,
        access,
        fleet,
        hub,
        shutdown,
    }
}

impl Rig {
    async fn mint(&self, owner: &str) -> (String, String) {
        let minted = self.mint_quietly(owner).await;
        self.access
            .replace(Snapshot::load(&*self.store).await.unwrap());
        minted
    }

    /// Only the store hears about it, as when the CLI mints while the gateway runs.
    async fn mint_quietly(&self, owner: &str) -> (String, String) {
        let minted = token::mint();
        self.store
            .add_node_token(&NodeToken {
                selector: minted.selector.clone(),
                secret_hash: minted.secret_hash,
                owner: user(owner),
                name: "laptop".into(),
                created_at: OffsetDateTime::now_utc(),
                revoked_at: None,
            })
            .await
            .unwrap();
        (minted.selector, minted.token)
    }

    fn dial(&self, token: &str) -> (NodeHandle, NodeEvents) {
        NodeLink::start(NodeConfig {
            endpoints: vec![format!("ws://{}", self.hub.server.local_addr())],
            token: token.to_owned(),
            backoff_min: Duration::from_millis(10),
            backoff_max: Duration::from_millis(50),
            ..Default::default()
        })
        .unwrap()
    }
}

async fn answer_initialize(node: &NodeHandle, events: &mut NodeEvents) {
    loop {
        match within(events.recv()).await.unwrap() {
            NodeEvent::Request {
                id,
                call: GatewayCall::Initialize(offer),
            } => {
                assert!(
                    offer
                        .capabilities
                        .resource_schemes
                        .contains(&"chat".to_owned())
                );
                let reply = GatewayReply::Initialize(Initialized {
                    protocol_version: rax::PROTOCOL_VERSION,
                    capabilities: NodeCapabilities {
                        tool_gate: ToolGate::EveryCall,
                        ..Default::default()
                    },
                });
                node.reply(id, reply).await.unwrap();
                return;
            }
            NodeEvent::Fresh => {}
            other => panic!("expected initialize, got {other:?}"),
        }
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn a_granted_node_attaches_and_is_assigned_to_its_owner() {
    let mut rig = rig().await;
    let (selector, token) = rig.mint("U0PERSON1").await;
    let (node, mut events) = rig.dial(&token);
    answer_initialize(&node, &mut events).await;

    assert_eq!(
        within(rig.hub.changes.recv()).await.unwrap(),
        FleetChange::Attached {
            selector: selector.clone()
        }
    );
    let snapshot = rig.access.snapshot();
    let assigned = rig.fleet.assign(&user("U0PERSON1"), &snapshot).unwrap();
    assert_eq!(assigned.selector, selector);
    assert_eq!(assigned.capabilities.tool_gate, ToolGate::EveryCall);
    assert!(rig.fleet.assign(&user("U0GUEST01"), &snapshot).is_none());
    node.close().await;
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_revoked_grant_disconnects_the_node_and_its_redial_is_refused() {
    let mut rig = rig().await;
    let (selector, token) = rig.mint("U0PERSON1").await;
    let (node, mut events) = rig.dial(&token);
    answer_initialize(&node, &mut events).await;
    within(rig.hub.changes.recv()).await.unwrap();
    tokio::spawn(hub::refresh(
        rig.store.clone(),
        rig.access.clone(),
        rig.fleet.clone(),
        Duration::from_millis(50),
        rig.shutdown.clone(),
    ));

    rig.store.revoke(&user("U0PERSON1")).await.unwrap();
    assert_eq!(
        within(rig.hub.changes.recv()).await.unwrap(),
        FleetChange::Gone { selector }
    );
    loop {
        match within(events.recv()).await {
            Some(NodeEvent::CredentialRejected) => break,
            Some(_) => {}
            None => panic!("node events ended before the credential was rejected"),
        }
    }
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_forged_token_never_gets_a_link() {
    let rig = rig().await;
    let (_, token) = rig.mint("U0PERSON1").await;
    let (_node, mut events) = rig.dial(&format!("{token}forged"));
    assert!(matches!(
        within(events.recv()).await,
        Some(NodeEvent::CredentialRejected)
    ));
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_node_started_right_after_its_token_is_minted_is_admitted() {
    let mut rig = rig().await;
    let (selector, token) = rig.mint_quietly("U0PERSON1").await;
    let (node, mut events) = rig.dial(&token);
    answer_initialize(&node, &mut events).await;
    assert_eq!(
        within(rig.hub.changes.recv()).await.unwrap(),
        FleetChange::Attached { selector }
    );
    node.close().await;
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_token_revoked_before_it_first_dials_is_still_refused() {
    let rig = rig().await;
    let (selector, token) = rig.mint_quietly("U0PERSON1").await;
    rig.store.revoke_node_token(&selector).await.unwrap();
    let (_node, mut events) = rig.dial(&token);
    assert!(matches!(
        within(events.recv()).await,
        Some(NodeEvent::CredentialRejected)
    ));
    rig.shutdown.cancel();
}
