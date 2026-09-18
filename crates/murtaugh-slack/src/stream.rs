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
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Chunk {
    MarkdownText {
        text: String,
    },
    TaskUpdate {
        id: String,
        title: String,
        status: TaskStatus,
        #[serde(skip_serializing_if = "Option::is_none")]
        details: Option<String>,
    },
    /// Opens the task list that following task updates are drawn in.
    PlanUpdate {
        title: String,
    },
}

impl Chunk {
    pub fn markdown(text: impl Into<String>) -> Self {
        Self::MarkdownText { text: text.into() }
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
