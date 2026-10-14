//! Reads a Slack file from its id or link. The bytes never ride the answer: the file is lent to
//! the node as a resource, which the node fetches before its agent hears back and hands over as a
//! local file. What the bot may read is what Slack lets it see; anything else is refused by
//! `files.info`, and that refusal reaches the agent as one, never as an empty file.

use async_trait::async_trait;
use murtaugh_slack::{CANVAS_MIMETYPE, FileRef, SlackClient, SlackError};
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};
use url::Url;

use super::canvas::is_file_id;
use super::{Context, Tool};

pub const NAME: &str = "read_file";

pub struct ReadFile {
    slack: Option<SlackClient>,
}

impl ReadFile {
    pub fn new(slack: Option<SlackClient>) -> Self {
        Self { slack }
    }
}

#[async_trait]
impl Tool for ReadFile {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: "Read a file shared in Slack, from its id or its link. The file is saved \
                 on your machine and the answer gives its local path; open it from there with \
                 your own tools. Returns an error if this bot cannot see the file — when that \
                 happens, say so and ask for it to be shared rather than assuming it is empty."
                .to_owned(),
            input_schema: Some(json!({
                "type": "object",
                "required": ["file"],
                "properties": {
                    "file": {
                        "type": "string",
                        "description": "The file's id (F…), or its link as copied from Slack.",
                    },
                },
            })),
            kind: ToolKind::Read,
        }
    }

    async fn invoke(&self, _arguments: Value) -> Result<String, String> {
        Err("read_file needs the calling session, which this call did not carry.".into())
    }

    async fn invoke_in(&self, context: &Context, arguments: Value) -> Result<String, String> {
        let Some(lending) = &context.lending else {
            return Err(
                "Your machine's Riggs is too old to receive files from Slack. Say so \
                 and ask its owner to update Riggs; do not retry."
                    .into(),
            );
        };
        let Some(slack) = &self.slack else {
            return Err("This gateway has no Slack credentials, so it cannot read files.".into());
        };
        let file = arguments
            .get("file")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let id = file_id(file)?;
        let info = slack
            .file_info(&id)
            .await
            .map_err(|err| explain(&err, &id))?;
        if info.mimetype == CANVAS_MIMETYPE {
            return Err(format!(
                "{id} is a canvas. Read it with read_canvas, which returns its text."
            ));
        }
        lending.lend(&FileRef {
            id: info.id.clone(),
            name: Some(info.name.clone()),
            mimetype: (!info.mimetype.is_empty()).then(|| info.mimetype.clone()),
            size: Some(info.size),
            permalink: (!info.permalink.is_empty()).then(|| info.permalink.clone()),
        });
        Ok(format!(
            "{} ({}, {} bytes, id {}). Its local copy is linked below.",
            info.name, info.mimetype, info.size, info.id
        ))
    }
}

/// The file id, from the bare id, Slack's `<url|label>` markup or any link Slack makes for a file:
/// its permalink (`/files/U…/F…/name`), its download link (`/files-pri/T…-F…/name`) or a canvas
/// link (`/docs/T…/F…`).
fn file_id(file: &str) -> Result<String, String> {
    let file = file.trim();
    let file = file
        .strip_prefix('<')
        .and_then(|inner| inner.strip_suffix('>'))
        .map_or(file, |inner| inner.split('|').next().unwrap_or(inner));
    if is_file_id(file) {
        return Ok(file.to_owned());
    }
    let url = Url::parse(file).map_err(|_| {
        format!(
            "`{file}` is neither a Slack file link nor a file id. Pass the link copied from Slack."
        )
    })?;
    url.path_segments()
        .into_iter()
        .flatten()
        .flat_map(|segment| segment.split('-'))
        .find(|part| is_file_id(part))
        .map(str::to_owned)
        .ok_or_else(|| {
            format!(
                "`{file}` names no file. A file link looks like \
                 https://<workspace>.slack.com/files/U…/F…/name"
            )
        })
}

/// Slack's refusals in words an agent can act on.
fn explain(error: &SlackError, file: &str) -> String {
    let SlackError::Api { error, .. } = error else {
        return format!("Could not reach Slack for the file {file}: {error}");
    };
    match error.as_str() {
        "file_not_found" | "file_deleted" | "access_denied" | "not_in_channel" => format!(
            "I cannot see the file {file}: it was deleted, or it was shared somewhere this bot \
             is not. Ask someone to share it in a channel Murtaugh is in, then try again."
        ),
        "missing_scope" | "not_allowed_token_type" => format!(
            "This bot's Slack token lacks the permission files need (Slack said {error}). \
             Nothing was read."
        ),
        other => format!("Slack refused the file {file}: {other}."),
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used)]

    use super::*;

    #[test]
    fn a_file_is_found_in_every_shape_its_link_comes_in() {
        for link in [
            "F0C3E7TQGQK",
            "  F0C3E7TQGQK\n",
            "https://immersiveloop.slack.com/files/U0B20G0ET9T/F0C3E7TQGQK/q3_report.pdf",
            "<https://immersiveloop.slack.com/files/U0B20G0ET9T/F0C3E7TQGQK/q3.pdf|q3.pdf>",
            "https://files.slack.com/files-pri/T0B21S3U72S-F0C3E7TQGQK/q3_report.pdf",
            "https://files.slack.com/files-pri/T0B21S3U72S-F0C3E7TQGQK/download/q3_report.pdf",
            "https://immersiveloop.slack.com/docs/T0B21S3U72S/F0C3E7TQGQK",
        ] {
            assert_eq!(file_id(link).unwrap(), "F0C3E7TQGQK", "{link}");
        }
    }

    #[test]
    fn a_link_with_no_file_in_it_is_refused_with_the_shape_to_use() {
        let refused =
            file_id("https://immersiveloop.slack.com/archives/C0B21S3U7/p1700000000123456")
                .unwrap_err();
        assert!(refused.contains("/files/U…/F…/name"), "{refused}");
        assert!(file_id("the report").unwrap_err().contains("neither"));
    }

    #[test]
    fn no_access_never_reads_as_an_empty_file() {
        let refused = explain(
            &SlackError::Api {
                method: "files.info".into(),
                error: "file_not_found".into(),
                detail: None,
            },
            "F1",
        );
        assert!(refused.contains("cannot see"), "{refused}");
    }

    /// A node that cannot take files is told so, and nothing is lent.
    #[tokio::test]
    async fn an_old_node_is_told_to_update() {
        let tool = ReadFile::new(None);
        let refused = tool
            .invoke_in(&Context::default(), json!({"file": "F0C3E7TQGQK"}))
            .await
            .unwrap_err();
        assert!(refused.contains("too old"), "{refused}");
    }
}
