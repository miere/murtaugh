#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

//! The real Riggs binary serving a Slack mention through the gateway. Runs when `RIGGS_BIN`,
//! `FAKE_CLAUDE_BIN` and `FAKE_CLAUDE_SCRIPT` are set (Riggs builds all three).

use std::os::unix::fs::PermissionsExt;
use std::process::{Child, Command, Stdio};
use std::time::Duration;

use murtaugh_gateway::run::{self, Options};
use murtaugh_gateway::token;
use murtaugh_store::{NodeToken, SqliteStore, Store, UserId};
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

#[tokio::test(flavor = "multi_thread")]
async fn riggs_answers_a_mention_through_the_gateway() {
    let (Ok(riggs), Ok(fake_claude), Ok(script)) = (
        std::env::var("RIGGS_BIN"),
        std::env::var("FAKE_CLAUDE_BIN"),
        std::env::var("FAKE_CLAUDE_SCRIPT"),
    ) else {
        eprintln!("skipped: set RIGGS_BIN, FAKE_CLAUDE_BIN and FAKE_CLAUDE_SCRIPT");
        return;
    };
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
    let minted = token::mint();
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
    let _riggs = Killed(
        Command::new(&riggs)
            .args(["run", "--config"])
            .arg(&riggs_config)
            .env("HOME", dir.path())
            .stdout(Stdio::null())
            .stderr(Stdio::inherit())
            .spawn()
            .unwrap(),
    );
    let deadline = tokio::time::Instant::now() + Duration::from_secs(60);
    'asking: loop {
        let ts = sim.mention(ALICE, GENERAL, "ping", None).await.unwrap();
        loop {
            let replies: Vec<String> = sim
                .thread(GENERAL, &ts)
                .into_iter()
                .filter(|m| m.user.as_deref() == Some(BOT_USER_ID))
                .map(|m| m.text)
                .collect();
            if replies.iter().any(|text| text.contains("pong")) {
                break 'asking;
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
    assert_eq!(sim.violations(), vec![]);
    shutdown.cancel();
}
