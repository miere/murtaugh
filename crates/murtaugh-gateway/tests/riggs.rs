#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

//! The real Riggs binary serving Slack mentions through the gateway. Runs when `RIGGS_BIN` and
//! `FAKE_CLAUDE_BIN` are set, and the first test also wants `FAKE_CLAUDE_SCRIPT` (Riggs builds all
//! three).

use std::os::unix::fs::PermissionsExt;
use std::process::{Child, Command, Stdio};
use std::time::Duration;

use murtaugh_gateway::run::{self, Options};
use murtaugh_gateway::token;
use murtaugh_store::{NodeToken, SqliteStore, Store, ToolMode, UserId};
use slack_sim::{BOT_USER_ID, GENERAL, SlackSim};
use time::OffsetDateTime;
use tokio_util::sync::CancellationToken;

const ALICE: &str = "U0ALICE01";
const ADMIN: &str = "U0ADMIN01";

struct Killed(Child);

impl Drop for Killed {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

fn user(raw: &str) -> UserId {
    UserId::parse(raw).unwrap()
}

/// A gateway, a Slack workspace and a real Riggs node running `script` through fake-claude.
struct Stack {
    sim: SlackSim,
    shutdown: CancellationToken,
    _riggs: Killed,
    _dir: tempfile::TempDir,
}

impl Drop for Stack {
    fn drop(&mut self) {
        self.shutdown.cancel();
    }
}

/// `None` unless `RIGGS_BIN` and `FAKE_CLAUDE_BIN` are set.
fn binaries() -> Option<(String, String)> {
    match (std::env::var("RIGGS_BIN"), std::env::var("FAKE_CLAUDE_BIN")) {
        (Ok(riggs), Ok(fake_claude)) => Some((riggs, fake_claude)),
        _ => None,
    }
}

async fn stack(riggs: &str, fake_claude: &str, script: &str, tool_mode: Option<ToolMode>) -> Stack {
    let _ = tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::new("info"))
        .try_init();
    let sim = SlackSim::start().await.unwrap();
    sim.add_user(ALICE, "alice");
    sim.add_user(ADMIN, "admin");
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
    let store = SqliteStore::open(&dir.path().join("murtaugh.db")).unwrap();
    store.set_admin(&user(ADMIN)).await.unwrap();
    store.approve(&user(ALICE), &user(ADMIN)).await.unwrap();
    if let Some(mode) = tool_mode {
        store.set_tool_mode(&user(ALICE), mode).await.unwrap();
    }
    let minted = token::mint(token::NODE_PREFIX);
    store
        .add_node_token(&NodeToken {
            selector: minted.selector,
            secret_hash: minted.secret_hash,
            owner: user(ALICE),
            name: "riggs".into(),
            created_at: OffsetDateTime::now_utc(),
            revoked_at: None,
            disabled_at: None,
        })
        .await
        .unwrap();

    let shutdown = CancellationToken::new();
    let options = Options {
        slack_api: sim.api_base(),
        refresh: Duration::from_millis(100),
        ..Options::default()
    };
    tokio::spawn({
        let shutdown = shutdown.clone();
        async move { run::serve(&config, options, shutdown).await }
    });

    let node_dir = dir.path().join("riggs");
    let work = node_dir.join("work");
    std::fs::create_dir_all(&work).unwrap();
    let token_file = node_dir.join("node-token");
    std::fs::write(&token_file, format!("{}\n", minted.token)).unwrap();
    std::fs::set_permissions(&token_file, std::fs::Permissions::from_mode(0o600)).unwrap();
    let riggs_config = node_dir.join("riggs.toml");
    std::fs::write(
        &riggs_config,
        format!(
            "[gateway]\nurls = [\"ws://{listen}\"]\n\n[agent]\nkind = \"claude_code\"\ncommand = {fake_claude:?}\nworkdir = {work:?}\nhandshake_timeout = \"20s\"\nenv = {{ FAKE_CLAUDE_SCRIPT = {script:?}, FAKE_CLAUDE_STATE = {state:?} }}\n\n[sessions]\ndurable = false\n",
            state = node_dir.display().to_string(),
            work = work.display().to_string(),
        ),
    )
    .unwrap();
    let riggs = Killed(
        Command::new(riggs)
            .args(["run", "--config"])
            .arg(&riggs_config)
            .env("HOME", dir.path())
            .stdout(Stdio::null())
            .stderr(Stdio::inherit())
            .spawn()
            .unwrap(),
    );
    Stack {
        sim,
        shutdown,
        _riggs: riggs,
        _dir: dir,
    }
}

impl Stack {
    /// Mentions the bot with `text` until a reply contains `wanted`, asking again while no machine
    /// has attached yet. Returns every bot reply in that thread.
    async fn ask(&self, text: &str, wanted: &str) -> Vec<String> {
        let deadline = tokio::time::Instant::now() + Duration::from_secs(60);
        'asking: loop {
            let ts = self.sim.mention(ALICE, GENERAL, text, None).await.unwrap();
            loop {
                let replies: Vec<String> = self
                    .sim
                    .thread(GENERAL, &ts)
                    .into_iter()
                    .filter(|m| m.user.as_deref() == Some(BOT_USER_ID))
                    .map(|m| m.text)
                    .collect();
                if replies.iter().any(|text| text.contains(wanted)) {
                    return replies;
                }
                assert!(
                    tokio::time::Instant::now() < deadline,
                    "no answer from Riggs; replies: {replies:?}"
                );
                if replies
                    .iter()
                    .any(|text| text == "No machine available" || text == "Machine offline")
                {
                    tokio::time::sleep(Duration::from_millis(500)).await;
                    continue 'asking;
                }
                tokio::time::sleep(Duration::from_millis(100)).await;
            }
        }
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn riggs_answers_a_mention_through_the_gateway() {
    let (Some((riggs, fake_claude)), Ok(script)) =
        (binaries(), std::env::var("FAKE_CLAUDE_SCRIPT"))
    else {
        eprintln!("skipped: set RIGGS_BIN, FAKE_CLAUDE_BIN and FAKE_CLAUDE_SCRIPT");
        return;
    };
    let stack = stack(&riggs, &fake_claude, &script, None).await;
    stack.ask("ping", "pong").await;
    assert_eq!(stack.sim.violations(), vec![]);
}

/// The agent on a real Riggs node reads a Slack message through the `slack` group its session was
/// opened with: `mcp__slack__read_message` travels as `tool.call` in namespace `slack`, and the
/// gateway serves it from the workspace.
#[tokio::test(flavor = "multi_thread")]
async fn riggs_reads_a_slack_message_through_the_sessions_slack_tools() {
    let Some((riggs, fake_claude)) = binaries() else {
        eprintln!("skipped: set RIGGS_BIN and FAKE_CLAUDE_BIN");
        return;
    };
    let scripts = tempfile::tempdir().unwrap();
    let script = scripts.path().join("read-message.json");
    // The stack starts the workspace, so the message the agent reads is posted once it is up and
    // the script is written before the first mention reaches the agent.
    let stack = stack(
        &riggs,
        &fake_claude,
        &script.display().to_string(),
        Some(ToolMode::AlwaysAllowed),
    )
    .await;
    let ts = stack
        .sim
        .say(ALICE, GENERAL, "the build is green", None)
        .await
        .unwrap();
    // The whole thread, because the simulator serves `conversations.replies` but not history.
    let link = format!(
        "https://example.slack.com/archives/{GENERAL}/p{}",
        ts.replace('.', "")
    );
    let call = serde_json::json!({"turns": [[
        {"hook": {
            "id": "toolu_01SLACKREADaaaaaaaaaaaa",
            "name": "mcp__slack__read_message",
            "input": {"link": link},
            "allow": [
                {"call_tool": {"id": "toolu_01SLACKREADaaaaaaaaaaaa", "server": "slack",
                               "name": "read_message", "arguments": {"link": link, "thread": true},
                               "as": "read"}},
                {"say": "it said: {{read}}"},
            ],
        }},
        {"result": {"text": "done", "num_turns": 2}},
    ]]});
    std::fs::write(&script, call.to_string()).unwrap();

    let replies = stack.ask("what did that say?", "it said:").await;
    let answer = replies
        .iter()
        .find(|text| text.contains("it said:"))
        .unwrap();
    assert!(answer.contains("the build is green"), "{answer}");
    assert_eq!(stack.sim.violations(), vec![]);
}
