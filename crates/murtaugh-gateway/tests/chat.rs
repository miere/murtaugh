#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

//! The real `serve` loop between the Slack simulator and scripted RAX nodes.

use std::sync::Arc;
use std::time::Duration;

use murtaugh_gateway::run::{self, Options};
use murtaugh_gateway::token;
use murtaugh_store::{NodeToken, SqliteStore, Store, UserId};
use rax::content::ContentBlock;
use rax::event::BackgroundEvent;
use rax::id::PromptId;
use rax::id::{RequestId, SessionId, ToolCallId};
use rax::interaction::{DisplayAnswer, PlanRequest, Question, QuestionOption, QuestionRequest};
use rax::resource::ReadResource;
use rax::session::{Initialized, NodeCapabilities, PromptAccepted, SessionCreated, ToolGate};
use rax::tool::{Decision, ToolCall, ToolKind};
use rax::{Event, GatewayCall, GatewayReply, NodeCall, NodeReply, Open};
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
    /// A metadata key the gateway reported it ignored.
    Ignored(String),
    /// The gateway ended the link over this node's metadata.
    Rejected(rax::Rejection),
}

struct FakeNode {
    handle: NodeHandle,
    seen: mpsc::UnboundedReceiver<Seen>,
    answers: tokio::sync::broadcast::Sender<DisplayAnswer>,
}

impl FakeNode {
    async fn next_prompt(&mut self) -> (SessionId, String) {
        let (session, text, _) = self.next_prompt_with_links().await;
        (session, text)
    }

    async fn next_prompt_with_links(&mut self) -> (SessionId, String, Vec<ContentBlock>) {
        loop {
            if let Seen::Prompt {
                session,
                text,
                links,
            } = within(self.seen.recv()).await.unwrap()
            {
                return (session, text, links);
            }
        }
    }

    /// The next thing the gateway said about this node's metadata.
    async fn next_notice(&mut self) -> Seen {
        loop {
            if let notice @ (Seen::Ignored(_) | Seen::Rejected(_)) =
                within(self.seen.recv()).await.unwrap()
            {
                return notice;
            }
        }
    }

    async fn update_metadata(&self, metadata: serde_json::Value) -> Result<NodeReply, CallError> {
        let metadata = serde_json::from_value(metadata).unwrap();
        within(
            self.handle
                .call(NodeCall::UpdateMetadata(rax::UpdateMetadata { metadata })),
        )
        .await
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
    rig_tuned(Tuning::default()).await
}

async fn rig_with(approval_timeout: Duration) -> Rig {
    rig_tuned(Tuning {
        approval_timeout,
        ..Tuning::default()
    })
    .await
}

#[derive(Clone, Copy)]
struct Tuning {
    approval_timeout: Duration,
    turn_idle_timeout: Duration,
    tool_ceiling: Duration,
}

impl Default for Tuning {
    fn default() -> Self {
        Self {
            approval_timeout: murtaugh_gateway::approval::TIMEOUT,
            turn_idle_timeout: murtaugh_gateway::chat::TURN_IDLE_TIMEOUT,
            tool_ceiling: murtaugh_gateway::chat::TOOL_CEILING,
        }
    }
}

async fn rig_tuned(tuning: Tuning) -> Rig {
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
    let shutdown = gateway_with(&sim, config, tuning);
    Rig {
        sim,
        _dir: dir,
        store,
        listen,
        shutdown,
    }
}

fn gateway(sim: &SlackSim, config: std::path::PathBuf) -> CancellationToken {
    gateway_with(sim, config, Tuning::default())
}

fn gateway_with(sim: &SlackSim, config: std::path::PathBuf, tuning: Tuning) -> CancellationToken {
    let shutdown = CancellationToken::new();
    let options = Options {
        slack_api: sim.api_base(),
        refresh: Duration::from_millis(100),
        approval_timeout: tuning.approval_timeout,
        prompt_timeout: murtaugh_gateway::prompts::TIMEOUT,
        turn_idle_timeout: tuning.turn_idle_timeout,
        tool_ceiling: tuning.tool_ceiling,
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
        self.node_declaring(owner, name, gateways, rax::Metadata::new())
            .await
    }

    /// A node whose owner lets in only `people`, beside themselves.
    async fn node_allowing(&self, owner: &str, name: &str, people: &[&str]) -> FakeNode {
        let access = serde_json::json!({"policy": "allow_list", "people": people});
        let metadata = rax::Metadata::from([("murtaugh_access".to_owned(), access)]);
        self.node_declaring(owner, name, &[self.listen], metadata)
            .await
    }

    async fn node_declaring(
        &self,
        owner: &str,
        name: &str,
        gateways: &[std::net::SocketAddr],
        metadata: rax::Metadata,
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
                disabled_at: None,
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
        let (answers, _) = tokio::sync::broadcast::channel::<DisplayAnswer>(16);
        let (cancels, _) = tokio::sync::broadcast::channel::<SessionId>(16);
        let (initialized, mut ready) = mpsc::channel(1);
        let script = Script {
            node: handle.clone(),
            seen,
            verdicts,
            answers: answers.clone(),
            cancels,
            sessions: Arc::new(Mutex::new(0u32)),
            refused: Arc::new(Mutex::new(false)),
            name: name.to_owned(),
            initialized,
            metadata,
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
                    NodeEvent::Answer(answer) => {
                        let _ = script.answers.send(answer);
                    }
                    NodeEvent::Unhandled {
                        body:
                            rax::Unhandled {
                                subject: rax::open::Subject::MetadataKey { key },
                                ..
                            },
                        ..
                    } => {
                        let _ = script.seen.send(Seen::Ignored(key));
                    }
                    NodeEvent::Rejected(rejection) => {
                        let _ = script.seen.send(Seen::Rejected(rejection));
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
            answers,
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
    answers: tokio::sync::broadcast::Sender<DisplayAnswer>,
    cancels: tokio::sync::broadcast::Sender<SessionId>,
    sessions: Arc<Mutex<u32>>,
    /// Set once a "refuse this once" prompt has been faulted, so the retry of it is taken.
    refused: Arc<Mutex<bool>>,
    name: String,
    initialized: mpsc::Sender<()>,
    metadata: rax::Metadata,
}

async fn answer(script: Script, id: RequestId, call: GatewayCall) {
    let Script {
        node,
        seen,
        verdicts,
        answers,
        cancels,
        sessions,
        refused,
        name,
        initialized,
        metadata,
    } = script;
    match call {
        GatewayCall::Initialize(_) => {
            let reply = GatewayReply::Initialize(Initialized {
                protocol_version: rax::PROTOCOL_VERSION,
                capabilities: NodeCapabilities {
                    tool_gate: ToolGate::EveryCall,
                    ..Default::default()
                },
                metadata,
            });
            node.reply(id, reply).await.unwrap();
            let _ = initialized.send(()).await;
        }
        GatewayCall::Cancel(target) => {
            let _ = cancels.send(target.session_id);
            node.reply(id, GatewayReply::Cancel).await.unwrap();
        }
        GatewayCall::RenewCredential => {
            let reply = GatewayReply::RenewCredential(rax::credential::CredentialRenewal::Started);
            node.reply(id, reply).await.unwrap();
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
            // "refuse this" always faults; "refuse this once" faults the first time only, so a
            // retry of it is taken and answered.
            let refuse = if text.contains("refuse this once") {
                let mut refused = refused.lock().await;
                let first = !*refused;
                *refused = true;
                first
            } else {
                text.contains("refuse this")
            };
            if refuse {
                let provider = rax::error::ProviderFailure {
                    kind: "overloaded_error".into(),
                    provider: Some("anthropic".into()),
                    status_code: Some(529),
                    message: None,
                    retryable: true,
                };
                let error = rax::Error::new(
                    rax::ErrorKind::Provider { provider },
                    "<@U0ALICE01> upstream said: Overloaded",
                );
                node.fault(id, error).await.unwrap();
                return;
            }
            node.reply(id.clone(), GatewayReply::Prompt(PromptAccepted::default()))
                .await
                .unwrap();
            if text.contains("fail mid-answer") {
                let started = Event::Message {
                    content: ContentBlock::text("Half an answer…").into(),
                };
                node.event(id.clone(), started).await.unwrap();
                let error = rax::Error::new(rax::ErrorKind::ToolCeiling, "40 tool calls used");
                node.event(id.clone(), Event::Error { error })
                    .await
                    .unwrap();
                node.end(id).await.unwrap();
                return;
            }
            if text.contains("silent") || text.contains("slow") || text.contains("long tool") {
                let mut cancelled = cancels.subscribe();
                let session = prompt.session_id.clone();
                if text.contains("slow") {
                    let started = Event::Message {
                        content: ContentBlock::text("Working on it slowly…").into(),
                    };
                    node.event(id.clone(), started).await.unwrap();
                }
                if text.contains("long tool") {
                    // A build that runs for minutes: announced once, then silent until it ends,
                    // which for this node it never does.
                    let running = ToolCall {
                        id: ToolCallId("tc-long".into()),
                        name: "Bash".into(),
                        title: Some("gradle build".into()),
                        kind: ToolKind::Execute,
                        input: Some(serde_json::json!({"command": "./gradlew build"})),
                        content: vec![],
                    };
                    node.event(id.clone(), Event::ToolCall { tool_call: running })
                        .await
                        .unwrap();
                }
                let waited = tokio::time::timeout(Duration::from_secs(20), async {
                    while let Ok(which) = cancelled.recv().await {
                        if which == session {
                            return;
                        }
                    }
                })
                .await;
                let reason = if waited.is_ok() {
                    "cancelled"
                } else {
                    "never cancelled"
                };
                // A real agent takes a moment to wind a cancelled turn down.
                tokio::time::sleep(Duration::from_millis(300)).await;
                let _ = seen.send(Seen::Prompt {
                    session: session.clone(),
                    text: format!("<{reason}>"),
                    links: vec![],
                });
                node.event(id.clone(), Event::Complete { stop_reason: None })
                    .await
                    .unwrap();
                node.end(id).await.unwrap();
                return;
            }
            let tool = ToolCall {
                id: ToolCallId("tc1".into()),
                name: if text.contains("no approval") {
                    "mcp__riggs__ask".into()
                } else {
                    "Bash".into()
                },
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
            let mut heard = String::new();
            let asked = if text.contains("ask me") {
                Some(Event::Question {
                    question: QuestionRequest {
                        id: PromptId("q1".into()),
                        title: Some("Pick a database".into()),
                        questions: vec![
                            Question {
                                key: "db".into(),
                                header: Some("Storage".into()),
                                question: "Which database?".into(),
                                options: vec![
                                    QuestionOption {
                                        label: "Postgres".into(),
                                        description: Some("The big one".into()),
                                    },
                                    QuestionOption {
                                        label: "SQLite".into(),
                                        description: None,
                                    },
                                ],
                                multi_select: false,
                            },
                            Question {
                                key: "notes".into(),
                                header: None,
                                question: "Anything else?".into(),
                                options: vec![],
                                multi_select: false,
                            },
                        ],
                    },
                })
            } else if text.contains("plan") {
                Some(Event::Plan {
                    plan: PlanRequest {
                        id: PromptId("p1".into()),
                        title: None,
                        plan: "1. Drop the table\n2. Rebuild it".into(),
                    },
                })
            } else {
                None
            };
            if text.contains("sign me in") {
                use rax::interaction::{SignInRequest, SignInSettled, SignInState};
                let mut answered = answers.subscribe();
                let request = SignInRequest {
                    id: PromptId("t1".into()),
                    tool: "claude".into(),
                    url: None,
                    needs_code: false,
                    command: Some("claude login".into()),
                };
                node.event(id.clone(), Event::SignIn { sign_in: request })
                    .await
                    .unwrap();
                let answer = tokio::time::timeout(Duration::from_secs(20), answered.recv())
                    .await
                    .unwrap()
                    .unwrap();
                let settled = SignInSettled {
                    id: PromptId("t1".into()),
                    state: SignInState::Success,
                    reason: None,
                    url: None,
                };
                node.event(
                    id.clone(),
                    Event::SignInSettled {
                        sign_in_settled: settled,
                    },
                )
                .await
                .unwrap();
                heard = format!(" [sign-in {:?}]", answer.outcome);
            }
            if let Some(asked) = asked {
                let mut answered = answers.subscribe();
                node.event(id.clone(), asked).await.unwrap();
                let answer = tokio::time::timeout(Duration::from_secs(20), answered.recv())
                    .await
                    .unwrap()
                    .unwrap();
                heard = format!(
                    " [{:?} {:?} {:?} by {:?}]",
                    answer.outcome, answer.answers, answer.choice, answer.user_id
                );
            }
            let ruling = if allowed { "tool ran" } else { "tool refused" };
            let reply = Event::Message {
                content: ContentBlock::text(format!(
                    "**{name}** says pong to: {last} ({ruling}){heard}"
                ))
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
    assert_eq!(stream.plan_blocks.len(), 1);
    let tasks = &stream.plan_blocks[0].tasks;
    assert_eq!(tasks.len(), 1);
    assert_eq!(
        (tasks[0].title.as_str(), tasks[0].status.as_str()),
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
        replies.iter().any(|m| m.text == "No machine available")
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
        .position(|m| m.text.contains("went offline. Continuing on _*desktop*_"))
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
            if replies
                .iter()
                .any(|text| text == "No machine available" || text == "Machine offline")
            {
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
                    .position(|text| text.contains("picking this conversation up on _*laptop*_"))
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
            if replies
                .iter()
                .any(|text| text == "No machine available" || text == "Machine offline")
            {
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

/// Both halves in one test, because a catch-up that found no history would announce nothing either
/// way and leave the silence proving nothing.
#[tokio::test(flavor = "multi_thread")]
async fn only_a_machine_that_is_not_the_admins_says_it_picked_a_thread_up() {
    let rig = rig().await;
    let mut hangar = rig.node(ADMIN, "hangar").await;
    let mut laptop = rig.node(ALICE, "laptop").await;

    let quiet = rig
        .sim
        .say(ADMIN, GENERAL, "thinking out loud", None)
        .await
        .unwrap();
    rig.sim
        .mention(ADMIN, GENERAL, "what do you reckon?", Some(&quiet))
        .await
        .unwrap();
    hangar.next_prompt().await;
    let replies = rig
        .bot_replies(&quiet, |replies| {
            replies.iter().any(|m| m.text.contains("pong to:"))
        })
        .await;
    assert!(
        !replies
            .iter()
            .any(|m| m.text.contains("picking this conversation up")),
        "the admin's own machine announced itself: {replies:?}"
    );

    let loud = rig
        .sim
        .say(ALICE, GENERAL, "thinking out loud", None)
        .await
        .unwrap();
    rig.sim
        .mention(ALICE, GENERAL, "what do you reckon?", Some(&loud))
        .await
        .unwrap();
    laptop.next_prompt().await;
    let replies = rig
        .bot_replies(&loud, |replies| {
            replies.iter().any(|m| m.text.contains("pong to:"))
        })
        .await;
    assert!(
        replies.iter().any(|m| m
            .text
            .contains("picking this conversation up on _*laptop*_")),
        "somebody else's machine said nothing: {replies:?}"
    );
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_thread_moves_to_the_machine_the_menu_picked_and_catches_it_up() {
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
    let mut desktop = rig.node(ALICE, "desktop").await;

    rig.sim
        .mention(ALICE, GENERAL, "/node", Some(&ts))
        .await
        .unwrap();
    let menu = eventually("the node menu", || {
        rig.sim
            .ephemeral_messages()
            .into_iter()
            .rev()
            .find(|e| e.user == ALICE && !e.blocks.is_empty())
    })
    .await;
    assert_eq!(menu.thread_ts.as_deref(), Some(ts.as_str()));
    let options = menu
        .blocks
        .iter()
        .find_map(|block| block["elements"][0]["options"].as_array().cloned())
        .expect("the menu offered nothing");
    let names: Vec<&str> = options
        .iter()
        .filter_map(|o| o["text"]["text"].as_str())
        .collect();
    assert_eq!(names, ["desktop", "laptop"], "{options:?}");
    let pick = options
        .iter()
        .find(|o| o["text"]["text"] == "desktop")
        .and_then(|o| o["value"].as_str())
        .unwrap()
        .to_owned();

    rig.sim
        .choose(ALICE, murtaugh_gateway::picker::CHOOSE, &pick)
        .await
        .unwrap();
    eventually("the move to be confirmed", || {
        rig.sim
            .ephemerals()
            .into_iter()
            .find(|(_, user, text)| user == ALICE && text.contains("now runs on *desktop*"))
    })
    .await;

    rig.sim
        .mention(ALICE, GENERAL, "second question", Some(&ts))
        .await
        .unwrap();
    let (_, text) = desktop.next_prompt().await;
    assert!(text.contains("second question"), "{text}");
    assert!(
        text.contains("first question"),
        "the new machine was not caught up: {text}"
    );
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
    let card = thread
        .iter()
        .find(|m| m.text == "laptop could not attach chart.png")
        .unwrap_or_else(|| panic!("no card for the file that never arrived: {thread:?}"));
    assert!(card_says(card, "murtaugh_alert_card"));
    assert!(card_says(card, "bytes arrived"));
    // The answer is the agent's; what became of the file is the gateway's, and sits apart.
    let answer = thread
        .iter()
        .find(|m| m.text.contains("pong to: attach and lie"))
        .expect("the answer went missing");
    assert!(!answer.text.contains("Could not attach"), "{answer:?}");
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
async fn the_nodes_own_talking_tools_never_wait_for_approval() {
    let rig = rig().await;
    rig.store
        .set_tool_mode(&user(ALICE), murtaugh_store::ToolMode::AllowedWhitelist)
        .await
        .unwrap();
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "ask me, no approval", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    let card = eventually("the question card", || prompt_card(&rig, &ts)).await;
    assert!(approval_card(&rig, &ts).is_none(), "the question was gated");
    rig.sim
        .click(ALICE, GENERAL, &card.ts, murtaugh_gateway::prompts::CHAT)
        .await
        .unwrap();
    wait_for_turn_end(&rig, GENERAL, &ts, "[Chat {} None").await;
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

fn prompt_card(rig: &Rig, ts: &str) -> Option<SimMessage> {
    rig.sim.thread(GENERAL, ts).into_iter().find(|m| {
        m.blocks
            .as_ref()
            .is_some_and(|b| b.to_string().contains("murtaugh_prompt_card"))
    })
}

#[tokio::test(flavor = "multi_thread")]
async fn a_question_is_answered_in_place_once_every_question_has_an_answer() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "ask me", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    let card = eventually("the question card", || prompt_card(&rig, &ts)).await;
    assert!(card_says(&card, "1. Storage - Which database?"));
    assert!(card_says(&card, "_Postgres_ - The big one"));
    assert!(card_says(&card, "plain_text_input"));

    let only_db = serde_json::json!({
        "prompt_q:0": {"prompt_answer": {"type": "radio_buttons", "selected_option": {"value": "0"}}},
    });
    rig.sim
        .click_with_state(
            ALICE,
            GENERAL,
            &card.ts,
            murtaugh_gateway::prompts::SUBMIT,
            only_db,
        )
        .await
        .unwrap();
    eventually("the note about the missing answer", || {
        rig.sim
            .ephemerals()
            .iter()
            .any(|(_, who, text)| who == ALICE && text == "One question still needs an answer.")
            .then_some(())
    })
    .await;

    rig.sim
        .click(STRANGER, GENERAL, &card.ts, murtaugh_gateway::prompts::CHAT)
        .await
        .unwrap();
    eventually("the note to the stranger", || {
        rig.sim
            .ephemerals()
            .iter()
            .any(|(_, who, _)| who == STRANGER)
            .then_some(())
    })
    .await;

    let both = serde_json::json!({
        "prompt_q:0": {"prompt_answer": {"type": "radio_buttons", "selected_option": {"value": "0"}}},
        "prompt_q:1": {"prompt_answer": {"type": "plain_text_input", "value": "Use WAL"}},
    });
    rig.sim
        .click_with_state(
            ALICE,
            GENERAL,
            &card.ts,
            murtaugh_gateway::prompts::SUBMIT,
            both,
        )
        .await
        .unwrap();
    wait_for_turn_end(
        &rig,
        GENERAL,
        &ts,
        r#"[Answered {"db": ["Postgres"], "notes": ["Use WAL"]} None by Some("U0ALICE01")]"#,
    )
    .await;
    let settled = prompt_card(&rig, &ts).unwrap();
    assert!(card_says(&settled, "*✓* Postgres"), "{:?}", settled.blocks);
    assert!(card_says(&settled, "Answered by <@U0ALICE01>"));
    assert!(!card_says(&settled, "prompt_submit"), "the buttons stayed");
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_plan_is_approved_from_its_card() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "make a plan", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    let card = eventually("the plan card", || prompt_card(&rig, &ts)).await;
    assert!(card_says(&card, "Plan Ready for Review"));
    assert!(card_says(&card, "1. Drop the table"));
    rig.sim
        .click(ALICE, GENERAL, &card.ts, murtaugh_gateway::prompts::PROCEED)
        .await
        .unwrap();
    wait_for_turn_end(&rig, GENERAL, &ts, r#"Some(Proceed) by Some("U0ALICE01")]"#).await;
    let settled = prompt_card(&rig, &ts).unwrap();
    assert!(
        card_says(&settled, "Approved by <@U0ALICE01>"),
        "{:?}",
        settled.blocks
    );
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn chat_about_this_hands_the_question_back_to_the_conversation() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "ask me", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    let card = eventually("the question card", || prompt_card(&rig, &ts)).await;
    rig.sim
        .click(ALICE, GENERAL, &card.ts, murtaugh_gateway::prompts::CHAT)
        .await
        .unwrap();
    wait_for_turn_end(&rig, GENERAL, &ts, "[Chat {} None").await;
    assert!(card_says(
        &prompt_card(&rig, &ts).unwrap(),
        "Raised by <@U0ALICE01>"
    ));
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_thread_whose_machine_left_with_no_other_gets_a_machine_offline_card() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "first", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    wait_for_turn_end(&rig, GENERAL, &ts, "pong to: first").await;

    laptop.handle.close().await;
    tokio::time::sleep(Duration::from_millis(300)).await;
    rig.sim
        .mention(ALICE, GENERAL, "still there?", Some(&ts))
        .await
        .unwrap();
    let card = eventually("the offline card", || {
        rig.sim
            .thread(GENERAL, &ts)
            .into_iter()
            .find(|m| m.text == "Machine offline")
    })
    .await;
    assert!(card_says(&card, "murtaugh_alert_card"));
    assert!(card_says(&card, "no other machine you may use"));
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_refused_message_gets_a_card_that_diagnoses_it_rather_than_dumping_it() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "refuse this", None)
        .await
        .unwrap();
    laptop.next_prompt().await;

    let card = eventually("the refusal card", || {
        rig.sim
            .thread(GENERAL, &ts)
            .into_iter()
            .find(|m| m.text == "laptop could not take this message")
    })
    .await;

    assert!(card_says(&card, "murtaugh_alert_card"));
    // The kind carried the status and the verdict on retrying, so the card says both.
    assert!(card_says(&card, "HTTP 529"));
    assert!(card_says(&card, "overloaded_error"));
    assert!(card_says(&card, "retryable"));
    // The producer's own words are kept as the detail, with their mention defused.
    assert!(card_says(&card, "upstream said: Overloaded"));
    assert!(card_says(&card, "&lt;@U0ALICE01&gt;"));
    // Nothing anywhere in the thread names the transport it came over.
    let thread = rig.sim.thread(GENERAL, &ts);
    assert!(
        !format!("{thread:?}").contains("rax-tokio"),
        "the wire's own wording reached Slack"
    );
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_send_again_button_belongs_to_its_sender_and_spends_itself() {
    use murtaugh_gateway::faults::SEND_AGAIN;

    let rig = rig().await;
    rig.store.set_allowed(&user(BOB), true).await.unwrap();
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "refuse this once, no approval", None)
        .await
        .unwrap();
    laptop.next_prompt().await;

    let card = eventually("the refusal card", || {
        rig.sim
            .thread(GENERAL, &ts)
            .into_iter()
            .find(|m| m.text == "laptop could not take this message")
    })
    .await;
    // The provider called its own failure retryable, so the card carries the button.
    assert!(card_says(&card, SEND_AGAIN));
    assert!(card_says(&card, "Send Again"));

    // Bob may chat in here, and it is still not his message to send.
    rig.sim
        .click(BOB, GENERAL, &card.ts, SEND_AGAIN)
        .await
        .unwrap();
    eventually("Bob being turned away", || {
        rig.sim
            .ephemerals()
            .into_iter()
            .find(|(_, who, text)| who == BOB && text.contains("not your message"))
    })
    .await;

    // Alice's press sends the very same message again, and this time it is taken.
    rig.sim
        .click(ALICE, GENERAL, &card.ts, SEND_AGAIN)
        .await
        .unwrap();
    let (_, sent) = within(laptop.next_prompt()).await;
    assert!(sent.contains("refuse this once"), "{sent}");
    wait_for_turn_end(&rig, GENERAL, &ts, "pong to: refuse this once").await;

    // The offer is spent: one card sends one message however often it is pressed.
    rig.sim
        .click(ALICE, GENERAL, &card.ts, SEND_AGAIN)
        .await
        .unwrap();
    eventually("the lapsed note", || {
        rig.sim
            .ephemerals()
            .into_iter()
            .find(|(_, who, text)| who == ALICE && text.contains("no longer on offer"))
    })
    .await;
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_turn_that_fails_partway_keeps_its_answer_and_gets_a_card_of_its_own() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "fail mid-answer", None)
        .await
        .unwrap();
    laptop.next_prompt().await;

    let card = eventually("the failure card", || {
        rig.sim
            .thread(GENERAL, &ts)
            .into_iter()
            .find(|m| m.text == "laptop could not finish this answer")
    })
    .await;
    assert!(card_says(&card, "every tool call"));
    assert!(card_says(&card, "40 tool calls used"));
    // The same message would hit the same ceiling, so this card offers no button.
    assert!(!card_says(&card, murtaugh_gateway::faults::SEND_AGAIN));

    let thread = rig.sim.thread(GENERAL, &ts);
    let answer = thread
        .iter()
        .find(|m| m.text.contains("Half an answer"))
        .expect("the half-answer went missing");
    // The fault is the gateway talking, so it is not pasted inside what the agent said.
    assert!(!answer.text.contains("The turn failed"));
    assert!(!answer.text.contains("40 tool calls used"));
    // And a turn that said something and then failed is not also "done, with nothing to say".
    assert!(!thread.iter().any(|m| m.text.contains("nothing to say")));
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_failing_credential_is_told_to_the_owner_by_dm_and_settled_on_recovery() {
    let rig = rig().await;
    let laptop = rig.node(ALICE, "laptop").await;
    let health = |degraded: bool| rax::credential::CredentialHealth {
        credential: "claude".into(),
        degraded,
        reason: degraded.then(|| "token expired".to_owned()),
        since: None,
        expires_at: None,
    };
    let call = rax::NodeCall::CredentialHealth(health(true));
    within(laptop.handle.call(call)).await.unwrap();
    within(
        laptop
            .handle
            .call(rax::NodeCall::CredentialHealth(health(true))),
    )
    .await
    .unwrap();
    let dm = rig.sim.im_channel(ALICE).expect("no DM with the owner");
    let cards = rig.sim.messages(&dm);
    assert_eq!(cards.len(), 1, "a repeat report posted a second card");
    assert_eq!(cards[0].text, "A credential on laptop is failing");
    assert!(card_says(&cards[0], "token expired"));
    assert!(card_says(&cards[0], "credential_renew"));

    within(
        laptop
            .handle
            .call(rax::NodeCall::CredentialHealth(health(false))),
    )
    .await
    .unwrap();
    let cards = rig.sim.messages(&dm);
    assert_eq!(cards.len(), 1);
    assert_eq!(cards[0].text, "A credential on laptop recovered");
    assert!(!card_says(&cards[0], "credential_renew"));
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

fn dm_card(rig: &Rig, user: &str, marker: &str) -> Option<SimMessage> {
    let dm = rig.sim.im_channel(user)?;
    rig.sim.messages(&dm).into_iter().find(|m| {
        m.blocks
            .as_ref()
            .is_some_and(|b| b.to_string().contains(marker))
    })
}

#[tokio::test(flavor = "multi_thread")]
async fn a_sign_in_is_worked_through_by_the_owner_alone_in_their_dm() {
    use rax::interaction::{SignInRequest, SignInSettled, SignInState};
    let rig = rig().await;
    let laptop = rig.node(ALICE, "laptop").await;
    let mut answers = laptop.answers.subscribe();
    let request = SignInRequest {
        id: PromptId("s1".into()),
        tool: "claude".into(),
        url: None,
        needs_code: true,
        command: Some("claude setup-token".into()),
    };
    within(laptop.handle.call(rax::NodeCall::SignIn(request.clone())))
        .await
        .unwrap();
    let busy = within(laptop.handle.call(rax::NodeCall::SignIn(request))).await;
    assert!(
        matches!(&busy, Err(rax_tokio::CallError::Fault(e)) if e.kind == rax::ErrorKind::Credential),
        "{busy:?}"
    );
    let dm = rig.sim.im_channel(ALICE).unwrap();
    let card = eventually("the owner's card", || {
        dm_card(&rig, ALICE, "murtaugh_signin_card")
    })
    .await;
    assert!(card_says(&card, "claude setup-token"));
    assert!(card_says(&card, "signin_approve"));

    rig.sim
        .click(BOB, &dm, &card.ts, murtaugh_gateway::signin::APPROVE)
        .await
        .unwrap();
    rig.sim
        .click(ALICE, &dm, &card.ts, murtaugh_gateway::signin::APPROVE)
        .await
        .unwrap();
    let approved = within(answers.recv()).await.unwrap();
    assert_eq!(approved.outcome, rax::interaction::DisplayOutcome::Approved);
    assert!(
        rig.sim
            .ephemerals()
            .iter()
            .any(|(_, who, text)| who == BOB && text.contains("Only <@U0ALICE01>"))
    );

    let settle = |state: SignInState, url: Option<&str>| {
        rax::NodeCall::SignInSettled(SignInSettled {
            id: PromptId("s1".into()),
            state,
            reason: None,
            url: url.map(str::to_owned),
        })
    };
    within(laptop.handle.call(settle(
        SignInState::Ready,
        Some("https://claude.ai/oauth?x=1"),
    )))
    .await
    .unwrap();
    let card = dm_card(&rig, ALICE, "murtaugh_signin_card").unwrap();
    assert!(card_says(&card, "https://claude.ai/oauth?x=1"));
    assert!(card_says(&card, "signin_code_input"));

    let code = serde_json::json!({
        "signin_code_input": {"signin_code_value": {"type": "plain_text_input", "value": " ABC-123 "}},
    });
    rig.sim
        .click_with_state(
            ALICE,
            &dm,
            &card.ts,
            murtaugh_gateway::signin::SUBMIT_CODE,
            code,
        )
        .await
        .unwrap();
    let answered = within(answers.recv()).await.unwrap();
    assert_eq!(answered.code.as_deref(), Some("ABC-123"));

    within(laptop.handle.call(settle(SignInState::Confirming, None)))
        .await
        .unwrap();
    let confirmed = within(answers.recv()).await.unwrap();
    assert_eq!(
        confirmed.outcome,
        rax::interaction::DisplayOutcome::Approved
    );
    within(laptop.handle.call(settle(SignInState::Success, None)))
        .await
        .unwrap();
    let card = dm_card(&rig, ALICE, "murtaugh_signin_card").unwrap();
    assert!(
        card_says(&card, "Authentication succeeded."),
        "{:?}",
        card.blocks
    );
    assert!(!card_says(&card, "signin_deny"), "the buttons stayed");
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_sign_in_raised_in_a_turn_tells_the_thread_and_goes_to_the_owner() {
    let rig = rig().await;
    rig.store.set_allowed(&user(BOB), true).await.unwrap();
    let mut desktop = rig.node(ADMIN, "desktop").await;
    tokio::time::sleep(Duration::from_millis(300)).await;
    let ts = rig
        .sim
        .mention(BOB, GENERAL, "sign me in", None)
        .await
        .unwrap();
    desktop.next_prompt().await;
    let note = eventually("the note in the thread", || {
        rig.sim.thread(GENERAL, &ts).into_iter().find(|m| {
            m.blocks
                .as_ref()
                .is_some_and(|b| b.to_string().contains("murtaugh_signin_card"))
        })
    })
    .await;
    assert!(card_says(
        &note,
        "<@U0ADMIN01> has been sent a direct message"
    ));
    assert!(
        !card_says(&note, "claude login"),
        "the command leaked into the thread"
    );

    let dm = rig.sim.im_channel(ADMIN).unwrap();
    let card = dm_card(&rig, ADMIN, "murtaugh_signin_card").unwrap();
    rig.sim
        .click(ADMIN, &dm, &card.ts, murtaugh_gateway::signin::APPROVE)
        .await
        .unwrap();
    wait_for_turn_end(&rig, GENERAL, &ts, "[sign-in Approved]").await;
    let note = rig
        .sim
        .thread(GENERAL, &ts)
        .into_iter()
        .find(|m| m.ts == note.ts)
        .unwrap();
    assert!(
        card_says(&note, "completed the sign-in"),
        "{:?}",
        note.blocks
    );
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

async fn home_of(rig: &Rig, user: &str) -> String {
    rig.sim.open_home(user).await.unwrap();
    eventually("the Home tab", || {
        rig.sim.home(user).map(|home| home.to_string())
    })
    .await
}

#[tokio::test(flavor = "multi_thread")]
async fn the_home_tab_shows_the_version_and_the_viewers_nodes_or_all_for_the_admin() {
    let rig = rig().await;
    let _laptop = rig.node(ALICE, "laptop").await;
    let _desktop = rig.node(ADMIN, "desktop").await;
    rig.store
        .add_node_token(&NodeToken {
            selector: "0000000000000000".into(),
            secret_hash: "x".into(),
            owner: user(ALICE),
            name: "old-mac".into(),
            created_at: OffsetDateTime::now_utc(),
            revoked_at: None,
            disabled_at: None,
        })
        .await
        .unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;

    let alice = home_of(&rig, ALICE).await;
    assert!(
        alice.contains(murtaugh_gateway::version::VERSION),
        "{alice}"
    );
    assert!(alice.contains("Your nodes"));
    assert!(alice.contains("*laptop*") && alice.contains("Connected · 0 live conversations"));
    assert!(alice.contains("*old-mac*") && alice.contains("Offline"));
    assert!(
        !alice.contains("desktop"),
        "Alice sees someone else's node: {alice}"
    );

    let admin = home_of(&rig, ADMIN).await;
    assert!(admin.contains("All nodes"));
    assert!(
        admin.contains("*desktop*") && admin.contains("*laptop* · <@U0ALICE01>"),
        "{admin}"
    );

    let stranger = home_of(&rig, STRANGER).await;
    assert!(stranger.contains("don't have access"));
    assert!(!stranger.contains("laptop"));
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

async fn node_selector(rig: &Rig, owner: &str, name: &str) -> String {
    rig.store
        .node_tokens()
        .await
        .unwrap()
        .into_iter()
        .find(|token| token.owner == user(owner) && token.name == name)
        .unwrap()
        .selector
}

#[tokio::test(flavor = "multi_thread")]
async fn a_disabled_node_drops_its_thread_and_takes_it_back_once_re_enabled() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let selector = node_selector(&rig, ALICE, "laptop").await;

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

    let before = home_of(&rig, ALICE).await;
    assert!(before.contains("\"Disable\""), "{before}");

    rig.sim
        .click_home(
            ALICE,
            murtaugh_gateway::home::TOGGLE,
            &format!("{selector}:disable"),
        )
        .await
        .unwrap();
    let disabled = eventually("the disabled row", || {
        let home = rig.sim.home(ALICE)?.to_string();
        home.contains("Disabled").then_some(home)
    })
    .await;
    assert!(disabled.contains("\"Enable\""), "{disabled}");

    rig.sim
        .mention(ALICE, GENERAL, "second question", Some(&ts))
        .await
        .unwrap();
    rig.bot_replies(&ts, |replies| {
        replies.iter().any(|m| m.text == "Machine offline")
    })
    .await;

    rig.sim
        .click_home(
            ALICE,
            murtaugh_gateway::home::TOGGLE,
            &format!("{selector}:enable"),
        )
        .await
        .unwrap();
    eventually("the re-enabled row", || {
        let home = rig.sim.home(ALICE)?.to_string();
        (!home.contains("Disabled")).then_some(home)
    })
    .await;

    rig.sim
        .mention(ALICE, GENERAL, "third question", Some(&ts))
        .await
        .unwrap();
    let (_, text) = laptop.next_prompt().await;
    assert!(text.ends_with("third question"), "{text}");
    rig.bot_replies(&ts, |replies| {
        replies
            .iter()
            .any(|m| m.text.contains("pong to: third question"))
    })
    .await;
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

async fn next_cancel(node: &mut FakeNode) -> String {
    loop {
        let (_, text) = node.next_prompt().await;
        if text.starts_with('<') {
            return text;
        }
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn a_message_mid_turn_interrupts_it_and_the_waiting_ones_run_together() {
    let rig = rig().await;
    rig.store.set_allowed(&user(BOB), true).await.unwrap();
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "slow job", None)
        .await
        .unwrap();
    assert_eq!(laptop.next_prompt().await.1, "slow job");
    rig.bot_replies(&ts, |r| r.iter().any(|m| m.text.contains("slowly")))
        .await;

    rig.sim
        .mention(ALICE, GENERAL, "actually, do this", Some(&ts))
        .await
        .unwrap();
    rig.sim
        .mention(BOB, GENERAL, "and this too", Some(&ts))
        .await
        .unwrap();
    assert_eq!(next_cancel(&mut laptop).await, "<cancelled>");
    let (_, merged) = laptop.next_prompt().await;
    assert!(
        merged == "<@U0ALICE01>: actually, do this\n\n<@U0BOB0001>: and this too",
        "{merged}"
    );
    let replies = rig
        .bot_replies(&ts, |r| r.iter().any(|m| m.text.contains("pong to:")))
        .await;
    assert!(
        replies.iter().any(|m| m
            .text
            .contains("_*Notice*_: interrupted by a newer message.")),
        "{:?}",
        replies.iter().map(|m| &m.text).collect::<Vec<_>>()
    );
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn stop_inside_a_thread_cancels_its_turn_and_nothing_else_runs() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "slow job", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    rig.bot_replies(&ts, |r| r.iter().any(|m| m.text.contains("slowly")))
        .await;

    rig.sim.slash(ALICE, GENERAL, "/stop", "").await.unwrap();
    rig.sim
        .slash_in_thread(ALICE, GENERAL, Some(&ts), "/carmen", "stop")
        .await
        .unwrap();
    assert_eq!(next_cancel(&mut laptop).await, "<cancelled>");
    let replies = rig
        .bot_replies(&ts, |r| {
            r.iter().any(|m| m.text.contains("_*Notice*_: stopped."))
        })
        .await;
    assert!(!replies.iter().any(|m| m.text.contains("pong to:")));
    rig.sim
        .slash_in_thread(ALICE, GENERAL, Some(&ts), "/stop", "")
        .await
        .unwrap();
    // The stop that landed said so in the thread, so only the two that could not stop anything
    // are worth a note.
    let notes = eventually("both notes", || {
        let mut notes: Vec<String> = rig
            .sim
            .ephemerals()
            .into_iter()
            .filter(|(_, who, _)| who == ALICE)
            .map(|(_, _, text)| text)
            .collect();
        notes.sort();
        (notes.len() == 2).then_some(notes)
    })
    .await;
    assert_eq!(
        notes,
        [
            "Nothing to stop.",
            "Slack does not run slash commands inside threads. Mention me with `/stop` in the thread you want to stop.",
        ]
    );
    rig.sim
        .slash_in_thread(STRANGER, GENERAL, Some(&ts), "/stop", "")
        .await
        .unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        rig.sim
            .ephemerals()
            .iter()
            .all(|(_, who, _)| who != STRANGER)
    );
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_mentioned_stop_cancels_the_turn_and_never_reaches_the_node() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "slow job", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    rig.bot_replies(&ts, |r| r.iter().any(|m| m.text.contains("slowly")))
        .await;

    rig.sim
        .mention(ALICE, GENERAL, "/stop", Some(&ts))
        .await
        .unwrap();
    assert_eq!(next_cancel(&mut laptop).await, "<cancelled>");
    let replies = rig
        .bot_replies(&ts, |r| {
            r.iter().any(|m| m.text.contains("_*Notice*_: stopped."))
        })
        .await;
    assert!(!replies.iter().any(|m| m.text.contains("pong to:")));
    // The gateway answered it, so it is never sent on as the prompt that replaces the turn.
    assert!(
        tokio::time::timeout(Duration::from_millis(300), laptop.next_prompt())
            .await
            .is_err()
    );

    // The same verb at the root of a channel has no turn to act on, and says so.
    rig.sim
        .mention(ALICE, GENERAL, "/stop", None)
        .await
        .unwrap();
    // One note, not two: the stop itself was answered by the marker in the thread.
    let notes = eventually("the note", || {
        let notes: Vec<String> = rig
            .sim
            .ephemerals()
            .into_iter()
            .filter(|(_, who, _)| who == ALICE)
            .map(|(_, _, text)| text)
            .collect();
        (notes.len() == 1).then_some(notes)
    })
    .await;
    assert_eq!(notes, ["`/stop` only works inside a thread."]);
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_command_in_backticks_is_a_prompt_the_node_answers() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "`/stop`", None)
        .await
        .unwrap();
    assert_eq!(laptop.next_prompt().await.1, "`/stop`");
    rig.bot_replies(&ts, |r| r.iter().any(|m| m.text.contains("pong to:")))
        .await;
    // Nothing was intercepted, so nobody was told a command had run.
    assert_eq!(rig.sim.ephemerals(), vec![]);
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_turn_the_agent_goes_silent_on_is_stopped_after_the_idle_timeout() {
    let rig = rig_tuned(Tuning {
        turn_idle_timeout: Duration::from_millis(400),
        ..Tuning::default()
    })
    .await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "go silent", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    assert_eq!(next_cancel(&mut laptop).await, "<cancelled>");
    rig.bot_replies(&ts, |r| r.iter().any(|m| m.text.contains("went quiet")))
        .await;

    let next = rig
        .sim
        .mention(ALICE, GENERAL, "hello again", Some(&ts))
        .await
        .unwrap();
    assert_eq!(laptop.next_prompt().await.1, "hello again");
    let _ = next;
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_tool_still_running_keeps_a_silent_turn_alive() {
    let rig = rig_tuned(Tuning {
        turn_idle_timeout: Duration::from_millis(300),
        ..Tuning::default()
    })
    .await;
    rig.store
        .set_tool_mode(&user(ALICE), murtaugh_store::ToolMode::AlwaysAllowed)
        .await
        .unwrap();
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "run a long tool", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    tokio::time::sleep(Duration::from_millis(900)).await;
    assert!(
        !rig.sim
            .thread(GENERAL, &ts)
            .iter()
            .any(|m| m.text.contains("went quiet")),
        "the turn was stopped while a tool ran"
    );
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_tool_that_never_finishes_is_stopped_at_its_ceiling() {
    let rig = rig_tuned(Tuning {
        turn_idle_timeout: Duration::from_millis(200),
        tool_ceiling: Duration::from_millis(500),
        ..Tuning::default()
    })
    .await;
    rig.store
        .set_tool_mode(&user(ALICE), murtaugh_store::ToolMode::AlwaysAllowed)
        .await
        .unwrap();
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "run a long tool", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    assert_eq!(next_cancel(&mut laptop).await, "<cancelled>");
    rig.bot_replies(&ts, |r| {
        r.iter().any(|m| m.text.contains("without finishing"))
    })
    .await;
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_card_waiting_on_a_person_keeps_a_silent_turn_alive() {
    let rig = rig_tuned(Tuning {
        turn_idle_timeout: Duration::from_millis(300),
        ..Tuning::default()
    })
    .await;
    let mut laptop = rig.node(ALICE, "laptop").await;
    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "ask me", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    let card = eventually("the question card", || prompt_card(&rig, &ts)).await;
    tokio::time::sleep(Duration::from_millis(900)).await;
    assert!(
        !rig.sim
            .thread(GENERAL, &ts)
            .iter()
            .any(|m| m.text.contains("went quiet")),
        "the turn was stopped while a card waited"
    );
    rig.sim
        .click(ALICE, GENERAL, &card.ts, murtaugh_gateway::prompts::CHAT)
        .await
        .unwrap();
    wait_for_turn_end(&rig, GENERAL, &ts, "[Chat {} None").await;
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_session_that_wakes_up_after_its_turn_answers_in_its_thread() {
    let rig = rig().await;
    let mut laptop = rig.node(ALICE, "laptop").await;

    let ts = rig.sim.mention(ALICE, GENERAL, "ping", None).await.unwrap();
    let (session, _) = laptop.next_prompt().await;
    wait_for_turn_end(&rig, GENERAL, &ts, "says pong to: ping").await;
    assert!(matches!(
        within(laptop.seen.recv()).await.unwrap(),
        Seen::Verdict { allowed: true }
    ));

    // A sub-agent the turn left running asks for a tool, then the task it ran finishes and the
    // agent writes about it, all with no turn open.
    let asked = ToolCall {
        id: ToolCallId("tc-late".into()),
        name: "Bash".into(),
        title: Some("cargo test".into()),
        kind: ToolKind::Execute,
        input: Some(serde_json::json!({"command": "cargo test"})),
        content: vec![],
    };
    laptop
        .handle
        .background(
            session.clone(),
            BackgroundEvent::ToolCall { tool_call: asked },
        )
        .await
        .unwrap();
    assert!(matches!(
        within(laptop.seen.recv()).await.unwrap(),
        Seen::Verdict { allowed: true }
    ));
    let woke = BackgroundEvent::Message {
        content: ContentBlock::text("The background build finished green.").into(),
    };
    laptop
        .handle
        .background(session.clone(), woke)
        .await
        .unwrap();
    laptop
        .handle
        .background(session, BackgroundEvent::Complete { stop_reason: None })
        .await
        .unwrap();

    rig.bot_replies(&ts, |replies| {
        replies.iter().any(|message| {
            message
                .text
                .contains("The background build finished green.")
        })
    })
    .await;
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_background_event_for_a_session_no_thread_holds_is_dropped() {
    let rig = rig().await;
    let laptop = rig.node(ALICE, "laptop").await;

    let stray = BackgroundEvent::Message {
        content: ContentBlock::text("nobody asked").into(),
    };
    laptop
        .handle
        .background(SessionId("laptop-9".into()), stray)
        .await
        .unwrap();
    tokio::time::sleep(Duration::from_millis(500)).await;
    assert!(
        !rig.sim
            .messages(GENERAL)
            .iter()
            .any(|message| message.text.contains("nobody asked"))
    );
    rig.shutdown.cancel();
}

async fn zipped(rig: &Rig, ts: &str) {
    eventually("the zipped mouth", || {
        rig.sim
            .reactions(GENERAL, ts)
            .contains(&"zipper_mouth_face".to_owned())
            .then_some(())
    })
    .await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_allow_list_lets_in_the_owner_and_the_listed_and_keeps_even_the_admin_out() {
    let rig = rig().await;
    rig.store.set_allowed(&user(BOB), true).await.unwrap();
    let _desktop = rig.node(ADMIN, "desktop").await;
    let mut laptop = rig.node_allowing(ALICE, "laptop", &[BOB]).await;
    tokio::time::sleep(Duration::from_millis(300)).await;

    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "first", None)
        .await
        .unwrap();
    assert_eq!(laptop.next_prompt().await.1, "first");
    rig.bot_replies(&ts, |replies| {
        replies.iter().any(|m| m.text.contains("pong to: first"))
    })
    .await;
    rig.sim
        .mention(BOB, GENERAL, "from bob", Some(&ts))
        .await
        .unwrap();
    assert_eq!(laptop.next_prompt().await.1, "from bob");
    rig.bot_replies(&ts, |replies| {
        replies.iter().any(|m| m.text.contains("pong to: from bob"))
    })
    .await;

    let from_admin = rig
        .sim
        .mention(ADMIN, GENERAL, "from the admin", Some(&ts))
        .await
        .unwrap();
    zipped(&rig, &from_admin).await;
    rig.sim
        .mention(ALICE, GENERAL, "second", Some(&ts))
        .await
        .unwrap();
    assert_eq!(laptop.next_prompt().await.1, "second");
    assert_eq!(rig.sim.violations(), vec![]);
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn someone_shut_out_of_a_thread_cannot_interrupt_its_turn() {
    let rig = rig().await;
    rig.store.set_allowed(&user(BOB), true).await.unwrap();
    let mut laptop = rig.node_allowing(ALICE, "laptop", &[]).await;
    tokio::time::sleep(Duration::from_millis(300)).await;

    let ts = rig.sim.mention(ALICE, GENERAL, "slow", None).await.unwrap();
    laptop.next_prompt().await;
    let from_bob = rig
        .sim
        .mention(BOB, GENERAL, "stop that", Some(&ts))
        .await
        .unwrap();
    zipped(&rig, &from_bob).await;
    let heard = tokio::time::timeout(Duration::from_secs(1), laptop.seen.recv()).await;
    assert!(heard.is_err(), "the turn heard {heard:?}");
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_person_falls_back_only_onto_a_machine_that_lets_them_in() {
    let rig = rig().await;
    rig.store.set_allowed(&user(BOB), true).await.unwrap();
    let _desktop = rig.node_allowing(ADMIN, "desktop", &[]).await;
    tokio::time::sleep(Duration::from_millis(300)).await;

    let ts = rig
        .sim
        .mention(BOB, GENERAL, "anyone?", None)
        .await
        .unwrap();
    zipped(&rig, &ts).await;
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        rig.sim
            .thread(GENERAL, &ts)
            .iter()
            .all(|m| m.user.as_deref() != Some(BOT_USER_ID))
    );
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn only_someone_let_in_may_answer_the_nodes_question() {
    let rig = rig().await;
    rig.store.set_allowed(&user(BOB), true).await.unwrap();
    let mut laptop = rig.node_allowing(ALICE, "laptop", &[]).await;
    tokio::time::sleep(Duration::from_millis(300)).await;

    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "ask me", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    let card = eventually("the question card", || prompt_card(&rig, &ts)).await;
    rig.sim
        .click(BOB, GENERAL, &card.ts, murtaugh_gateway::prompts::CHAT)
        .await
        .unwrap();
    eventually("the note to bob", || {
        rig.sim
            .ephemerals()
            .iter()
            .any(|(_, who, text)| who == BOB && text == "You can't answer this one.")
            .then_some(())
    })
    .await;
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_node_whose_access_is_malformed_is_told_why_and_let_go() {
    let rig = rig().await;
    let metadata = serde_json::from_value(serde_json::json!({
        "murtaugh_access": "everyone",
    }))
    .unwrap();
    let mut laptop = rig
        .node_declaring(ALICE, "laptop", &[rig.listen], metadata)
        .await;
    let Seen::Rejected(rejection) = laptop.next_notice().await else {
        panic!("expected a rejection")
    };
    assert_eq!(rejection.key.as_deref(), Some("murtaugh_access"));
    assert!(
        rejection.message.contains("must be a table"),
        "{}",
        rejection.message
    );

    let ts = rig.sim.mention(ALICE, GENERAL, "ping", None).await.unwrap();
    rig.bot_replies(&ts, |replies| {
        replies.iter().any(|m| m.text == "No machine available")
    })
    .await;
    rig.shutdown.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_node_changes_who_it_lets_in_live_and_a_bad_change_ends_its_link() {
    let rig = rig().await;
    rig.store.set_allowed(&user(BOB), true).await.unwrap();
    let mut laptop = rig.node(ALICE, "laptop").await;
    tokio::time::sleep(Duration::from_millis(300)).await;

    let ts = rig
        .sim
        .mention(ALICE, GENERAL, "first", None)
        .await
        .unwrap();
    laptop.next_prompt().await;
    rig.bot_replies(&ts, |replies| {
        replies.iter().any(|m| m.text.contains("pong to: first"))
    })
    .await;
    rig.sim
        .mention(BOB, GENERAL, "one", Some(&ts))
        .await
        .unwrap();
    assert_eq!(laptop.next_prompt().await.1, "one");
    rig.bot_replies(&ts, |replies| {
        replies.iter().any(|m| m.text.contains("pong to: one"))
    })
    .await;

    let narrowed = laptop
        .update_metadata(serde_json::json!({
            "murtaugh_access": {"policy": "allow_list", "people": []},
            "murtaugh_colour": "teal",
        }))
        .await;
    assert!(matches!(narrowed, Ok(NodeReply::UpdateMetadata)));
    assert!(matches!(laptop.next_notice().await, Seen::Ignored(key) if key == "murtaugh_colour"));
    let two = rig
        .sim
        .mention(BOB, GENERAL, "two", Some(&ts))
        .await
        .unwrap();
    zipped(&rig, &two).await;

    let broken = laptop
        .update_metadata(serde_json::json!({"murtaugh_access": {"policy": "allow_list"}}))
        .await;
    assert!(matches!(
        broken,
        Err(CallError::Fault(fault)) if fault.kind == rax::ErrorKind::Rejected
    ));
    assert!(matches!(laptop.next_notice().await, Seen::Rejected(_)));
    rig.shutdown.cancel();
}
