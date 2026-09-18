#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

//! The real `serve` loop between the Slack simulator and scripted RAX nodes.

use std::sync::Arc;
use std::time::Duration;

use murtaugh_gateway::run::{self, Options};
use murtaugh_gateway::token;
use murtaugh_store::{NodeToken, SqliteStore, Store, UserId};
use rax::content::ContentBlock;
use rax::id::{RequestId, SessionId, ToolCallId};
use rax::resource::ReadResource;
use rax::session::{Initialized, NodeCapabilities, PromptAccepted, SessionCreated, ToolGate};
use rax::tool::{Decision, ToolCall, ToolKind};
use rax::{Event, GatewayCall, GatewayReply, Open};
use rax_tokio::CallError;
use rax_tokio::node::{NodeConfig, NodeEvent, NodeHandle, NodeLink, Resource};
use slack_sim::{BOT_USER_ID, GENERAL, SimMessage, SlackSim, TEAM_ID};
use time::OffsetDateTime;
use tokio::sync::{Mutex, mpsc};
use tokio_util::sync::CancellationToken;

const ALICE: &str = "U0ALICE01";
const ADMIN: &str = "U0ADMIN01";
const STRANGER: &str = "U0STRANGE";
const BOB: &str = "U0BOB0001";
const CHART: &[u8] = b"\x89PNG a very fine chart";

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
    Prompt {
        session: SessionId,
        text: String,
        links: Vec<ContentBlock>,
    },
    Verdict {
        allowed: bool,
    },
}

struct FakeNode {
    handle: NodeHandle,
    seen: mpsc::UnboundedReceiver<Seen>,
}

impl FakeNode {
    async fn next_prompt(&mut self) -> (SessionId, String) {
        let (session, text, _) = self.next_prompt_with_links().await;
        (session, text)
    }

    async fn next_prompt_with_links(&mut self) -> (SessionId, String, Vec<ContentBlock>) {
        loop {
            match within(self.seen.recv()).await.unwrap() {
                Seen::Prompt {
                    session,
                    text,
                    links,
                } => return (session, text, links),
                Seen::Verdict { .. } => {}
            }
        }
    }

    async fn read(&self, uri: &str, max_bytes: Option<u64>) -> Result<Resource, CallError> {
        let read = ReadResource {
            uri: uri.to_owned(),
            max_bytes,
        };
        within(self.handle.read_resource(read)).await
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
    rig_with(murtaugh_gateway::approval::TIMEOUT).await
}

async fn rig_with(approval_timeout: Duration) -> Rig {
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
    let shutdown = gateway_with(&sim, config, approval_timeout);
    Rig {
        sim,
        _dir: dir,
        store,
        listen,
        shutdown,
    }
}

fn gateway(sim: &SlackSim, config: std::path::PathBuf) -> CancellationToken {
    gateway_with(sim, config, murtaugh_gateway::approval::TIMEOUT)
}

fn gateway_with(
    sim: &SlackSim,
    config: std::path::PathBuf,
    approval_timeout: Duration,
) -> CancellationToken {
    let shutdown = CancellationToken::new();
    let options = Options {
        slack_api: sim.api_base(),
        refresh: Duration::from_millis(100),
        approval_timeout,
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
        let (verdicts, _) = tokio::sync::broadcast::channel::<bool>(16);
        let (initialized, mut ready) = mpsc::channel(1);
        let script = Script {
            node: handle.clone(),
            seen,
            verdicts,
            sessions: Arc::new(Mutex::new(0u32)),
            name: name.to_owned(),
            initialized,
        };
        tokio::spawn(async move {
            while let Some(event) = events.recv().await {
                match event {
                    NodeEvent::Request { id, call } => {
                        tokio::spawn(answer(script.clone(), id, call));
                    }
                    NodeEvent::Verdict(verdict) => {
                        let allowed = matches!(verdict.decision, Decision::Allow);
                        let _ = script.seen.send(Seen::Verdict { allowed });
                        let _ = script.verdicts.send(allowed);
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

/// What a scripted node answers with, shared by every call it handles.
#[derive(Clone)]
struct Script {
    node: NodeHandle,
    seen: mpsc::UnboundedSender<Seen>,
    verdicts: tokio::sync::broadcast::Sender<bool>,
    sessions: Arc<Mutex<u32>>,
    name: String,
    initialized: mpsc::Sender<()>,
}

async fn answer(script: Script, id: RequestId, call: GatewayCall) {
    let Script {
        node,
        seen,
        verdicts,
        sessions,
        name,
        initialized,
    } = script;
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
            let links = prompt
                .content
                .iter()
                .filter_map(|block| match block {
                    Open::Known(link @ ContentBlock::ResourceLink { .. }) => Some(link.clone()),
                    _ => None,
                })
                .collect();
            let _ = seen.send(Seen::Prompt {
                session: prompt.session_id.clone(),
                text: text.clone(),
                links,
            });
            node.reply(id.clone(), GatewayReply::Prompt(PromptAccepted::default()))
                .await
                .unwrap();
            let tool = ToolCall {
                id: ToolCallId("tc1".into()),
                name: "Bash".into(),
                title: Some("ls".into()),
                kind: ToolKind::Execute,
                input: Some(serde_json::json!({"command": "git push origin main"})),
                content: vec![],
            };
            let mut ruled = verdicts.subscribe();
            node.event(id.clone(), Event::ToolCall { tool_call: tool })
                .await
                .unwrap();
            let allowed = tokio::time::timeout(Duration::from_secs(20), ruled.recv())
                .await
                .map(|verdict| verdict.unwrap_or(false))
                .unwrap_or(false);
            let status = if allowed {
                rax::tool::ToolCallStatus::Completed
            } else {
                rax::tool::ToolCallStatus::Denied
            };
            let done = Event::ToolCallUpdate {
                tool_call_update: rax::tool::ToolCallUpdate {
                    id: ToolCallId("tc1".into()),
                    status,
                    title: None,
                    content: vec![],
                    output: None,
                },
            };
            node.event(id.clone(), done).await.unwrap();
            if text.contains("attach") {
                let bytes = CHART.to_vec();
                let transfer_id = node.send_attachment(&bytes).await.unwrap();
                let lie = u64::from(text.contains("lie about"));
                let attachment = rax::attachment::Attachment {
                    transfer_id,
                    size: bytes.len() as u64 + lie,
                    filename: Some("chart.png".into()),
                    title: Some("The chart".into()),
                    comment: Some("Your chart".into()),
                    mimetype: Some("image/png".into()),
                };
                node.event(id.clone(), Event::Attachment { attachment })
                    .await
                    .unwrap();
            }
            let last = text.lines().last().unwrap_or_default().to_owned();
            let ruling = if allowed { "tool ran" } else { "tool refused" };
            let reply = Event::Message {
                content: ContentBlock::text(format!("**{name}** says pong to: {last} ({ruling})"))
                    .into(),
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
    let answer = replies
        .iter()
        .find(|message| message.text.contains("**laptop** says pong"))
        .expect("no streamed answer");
    let stream = eventually("the stream to stop", || {
        rig.sim
            .thread(GENERAL, &ts)
            .into_iter()
            .find(|m| m.ts == answer.ts)
            .and_then(|m| m.stream)
            .filter(|stream| !stream.open)
    })
    .await;
    assert_eq!(
        stream.recipient,
        Some((TEAM_ID.to_owned(), ALICE.to_owned()))
    );
    assert_eq!(stream.plans, ["Task list"]);
    assert_eq!(stream.tasks.len(), 1);
    assert_eq!(
        (
            stream.tasks[0].title.as_str(),
            stream.tasks[0].status.as_str()
        ),
        ("ls", "complete")
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
            .any(|m| m.text.contains("**desktop** says pong"))
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

#[tokio::test(flavor = "multi_thread")]
async fn a_direct_message_an_app_posted_for_a_person_is_answered() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let (channel, first) = rig.sim.dm(ALICE, "first").await.unwrap();
    laptop.next_prompt().await;
    eventually("the first turn to end", || {
        let answered =
            rig.sim.thread(&channel, &first).iter().any(|m| {
                m.user.as_deref() == Some(BOT_USER_ID) && m.text.contains("pong to: first")
            });
        let cleared = rig.sim.calls().iter().any(|c| {
            c.method == "assistant.threads.setStatus" && c.params["status"].as_str() == Some("")
        });
        (answered && cleared).then_some(())
    })
    .await;
    tokio::time::sleep(Duration::from_millis(200)).await;

    rig.sim
        .emit(serde_json::json!({
            "type": "message",
            "channel": channel,
            "channel_type": "im",
            "user": ALICE,
            "bot_id": "B0SOMEAPP",
            "app_id": "A0SOMEAPP",
            "text": "second, sent through an app",
            "ts": "1789000000.000200",
            "thread_ts": first,
            "event_ts": "1789000000.000200",
        }))
        .await;
    assert_eq!(laptop.next_prompt().await.1, "second, sent through an app");
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_thread_caught_up_after_a_gateway_restart_is_told_first() {
    let mut rig = rig().await;
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

    rig.shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(500)).await;
    rig.shutdown = gateway(&rig.sim, rig._dir.path().join("murtaugh.toml"));
    let deadline = tokio::time::Instant::now() + Duration::from_secs(60);
    'asking: loop {
        let asked = rig
            .sim
            .mention(ALICE, GENERAL, "second question", Some(&ts))
            .await
            .unwrap();
        loop {
            let replies: Vec<String> = rig
                .sim
                .thread(GENERAL, &ts)
                .into_iter()
                .filter(|m| m.user.as_deref() == Some(BOT_USER_ID) && m.ts > asked)
                .map(|m| m.text)
                .collect();
            if replies
                .iter()
                .any(|text| text.contains("pong to: second question"))
            {
                let notice = replies
                    .iter()
                    .position(|text| text.contains("Picking this conversation up on *laptop*"))
                    .expect("no notice before the catch-up");
                let answer = replies
                    .iter()
                    .position(|text| text.contains("pong to: second question"))
                    .unwrap();
                assert!(
                    notice < answer,
                    "the notice came after the answer: {replies:?}"
                );
                break 'asking;
            }
            assert!(
                tokio::time::Instant::now() < deadline,
                "no answer: {replies:?}"
            );
            if replies.iter().any(|text| text.contains("No machine")) {
                tokio::time::sleep(Duration::from_millis(300)).await;
                continue 'asking;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    }
    let (_, text) = laptop.next_prompt().await;
    assert!(text.contains("first question"), "{text}");
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn the_thread_shows_the_agent_thinking_until_the_turn_ends() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "think", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    rig.bot_replies(&ts, |replies| {
        replies.iter().any(|m| m.text.contains("pong to: think"))
    })
    .await;

    let order: Vec<String> = eventually("the status to be cleared", || {
        let calls: Vec<String> = rig
            .sim
            .calls()
            .into_iter()
            .filter(|c| c.params.to_string().contains(&ts))
            .filter_map(|c| match c.method.as_str() {
                "assistant.threads.setStatus" => Some(format!(
                    "status:{}",
                    c.params["status"].as_str().unwrap_or_default()
                )),
                "chat.startStream" => Some("stream".into()),
                _ => None,
            })
            .collect();
        (calls.last().map(String::as_str) == Some("status:")).then_some(calls)
    })
    .await;
    assert_eq!(
        order.first().map(String::as_str),
        Some("status:is thinking...")
    );
    let streamed = order.iter().position(|c| c == "stream").expect("no stream");
    assert!(
        streamed > 0,
        "the status came after the reply started: {order:?}"
    );
    assert_eq!(rig.sim.thread_status(GENERAL, &ts), None);
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

fn fault_kind(read: Result<Resource, CallError>) -> rax::ErrorKind {
    match read {
        Err(CallError::Fault(error)) => error.kind,
        other => panic!("expected a fault, got {:?}", other.map(|r| r.bytes.len())),
    }
}

async fn wait_for_turn_end(rig: &Rig, channel: &str, ts: &str, answer: &str) {
    eventually("the turn to end", || {
        let answered = rig
            .sim
            .thread(channel, ts)
            .iter()
            .any(|m| m.user.as_deref() == Some(BOT_USER_ID) && m.text.contains(answer));
        let cleared = rig.sim.calls().iter().any(|c| {
            c.method == "assistant.threads.setStatus"
                && c.params["thread_ts"].as_str() == Some(ts)
                && c.params["status"].as_str() == Some("")
        });
        (answered && cleared).then_some(())
    })
    .await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_file_shared_with_a_mention_is_readable_by_its_node_and_no_other() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let desktop = rig.node(ADMIN, "desktop").await;
    let report = b"quarterly numbers: up and to the right".repeat(1000);

    let (file_id, ts) = rig
        .sim
        .mention_with_file(
            ALICE,
            GENERAL,
            "what's in this?",
            "report.txt",
            "text/plain",
            &report,
            None,
        )
        .await
        .unwrap();
    let (_, text, links) = laptop.next_prompt_with_links().await;
    assert_eq!(text, "what's in this?");
    let uri = format!("gateway://files/{file_id}");
    assert_eq!(
        links,
        [ContentBlock::ResourceLink {
            uri: uri.clone(),
            name: "report.txt".into(),
            mime_type: Some("text/plain".into()),
            title: None,
            description: None,
            size: Some(report.len() as u64),
        }]
    );

    let read = laptop.read(&uri, None).await.unwrap();
    assert_eq!(read.bytes, report);
    assert_eq!(read.mimetype.as_deref(), Some("text/plain"));
    assert_eq!(
        fault_kind(laptop.read(&uri, Some(10)).await),
        rax::ErrorKind::TooLarge
    );
    assert_eq!(
        fault_kind(desktop.read(&uri, None).await),
        rax::ErrorKind::Forbidden
    );
    assert_eq!(
        fault_kind(laptop.read("gateway://files/F0SIM999999", None).await),
        rax::ErrorKind::Forbidden
    );
    wait_for_turn_end(&rig, GENERAL, &ts, "pong to: what's in this?").await;
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_file_dropped_into_a_direct_message_reaches_the_node() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let (channel, first) = rig.sim.dm(ALICE, "hello").await.unwrap();
    laptop.next_prompt().await;
    wait_for_turn_end(&rig, &channel, &first, "pong to: hello").await;

    let (file_id, _) = rig
        .sim
        .upload(
            ALICE,
            &channel,
            "photo.png",
            "image/png",
            b"\x89PNG",
            Some(&first),
        )
        .await
        .unwrap();
    let (_, _, links) = laptop.next_prompt_with_links().await;
    let uri = format!("gateway://files/{file_id}");
    assert!(
        matches!(&links[..], [ContentBlock::ResourceLink { uri: got, .. }] if *got == uri),
        "{links:?}"
    );
    assert_eq!(laptop.read(&uri, None).await.unwrap().bytes, b"\x89PNG");
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn an_attachment_from_the_agent_is_uploaded_into_the_thread() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "attach a chart", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    wait_for_turn_end(&rig, GENERAL, &ts, "pong to: attach a chart").await;

    let shared = rig
        .sim
        .thread(GENERAL, &ts)
        .into_iter()
        .find(|m| m.user.as_deref() == Some(BOT_USER_ID) && !m.files.is_empty())
        .expect("no file in the thread");
    assert_eq!(shared.text, "Your chart");
    let file = rig.sim.file(&shared.files[0]).unwrap();
    assert_eq!(
        (
            file.name.as_str(),
            file.title.as_str(),
            file.bytes.as_slice()
        ),
        ("chart.png", "The chart", CHART)
    );
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn an_attachment_whose_bytes_do_not_match_is_reported_not_uploaded() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "attach and lie about it", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    wait_for_turn_end(&rig, GENERAL, &ts, "pong to: attach and lie").await;

    let thread = rig.sim.thread(GENERAL, &ts);
    assert!(thread.iter().all(|m| m.files.is_empty()), "{thread:?}");
    assert!(
        thread
            .iter()
            .any(|m| m.text.contains("Could not attach chart.png")),
        "{thread:?}"
    );
    assert!(
        rig.sim
            .calls()
            .iter()
            .all(|c| !c.method.starts_with("files.getUpload")),
    );
    rig.shutdown.cancel();
}

fn approval_card(rig: &Rig, ts: &str) -> Option<SimMessage> {
    rig.sim.thread(GENERAL, ts).into_iter().find(|m| {
        m.blocks
            .as_ref()
            .is_some_and(|b| b.to_string().contains("murtaugh_approval_card"))
    })
}

fn card_says(card: &SimMessage, text: &str) -> bool {
    card.blocks
        .as_ref()
        .is_some_and(|b| b.to_string().contains(text))
}

#[tokio::test(flavor = "multi_thread")]
async fn only_the_node_owner_can_approve_a_tool_off_their_whitelist() {
    let rig = rig().await;
    rig.store
        .set_tool_mode(&user(ALICE), murtaugh_store::ToolMode::AllowedWhitelist)
        .await
        .unwrap();
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "push it", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    let card = eventually("the approval card", || approval_card(&rig, &ts)).await;
    assert!(card_says(
        &card,
        "Waiting on <@U0ALICE01>'s approval. Denied automatically in 30 minutes."
    ));
    assert!(card_says(
        &card,
        "The agent on laptop wants to use the 'Bash' tool"
    ));
    assert!(card_says(&card, "git push origin main"));

    rig.sim
        .click(
            BOB,
            GENERAL,
            &card.ts,
            murtaugh_gateway::approval::ALLOW_ONCE,
        )
        .await
        .unwrap();
    eventually("the note to Bob", || {
        rig.sim
            .ephemerals()
            .iter()
            .any(|(_, who, text)| who == BOB && text.contains("Only <@U0ALICE01> can decide"))
            .then_some(())
    })
    .await;
    assert!(approval_card(&rig, &ts).is_some_and(|c| card_says(&c, "Approval Needed")));

    rig.sim
        .click(
            ALICE,
            GENERAL,
            &card.ts,
            murtaugh_gateway::approval::ALLOW_ONCE,
        )
        .await
        .unwrap();
    wait_for_turn_end(&rig, GENERAL, &ts, "pong to: push it (tool ran)").await;
    let settled = approval_card(&rig, &ts).unwrap();
    assert!(
        card_says(&settled, "Approved by <@U0ALICE01>."),
        "{:?}",
        settled.blocks
    );
    assert!(
        !card_says(&settled, "tool_approval_once"),
        "the buttons stayed"
    );
    assert!(
        rig.store
            .user(&user(ALICE))
            .await
            .unwrap()
            .whitelist
            .is_empty()
    );
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn always_allow_whitelists_the_tool_so_the_next_call_runs_unasked() {
    let rig = rig().await;
    rig.store
        .set_tool_mode(&user(ALICE), murtaugh_store::ToolMode::AllowedWhitelist)
        .await
        .unwrap();
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "first", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    let card = eventually("the approval card", || approval_card(&rig, &ts)).await;
    rig.sim
        .click(
            ALICE,
            GENERAL,
            &card.ts,
            murtaugh_gateway::approval::ALLOW_ALWAYS,
        )
        .await
        .unwrap();
    wait_for_turn_end(&rig, GENERAL, &ts, "pong to: first (tool ran)").await;
    assert!(card_says(
        &approval_card(&rig, &ts).unwrap(),
        "added *Bash* to their whitelist"
    ));
    assert!(
        rig.store
            .user(&user(ALICE))
            .await
            .unwrap()
            .whitelist
            .contains("Bash")
    );

    let second = rig
        .sim
        .mention(ALICE, GENERAL, "second", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    wait_for_turn_end(&rig, GENERAL, &second, "pong to: second (tool ran)").await;
    assert!(approval_card(&rig, &second).is_none());
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn an_unanswered_approval_is_denied_when_it_times_out() {
    let rig = rig_with(Duration::from_millis(300)).await;
    rig.store
        .set_tool_mode(&user(ALICE), murtaugh_store::ToolMode::AllowedWhitelist)
        .await
        .unwrap();
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig.sim.mention(ALICE, GENERAL, "wait", None).await.unwrap();
    laptop.next_prompt().await;
    wait_for_turn_end(&rig, GENERAL, &ts, "pong to: wait (tool refused)").await;
    let card = eventually("the card to settle", || {
        approval_card(&rig, &ts).filter(|c| card_says(c, "Approval Timed Out"))
    })
    .await;
    assert!(card_says(&card, "No answer from <@U0ALICE01>"));
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn an_owner_in_denied_mode_has_every_tool_refused_without_a_card() {
    let rig = rig().await;
    rig.store
        .set_tool_mode(&user(ALICE), murtaugh_store::ToolMode::Denied)
        .await
        .unwrap();
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig.sim.mention(ALICE, GENERAL, "try", None).await.unwrap();
    laptop.next_prompt().await;
    wait_for_turn_end(&rig, GENERAL, &ts, "pong to: try (tool refused)").await;
    assert!(approval_card(&rig, &ts).is_none());
    rig.shutdown.cancel();
}
