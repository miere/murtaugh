#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod support;

use serde_json::{Value, json};
use slack_sim::{ALICE, BOT_USER_ID, GENERAL, RANDOM, SECRET, SimError, SlackSim};
use support::{assert_shape, bot, fixture, form_call, json_call, post};

async fn sim() -> SlackSim {
    SlackSim::start().await.unwrap()
}

fn errors(sim: &SlackSim) -> Vec<String> {
    sim.violations().into_iter().map(|v| v.error).collect()
}

#[tokio::test]
async fn tokens_are_checked_per_method() {
    let sim = sim().await;
    let t = sim.tokens();
    let body = json!({"channel": GENERAL, "text": "hi"});
    let missing = json_call(&sim, "chat.postMessage", None, body.clone()).await;
    let wrong = json_call(&sim, "chat.postMessage", Some("xoxb-nope"), body.clone()).await;
    let app_for_bot = json_call(&sim, "chat.postMessage", Some(&t.app), body).await;
    let bot_for_app = form_call(&sim, "apps.connections.open", Some(&t.bot), &[]).await;
    let auth = form_call(&sim, "auth.test", Some(&t.bot), &[]).await;

    assert_eq!(missing["error"], "not_authed");
    assert_eq!(wrong["error"], "invalid_auth");
    assert_eq!(app_for_bot["error"], "not_allowed_token_type");
    assert_eq!(bot_for_app["error"], "not_allowed_token_type");
    assert_eq!(auth["user_id"], BOT_USER_ID);
    assert_shape(&fixture("auth_test"), &auth);
    assert_eq!(
        errors(&sim),
        [
            "not_authed",
            "invalid_auth",
            "not_allowed_token_type",
            "not_allowed_token_type"
        ]
    );
}

#[tokio::test]
async fn token_in_a_form_body_is_accepted() {
    let sim = sim().await;
    let bot = sim.tokens().bot;
    let res = form_call(&sim, "auth.test", None, &[("token", &bot)]).await;
    assert_eq!(res["ok"], true, "{res}");
    assert!(sim.violations().is_empty());
}

#[tokio::test]
async fn post_message_updates_the_model_and_matches_slack_shape() {
    let sim = sim().await;
    let res = bot(
        &sim,
        "chat.postMessage",
        json!({"channel": GENERAL, "text": "Here's a message for you"}),
    )
    .await;
    assert_shape(&fixture("chat_post_message"), &res);
    let ts = res["ts"].as_str().unwrap();

    let reply = bot(
        &sim,
        "chat.postMessage",
        json!({"channel": GENERAL, "text": "in thread", "thread_ts": ts}),
    )
    .await;
    assert_eq!(reply["message"]["thread_ts"], ts);
    assert_eq!(reply["message"]["parent_user_id"], BOT_USER_ID);

    let messages = sim.messages(GENERAL);
    assert_eq!(messages.len(), 1);
    assert_eq!(messages[0].text, "Here's a message for you");
    let thread = sim.thread(GENERAL, ts);
    assert_eq!(thread.len(), 2);
    assert_eq!(thread[1].text, "in thread");
    assert_eq!(sim.calls().len(), 2);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn posting_to_a_user_id_opens_the_dm() {
    let sim = sim().await;
    let res = bot(
        &sim,
        "chat.postMessage",
        json!({"channel": ALICE, "text": "psst"}),
    )
    .await;
    let channel = res["channel"].as_str().unwrap();
    assert!(channel.starts_with('D'), "{res}");
    assert_eq!(sim.messages(channel).len(), 1);
}

#[tokio::test]
async fn channel_membership_is_enforced() {
    let sim = sim().await;
    for (channel, error) in [
        (json!(null), "channel_not_found"),
        (json!("C0NOPE"), "channel_not_found"),
        (json!(SECRET), "channel_not_found"),
        (json!(RANDOM), "not_in_channel"),
    ] {
        let res = bot(
            &sim,
            "chat.postMessage",
            json!({"channel": channel, "text": "x"}),
        )
        .await;
        assert_eq!(res["error"], error, "{channel}");
    }
    assert_eq!(sim.violations().len(), 4);
}

#[tokio::test]
async fn a_message_needs_text_or_blocks() {
    let sim = sim().await;
    let empty = bot(
        &sim,
        "chat.postMessage",
        json!({"channel": GENERAL, "text": ""}),
    )
    .await;
    let no_blocks = bot(
        &sim,
        "chat.postMessage",
        json!({"channel": GENERAL, "blocks": []}),
    )
    .await;
    assert_eq!(empty["error"], "no_text");
    assert_eq!(no_blocks["error"], "no_text");
    assert_eq!(errors(&sim), ["no_text", "no_text"]);
}

#[tokio::test]
async fn text_over_40000_characters_is_too_long() {
    let sim = sim().await;
    let ok = bot(
        &sim,
        "chat.postMessage",
        json!({"channel": GENERAL, "text": "é".repeat(40_000)}),
    )
    .await;
    let long = bot(
        &sim,
        "chat.postMessage",
        json!({"channel": GENERAL, "text": "a".repeat(40_001)}),
    )
    .await;
    assert_eq!(ok["ok"], true);
    assert_eq!(long["error"], "msg_too_long");
    assert_eq!(errors(&sim), ["msg_too_long"]);
}

fn section(text: &str) -> Value {
    json!({"type": "section", "text": {"type": "mrkdwn", "text": text}})
}

#[tokio::test]
async fn blocks_are_validated_like_slack() {
    let sim = sim().await;
    let fine = json!([
        section(&"x".repeat(3000)),
        {"type": "divider"},
        {"type": "context", "elements": [{"type": "mrkdwn", "text": "ctx"}]},
        {"type": "actions", "elements": [
            {"type": "button", "text": {"type": "plain_text", "text": "Go"}, "action_id": "go", "value": "1", "style": "danger"}
        ]},
        {"type": "header", "text": {"type": "plain_text", "text": "H1"}, "level": 1},
        {"type": "header", "text": {"type": "plain_text", "text": "H4"}, "level": 4},
        {"type": "header", "text": {"type": "plain_text", "text": "no level"}},
    ]);
    let res = bot(
        &sim,
        "chat.postMessage",
        json!({"channel": GENERAL, "blocks": fine}),
    )
    .await;
    assert_eq!(res["ok"], true, "{res}");

    let fifty_one: Vec<Value> = (0..51).map(|_| json!({"type": "divider"})).collect();
    let button = |extra: Value| {
        let mut b = json!({"type": "button", "text": {"type": "plain_text", "text": "Go"}, "action_id": "go"});
        b.as_object_mut()
            .unwrap()
            .extend(extra.as_object().unwrap().clone());
        json!([{"type": "actions", "elements": [b]}])
    };
    let cases = [
        json!(fifty_one),
        json!([section(&"x".repeat(3001))]),
        json!([{"type": "carousel"}]),
        json!([section("")]),
        json!([{"type": "section", "text": {"type": "markdown", "text": "x"}}]),
        json!([{"type": "section"}]),
        json!([{"type": "divider", "text": "stray"}]),
        button(json!({"style": null})),
        button(json!({"style": "secondary"})),
        json!([{"type": "actions", "elements": [
            {"type": "button", "text": {"type": "mrkdwn", "text": "Go"}, "action_id": "go"}
        ]}]),
        json!([{"type": "actions", "elements": [
            {"type": "button", "text": {"type": "plain_text", "text": "A"}, "action_id": "same"},
            {"type": "button", "text": {"type": "plain_text", "text": "B"}, "action_id": "same"}
        ]}]),
        json!([{"type": "header", "text": {"type": "plain_text", "text": "H"}, "level": 0}]),
        json!([{"type": "header", "text": {"type": "plain_text", "text": "H"}, "level": 5}]),
        json!([{"type": "header", "text": {"type": "plain_text", "text": "H"}, "level": "1"}]),
    ];
    for blocks in &cases {
        let res = bot(
            &sim,
            "chat.postMessage",
            json!({"channel": GENERAL, "blocks": blocks, "text": "fallback"}),
        )
        .await;
        assert_eq!(res["error"], "invalid_blocks", "{blocks}");
        assert!(res["response_metadata"]["messages"][0].is_string(), "{res}");
    }
    let format = bot(
        &sim,
        "chat.postMessage",
        json!({"channel": GENERAL, "blocks": "not json"}),
    )
    .await;
    assert_eq!(format["error"], "invalid_blocks_format");
    assert_eq!(sim.violations().len(), cases.len() + 1);
    assert_eq!(sim.messages(GENERAL).len(), 1);
}

#[tokio::test]
async fn blocks_may_arrive_as_a_form_encoded_json_string() {
    let sim = sim().await;
    let bot_token = sim.tokens().bot;
    let blocks = json!([section("hi")]).to_string();
    let res = form_call(
        &sim,
        "chat.postMessage",
        Some(&bot_token),
        &[("channel", GENERAL), ("blocks", &blocks)],
    )
    .await;
    assert_eq!(res["ok"], true, "{res}");
    assert!(sim.violations().is_empty());
}

#[tokio::test]
async fn update_replaces_text_and_keeps_blocks_unless_told() {
    let sim = sim().await;
    let res = bot(
        &sim,
        "chat.postMessage",
        json!({"channel": GENERAL, "text": "v1", "blocks": [section("v1")]}),
    )
    .await;
    let ts = res["ts"].as_str().unwrap();

    let res = bot(
        &sim,
        "chat.update",
        json!({"channel": GENERAL, "ts": ts, "text": "v2"}),
    )
    .await;
    assert_eq!(res["ok"], true, "{res}");
    let msg = &sim.messages(GENERAL)[0];
    assert_eq!(msg.text, "v2");
    assert!(msg.blocks.is_some());
    assert!(msg.edited);

    bot(
        &sim,
        "chat.update",
        json!({"channel": GENERAL, "ts": ts, "text": "v3", "blocks": []}),
    )
    .await;
    assert!(sim.messages(GENERAL)[0].blocks.is_none());
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn update_rejects_missing_and_foreign_messages() {
    let sim = sim().await;
    let theirs = sim.say(ALICE, GENERAL, "mine", None).await.unwrap();
    let missing = bot(
        &sim,
        "chat.update",
        json!({"channel": GENERAL, "ts": "1.000001", "text": "x"}),
    )
    .await;
    let foreign = bot(
        &sim,
        "chat.update",
        json!({"channel": GENERAL, "ts": theirs, "text": "x"}),
    )
    .await;
    assert_eq!(missing["error"], "message_not_found");
    assert_eq!(foreign["error"], "cant_update_message");
}

#[tokio::test]
async fn reactions_validate_names_and_tolerate_repeats() {
    let sim = sim().await;
    let ts = post(&sim, GENERAL, "react to me").await;
    let react = |name: &'static str, ts: String| {
        let sim = &sim;
        async move {
            bot(
                sim,
                "reactions.add",
                json!({"channel": GENERAL, "timestamp": ts, "name": name}),
            )
            .await
        }
    };
    assert_eq!(react("eyes", ts.clone()).await["ok"], true);
    assert_eq!(react("eyes", ts.clone()).await["error"], "already_reacted");
    assert!(sim.violations().is_empty());
    assert_eq!(react(":eyes:", ts.clone()).await["error"], "invalid_name");
    assert_eq!(
        react("eyes", "1.000001".into()).await["error"],
        "message_not_found"
    );
    let with_ts_key = bot(
        &sim,
        "reactions.add",
        json!({"channel": GENERAL, "ts": ts, "name": "tada"}),
    )
    .await;
    assert_eq!(with_ts_key["error"], "no_item_specified");
    assert_eq!(sim.reactions(GENERAL, &ts), ["eyes"]);
    assert_eq!(
        errors(&sim),
        ["invalid_name", "message_not_found", "no_item_specified"]
    );
}

#[tokio::test]
async fn threads_must_exist() {
    let sim = sim().await;
    let bot_token = sim.tokens().bot;
    let post = bot(
        &sim,
        "chat.postMessage",
        json!({"channel": GENERAL, "text": "x", "thread_ts": "1.000001"}),
    )
    .await;
    let replies = form_call(
        &sim,
        "conversations.replies",
        Some(&bot_token),
        &[("channel", GENERAL), ("ts", "1.000001")],
    )
    .await;
    assert_eq!(post["error"], "thread_not_found");
    assert_eq!(replies["error"], "thread_not_found");
}

#[tokio::test]
async fn replies_paginate_and_repeat_the_parent_on_every_page() {
    let sim = sim().await;
    let bot_token = sim.tokens().bot;
    let root = sim.say(ALICE, GENERAL, "island", None).await.unwrap();
    for i in 0..5 {
        sim.say(ALICE, GENERAL, &format!("reply {i}"), Some(&root))
            .await
            .unwrap();
    }
    sim.set_replies_page_size(3);

    let mut cursor = String::new();
    let mut pages = Vec::new();
    loop {
        let res = form_call(
            &sim,
            "conversations.replies",
            Some(&bot_token),
            &[
                ("channel", GENERAL),
                ("ts", &root),
                ("cursor", &cursor),
                ("limit", "200"),
            ],
        )
        .await;
        assert_eq!(res["ok"], true, "{res}");
        if pages.is_empty() {
            assert_shape(
                &fixture("conversations_replies"),
                &json!({
                    "ok": res["ok"],
                    "messages": [res["messages"][0], res["messages"][1]],
                    "has_more": res["has_more"],
                    "response_metadata": res["response_metadata"],
                }),
            );
        }
        let texts: Vec<String> = res["messages"]
            .as_array()
            .unwrap()
            .iter()
            .map(|m| m["text"].as_str().unwrap().to_owned())
            .collect();
        pages.push(texts);
        match res["response_metadata"]["next_cursor"].as_str() {
            Some(next) if !next.is_empty() => cursor = next.to_owned(),
            _ => break,
        }
    }
    assert_eq!(
        pages,
        [
            vec!["island", "reply 0", "reply 1"],
            vec!["island", "reply 2", "reply 3"],
            vec!["island", "reply 4"],
        ]
    );

    let bad = form_call(
        &sim,
        "conversations.replies",
        Some(&bot_token),
        &[("channel", GENERAL), ("ts", &root), ("cursor", "bogus")],
    )
    .await;
    assert_eq!(bad["error"], "invalid_cursor");
}

#[tokio::test]
async fn files_are_described_and_downloadable_with_the_bot_token_only() {
    let sim = sim().await;
    let bot_token = sim.tokens().bot;
    let (file, _) = sim
        .upload(
            ALICE,
            GENERAL,
            "report.pdf",
            "application/pdf",
            b"%PDF-1.7",
            None,
        )
        .await
        .unwrap();
    let info = form_call(&sim, "files.info", Some(&bot_token), &[("file", &file)]).await;
    assert_shape(&fixture("files_info"), &info);
    let url = info["file"]["url_private_download"].as_str().unwrap();

    let client = reqwest::Client::new();
    let authed = client
        .get(url)
        .bearer_auth(&bot_token)
        .send()
        .await
        .unwrap();
    assert_eq!(authed.headers()["content-type"], "application/pdf");
    assert_eq!(authed.bytes().await.unwrap().as_ref(), b"%PDF-1.7");
    assert!(sim.violations().is_empty());

    let anonymous = client.get(url).send().await.unwrap();
    assert!(
        anonymous.headers()["content-type"]
            .to_str()
            .unwrap()
            .starts_with("text/html")
    );
    assert_eq!(errors(&sim), ["not_authed"]);

    let missing = form_call(&sim, "files.info", Some(&bot_token), &[("file", "F0NOPE")]).await;
    assert_eq!(missing["error"], "file_not_found");
}

#[tokio::test]
async fn malformed_requests_are_violations() {
    let sim = sim().await;
    let bot_token = sim.tokens().bot;
    let res = json_call(
        &sim,
        "conversations.replies",
        Some(&bot_token),
        json!({"channel": GENERAL, "ts": "1.1"}),
    )
    .await;
    assert_eq!(res["error"], "channel_not_found");

    let res = reqwest::Client::new()
        .post(sim.api_base().join("chat.postMessage").unwrap())
        .bearer_auth(&bot_token)
        .header("content-type", "application/json")
        .body(json!({"channel": GENERAL, "text": "x"}).to_string())
        .send()
        .await
        .unwrap();
    assert_eq!(res.status(), 200);

    let unknown = bot(&sim, "chat.postEphemeralish", json!({})).await;
    assert_eq!(unknown["error"], "unknown_method");

    let details: Vec<String> = sim.violations().into_iter().map(|v| v.detail).collect();
    assert!(details[0].contains("does not accept JSON"), "{details:?}");
    assert!(details.iter().any(|d| d.contains("charset")), "{details:?}");
    assert_eq!(
        errors(&sim),
        [
            "malformed_request",
            "channel_not_found",
            "malformed_request",
            "unknown_method"
        ]
    );
}

#[tokio::test]
async fn faults_apply_to_the_next_call_only() {
    let sim = sim().await;
    sim.rate_limit("chat.postMessage", 7);
    sim.fail("chat.postMessage", "fatal_error");
    let bot_token = sim.tokens().bot;

    let limited = reqwest::Client::new()
        .post(sim.api_base().join("chat.postMessage").unwrap())
        .bearer_auth(&bot_token)
        .header("content-type", "application/json; charset=utf-8")
        .body(json!({"channel": GENERAL, "text": "x"}).to_string())
        .send()
        .await
        .unwrap();
    assert_eq!(limited.status(), 429);
    assert_eq!(limited.headers()["retry-after"], "7");

    let failed = bot(
        &sim,
        "chat.postMessage",
        json!({"channel": GENERAL, "text": "x"}),
    )
    .await;
    assert_eq!(failed["error"], "fatal_error");
    let ok = bot(
        &sim,
        "chat.postMessage",
        json!({"channel": GENERAL, "text": "x"}),
    )
    .await;
    assert_eq!(ok["ok"], true);
    assert!(sim.violations().is_empty());
    assert_eq!(sim.messages(GENERAL).len(), 1);
}

#[tokio::test]
async fn wait_for_call_sees_past_calls_and_reports_what_arrived() {
    let sim = sim().await;
    post(&sim, GENERAL, "early").await;
    let found = sim
        .wait_for_call("chat.postMessage", |p| p["text"] == "early")
        .await
        .unwrap();
    assert_eq!(found.params["channel"], GENERAL);

    let later = async {
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        post(&sim, GENERAL, "late").await;
    };
    let (found, _) = tokio::join!(
        sim.wait_for_call("chat.postMessage", |p| p["text"] == "late"),
        later
    );
    assert!(found.is_ok());

    match sim.wait_for_call("reactions.add", |_| true).await {
        Err(SimError::Timeout { arrived, .. }) => {
            assert_eq!(arrived.len(), 2);
            let message = SimError::Timeout {
                method: "reactions.add".into(),
                waited: std::time::Duration::from_secs(5),
                arrived,
            }
            .to_string();
            assert!(
                message.contains("early") && message.contains("late"),
                "{message}"
            );
        }
        other => panic!("expected a timeout, got {other:?}"),
    }
}

#[tokio::test]
async fn an_external_upload_must_send_what_it_declared_before_it_is_shared() {
    let sim = sim().await;
    let bot = sim.tokens().bot;
    let reserved = form_call(
        &sim,
        "files.getUploadURLExternal",
        Some(&bot),
        &[("filename", "a.txt"), ("length", "10")],
    )
    .await;
    let file_id = reserved["file_id"].as_str().unwrap();
    let files = json!([{"id": file_id}]).to_string();
    let early = form_call(
        &sim,
        "files.completeUploadExternal",
        Some(&bot),
        &[("files", &files), ("channel_id", GENERAL)],
    )
    .await;
    assert_eq!(early["error"], "file_not_found", "{early}");

    let part = reqwest::multipart::Part::bytes(b"short".to_vec()).file_name("a.txt");
    let status = reqwest::Client::new()
        .post(reserved["upload_url"].as_str().unwrap())
        .bearer_auth(&bot)
        .multipart(reqwest::multipart::Form::new().part("file", part))
        .send()
        .await
        .unwrap()
        .status();
    assert!(status.is_success());
    let shared = form_call(
        &sim,
        "files.completeUploadExternal",
        Some(&bot),
        &[("files", &files), ("channel_id", GENERAL)],
    )
    .await;
    assert_eq!(shared["files"][0]["id"], file_id, "{shared}");
    assert_eq!(errors(&sim), ["file_not_found", "length_mismatch"]);
}
