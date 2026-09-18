#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod support;

use std::time::Duration;

use serde_json::json;
use slack_sim::{ALICE, BOT_USER_ID, GENERAL, RANDOM, SlackSim};
use support::{
    ack, assert_shape, bot, connect, connect_raw, fixture, form_call, recv, recv_within,
};

async fn sim() -> SlackSim {
    SlackSim::start().await.unwrap()
}

#[tokio::test]
async fn connections_open_hands_out_a_single_use_ticket() {
    let sim = sim().await;
    let open = form_call(&sim, "apps.connections.open", Some(&sim.tokens().app), &[]).await;
    assert_shape(&fixture("apps_connections_open"), &open);
    let url = open["url"].as_str().unwrap();
    assert!(url.starts_with("ws://127.0.0.1:"), "{url}");

    let (_ws, _) = tokio_tungstenite::connect_async(url).await.unwrap();
    assert!(tokio_tungstenite::connect_async(url).await.is_err());
    let errors: Vec<_> = sim.violations().into_iter().map(|v| v.error).collect();
    assert_eq!(errors, ["invalid_ticket"]);
}

#[tokio::test]
async fn hello_matches_slack() {
    let sim = sim().await;
    let (_ws, hello) = connect_raw(&sim).await;
    assert_shape(&fixture("hello"), &hello);
    assert_eq!(sim.connections(), 1);
}

#[tokio::test]
async fn a_mention_delivers_app_mention_then_message() {
    let sim = sim().await;
    let mut ws = connect(&sim).await;
    let ts = sim
        .mention(ALICE, GENERAL, "is it everything a river should be?", None)
        .await
        .unwrap();

    let mention = recv(&mut ws).await;
    ack(&mut ws, &mention).await;
    let message = recv(&mut ws).await;
    ack(&mut ws, &message).await;

    assert_shape(&fixture("envelope_app_mention"), &mention);
    let event = &mention["payload"]["event"];
    assert_eq!(event["type"], "app_mention");
    assert_eq!(event["ts"], ts);
    assert_eq!(
        event["text"],
        format!("<@{BOT_USER_ID}> is it everything a river should be?")
    );
    assert_eq!(message["payload"]["event"]["type"], "message");
    assert_eq!(message["payload"]["event"]["channel_type"], "channel");
    assert_ne!(
        mention["payload"]["event_id"],
        message["payload"]["event_id"]
    );

    sim.set_channel_message_events(false);
    sim.mention(ALICE, GENERAL, "again", None).await.unwrap();
    let only = recv(&mut ws).await;
    ack(&mut ws, &only).await;
    assert_eq!(only["payload"]["event"]["type"], "app_mention");
    assert!(
        recv_within(&mut ws, Duration::from_millis(300))
            .await
            .is_none()
    );

    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(sim.acks().len(), 3);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn nothing_is_delivered_from_channels_the_bot_is_not_in() {
    let sim = sim().await;
    let mut ws = connect(&sim).await;
    sim.mention(ALICE, RANDOM, "hello?", None).await.unwrap();
    assert!(
        recv_within(&mut ws, Duration::from_millis(300))
            .await
            .is_none()
    );
    assert_eq!(sim.messages(RANDOM).len(), 1);
}

#[tokio::test]
async fn dm_thread_reply_and_file_share_match_slack() {
    let sim = sim().await;
    let mut ws = connect(&sim).await;

    let (channel, _) = sim.dm(ALICE, "Hello hello can you hear me?").await.unwrap();
    let dm = recv(&mut ws).await;
    ack(&mut ws, &dm).await;
    assert_shape(&fixture("envelope_message_im"), &dm);
    assert_eq!(dm["payload"]["event"]["channel"], channel.as_str());

    sim.set_channel_message_events(false);
    let root = sim.say(ALICE, GENERAL, "island", None).await.unwrap();
    let root_event = recv(&mut ws).await;
    ack(&mut ws, &root_event).await;
    sim.say(ALICE, GENERAL, "one island", Some(&root))
        .await
        .unwrap();
    let reply = recv(&mut ws).await;
    ack(&mut ws, &reply).await;
    assert_shape(&fixture("envelope_message_thread_reply"), &reply);
    assert_eq!(reply["payload"]["event"]["thread_ts"], root.as_str());

    let (file, ts) = sim
        .upload(
            ALICE,
            GENERAL,
            "report.pdf",
            "application/pdf",
            b"%PDF",
            None,
        )
        .await
        .unwrap();
    let share = recv(&mut ws).await;
    ack(&mut ws, &share).await;
    assert_shape(&fixture("envelope_file_share"), &share);
    assert_eq!(share["payload"]["event"]["files"][0]["id"], file.as_str());
    assert_eq!(share["payload"]["event"]["ts"], ts.as_str());

    tokio::time::sleep(Duration::from_millis(100)).await;
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn clicks_and_slash_commands_match_slack() {
    let sim = sim().await;
    let mut ws = connect(&sim).await;
    let res = bot(
        &sim,
        "chat.postMessage",
        json!({
            "channel": GENERAL,
            "text": "Approve the plan?",
            "blocks": [{"type": "actions", "block_id": "plan", "elements": [{
                "type": "button",
                "text": {"type": "plain_text", "text": "Approve", "emoji": true},
                "action_id": "approve",
                "value": "plan-1",
                "style": "primary"
            }]}]
        }),
    )
    .await;
    let ts = res["ts"].as_str().unwrap();

    sim.click(ALICE, GENERAL, ts, "approve").await.unwrap();
    let click = recv(&mut ws).await;
    ack(&mut ws, &click).await;
    assert_shape(&fixture("envelope_block_actions"), &click);
    assert_eq!(click["payload"]["actions"][0]["value"], "plan-1");
    assert!(sim.click(ALICE, GENERAL, ts, "nope").await.is_err());

    sim.slash(ALICE, GENERAL, "/murtaugh", "status")
        .await
        .unwrap();
    let slash = recv(&mut ws).await;
    ack(&mut ws, &slash).await;
    assert_shape(&fixture("envelope_slash_command"), &slash);

    tokio::time::sleep(Duration::from_millis(100)).await;
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn an_unacked_envelope_is_a_violation_and_is_redelivered() {
    let sim = sim().await;
    let mut ws = connect(&sim).await;
    sim.set_channel_message_events(false);
    sim.mention(ALICE, GENERAL, "hi", None).await.unwrap();

    let first = recv(&mut ws).await;
    assert_eq!(first["retry_attempt"], 0);
    let retry = recv_within(&mut ws, Duration::from_secs(4)).await.unwrap();
    assert_eq!(retry["envelope_id"], first["envelope_id"]);
    assert_eq!(retry["payload"]["event_id"], first["payload"]["event_id"]);
    assert_eq!(retry["retry_attempt"], 1);
    assert_eq!(retry["retry_reason"], "timeout");
    ack(&mut ws, &retry).await;

    tokio::time::sleep(Duration::from_millis(100)).await;
    let violations = sim.violations();
    assert_eq!(violations.len(), 1, "{violations:?}");
    assert_eq!(violations[0].error, "ack_timeout");
    assert!(
        recv_within(&mut ws, Duration::from_millis(3200))
            .await
            .is_none()
    );
}

#[tokio::test]
async fn acking_an_unknown_envelope_is_a_violation() {
    let sim = sim().await;
    let mut ws = connect(&sim).await;
    ack(&mut ws, &json!({"envelope_id": "never-sent"})).await;
    tokio::time::sleep(Duration::from_millis(100)).await;
    let errors: Vec<_> = sim.violations().into_iter().map(|v| v.error).collect();
    assert_eq!(errors, ["unknown_envelope"]);
}

#[tokio::test]
async fn disconnect_matches_slack_and_events_wait_for_a_new_socket() {
    let sim = sim().await;
    let mut ws = connect(&sim).await;
    sim.send_disconnect();
    let disconnect = recv(&mut ws).await;
    assert_shape(&fixture("disconnect"), &disconnect);

    sim.set_channel_message_events(false);
    sim.mention(ALICE, GENERAL, "after", None).await.unwrap();
    assert!(
        recv_within(&mut ws, Duration::from_millis(300))
            .await
            .is_none()
    );

    let mut fresh = connect(&sim).await;
    let event = recv(&mut fresh).await;
    ack(&mut fresh, &event).await;
    assert_eq!(event["payload"]["event"]["type"], "app_mention");
}

#[tokio::test]
async fn a_dropped_socket_closes_without_a_close_frame() {
    let sim = sim().await;
    let mut ws = connect(&sim).await;
    sim.drop_socket();
    assert!(recv_within(&mut ws, Duration::from_secs(2)).await.is_none());
    assert_eq!(sim.connections(), 0);

    sim.set_channel_message_events(false);
    sim.mention(ALICE, GENERAL, "queued", None).await.unwrap();
    let mut fresh = connect(&sim).await;
    let event = recv(&mut fresh).await;
    assert_eq!(event["payload"]["event"]["type"], "app_mention");
}
