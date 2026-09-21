//! The menu `/node` shows, and the reading of what it sends back.

use murtaugh_slack::Block;
use murtaugh_store::Conversation;
use serde_json::json;

use crate::fleet::Summary;

pub const CHOOSE: &str = "node_choose";
const BLOCK_ID: &str = "murtaugh_node_choices";
pub const PROMPT: &str = "Where should we run this workload?";

/// The menu is ephemeral, so the payload it sends back names no thread. Each option carries the
/// conversation it was offered for, which is the only thing that survives the round trip.
pub fn view(conversation: &Conversation, choices: &[Summary]) -> Vec<Block> {
    let options: Vec<_> = choices
        .iter()
        .map(|choice| {
            json!({
                "text": {"type": "plain_text", "text": choice.name},
                "value": value(conversation, &choice.selector),
            })
        })
        .collect();
    vec![
        Block::Raw(json!({
            "type": "header",
            "text": {"type": "plain_text", "text": PROMPT, "emoji": false},
        })),
        Block::Raw(json!({
            "type": "actions",
            "block_id": BLOCK_ID,
            "elements": [{
                "type": "static_select",
                "action_id": CHOOSE,
                "placeholder": {"type": "plain_text", "text": "Select a node", "emoji": false},
                "options": options,
            }],
        })),
    ]
}

fn value(conversation: &Conversation, selector: &str) -> String {
    format!(
        "{}:{}:{selector}",
        conversation.channel, conversation.thread_ts
    )
}

/// The conversation and node an option stands for. Neither part may hold a colon: a channel id and
/// a selector are alphanumeric, and a timestamp separates its halves with a dot.
pub fn chosen(value: &str) -> Option<(Conversation, String)> {
    let mut parts = value.splitn(3, ':');
    let channel = parts.next().filter(|part| !part.is_empty())?;
    let thread_ts = parts.next().filter(|part| !part.is_empty())?;
    let selector = parts.next().filter(|part| !part.is_empty())?;
    Some((
        Conversation {
            channel: channel.to_owned(),
            thread_ts: thread_ts.to_owned(),
        },
        selector.to_owned(),
    ))
}

#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod tests {
    use super::*;
    use murtaugh_store::UserId;

    fn conversation() -> Conversation {
        Conversation {
            channel: "C0GENERAL".into(),
            thread_ts: "1700000000.000100".into(),
        }
    }

    fn summary(name: &str, selector: &str) -> Summary {
        Summary {
            selector: selector.into(),
            owner: UserId::parse("U0ALICE01").unwrap(),
            name: name.into(),
            sessions: 0,
            connected: true,
        }
    }

    #[test]
    fn an_option_names_a_node_and_carries_the_thread_back() {
        let blocks = view(
            &conversation(),
            &[summary("Hangar", "a1b2"), summary("MacBook", "c3d4")],
        );
        let json = Block::Raw(json!(blocks.iter().map(Block::to_json).collect::<Vec<_>>()))
            .to_json()
            .to_string();
        assert!(json.contains(PROMPT), "{json}");
        assert!(
            json.contains("\"Hangar\"") && json.contains("\"MacBook\""),
            "{json}"
        );

        let options = blocks[1].to_json()["elements"][0]["options"].clone();
        let first = options[0]["value"].as_str().unwrap().to_owned();
        assert_eq!(chosen(&first), Some((conversation(), "a1b2".to_owned())));
    }

    #[test]
    fn a_value_that_is_not_three_parts_chooses_nothing() {
        assert_eq!(chosen(""), None);
        assert_eq!(chosen("C0GENERAL"), None);
        assert_eq!(chosen("C0GENERAL:1700000000.000100"), None);
        assert_eq!(chosen("C0GENERAL:1700000000.000100:"), None);
        assert_eq!(chosen(":1700000000.000100:a1b2"), None);
    }

    /// Slack refuses an option whose value runs past 75 characters, which would take the picker
    /// down for every thread at once rather than for one node.
    #[test]
    fn an_option_value_fits_what_slack_accepts() {
        let selector = "0123456789abcdef";
        let value = value(&conversation(), selector);
        assert!(value.len() <= 75, "{} characters: {value}", value.len());
    }
}
