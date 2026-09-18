#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use std::time::{Duration, Instant};

use murtaugh_slack::{
    Block, Button, ButtonStyle, FileInfo, PostMessage, SlackClient, SlackError, Text, Tokens,
    UpdateMessage, mrkdwn,
};
use serde_json::json;
use slack_sim::{ALICE, BOT_ID, BOT_USER_ID, GENERAL, SECRET, SlackSim, TEAM_ID};

async fn setup() -> (SlackSim, SlackClient) {
    let sim = SlackSim::start().await.unwrap();
    let client = SlackClient::new(tokens(&sim), sim.api_base());
    (sim, client)
}

fn tokens(sim: &SlackSim) -> Tokens {
    let t = sim.tokens();
    Tokens {
        app: t.app,
        bot: t.bot,
    }
}

fn post(channel: &str, text: &str) -> PostMessage {
    PostMessage {
        channel: channel.into(),
        thread_ts: None,
        text: text.into(),
        blocks: vec![],
    }
}

#[tokio::test]
async fn auth_test_returns_the_bot_identity() {
    let (sim, client) = setup().await;
    let me = client.auth_test().await.unwrap();
    assert_eq!(me.team_id, TEAM_ID);
    assert_eq!(me.user_id, BOT_USER_ID);
    assert_eq!(me.bot_id, BOT_ID);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn post_message_sends_every_block_kind_slack_accepts() {
    let (sim, client) = setup().await;
    let root = client.post_message(&post(GENERAL, "root")).await.unwrap();
    assert_eq!(root.channel, GENERAL);

    let blocks = vec![
        Block::mrkdwn("*bold* & <escaped>"),
        Block::plain("plain"),
        Block::Divider,
        Block::Context(vec![Text::Mrkdwn("ctx".into()), Text::Plain("p".into())]),
        Block::Actions(vec![
            Button::new("Approve", "approve", "plan-1").style(ButtonStyle::Primary),
            Button::new("Reject", "reject", "plan-1").style(ButtonStyle::Danger),
            Button::new("Later", "later", "plan-1"),
        ]),
        Block::Raw(json!({"type": "header", "text": {"type": "plain_text", "text": "Hi"}})),
    ];
    let reply = client
        .post_message(&PostMessage {
            channel: GENERAL.into(),
            thread_ts: Some(root.ts.clone()),
            text: "fallback".into(),
            blocks,
        })
        .await
        .unwrap();

    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
    let thread = sim.thread(GENERAL, &root.ts);
    assert_eq!(thread.len(), 2);
    assert_eq!(thread[1].ts, reply.ts);
    assert_eq!(
        thread[1].blocks.as_ref().unwrap().as_array().unwrap().len(),
        6
    );

    let first = &sim.calls()[0];
    assert_eq!(first.method, "chat.postMessage");
    assert!(first.params.get("thread_ts").is_none());
    assert!(first.params.get("blocks").is_none());
}

#[tokio::test]
async fn update_message_replaces_text_and_clears_blocks() {
    let (sim, client) = setup().await;
    let posted = client
        .post_message(&PostMessage {
            blocks: vec![Block::mrkdwn("v1")],
            ..post(GENERAL, "v1")
        })
        .await
        .unwrap();
    client
        .update_message(&UpdateMessage {
            channel: posted.channel.clone(),
            ts: posted.ts.clone(),
            text: "v2".into(),
            blocks: vec![],
        })
        .await
        .unwrap();
    let msg = &sim.messages(GENERAL)[0];
    assert_eq!(msg.text, "v2");
    assert!(msg.blocks.is_none());
    assert!(msg.edited);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn add_reaction_treats_already_reacted_as_success() {
    let (sim, client) = setup().await;
    let ts = sim.say(ALICE, GENERAL, "look", None).await.unwrap();
    client.add_reaction(GENERAL, &ts, "eyes").await.unwrap();
    client.add_reaction(GENERAL, &ts, "eyes").await.unwrap();
    assert_eq!(sim.reactions(GENERAL, &ts), ["eyes"]);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn replies_follows_the_cursor_and_returns_the_thread_once() {
    let (sim, client) = setup().await;
    let root = sim.say(ALICE, GENERAL, "root", None).await.unwrap();
    for i in 0..7 {
        sim.say(ALICE, GENERAL, &format!("r{i}"), Some(&root))
            .await
            .unwrap();
    }
    client
        .post_message(&PostMessage {
            thread_ts: Some(root.clone()),
            ..post(GENERAL, "bot reply")
        })
        .await
        .unwrap();
    sim.set_replies_page_size(3);

    let thread = client.replies(GENERAL, &root).await.unwrap();
    let texts: Vec<&str> = thread.iter().map(|m| m.text.as_str()).collect();
    assert_eq!(
        texts,
        [
            "root",
            "r0",
            "r1",
            "r2",
            "r3",
            "r4",
            "r5",
            "r6",
            "bot reply"
        ]
    );
    assert_eq!(thread[0].thread_ts.as_deref(), Some(root.as_str()));
    assert_eq!(thread[8].bot_id.as_deref(), Some(BOT_ID));
    let pages = sim
        .calls()
        .iter()
        .filter(|c| c.method == "conversations.replies")
        .count();
    assert_eq!(pages, 4);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn file_info_and_download_fetch_the_uploaded_bytes() {
    let (sim, client) = setup().await;
    let (file_id, _) = sim
        .upload(
            ALICE,
            GENERAL,
            "notes.txt",
            "text/plain",
            b"hello file",
            None,
        )
        .await
        .unwrap();
    let info = client.file_info(&file_id).await.unwrap();
    assert_eq!(info.name, "notes.txt");
    assert_eq!(info.mimetype, "text/plain");
    assert_eq!(info.size, 10);
    assert_eq!(client.download(&info).await.unwrap(), b"hello file");
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn a_refused_download_is_an_error_not_an_html_file() {
    let (sim, client) = setup().await;
    let (file_id, _) = sim
        .upload(
            ALICE,
            GENERAL,
            "a.bin",
            "application/octet-stream",
            b"\x00",
            None,
        )
        .await
        .unwrap();
    let info: FileInfo = client.file_info(&file_id).await.unwrap();
    let stranger = SlackClient::new(
        Tokens {
            app: "xapp-wrong".into(),
            bot: "xoxb-wrong".into(),
        },
        sim.api_base(),
    );
    match stranger.download(&info).await {
        Err(SlackError::Download { reason, .. }) => assert!(reason.contains("HTML"), "{reason}"),
        other => panic!("expected a download error, got {other:?}"),
    }
}

#[tokio::test]
async fn rate_limits_are_waited_out() {
    let (sim, client) = setup().await;
    sim.rate_limit("chat.postMessage", 1);
    let started = Instant::now();
    client
        .post_message(&post(GENERAL, "patience"))
        .await
        .unwrap();
    assert!(started.elapsed() >= Duration::from_secs(1));
    assert_eq!(sim.calls().len(), 2);
    assert_eq!(sim.messages(GENERAL).len(), 1);
}

#[tokio::test]
async fn rate_limits_give_up_after_the_retry_budget() {
    let sim = SlackSim::start().await.unwrap();
    let client = SlackClient::with_max_retries(tokens(&sim), sim.api_base(), 2);
    for _ in 0..3 {
        sim.rate_limit("reactions.add", 0);
    }
    match client.add_reaction(GENERAL, "1.1", "eyes").await {
        Err(SlackError::RateLimited {
            method,
            retry_after,
        }) => {
            assert_eq!(method, "reactions.add");
            assert_eq!(retry_after, Duration::ZERO);
        }
        other => panic!("expected RateLimited, got {other:?}"),
    }
    assert_eq!(sim.calls().len(), 3);
}

#[tokio::test]
async fn slack_errors_are_surfaced_verbatim() {
    let (sim, client) = setup().await;
    sim.fail("chat.postMessage", "fatal_error");
    let injected = client.post_message(&post(GENERAL, "x")).await.unwrap_err();
    let hidden = client.post_message(&post(SECRET, "x")).await.unwrap_err();
    let long = client
        .post_message(&PostMessage {
            blocks: vec![Block::mrkdwn("x".repeat(3001))],
            ..post(GENERAL, "x")
        })
        .await
        .unwrap_err();
    let missing = client.file_info("F0NOPE").await.unwrap_err();

    let codes: Vec<(String, String)> = [injected, hidden, long, missing]
        .into_iter()
        .map(|e| match e {
            SlackError::Api { method, error } => (method, error),
            other => panic!("expected an API error, got {other:?}"),
        })
        .collect();
    assert_eq!(
        codes,
        [
            ("chat.postMessage".into(), "fatal_error".into()),
            ("chat.postMessage".into(), "channel_not_found".into()),
            ("chat.postMessage".into(), "invalid_blocks".into()),
            ("files.info".into(), "file_not_found".into()),
        ]
    );
}

#[tokio::test]
async fn a_bad_token_is_invalid_auth() {
    let sim = SlackSim::start().await.unwrap();
    let client = SlackClient::new(
        Tokens {
            app: "xapp-1-nope".into(),
            bot: "xoxb-1-nope".into(),
        },
        sim.api_base(),
    );
    match client.auth_test().await {
        Err(SlackError::Api { error, .. }) => assert_eq!(error, "invalid_auth"),
        other => panic!("expected invalid_auth, got {other:?}"),
    }
}

#[test]
fn tokens_never_print_their_secrets() {
    let tokens = Tokens {
        app: "xapp-1-A0-123-secretapp".into(),
        bot: "xoxb-1-2-secretbot".into(),
    };
    let printed = format!("{tokens:?}");
    assert!(!printed.contains("secret"), "{printed}");
    assert!(
        printed.contains("xapp") && printed.contains("xoxb"),
        "{printed}"
    );
}

#[test]
fn mrkdwn_escape_covers_the_control_characters() {
    assert_eq!(
        mrkdwn::escape("a & b <@U1> > c"),
        "a &amp; b &lt;@U1&gt; &gt; c"
    );
}

#[test]
fn blocks_serialise_to_slack_json() {
    let block = Block::Actions(vec![Button::new("Go", "go", "1")]);
    assert_eq!(
        serde_json::to_value(&block).unwrap(),
        json!({"type": "actions", "elements": [{
            "type": "button",
            "text": {"type": "plain_text", "text": "Go", "emoji": true},
            "action_id": "go",
            "value": "1"
        }]})
    );
}
