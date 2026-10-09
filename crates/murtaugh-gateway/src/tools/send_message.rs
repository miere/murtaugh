//! Posts a message into the session's own Slack thread. This is how an agent speaks when its
//! reply is not shown there, such as a quiet workload.
//!
//! The Markdown goes out as Slack's `markdown` block, which renders it as written: tables, and
//! code fences highlighted for the language they name.
//!
//! It posts nowhere but the session's thread: the thread is resolved here from the session, never
//! taken from the agent, so a session cannot write into a conversation it is not part of.
//!
//! While the gateway is writing the agent's reply into that thread, the tool refuses. Slack orders
//! a thread by when each message started, so a message posted mid-reply lands below a reply that
//! is still being written above it.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use murtaugh_slack::{Block, PostMessage, SlackClient};
use murtaugh_store::Conversation;
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};

use super::{Context, Tool, lock};
use crate::render;
use crate::reply::{STREAM_BUDGET, markdown_parts};

pub const NAME: &str = "send_message";

/// The threads the gateway is writing an agent's reply into right now.
#[derive(Clone, Default)]
pub struct Replying {
    threads: Arc<Mutex<HashMap<Conversation, usize>>>,
}

impl Replying {
    /// Marks a thread until the guard is dropped. Counted, because a turn and a session's
    /// background run can both be writing into one thread.
    pub fn hold(&self, conversation: &Conversation) -> Held {
        *lock(&self.threads).entry(conversation.clone()).or_default() += 1;
        Held {
            replying: self.clone(),
            conversation: conversation.clone(),
        }
    }

    fn contains(&self, conversation: &Conversation) -> bool {
        lock(&self.threads).contains_key(conversation)
    }
}

pub struct Held {
    replying: Replying,
    conversation: Conversation,
}

impl Drop for Held {
    fn drop(&mut self) {
        let mut threads = lock(&self.replying.threads);
        if let Some(held) = threads.get_mut(&self.conversation) {
            *held -= 1;
            if *held == 0 {
                threads.remove(&self.conversation);
            }
        }
    }
}

pub struct SendMessage {
    slack: Option<SlackClient>,
    replying: Replying,
}

impl SendMessage {
    pub fn new(slack: Option<SlackClient>, replying: Replying) -> Self {
        Self { slack, replying }
    }
}

#[async_trait]
impl Tool for SendMessage {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: "Post a message into this conversation's Slack thread, in Markdown. \
                 It is only for when your reply is not shown in the thread, such as a quiet \
                 workload. It refuses while your reply is shown there, because everything you \
                 write already goes to the thread."
                .to_owned(),
            input_schema: Some(json!({
                "type": "object",
                "required": ["text"],
                "properties": {
                    "text": {"type": "string", "description": "What to say, in Markdown."},
                },
            })),
            kind: ToolKind::Edit,
        }
    }

    async fn invoke(&self, _arguments: Value) -> Result<String, String> {
        Err("This message has no conversation to post to.".into())
    }

    async fn invoke_in(&self, context: &Context, arguments: Value) -> Result<String, String> {
        let Some(slack) = &self.slack else {
            return Err("This gateway has no Slack credentials, so it cannot post.".into());
        };
        let Some(conversation) = &context.conversation else {
            return Err("This session has no Slack thread to post to.".into());
        };
        if self.replying.contains(conversation) {
            return Err(
                "`send_message` was not designed to post into the thread you are replying in: \
                 everything you write already goes to that thread. Nothing was posted."
                    .into(),
            );
        }
        let text = arguments
            .get("text")
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|text| !text.is_empty())
            .ok_or("`text` is empty, so there is nothing to post.")?;
        // The block is what Slack shows; `text` is what a notification reads out.
        for part in markdown_parts(text, STREAM_BUDGET) {
            let message = PostMessage {
                channel: conversation.channel.clone(),
                thread_ts: Some(conversation.thread_ts.clone()),
                text: render::mrkdwn(&part),
                blocks: vec![Block::Raw(json!({"type": "markdown", "text": part}))],
            };
            slack
                .post_message(&message)
                .await
                .map_err(|err| format!("Slack did not take the message: {err}"))?;
        }
        Ok("Posted.".to_owned())
    }
}
