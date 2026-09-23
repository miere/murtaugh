//! Reads a Slack message the person linked, and optionally the thread under it.
//!
//! The link carries everything the read needs, so this tool resolves no conversation of its own.
//! What it does carry is a refusal: the bot only sees channels it was invited to, and Slack
//! answers a read of any other with `not_in_channel` or `channel_not_found`. Those must reach the
//! agent as a refusal it can act on — an empty result would read as "the thread is empty" and it
//! would carry on believing it.

use async_trait::async_trait;
use murtaugh_slack::{Message, SlackClient, SlackError};
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};
use url::Url;

use super::Tool;

pub const NAME: &str = "slack_read_message";

pub struct SlackReadMessage {
    slack: Option<SlackClient>,
}

impl SlackReadMessage {
    pub fn new(slack: Option<SlackClient>) -> Self {
        Self { slack }
    }
}

#[async_trait]
impl Tool for SlackReadMessage {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: "Read a Slack message from its link, and optionally the thread under it. \
                 Use it whenever someone shares a Slack link and you need what it says. \
                 Returns an error if this bot is not a member of that channel — when that happens, \
                 say so and ask to be invited rather than assuming the message is empty."
                .to_owned(),
            input_schema: Some(json!({
                "type": "object",
                "required": ["link"],
                "properties": {
                    "link": {
                        "type": "string",
                        "description": "The Slack message link, as copied from Slack \
                            (…/archives/C…/p…).",
                    },
                    "thread": {
                        "type": "boolean",
                        "description": "Read every reply in the thread as well. Defaults to false, \
                            which reads only the linked message.",
                    },
                },
            })),
            kind: ToolKind::Read,
        }
    }

    async fn invoke(&self, arguments: Value) -> Result<String, String> {
        let Some(slack) = &self.slack else {
            return Err(
                "This gateway has no Slack credentials, so it cannot read messages.".into(),
            );
        };
        let link = arguments
            .get("link")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let target = Target::parse(link)?;
        let whole_thread = arguments
            .get("thread")
            .and_then(Value::as_bool)
            .unwrap_or(false);

        if whole_thread {
            let messages = slack
                .replies(&target.channel, &target.thread_ts())
                .await
                .map_err(|err| explain(&err, &target.channel))?;
            // A thread always contains the message it was asked about, so nothing here is a
            // legitimate empty: something refused or the message is gone.
            if messages.is_empty() {
                return Err(vanished(&target));
            }
            Ok(render_thread(&target.channel, &messages))
        } else {
            let message = slack
                .message_at(&target.channel, &target.ts)
                .await
                .map_err(|err| explain(&err, &target.channel))?;
            match message {
                Some(message) => Ok(render_one(&target.channel, &message)),
                None => Err(vanished(&target)),
            }
        }
    }
}

/// What a permalink resolves to. `thread_ts` is present when the link points at a reply, and names
/// the parent the thread read has to start from.
#[derive(Debug, PartialEq, Eq)]
struct Target {
    channel: String,
    ts: String,
    thread_ts: Option<String>,
}

impl Target {
    fn parse(link: &str) -> Result<Target, String> {
        let url = Url::parse(link.trim())
            .map_err(|_| format!("`{link}` is not a URL. Paste the link copied from Slack."))?;
        let mut segments = url.path_segments().into_iter().flatten();
        if !segments.any(|segment| segment == "archives") {
            return Err(format!(
                "`{link}` is not a Slack message link. It should look like \
                 https://<workspace>.slack.com/archives/C…/p…"
            ));
        }
        let channel = segments
            .next()
            .filter(|channel| !channel.is_empty())
            .ok_or_else(|| format!("`{link}` names no channel."))?;
        let stamp = segments
            .next()
            .ok_or_else(|| format!("`{link}` names a channel but no message."))?;
        let ts = timestamp(stamp).ok_or_else(|| {
            format!("`{stamp}` is not a Slack message id. It should look like p1700000000123456.")
        })?;
        let thread_ts = url
            .query_pairs()
            .find(|(key, _)| key == "thread_ts")
            .map(|(_, value)| value.into_owned());
        Ok(Target {
            channel: channel.to_owned(),
            ts,
            thread_ts,
        })
    }

    /// A link to a reply carries its parent; a link to a parent is its own thread root.
    fn thread_ts(&self) -> String {
        self.thread_ts.clone().unwrap_or_else(|| self.ts.clone())
    }
}

/// Slack writes a permalink stamp as `p` then the timestamp with its dot removed.
fn timestamp(stamp: &str) -> Option<String> {
    let digits = stamp.strip_prefix('p')?;
    if digits.len() < 7 || !digits.bytes().all(|byte| byte.is_ascii_digit()) {
        return None;
    }
    let (seconds, micros) = digits.split_at(digits.len() - 6);
    Some(format!("{seconds}.{micros}"))
}

/// Turns Slack's refusal codes into something an agent can act on. Everything else keeps Slack's
/// own wording, because a code we have not seen before is better read than paraphrased.
fn explain(error: &SlackError, channel: &str) -> String {
    let SlackError::Api { error, .. } = error else {
        return format!("Could not read that message: {error}");
    };
    match error.as_str() {
        "not_in_channel" | "channel_not_found" => format!(
            "I cannot see {channel}: this bot is not a member of that channel, so the message \
             cannot be read. It is not empty — I have no access to it. Ask someone in the channel \
             to invite Murtaugh, then try again."
        ),
        "thread_not_found" => {
            format!("That message is not in {channel}, or it has been deleted. Nothing was read.")
        }
        "missing_scope" | "not_allowed_token_type" => format!(
            "This bot's Slack token is missing the permission needed to read {channel} \
             (Slack said {error}). Nothing was read."
        ),
        other => format!("Slack refused to read {channel}: {other}. Nothing was read."),
    }
}

fn vanished(target: &Target) -> String {
    format!(
        "No message exists at that timestamp in {}. It may have been deleted. \
         Nothing was read — do not treat this as an empty message.",
        target.channel
    )
}

fn render_one(channel: &str, message: &Message) -> String {
    format!(
        "Message in {channel} at {}:\n\n{}",
        message.ts,
        body(message)
    )
}

fn render_thread(channel: &str, messages: &[Message]) -> String {
    let mut out = format!(
        "Thread in {channel}, {} message{}, oldest first:\n",
        messages.len(),
        if messages.len() == 1 { "" } else { "s" }
    );
    for message in messages {
        out.push_str(&format!("\n--- {} ---\n{}\n", message.ts, body(message)));
    }
    out
}

/// Slack's own text, verbatim: its `<@U…>` mentions and `<url|label>` links mean more to the agent
/// intact than flattened, and a bot message with no text still has to say that it had none.
fn body(message: &Message) -> String {
    let who = message
        .user
        .clone()
        .or_else(|| message.bot_id.clone())
        .unwrap_or_else(|| "unknown".to_owned());
    let text = if message.text.trim().is_empty() {
        "(no text — the message is blocks, a file or an attachment)"
    } else {
        message.text.trim()
    };
    let files = match message.files.len() {
        0 => String::new(),
        count => format!("\n({count} file(s) attached)"),
    };
    format!("From {who}:\n{text}{files}")
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used)]

    use super::*;

    #[test]
    fn a_permalink_resolves_to_a_channel_and_a_timestamp() {
        let target =
            Target::parse("https://immersiveloop.slack.com/archives/C0B21S3U7/p1700000000123456")
                .unwrap();
        assert_eq!(target.channel, "C0B21S3U7");
        assert_eq!(target.ts, "1700000000.123456");
        assert_eq!(target.thread_ts, None);
        // With no parent in the link, the message is its own thread root.
        assert_eq!(target.thread_ts(), "1700000000.123456");
    }

    /// A link to a reply names its parent, and a thread read has to start from the parent or Slack
    /// returns nothing.
    #[test]
    fn a_link_to_a_reply_reads_its_thread_from_the_parent() {
        let target = Target::parse(
            "https://immersiveloop.slack.com/archives/C1/p1700000000123456?thread_ts=1699999999.000100&cid=C1",
        )
        .unwrap();
        assert_eq!(target.ts, "1700000000.123456");
        assert_eq!(target.thread_ts(), "1699999999.000100");
    }

    #[test]
    fn a_link_that_is_not_a_slack_message_is_refused_with_the_shape_to_use() {
        for link in [
            "https://example.com/hello",
            "https://immersiveloop.slack.com/archives/C1",
            "https://immersiveloop.slack.com/archives/C1/1700000000123456",
            "https://immersiveloop.slack.com/archives/C1/pnotdigits",
            "not a url at all",
        ] {
            assert!(Target::parse(link).is_err(), "{link} should not parse");
        }
    }

    /// The failure Miere asked for by name: a channel the bot was never invited to must not read as
    /// an empty one, or the agent carries on believing there was nothing to find.
    #[test]
    fn no_visibility_never_reads_as_an_empty_channel() {
        for code in ["not_in_channel", "channel_not_found"] {
            let refusal = explain(
                &SlackError::Api {
                    method: "conversations.history".into(),
                    error: code.into(),
                },
                "C1",
            );
            assert!(refusal.contains("not a member") || refusal.contains("no access"));
            assert!(refusal.contains("It is not empty"));
            assert!(refusal.contains("invite"));
        }
    }

    /// A code we have not met keeps Slack's own word for it rather than a guess at what it meant.
    #[test]
    fn an_unknown_slack_refusal_keeps_its_own_wording() {
        let refusal = explain(
            &SlackError::Api {
                method: "conversations.history".into(),
                error: "ratelimited_forever".into(),
            },
            "C1",
        );
        assert!(refusal.contains("ratelimited_forever"));
        assert!(refusal.contains("Nothing was read"));
    }

    #[test]
    fn a_missing_message_says_so_instead_of_returning_nothing() {
        let target = Target {
            channel: "C1".into(),
            ts: "1700000000.123456".into(),
            thread_ts: None,
        };
        let gone = vanished(&target);
        assert!(gone.contains("do not treat this as an empty message"));
    }

    #[test]
    fn a_rendered_thread_keeps_slack_markup_and_counts_its_messages() {
        let messages = vec![
            Message {
                ts: "1700000000.000100".into(),
                text: "ping <@U123>".into(),
                user: Some("U1".into()),
                bot_id: None,
                subtype: None,
                thread_ts: None,
                files: Vec::new(),
            },
            Message {
                ts: "1700000000.000200".into(),
                text: String::new(),
                user: None,
                bot_id: Some("B9".into()),
                subtype: None,
                thread_ts: Some("1700000000.000100".into()),
                files: Vec::new(),
            },
        ];
        let rendered = render_thread("C1", &messages);
        assert!(rendered.contains("2 messages"));
        assert!(rendered.contains("ping <@U123>"));
        assert!(rendered.contains("From U1:"));
        assert!(rendered.contains("From B9:"));
        // An empty body says why it is empty, so it is not mistaken for a read that lost the text.
        assert!(rendered.contains("no text"));
    }
}
