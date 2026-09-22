#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use murtaugh_gateway::reply::{Reply, Target};
use murtaugh_slack::{SlackClient, TaskStatus, Tokens};
use slack_sim::{ALICE, BOT_USER_ID, GENERAL, SimMessage, SlackSim, TEAM_ID};

async fn setup() -> (SlackSim, Reply, String) {
    let sim = SlackSim::start().await.unwrap();
    let t = sim.tokens();
    let slack = SlackClient::new(
        Tokens {
            app: t.app,
            bot: t.bot,
        },
        sim.api_base(),
    );
    let thread = sim.mention(ALICE, GENERAL, "question", None).await.unwrap();
    let reply = Reply::new(
        slack,
        Target {
            channel: GENERAL.into(),
            thread_ts: thread.clone(),
            recipient: Some((TEAM_ID.into(), ALICE.into())),
        },
    );
    (sim, reply, thread)
}

fn answers(sim: &SlackSim, thread: &str) -> Vec<SimMessage> {
    sim.thread(GENERAL, thread)
        .into_iter()
        .filter(|m| m.user.as_deref() == Some(BOT_USER_ID))
        .collect()
}

#[tokio::test]
async fn text_streams_in_order_and_the_stream_is_stopped() {
    let (sim, mut reply, thread) = setup().await;
    for piece in ["Hello", " there,", " friend.\n", "**Done**"] {
        reply.text(piece).await;
    }
    reply.finish().await;
    let answers = answers(&sim, &thread);
    assert_eq!(answers.len(), 1);
    assert_eq!(answers[0].text, "Hello there, friend.\n**Done**");
    assert!(!answers[0].stream.as_ref().unwrap().open);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn a_long_answer_rolls_over_and_a_code_fence_survives_the_cut() {
    let (sim, mut reply, thread) = setup().await;
    reply.text("Intro\n```rust\n").await;
    let line = "let value = 42; // a line of code that takes up some room\n";
    for _ in 0..400 {
        reply.text(line).await;
    }
    reply.text("```\nOutro").await;
    reply.finish().await;

    let answers = answers(&sim, &thread);
    assert!(
        answers.len() >= 2,
        "no rollover: {} message(s)",
        answers.len()
    );
    for answer in &answers {
        assert!(answer.text.chars().count() <= 12_000);
        assert!(!answer.stream.as_ref().unwrap().open);
        assert_eq!(
            answer.text.matches("```").count() % 2,
            0,
            "unbalanced fence"
        );
    }
    assert!(
        answers[1].text.starts_with("```rust\n"),
        "{}",
        &answers[1].text[..40]
    );
    let whole: String = answers.iter().map(|a| a.text.as_str()).collect();
    assert_eq!(whole.matches(line).count(), 400);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn a_stream_slack_finalized_mid_answer_carries_on_in_a_new_message() {
    let (sim, mut reply, thread) = setup().await;
    reply.text("First part.\n").await;
    let first = answers(&sim, &thread)[0].ts.clone();
    sim.finalize_stream(GENERAL, &first);
    reply.text("Second part.\n").await;
    reply.finish().await;
    let texts: Vec<String> = answers(&sim, &thread).into_iter().map(|a| a.text).collect();
    assert_eq!(texts, ["First part.\n", "Second part.\n"]);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn a_stream_that_expired_while_idle_carries_on_in_a_new_message() {
    let (sim, mut reply, thread) = setup().await;
    reply.text("Asked for approval.\n").await;
    sim.fail("chat.appendStream", "message_not_found");
    reply.text("Denied, so I stopped.\n").await;
    reply.finish().await;
    let texts: Vec<String> = answers(&sim, &thread).into_iter().map(|a| a.text).collect();
    assert_eq!(texts, ["Asked for approval.\n", "Denied, so I stopped.\n"]);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn a_tool_settles_on_its_own_beat_however_much_the_agent_says_after_it() {
    let (sim, mut reply, thread) = setup().await;
    reply
        .task("t1", Some("Read a file"), TaskStatus::InProgress)
        .await;
    reply.task("t1", None, TaskStatus::InProgress).await;
    reply.text("It says hello.").await;
    reply.task("t1", None, TaskStatus::Complete).await;
    reply
        .task("t2", Some("Run tests"), TaskStatus::InProgress)
        .await;
    reply.task("t2", None, TaskStatus::Error).await;
    reply.finish().await;

    let answers = answers(&sim, &thread);
    assert_eq!(answers.len(), 1);
    let stream = answers[0].stream.clone().unwrap();
    let beats: Vec<String> = stream
        .plan_blocks
        .into_iter()
        .map(|block| {
            let tasks: Vec<String> = block
                .tasks
                .into_iter()
                .map(|t| format!("{} {} {}", t.id, t.title, t.status))
                .collect();
            format!("{}: {}", block.block_id, tasks.join("; "))
        })
        .collect();
    // t1 started before the text and settles on beat-1; only t2 opens beat-2.
    assert_eq!(
        beats,
        [
            "beat-1: t1 Read a file complete",
            "beat-2: t2 Run tests error",
        ]
    );
    assert_eq!(answers[0].text, "It says hello.");
    let task_calls = sim
        .calls()
        .iter()
        .filter(|c| c.params.to_string().contains("\\\"type\\\":\\\"plan\\\""))
        .count();
    assert_eq!(
        task_calls, 4,
        "the repeated in-progress update was not throttled"
    );
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn a_surface_that_cannot_stream_gets_ordinary_messages_instead() {
    let (sim, mut reply, thread) = setup().await;
    sim.fail("chat.startStream", "channel_type_not_supported");
    reply.text("**Hello** from a canvas").await;
    reply
        .task("t1", Some("ignored"), TaskStatus::InProgress)
        .await;
    reply.text(", still here.").await;
    reply.finish().await;
    let answers = answers(&sim, &thread);
    assert_eq!(answers.len(), 1);
    assert!(answers[0].stream.is_none());
    assert_eq!(answers[0].text, "*Hello* from a canvas, still here.");
}
