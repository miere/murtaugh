#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

//! The real `serve` loop between the Slack simulator and scripted RAX nodes.

use std::sync::Arc;
use std::time::Duration;

use murtaugh_gateway::run::{self, Options};
use murtaugh_gateway::token;
use murtaugh_store::{NodeToken, SqliteStore, Store, UserId};
use rax::content::ContentBlock;
use rax::id::{RequestId, SessionId, ToolCallId};
use rax::session::{Initialized, NodeCapabilities, PromptAccepted, SessionCreated, ToolGate};
use rax::tool::{Decision, ToolCall, ToolKind};
use rax::{Event, GatewayCall, GatewayReply, Open};
use rax_tokio::node::{NodeConfig, NodeEvent, NodeHandle, NodeLink};
use slack_sim::{BOT_USER_ID, GENERAL, SimMessage, SlackSim};
use time::OffsetDateTime;
use tokio::sync::{Mutex, mpsc};
use tokio_util::sync::CancellationToken;

const ALICE: &str = "U0ALICE01";
const ADMIN: &str = "U0ADMIN01";
const STRANGER: &str = "U0STRANGE";
const BOB: &str = "U0BOB0001";

async fn within<F: std::future::Future>(future: F) -> F::Output {
    tokio::time::timeout(Duration::from_secs(20), future)
        .await
        .expect("timed out")
}

async fn eventually<T>(what: &str, mut check: impl FnMut() -> Option<T>) -> T {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(20);
    loop {
        if let Some(found) = check() {
            return found;
        }
        if tokio::time::Instant::now() > deadline {
            panic!("timed out waiting for {what}");
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

fn user(raw: &str) -> UserId {
    UserId::parse(raw).unwrap()
}

/// What a scripted node saw, so a test can assert on the prompts it was given.
#[derive(Debug, Clone)]
enum Seen {
    Prompt { session: SessionId, text: String },
    Verdict { allowed: bool },
}

struct FakeNode {
    handle: NodeHandle,
    seen: mpsc::UnboundedReceiver<Seen>,
}

impl FakeNode {
    async fn next_prompt(&mut self) -> (SessionId, String) {
        loop {
            match within(self.seen.recv()).await.unwrap() {
                Seen::Prompt { session, text } => return (session, text),
                Seen::Verdict { .. } => {}
            }
        }
    }
}

struct Rig {
    sim: SlackSim,
    _dir: tempfile::TempDir,
    store: Arc<dyn Store>,
    listen: std::net::SocketAddr,
    shutdown: CancellationToken,
}

async fn rig() -> Rig {
    if let Ok(filter) = std::env::var("TEST_LOG") {
        let _ = tracing_subscriber::fmt()
            .with_env_filter(tracing_subscriber::EnvFilter::new(filter))
            .try_init();
    }
    let sim = SlackSim::start().await.unwrap();
    for (id, name) in [
        (ALICE, "alice"),
        (ADMIN, "admin"),
        (STRANGER, "stranger"),
        (BOB, "bob"),
    ] {
        sim.add_user(id, name);
    }
    let dir = tempfile::tempdir().unwrap();
    let listen = std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap();
    let tokens = sim.tokens();
    let config = dir.path().join("murtaugh.toml");
    std::fs::write(
        &config,
        format!(
            "[slack]\napp_token = \"{}\"\nbot_token = \"{}\"\n[nodes]\nlisten = \"{listen}\"\n",
            tokens.app, tokens.bot
        ),
    )
    .unwrap();
    let store: Arc<dyn Store> =
        Arc::new(SqliteStore::open(&dir.path().join("murtaugh.db")).unwrap());
    store.set_admin(&user(ADMIN)).await.unwrap();
    store.approve(&user(ALICE), &user(ADMIN)).await.unwrap();
    let shutdown = gateway(&sim, config);
    Rig {
        sim,
        _dir: dir,
        store,
        listen,
        shutdown,
    }
}

fn gateway(sim: &SlackSim, config: std::path::PathBuf) -> CancellationToken {
    let shutdown = CancellationToken::new();
    let options = Options {
        slack_api: sim.api_base(),
        refresh: Duration::from_millis(100),
    };
    tokio::spawn({
        let shutdown = shutdown.clone();
        async move {
            if let Err(err) = run::serve(&config, options, shutdown).await {
                eprintln!("gateway stopped: {err}");
            }
        }
    });
    shutdown
}

impl Rig {
    async fn node(&self, owner: &str, name: &str) -> FakeNode {
        self.node_on(owner, name, &[self.listen]).await
    }

    async fn node_on(
        &self,
        owner: &str,
        name: &str,
        gateways: &[std::net::SocketAddr],
    ) -> FakeNode {
        let minted = token::mint();
        self.store
            .add_node_token(&NodeToken {
                selector: minted.selector,
                secret_hash: minted.secret_hash,
                owner: user(owner),
                name: name.into(),
                created_at: OffsetDateTime::now_utc(),
                revoked_at: None,
            })
            .await
            .unwrap();
        let (handle, mut events) = NodeLink::start(NodeConfig {
            endpoints: gateways.iter().map(|addr| format!("ws://{addr}")).collect(),
            token: minted.token,
            backoff_min: Duration::from_millis(20),
            backoff_max: Duration::from_millis(200),
            ..Default::default()
        })
        .unwrap();
        let (seen, receiver) = mpsc::unbounded_channel();
        let (initialized, mut ready) = mpsc::channel(1);
        let node = handle.clone();
        let sessions = Arc::new(Mutex::new(0u32));
        let prefix = name.to_owned();
        tokio::spawn(async move {
            while let Some(event) = events.recv().await {
                match event {
                    NodeEvent::Request { id, call } => {
                        tokio::spawn(answer(
                            node.clone(),
                            id,
                            call,
                            seen.clone(),
                            sessions.clone(),
                            prefix.clone(),
                            initialized.clone(),
                        ));
                    }
                    NodeEvent::Verdict(verdict) => {
                        let allowed = matches!(verdict.decision, Decision::Allow);
                        let _ = seen.send(Seen::Verdict { allowed });
                    }
                    _ => {}
                }
            }
        });
        within(ready.recv())
            .await
            .expect("the node never initialized");
        tokio::time::sleep(Duration::from_millis(100)).await;
        FakeNode {
            handle,
            seen: receiver,
        }
    }

    async fn bot_replies(
        &self,
        thread_ts: &str,
        wanted: impl Fn(&[SimMessage]) -> bool,
    ) -> Vec<SimMessage> {
        eventually("the bot's replies", || {
            let replies: Vec<SimMessage> = self
                .sim
                .thread(GENERAL, thread_ts)
                .into_iter()
                .filter(|message| message.user.as_deref() == Some(BOT_USER_ID))
                .collect();
            wanted(&replies).then_some(replies)
        })
        .await
    }
}

async fn answer(
    node: NodeHandle,
    id: RequestId,
    call: GatewayCall,
    seen: mpsc::UnboundedSender<Seen>,
    sessions: Arc<Mutex<u32>>,
    name: String,
    initialized: mpsc::Sender<()>,
) {
    match call {
        GatewayCall::Initialize(_) => {
            let reply = GatewayReply::Initialize(Initialized {
                protocol_version: rax::PROTOCOL_VERSION,
                capabilities: NodeCapabilities {
                    tool_gate: ToolGate::EveryCall,
                    ..Default::default()
                },
            });
            node.reply(id, reply).await.unwrap();
            let _ = initialized.send(()).await;
        }
        GatewayCall::NewSession(_) => {
            let mut count = sessions.lock().await;
            *count += 1;
            let session_id = SessionId(format!("{name}-{count}"));
            let reply = GatewayReply::NewSession(SessionCreated {
                session_id,
                unhandled: vec![],
            });
            node.reply(id, reply).await.unwrap();
        }
        GatewayCall::Prompt(prompt) => {
            let text = prompt
                .content
                .iter()
                .filter_map(|block| match block {
                    Open::Known(ContentBlock::Text { text }) => Some(text.clone()),
                    _ => None,
                })
                .collect::<Vec<_>>()
                .join("\n");
            let _ = seen.send(Seen::Prompt {
                session: prompt.session_id.clone(),
                text: text.clone(),
            });
            node.reply(id.clone(), GatewayReply::Prompt(PromptAccepted::default()))
                .await
                .unwrap();
            let tool = ToolCall {
                id: ToolCallId("tc1".into()),
                name: "Bash".into(),
                title: Some("ls".into()),
                kind: ToolKind::Execute,
                input: None,
                content: vec![],
            };
            node.event(id.clone(), Event::ToolCall { tool_call: tool })
                .await
                .unwrap();
            let last = text.lines().last().unwrap_or_default().to_owned();
            let reply = Event::Message {
                content: ContentBlock::text(format!("**{name}** says pong to: {last}")).into(),
            };
            node.event(id.clone(), reply).await.unwrap();
            node.event(id.clone(), Event::Complete { stop_reason: None })
                .await
                .unwrap();
            node.end(id).await.unwrap();
        }
        _ => {}
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn a_mention_is_answered_in_its_thread_by_the_persons_own_node() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;

    let ts = rig.sim.mention(ALICE, GENERAL, "ping", None).await.unwrap();
    let (session, text) = laptop.next_prompt().await;
    assert_eq!(session.0, "laptop-1");
    assert_eq!(text, "ping");
    let replies = rig
        .bot_replies(&ts, |replies| {
            replies
                .iter()
                .any(|message| message.text.contains("says pong to: ping"))
        })
        .await;
    assert!(
        replies
            .iter()
            .any(|message| message.text.contains("*laptop* says pong"))
    );
    assert!(matches!(
        within(laptop.seen.recv()).await.unwrap(),
        Seen::Verdict { allowed: true }
    ));
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_stranger_gets_a_zipped_mouth_and_nothing_else() {
    let rig = rig().await;
    let _laptop = rig.node(ALICE, "laptop").await;

    let ts = rig
        .sim
        .mention(STRANGER, GENERAL, "hello?", None)
        .await
        .unwrap();
    eventually("the reaction", || {
        rig.sim
            .reactions(GENERAL, &ts)
            .contains(&"zipper_mouth_face".to_owned())
            .then_some(())
    })
    .await;
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        rig.sim
            .thread(GENERAL, &ts)
            .iter()
            .all(|m| m.user.as_deref() != Some(BOT_USER_ID))
    );
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_pre_authorised_person_uses_the_admins_node_and_hears_when_none_is_up() {
    let rig = rig().await;
    rig.store.set_allowed(&user(BOB), true).await.unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;

    let ts = rig
        .sim
        .mention(BOB, GENERAL, "anyone?", None)
        .await
        .unwrap();
    rig.bot_replies(&ts, |replies| {
        replies
            .iter()
            .any(|m| m.text.contains("No machine can take this conversation"))
    })
    .await;

    let mut desktop = rig.node(ADMIN, "desktop").await;
    let ts = rig.sim.mention(BOB, GENERAL, "now?", None).await.unwrap();
    assert_eq!(desktop.next_prompt().await.1, "now?");
    rig.bot_replies(&ts, |replies| {
        replies
            .iter()
            .any(|m| m.text.contains("*desktop* says pong"))
    })
    .await;
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_thread_whose_node_left_continues_elsewhere_after_a_notice_and_a_catch_up() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "first question", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    rig.bot_replies(&ts, |replies| {
        replies
            .iter()
            .any(|m| m.text.contains("pong to: first question"))
    })
    .await;

    laptop.handle.close().await;
    let mut desktop = rig.node(ALICE, "desktop").await;
    tokio::time::sleep(Duration::from_millis(300)).await;
    rig.sim
        .mention(ALICE, GENERAL, "second question", Some(&ts))
        .await
        .unwrap();

    let (session, text) = desktop.next_prompt().await;
    assert_eq!(session.0, "desktop-1");
    assert!(text.contains("Earlier messages"), "{text}");
    assert!(text.contains("first question"), "{text}");
    assert!(text.ends_with("second question"), "{text}");
    let replies = rig
        .bot_replies(&ts, |replies| {
            replies
                .iter()
                .any(|m| m.text.contains("pong to: second question"))
        })
        .await;
    let notice = replies
        .iter()
        .position(|m| m.text.contains("went offline. Continuing on *desktop*"))
        .expect("no notice");
    let answer = replies
        .iter()
        .position(|m| m.text.contains("pong to: second question"))
        .unwrap();
    assert!(notice < answer, "the notice came after the answer");
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_direct_message_is_answered_in_its_own_thread() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;

    let (channel, ts) = rig.sim.dm(ALICE, "hi there").await.unwrap();
    assert_eq!(laptop.next_prompt().await.1, "hi there");
    eventually("the reply in the DM thread", || {
        rig.sim
            .thread(&channel, &ts)
            .iter()
            .any(|m| m.user.as_deref() == Some(BOT_USER_ID) && m.text.contains("pong to: hi there"))
            .then_some(())
    })
    .await;
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_standby_stays_off_slack_until_the_leader_stops_then_serves_the_same_node() {
    let rig = rig().await;
    let standby_listen = std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap();
    let tokens = rig.sim.tokens();
    let standby_config = rig._dir.path().join("standby.toml");
    std::fs::write(
        &standby_config,
        format!(
            "[slack]\napp_token = \"{}\"\nbot_token = \"{}\"\n[database.sqlite]\npath = \"murtaugh.db\"\n[nodes]\nlisten = \"{standby_listen}\"\n",
            tokens.app, tokens.bot
        ),
    )
    .unwrap();
    eventually("the first gateway to lead", || {
        (rig.sim.connections() == 1).then_some(())
    })
    .await;
    let standby = gateway(&rig.sim, standby_config);
    let mut laptop = rig
        .node_on(ALICE, "laptop", &[rig.listen, standby_listen])
        .await;
    tokio::time::sleep(Duration::from_millis(500)).await;
    assert_eq!(rig.sim.connections(), 1, "the standby connected to Slack");

    rig.shutdown.cancel();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(60);
    while rig
        .sim
        .calls()
        .iter()
        .filter(|call| call.method == "apps.connections.open")
        .count()
        < 2
    {
        assert!(
            tokio::time::Instant::now() < deadline,
            "the standby never took over"
        );
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    let deadline = tokio::time::Instant::now() + Duration::from_secs(60);
    'asking: loop {
        let ts = rig
            .sim
            .mention(ALICE, GENERAL, "still there?", None)
            .await
            .unwrap();
        loop {
            let replies: Vec<String> = rig
                .sim
                .thread(GENERAL, &ts)
                .into_iter()
                .filter(|m| m.user.as_deref() == Some(BOT_USER_ID))
                .map(|m| m.text)
                .collect();
            if replies
                .iter()
                .any(|text| text.contains("pong to: still there?"))
            {
                break 'asking;
            }
            assert!(
                tokio::time::Instant::now() < deadline,
                "the standby never answered; replies: {replies:?}"
            );
            if replies.iter().any(|text| text.contains("No machine")) {
                tokio::time::sleep(Duration::from_millis(300)).await;
                continue 'asking;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    }
    assert_eq!(laptop.next_prompt().await.1, "still there?");
    assert_eq!(rig.sim.violations(), vec![]);
    standby.cancel();
}
