#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use murtaugh_slack::{
    Chunk, PostMessage, STREAM_FINALIZED, SlackClient, SlackError, StartStream, TaskDisplayMode,
    TaskStatus, Tokens,
};
use slack_sim::{ALICE, GENERAL, SlackSim, TEAM_ID};

async fn setup() -> (SlackSim, SlackClient, String) {
    let sim = SlackSim::start().await.unwrap();
    let t = sim.tokens();
    let client = SlackClient::new(
        Tokens {
            app: t.app,
            bot: t.bot,
        },
        sim.api_base(),
    );
    let thread = sim.mention(ALICE, GENERAL, "question", None).await.unwrap();
    (sim, client, thread)
}

fn start(channel: &str, thread_ts: &str, chunks: Vec<Chunk>) -> StartStream {
    StartStream {
        channel: channel.into(),
        thread_ts: thread_ts.into(),
        recipient: Some((TEAM_ID.into(), ALICE.into())),
        task_display_mode: TaskDisplayMode::Plan,
        chunks,
    }
}

fn task(id: &str, status: TaskStatus) -> Chunk {
    Chunk::TaskUpdate {
        id: id.into(),
        title: format!("Run {id}"),
        status,
        details: None,
    }
}

fn api_error(result: Result<impl std::fmt::Debug, SlackError>) -> String {
    match result {
        Err(SlackError::Api { error, .. }) => error,
        other => panic!("expected a Slack error, got {other:?}"),
    }
}

#[tokio::test]
async fn a_stream_carries_markdown_and_task_cards_into_one_threaded_message() {
    let (sim, client, thread) = setup().await;
    let chunks = vec![
        Chunk::PlanUpdate {
            title: "Task list".into(),
        },
        task("t1", TaskStatus::InProgress),
    ];
    let posted = client
        .start_stream(&start(GENERAL, &thread, chunks))
        .await
        .unwrap();
    client
        .append_stream(GENERAL, &posted.ts, &[Chunk::markdown("**Hello**, ")])
        .await
        .unwrap();
    client
        .append_stream(
            GENERAL,
            &posted.ts,
            &[task("t1", TaskStatus::Complete), Chunk::markdown("world")],
        )
        .await
        .unwrap();
    client.stop_stream(GENERAL, &posted.ts).await.unwrap();

    let message = sim
        .thread(GENERAL, &thread)
        .into_iter()
        .find(|m| m.ts == posted.ts)
        .unwrap();
    assert_eq!(message.thread_ts.as_deref(), Some(thread.as_str()));
    assert_eq!(message.text, "**Hello**, world");
    let stream = message.stream.unwrap();
    assert!(!stream.open);
    assert_eq!(stream.plans, ["Task list"]);
    assert_eq!(stream.tasks.len(), 1);
    assert_eq!(stream.tasks[0].status, "complete");
    assert_eq!(stream.task_display_mode, "plan");
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn a_stream_slack_finalized_refuses_more_chunks_without_a_violation() {
    let (sim, client, thread) = setup().await;
    let posted = client
        .start_stream(&start(GENERAL, &thread, vec![]))
        .await
        .unwrap();
    sim.finalize_stream(GENERAL, &posted.ts);
    let refused = client
        .append_stream(GENERAL, &posted.ts, &[Chunk::markdown("late")])
        .await;
    assert_eq!(api_error(refused), STREAM_FINALIZED);
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}

#[tokio::test]
async fn slack_rejects_what_a_stream_cannot_hold() {
    let (sim, client, thread) = setup().await;
    let mut no_recipient = start(GENERAL, &thread, vec![]);
    no_recipient.recipient = None;
    assert_eq!(
        api_error(client.start_stream(&no_recipient).await),
        "invalid_arguments"
    );

    let posted = client
        .start_stream(&start(GENERAL, &thread, vec![]))
        .await
        .unwrap();
    let too_long = Chunk::markdown("x".repeat(12_001));
    assert_eq!(
        api_error(client.append_stream(GENERAL, &posted.ts, &[too_long]).await),
        "msg_too_long"
    );
    let unthreaded = client
        .post_message(&PostMessage {
            channel: GENERAL.into(),
            thread_ts: None,
            text: "top".into(),
            blocks: vec![],
        })
        .await
        .unwrap();
    assert_eq!(
        api_error(
            client
                .append_stream(GENERAL, &unthreaded.ts, &[Chunk::markdown("x")])
                .await
        ),
        STREAM_FINALIZED
    );
    let errors: Vec<String> = sim.violations().into_iter().map(|v| v.error).collect();
    assert_eq!(
        errors,
        ["invalid_arguments", "msg_too_long", STREAM_FINALIZED]
    );
}

#[tokio::test]
async fn a_direct_message_streams_without_a_recipient() {
    let (sim, client, _) = setup().await;
    let (channel, ts) = sim.dm(ALICE, "hi").await.unwrap();
    let mut dm = start(&channel, &ts, vec![Chunk::markdown("hello")]);
    dm.recipient = None;
    client.start_stream(&dm).await.unwrap();
    assert!(sim.violations().is_empty(), "{:?}", sim.violations());
}
