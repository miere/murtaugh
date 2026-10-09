//! Posts a file from the node's machine into the session's own Slack thread. The node never sends
//! a path: its argument is marked as a local file, so what arrives here is an identifier the node
//! minted, which this gateway reads once from that node.
//!
//! The answer is what Slack did with the file, so the agent never tells a person a file arrived
//! when it did not.

use async_trait::async_trait;
use murtaugh_slack::{SlackClient, Upload};
use rax::local_file::LOCAL_FILE_FORMAT;
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};

use super::{Context, Tool};

pub const NAME: &str = "attach";

pub struct Attach {
    slack: Option<SlackClient>,
}

impl Attach {
    pub fn new(slack: Option<SlackClient>) -> Self {
        Self { slack }
    }
}

fn text<'a>(arguments: &'a Value, name: &str) -> Option<&'a str> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|text| !text.is_empty())
}

#[async_trait]
impl Tool for Attach {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: "Post a file from your machine into this conversation's Slack thread, \
                 so the people in it receive the file itself. The machine you run on decides \
                 which files it will send."
                .to_owned(),
            input_schema: Some(json!({
                "type": "object",
                "required": ["path"],
                "properties": {
                    "path": {
                        "type": "string",
                        "format": LOCAL_FILE_FORMAT,
                        "description": "The file to post, as a path on your machine.",
                    },
                    "title": {"type": "string"},
                    "comment": {"type": "string", "description": "Shown above the file."},
                },
            })),
            kind: ToolKind::Edit,
        }
    }

    async fn invoke(&self, _arguments: Value) -> Result<String, String> {
        Err("This file has no conversation to be posted to.".into())
    }

    async fn invoke_in(&self, context: &Context, arguments: Value) -> Result<String, String> {
        let Some(slack) = &self.slack else {
            return Err("This gateway has no Slack credentials, so it cannot post.".into());
        };
        let Some(conversation) = &context.conversation else {
            return Err("This session has no Slack thread to post to.".into());
        };
        let Some(files) = &context.local_files else {
            return Err("This gateway cannot read files from your machine.".into());
        };
        let id = text(&arguments, "path").ok_or("`path` is empty, so there is no file to post.")?;
        let file = files.read(id).await.map_err(|reason| {
            format!("The file was not posted: {reason}. Tell the person it did not arrive.")
        })?;
        let name = file.name.unwrap_or_else(|| "attachment".to_owned());
        let size = file.bytes.len();
        let upload = Upload {
            channel: conversation.channel.clone(),
            thread_ts: Some(conversation.thread_ts.clone()),
            filename: name.clone(),
            title: text(&arguments, "title").map(str::to_owned),
            initial_comment: text(&arguments, "comment").map(str::to_owned),
            bytes: file.bytes,
        };
        match slack.upload_file(&upload).await {
            Ok(_) => Ok(format!("Posted {name} ({size} bytes) to the thread.")),
            Err(err) => Err(format!(
                "Slack did not take {name}: {err}. Tell the person it did not arrive."
            )),
        }
    }
}
