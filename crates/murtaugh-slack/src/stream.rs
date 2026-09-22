//! Slack's streaming messages: `chat.startStream`, `chat.appendStream` and `chat.stopStream`.
//! Text chunks take standard Markdown, and task cards ride the same message.

use serde::Serialize;

/// What Slack answers when a stream was already stopped, including by Slack itself.
pub const STREAM_FINALIZED: &str = "message_not_in_streaming_state";
/// What Slack answers when a surface, such as a canvas, cannot host a stream at all.
pub const STREAMING_UNSUPPORTED: &str = "channel_type_not_supported";

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum TaskStatus {
    Pending,
    InProgress,
    Complete,
    Error,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct PlanTask {
    pub task_id: String,
    pub title: String,
    pub status: TaskStatus,
}

/// A message may hold only one of Slack's own plan blocks, so task lists are sent as blocks of
/// our own instead; re-sending one with a `block_id` already in the message rewrites it in place.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(tag = "type", rename = "plan")]
pub struct PlanBlock {
    pub block_id: String,
    pub title: String,
    pub tasks: Vec<PlanTask>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Chunk {
    MarkdownText { text: String },
    Blocks { blocks: Vec<PlanBlock> },
}

impl Chunk {
    pub fn markdown(text: impl Into<String>) -> Self {
        Self::MarkdownText { text: text.into() }
    }

    pub fn plan(
        block_id: impl Into<String>,
        title: impl Into<String>,
        tasks: Vec<PlanTask>,
    ) -> Self {
        Self::Blocks {
            blocks: vec![PlanBlock {
                block_id: block_id.into(),
                title: title.into(),
                tasks,
            }],
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum TaskDisplayMode {
    Plan,
    Timeline,
}

/// `recipient` is the person being answered, as (team, user). Go's gateway omits it in DMs.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StartStream {
    pub channel: String,
    pub thread_ts: String,
    pub recipient: Option<(String, String)>,
    pub task_display_mode: TaskDisplayMode,
    pub chunks: Vec<Chunk>,
}
