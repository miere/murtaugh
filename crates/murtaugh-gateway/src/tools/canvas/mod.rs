//! Reading and editing Slack canvases, in Markdown both ways.
//!
//! Slack takes Markdown in `canvases.edit` but serves a canvas only as HTML, so reading converts
//! (see [`document`]) and writing does not. Every block in that HTML carries the id
//! `canvases.edit` aims at, and a read hands the agent a short anchor for each heading instead of
//! those ids, which would cost more tokens than the text they label. Anchors resolve against a
//! fresh read at edit time, so the gateway keeps nothing between calls.
//!
//! The same refusal rule as reading a message: a canvas the bot cannot see must never read as an
//! empty one.

use murtaugh_slack::{FileInfo, SlackClient, SlackError};
use url::Url;

pub mod document;
pub mod edit;
pub mod read;

use document::{Document, Span, Unresolved};

/// A canvas as one read found it.
pub(crate) struct Canvas {
    pub info: FileInfo,
    pub document: Document,
}

impl Canvas {
    pub(crate) async fn load(slack: &SlackClient, id: &str) -> Result<Canvas, String> {
        let info = slack.file_info(id).await.map_err(|err| explain(&err, id))?;
        if !info.is_canvas() {
            return Err(format!(
                "{id} is a file ({}), not a canvas. Nothing was read.",
                info.mimetype
            ));
        }
        let html = slack
            .canvas_html(&info)
            .await
            .map_err(|err| explain(&err, id))?;
        Ok(Canvas {
            info,
            document: Document::parse(&html),
        })
    }

    pub(crate) fn title(&self) -> &str {
        if self.info.title.is_empty() {
            &self.info.name
        } else {
            &self.info.title
        }
    }

    pub(crate) fn section(&self, anchor: &str) -> Result<Span, String> {
        self.document.section(anchor).map_err(|unresolved| match unresolved {
            Unresolved::Missing => format!(
                "No heading in this canvas has the anchor `{anchor}` any more: it was edited \
                 since you read it. Read it again for current anchors. Nothing was changed."
            ),
            Unresolved::Ambiguous => format!(
                "`{anchor}` now matches more than one heading. Read the canvas again for longer \
                 anchors. Nothing was changed."
            ),
        })
    }
}

/// The canvas's file id, from a link as Slack copies it, Slack's own `<url|label>` markup around
/// one, or the bare id.
pub(crate) fn canvas_id(link: &str) -> Result<String, String> {
    let link = link.trim();
    let link = link
        .strip_prefix('<')
        .and_then(|inner| inner.strip_suffix('>'))
        .map_or(link, |inner| inner.split('|').next().unwrap_or(inner));
    if is_file_id(link) {
        return Ok(link.to_owned());
    }
    let url = Url::parse(link).map_err(|_| {
        format!(
            "`{link}` is neither a canvas link nor a canvas id. Paste the link copied from Slack."
        )
    })?;
    url.path_segments()
        .into_iter()
        .flatten()
        .find(|segment| is_file_id(segment))
        .map(str::to_owned)
        .ok_or_else(|| {
            format!(
                "`{link}` names no canvas. A canvas link looks like \
                 https://<workspace>.slack.com/docs/T…/F…"
            )
        })
}

/// Slack's file ids, which canvases share: `F` then at least eight capitals or digits.
fn is_file_id(text: &str) -> bool {
    text.len() >= 9
        && text.starts_with('F')
        && text
            .bytes()
            .all(|byte| byte.is_ascii_uppercase() || byte.is_ascii_digit())
}

/// Slack's refusals in words an agent can act on. A code not met before keeps Slack's own word.
pub(crate) fn explain(error: &SlackError, canvas: &str) -> String {
    let SlackError::Api { error, detail, .. } = error else {
        if let SlackError::Download { .. } = error {
            return format!(
                "Slack would not hand over {canvas}'s content: this bot's token was refused. \
                 Nothing was read."
            );
        }
        return format!("Could not reach the canvas {canvas}: {error}");
    };
    match error.as_str() {
        "file_not_found" | "canvas_not_found" | "access_denied" | "not_in_channel" => format!(
            "I cannot see the canvas {canvas}: it is not shared with this bot, or it lives in a \
             channel the bot is not in. It is not empty — I have no access to it. Ask someone to \
             share the canvas with Murtaugh or invite Murtaugh to its channel, then try again."
        ),
        "canvas_editing_failed" => format!(
            "Slack refused the edit to {canvas}: {}. Read the canvas again before retrying.",
            detail.as_deref().unwrap_or("no reason given")
        ),
        "missing_scope" | "not_allowed_token_type" => format!(
            "This bot's Slack token lacks the permission canvases need (Slack said {error}). \
             Nothing was read or changed."
        ),
        other => match detail {
            Some(detail) => format!("Slack refused {canvas}: {other} ({detail})."),
            None => format!("Slack refused {canvas}: {other}."),
        },
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used)]

    use super::*;

    #[test]
    fn a_canvas_is_found_in_every_shape_its_link_comes_in() {
        for link in [
            "https://immersiveloop.slack.com/docs/T0B21S3U72S/F0C3E7TQGQK",
            "<https://immersiveloop.slack.com/docs/T0B21S3U72S/F0C3E7TQGQK|this Canvas>",
            "https://immersiveloop.slack.com/files/U0B20G0ET9T/F0C3E7TQGQK/untitled",
            "F0C3E7TQGQK",
            "  F0C3E7TQGQK\n",
        ] {
            assert_eq!(canvas_id(link).unwrap(), "F0C3E7TQGQK", "{link}");
        }
    }

    #[test]
    fn a_link_with_no_canvas_in_it_is_refused_with_the_shape_to_use() {
        for link in [
            "https://immersiveloop.slack.com/archives/C1/p1700000000123456",
            "not a link",
            "F123",
        ] {
            assert!(canvas_id(link).is_err(), "{link} should not parse");
        }
    }

    /// The failure that matters most: a canvas the bot cannot see must not read as an empty one.
    #[test]
    fn no_access_never_reads_as_an_empty_canvas() {
        for code in ["file_not_found", "canvas_not_found"] {
            let refusal = explain(
                &SlackError::Api {
                    method: "files.info".into(),
                    error: code.into(),
                    detail: None,
                },
                "F1",
            );
            assert!(refusal.contains("It is not empty"), "{refusal}");
            assert!(refusal.contains("share"), "{refusal}");
        }
    }

    #[test]
    fn a_refused_edit_carries_slacks_reason() {
        let refusal = explain(
            &SlackError::Api {
                method: "canvases.edit".into(),
                error: "canvas_editing_failed".into(),
                detail: Some("Invalid section ID: 'temp:C:GFFnope'".into()),
            },
            "F1",
        );
        assert!(refusal.contains("Invalid section ID"));
        assert!(refusal.contains("Read the canvas again"));
    }
}
