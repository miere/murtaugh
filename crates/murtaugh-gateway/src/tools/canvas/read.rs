//! Reads a canvas as Markdown, whole or one section of it.

use async_trait::async_trait;
use murtaugh_slack::SlackClient;
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};

use super::{Canvas, canvas_id};
use crate::tools::Tool;

pub const NAME: &str = "read_canvas";

pub struct ReadCanvas {
    slack: Option<SlackClient>,
}

impl ReadCanvas {
    pub fn new(slack: Option<SlackClient>) -> Self {
        Self { slack }
    }
}

#[async_trait]
impl Tool for ReadCanvas {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: "Read a Slack canvas as Markdown. Each heading ends with an anchor such \
                 as {#a1b2}: pass it as `section` here to read only that section, or to \
                 edit_canvas to change it. Anchors are labels, not part of the text. Returns an \
                 error if this bot cannot see the canvas — say so and ask for it to be shared \
                 rather than assuming it is empty."
                .to_owned(),
            input_schema: Some(json!({
                "type": "object",
                "required": ["link"],
                "properties": {
                    "link": {
                        "type": "string",
                        "description": "The canvas link as copied from Slack, or its id (F…).",
                    },
                    "section": {
                        "type": "string",
                        "description": "A heading's anchor: read only that heading and what is \
                            under it, subsections included.",
                    },
                },
            })),
            kind: ToolKind::Read,
        }
    }

    async fn invoke(&self, arguments: Value) -> Result<String, String> {
        let Some(slack) = &self.slack else {
            return Err(
                "This gateway has no Slack credentials, so it cannot read canvases.".into(),
            );
        };
        let link = arguments
            .get("link")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let id = canvas_id(link)?;
        let canvas = Canvas::load(slack, &id).await?;
        let section = arguments
            .get("section")
            .and_then(Value::as_str)
            .filter(|section| !section.trim().is_empty());
        let body = match section {
            Some(anchor) => {
                let span = canvas.section(anchor)?;
                canvas.document.markdown_of(span)
            }
            None => canvas.document.markdown(),
        };
        if body.is_empty() {
            return Ok(format!("Canvas \"{}\" ({id}) is empty.", canvas.title()));
        }
        Ok(format!("Canvas \"{}\" ({id}):\n\n{body}", canvas.title()))
    }
}
