//! Posts a message into the session's own Slack thread. A turn's words already land there as it
//! streams; this is how an agent speaks when the thread does not stream, such as a quiet workload,
//! or when it wants something said on its own.
//!
//! It posts nowhere but the session's thread: the thread is resolved here from the session, never
//! taken from the agent, so a session cannot write into a conversation it is not part of.

use async_trait::async_trait;
use murtaugh_slack::{PostMessage, SlackClient};
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};

use super::{Context, Tool};
use crate::render::{self, MESSAGE_LIMIT};

pub const NAME: &str = "send_message";

pub struct SendMessage {
    slack: Option<SlackClient>,
}

impl SendMessage {
    pub fn new(slack: Option<SlackClient>) -> Self {
        Self { slack }
    }
}

#[async_trait]
impl Tool for SendMessage {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: "Post a message into this conversation's Slack thread, in Markdown. \
                 Use it to tell the people in the thread something now, such as a result, when \
                 your reply is not being shown as you write it."
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
        let text = arguments
            .get("text")
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|text| !text.is_empty())
            .ok_or("`text` is empty, so there is nothing to post.")?;
        for part in render::split(&render::mrkdwn(text), MESSAGE_LIMIT) {
            let message = PostMessage {
                channel: conversation.channel.clone(),
                thread_ts: Some(conversation.thread_ts.clone()),
                text: part,
                blocks: Vec::new(),
            };
            slack
                .post_message(&message)
                .await
                .map_err(|err| format!("Slack did not take the message: {err}"))?;
        }
        Ok("Posted.".to_owned())
    }
}
