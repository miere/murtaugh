#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use std::time::Duration;

use futures_util::StreamExt;
use murtaugh_slack::{
    Block, Button, Event, EventEnvelope, PostMessage, SlackClient, SlackError, SocketEvent,
    SocketMode, Tokens,
};
use serde_json::json;
use slack_sim::{ALICE, BOT_USER_ID, GENERAL, SlackSim, TEAM_ID};

async fn setup() -> (SlackSim, SlackClient, SocketMode) {
    let sim = SlackSim::start().await.unwrap();
    let t = sim.tokens();
    let client = SlackClient::new(
        Tokens {
            app: t.app,
            bot: t.bot,
        },
        sim.api_base(),
    );
    let socket = SocketMode::connect(client.clone()).await.unwrap();
    (sim, client, socket)
}

async fn next(socket: &mut SocketMode) -> SocketEvent {
    tokio::time::timeout(Duration::from_secs(10), socket.next())
        .await
        .expect("no event within 10s")
        .expect("the stream ended")
}

async fn next_event(socket: &mut SocketMode) -> EventEnvelope {
    match next(socket).await {
        SocketEvent::Event(envelope) => envelope,
        other => panic!("expected an Events API envelope, got {other:?}"),
    }
}

fn opens(sim: &SlackSim) -> usize {
    sim.calls()
        .iter()
        .filter(|c| c.method == "apps.connections.open")
        .count()
}

#[tokio::test]
async fn a_mention_arrives_acked_before_it_is_handed_out() {
    let (sim, _, mut socket) = setup().await;
    sim.set_channel_message_events(false);
    let ts = sim.mention(ALICE, GENERAL, "hello", None).await.unwrap();

    tokio::time::sleep(Duration::from_millis(3500)).await;
    assert_eq!(sim.acks().len(), 1);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());

    let envelope = next_event(&mut socket).await;
    assert_eq!(envelope.team_id, TEAM_ID);
    assert!(envelope.event_id.starts_with("Ev"));
    assert_eq!(envelope.retry_attempt, 0);
    assert_eq!(envelope.retry_reason, None);
    assert_eq!(
        envelope.event,
        Event::AppMention {
            user: ALICE.into(),
            channel: GENERAL.into(),
            ts: ts.clone(),
            thread_ts: None,
            text: format!("<@{BOT_USER_ID}> hello"),
            files: vec![],
        }
    );
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn a_channel_mention_also_arrives_as_a_message() {
    let (sim, _, mut socket) = setup().await;
    let ts = sim.mention(ALICE, GENERAL, "hello", None).await.unwrap();
    assert!(matches!(
        next_event(&mut socket).await.event,
        Event::AppMention { .. }
    ));
    match next_event(&mut socket).await.event {
        Event::Message {
            ts: message_ts,
            channel_type,
            ..
        } => {
            assert_eq!(message_ts, ts);
            assert_eq!(channel_type, "channel");
        }
        other => panic!("expected the channel message twin, got {other:?}"),
    }
}

#[tokio::test]
async fn dms_threads_and_files_decode() {
    let (sim, client, mut socket) = setup().await;
    let (dm_channel, dm_ts) = sim.dm(ALICE, "psst").await.unwrap();
    match next_event(&mut socket).await.event {
        Event::Message {
            channel,
            channel_type,
            user,
            ts,
            text,
            ..
        } => {
            assert_eq!(channel, dm_channel);
            assert_eq!(channel_type, "im");
            assert_eq!(user.as_deref(), Some(ALICE));
            assert_eq!(ts, dm_ts);
            assert_eq!(text, "psst");
        }
        other => panic!("{other:?}"),
    }

    sim.set_channel_message_events(false);
    let root = client
        .post_message(&PostMessage {
            channel: GENERAL.into(),
            thread_ts: None,
            text: "working on it".into(),
            blocks: vec![],
        })
        .await
        .unwrap();
    sim.mention(ALICE, GENERAL, "more", Some(&root.ts))
        .await
        .unwrap();
    match next_event(&mut socket).await.event {
        Event::AppMention { thread_ts, .. } => assert_eq!(thread_ts, Some(root.ts.clone())),
        other => panic!("{other:?}"),
    }

    let (file_id, _) = sim
        .upload(
            ALICE,
            GENERAL,
            "log.txt",
            "text/plain",
            b"line",
            Some(&root.ts),
        )
        .await
        .unwrap();
    match next_event(&mut socket).await.event {
        Event::Message {
            subtype,
            files,
            thread_ts,
            ..
        } => {
            assert_eq!(subtype.as_deref(), Some("file_share"));
            assert_eq!(thread_ts, Some(root.ts.clone()));
            assert_eq!(files.len(), 1);
            assert_eq!(files[0].id, file_id);
            assert_eq!(files[0].name.as_deref(), Some("log.txt"));
            let info = client.file_info(&files[0].id).await.unwrap();
            assert_eq!(client.download(&info).await.unwrap(), b"line");
        }
        other => panic!("{other:?}"),
    }
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn unknown_and_malformed_events_decode_to_unknown() {
    let (sim, _, mut socket) = setup().await;
    sim.emit(json!({
        "type": "reaction_added",
        "user": ALICE,
        "reaction": "eyes",
        "item": {"type": "message", "channel": GENERAL, "ts": "1.000001"},
        "event_ts": "1.000002"
    }))
    .await;
    sim.emit(json!({"type": "app_mention", "text": "no user or channel"}))
        .await;

    match next_event(&mut socket).await.event {
        Event::Unknown { kind, raw } => {
            assert_eq!(kind, "reaction_added");
            assert_eq!(raw["reaction"], "eyes");
        }
        other => panic!("{other:?}"),
    }
    match next_event(&mut socket).await.event {
        Event::Unknown { kind, .. } => assert_eq!(kind, "app_mention"),
        other => panic!("{other:?}"),
    }
}

#[tokio::test]
async fn clicks_and_slash_commands_come_through_as_payloads() {
    let (sim, client, mut socket) = setup().await;
    let posted = client
        .post_message(&PostMessage {
            channel: GENERAL.into(),
            thread_ts: None,
            text: "Approve?".into(),
            blocks: vec![Block::Actions(vec![Button::new("Yes", "approve", "p1")])],
        })
        .await
        .unwrap();
    sim.click(ALICE, GENERAL, &posted.ts, "approve")
        .await
        .unwrap();
    sim.slash(ALICE, GENERAL, "/murtaugh", "status")
        .await
        .unwrap();

    match next(&mut socket).await {
        SocketEvent::Interactive(payload) => {
            assert_eq!(payload["type"], "block_actions");
            assert_eq!(
                murtaugh_slack::Click::from_interactive(&payload),
                Some(murtaugh_slack::Click {
                    user: ALICE.into(),
                    channel: GENERAL.into(),
                    message_ts: posted.ts.clone(),
                    thread_ts: None,
                    action_id: "approve".into(),
                    value: "p1".into(),
                    values: json!({}),
                })
            );
        }
        other => panic!("{other:?}"),
    }
    match next(&mut socket).await {
        SocketEvent::SlashCommand(payload) => assert_eq!(payload["command"], "/murtaugh"),
        other => panic!("{other:?}"),
    }
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(sim.acks().len(), 2);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn a_disconnect_moves_to_a_new_socket_without_losing_events() {
    let (sim, _, mut socket) = setup().await;
    sim.set_channel_message_events(false);
    sim.send_disconnect();
    sim.mention(ALICE, GENERAL, "during the switch", None)
        .await
        .unwrap();

    let envelope = next_event(&mut socket).await;
    assert!(matches!(envelope.event, Event::AppMention { .. }));
    assert_eq!(opens(&sim), 2);

    sim.mention(ALICE, GENERAL, "after", None).await.unwrap();
    assert!(matches!(
        next_event(&mut socket).await.event,
        Event::AppMention { .. }
    ));
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(sim.connections(), 1);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn a_dropped_socket_is_redialled() {
    let (sim, _, mut socket) = setup().await;
    sim.set_channel_message_events(false);
    sim.drop_socket();
    sim.mention(ALICE, GENERAL, "are you there?", None)
        .await
        .unwrap();

    let envelope = next_event(&mut socket).await;
    assert!(matches!(envelope.event, Event::AppMention { .. }));
    assert_eq!(opens(&sim), 2);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn an_envelope_another_socket_never_acked_arrives_with_its_retry_attempt() {
    let (sim, _, mut socket) = setup().await;
    sim.set_channel_message_events(false);
    let open = reqwest::Client::new()
        .post(sim.api_base().join("apps.connections.open").unwrap())
        .bearer_auth(sim.tokens().app)
        .header("content-type", "application/x-www-form-urlencoded")
        .send()
        .await
        .unwrap()
        .bytes()
        .await
        .unwrap();
    let open: serde_json::Value = serde_json::from_slice(&open).unwrap();
    let (mut rogue, _) = tokio_tungstenite::connect_async(open["url"].as_str().unwrap())
        .await
        .unwrap();
    let _hello = rogue.next().await;

    sim.mention(ALICE, GENERAL, "lost ack", None).await.unwrap();
    let unacked = rogue.next().await.unwrap().unwrap();
    assert!(unacked.to_text().unwrap().contains("lost ack"));
    drop(rogue);

    let retry = next_event(&mut socket).await;
    assert_eq!(retry.retry_attempt, 1);
    assert_eq!(retry.retry_reason.as_deref(), Some("timeout"));
    assert!(matches!(retry.event, Event::AppMention { .. }));

    tokio::time::sleep(Duration::from_millis(100)).await;
    let errors: Vec<String> = sim.violations().into_iter().map(|v| v.error).collect();
    assert_eq!(errors, ["ack_timeout"]);
}

#[tokio::test]
async fn a_refused_redial_ends_the_stream_with_the_reason() {
    let (sim, _, mut socket) = setup().await;
    sim.fail("apps.connections.open", "token_revoked");
    sim.drop_socket();
    let ended = tokio::time::timeout(Duration::from_secs(10), socket.next())
        .await
        .unwrap();
    assert!(ended.is_none());
    match socket.fatal_error() {
        Some(SlackError::Api { error, .. }) => assert_eq!(error, "token_revoked"),
        other => panic!("expected token_revoked, got {other:?}"),
    }
}

#[tokio::test]
async fn close_hangs_up_cleanly() {
    let (sim, _, socket) = setup().await;
    assert_eq!(sim.connections(), 1);
    socket.close().await;
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(sim.connections(), 0);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn a_bad_app_token_fails_at_connect() {
    let sim = SlackSim::start().await.unwrap();
    let client = SlackClient::new(
        Tokens {
            app: "xapp-1-nope".into(),
            bot: sim.tokens().bot,
        },
        sim.api_base(),
    );
    match SocketMode::connect(client).await {
        Err(SlackError::Api { method, error, .. }) => {
            assert_eq!(method, "apps.connections.open");
            assert_eq!(error, "invalid_auth");
        }
        Ok(_) => panic!("connected with a bad token"),
        Err(other) => panic!("expected invalid_auth, got {other:?}"),
    }
}
