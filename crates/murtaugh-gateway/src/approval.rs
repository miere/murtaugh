//! Tool approval for owners in `allowed-whitelist` mode: a card in the thread, a decision only the
//! node's owner can make, and a denial once the timeout passes. Ported from the Go gateway's
//! approval card; approving with "Always allow" puts the tool on the owner's whitelist.

use std::collections::HashMap;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use murtaugh_slack::{Block, Click, SlackClient, UpdateMessage};
use murtaugh_store::{Store, UserId};
use rax::tool::{Decision, DeniedBy, ToolCall, ToolKind, ToolVerdict};
use rax_tokio::gateway::GatewayLink;
use serde_json::{Value, json};
use tokio::sync::oneshot;
use tokio_util::sync::CancellationToken;

pub const TIMEOUT: Duration = Duration::from_secs(30 * 60);
pub const ALLOW_ONCE: &str = "tool_approval_once";
pub const ALLOW_ALWAYS: &str = "tool_approval_always";
pub const DENY: &str = "tool_approval_deny";
const BLOCK_ID: &str = "murtaugh_approval_card";
const ACTIONS_BLOCK_ID: &str = "murtaugh_approval_actions";
const DETAIL_LINES: usize = 20;
const DETAIL_CHARS: usize = 2_500;
const BUTTON_CHARS: usize = 75;
const ICON_PENDING: &str = "https://img.icons8.com/external-flaticons-lineal-color-flat-icons/64/external-protocols-back-to-work-flaticons-lineal-color-flat-icons.png";
const ICON_REFUSED: &str = "https://img.icons8.com/external-flaticons-lineal-color-flat-icons/64/external-warning-winter-season-flaticons-lineal-color-flat-icons-2.png";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Choice {
    Once,
    Always,
    Deny,
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum Outcome {
    Approved { by: String },
    AlwaysAllowed { by: String },
    Denied { by: String },
    TimedOut,
    Dismissed,
}

struct Waiting {
    owner: UserId,
    decide: oneshot::Sender<(Choice, String)>,
}

/// The cards still waiting for a click, by the id their buttons carry.
#[derive(Clone, Default)]
pub struct Approvals {
    waiting: Arc<Mutex<HashMap<String, Waiting>>>,
}

/// Everything one approval needs, owned so it can wait in its own task while the turn goes on.
pub struct Ask {
    pub slack: SlackClient,
    pub store: Arc<dyn Store>,
    pub link: GatewayLink,
    pub node_name: String,
    pub owner: UserId,
    pub channel: String,
    /// `None` posts the card on its own, such as in the owner's DM.
    pub thread_ts: Option<String>,
    /// Who the agent is working for, when that is not plain from where the card is posted.
    pub requester: Option<String>,
    pub tool: ToolCall,
    pub timeout: Duration,
    /// Cancelled when the turn ends, which dismisses a card nobody answered.
    pub turn: CancellationToken,
}

impl Approvals {
    fn waiting(&self) -> MutexGuard<'_, HashMap<String, Waiting>> {
        self.waiting
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Posts the card, waits for the owner, a timeout or the end of the turn, then rules on the
    /// call and settles the card.
    pub async fn ask(&self, ask: Ask) {
        let id = hex::encode(rand::random::<[u8; 8]>());
        let card = Card::of(&ask);
        let (decide, decided) = oneshot::channel();
        self.waiting().insert(
            id.clone(),
            Waiting {
                owner: ask.owner.clone(),
                decide,
            },
        );
        let posted = ask
            .slack
            .post_message(&murtaugh_slack::PostMessage {
                channel: ask.channel.clone(),
                thread_ts: ask.thread_ts.clone(),
                text: card.fallback(),
                blocks: vec![card.pending(&id, &ask.owner, ask.timeout)],
            })
            .await;
        let outcome = match &posted {
            Err(err) => {
                tracing::warn!(error = %err, "could not post an approval card; denying the tool");
                None
            }
            Ok(_) => Some(tokio::select! {
                decided = decided => match decided {
                    Ok((Choice::Once, by)) => Outcome::Approved { by },
                    Ok((Choice::Always, by)) => Outcome::AlwaysAllowed { by },
                    Ok((Choice::Deny, by)) => Outcome::Denied { by },
                    Err(_) => Outcome::Dismissed,
                },
                () = tokio::time::sleep(ask.timeout) => Outcome::TimedOut,
                () = ask.turn.cancelled() => Outcome::Dismissed,
            }),
        };
        self.waiting().remove(&id);
        let decision = match &outcome {
            Some(Outcome::Approved { .. } | Outcome::AlwaysAllowed { .. }) => Decision::Allow,
            Some(Outcome::Denied { .. }) => denied(DeniedBy::User, "The node's owner denied it."),
            Some(Outcome::TimedOut) => denied(
                DeniedBy::Timeout,
                "Nobody approved it in time, so it was not run.",
            ),
            Some(Outcome::Dismissed) | None => denied(
                DeniedBy::Unavailable,
                "It could not be put to the node's owner.",
            ),
        };
        if let Some(Outcome::AlwaysAllowed { .. }) = &outcome
            && let Err(err) = ask.store.whitelist_tool(&ask.owner, &ask.tool.name).await
        {
            tracing::warn!(error = %err, tool = %ask.tool.name, "could not whitelist a tool");
        }
        let verdict = ToolVerdict {
            id: ask.tool.id.clone(),
            decision,
        };
        if let Err(err) = ask.link.verdict(verdict).await {
            tracing::warn!(error = %err, "could not rule on a tool call");
        }
        if let (Some(outcome), Ok(posted)) = (outcome, posted) {
            let settled = UpdateMessage {
                channel: posted.channel,
                ts: posted.ts,
                text: card.fallback(),
                blocks: vec![card.settled(&outcome, &ask.owner, ask.timeout)],
            };
            if let Err(err) = ask.slack.update_message(&settled).await {
                tracing::warn!(error = %err, "could not settle an approval card");
            }
        }
    }

    /// A click on one of the cards' buttons. Returns the note to show the clicker alone, if any.
    pub fn click(&self, click: &Click) -> Option<String> {
        let choice = match click.action_id.as_str() {
            ALLOW_ONCE => Choice::Once,
            ALLOW_ALWAYS => Choice::Always,
            DENY => Choice::Deny,
            _ => return None,
        };
        let mut waiting = self.waiting();
        let Some(card) = waiting.get(&click.value) else {
            return Some("That approval is already settled.".into());
        };
        if card.owner.as_str() != click.user {
            return Some(format!("Only <@{}> can decide this one.", card.owner));
        }
        if let Some(card) = waiting.remove(&click.value) {
            let _ = card.decide.send((choice, click.user.clone()));
        }
        None
    }
}

fn denied(by: DeniedBy, reason: &str) -> Decision {
    Decision::Deny {
        by,
        reason: Some(reason.to_owned()),
    }
}

/// What the card shows about the call, whatever state the card is in.
struct Card {
    tool: String,
    node: String,
    requester: Option<String>,
    detail: Option<String>,
    language: Option<&'static str>,
}

impl Card {
    fn of(ask: &Ask) -> Card {
        let command = ask
            .tool
            .input
            .as_ref()
            .and_then(|input| input["command"].as_str())
            .map(str::to_owned);
        let execute = command.is_some() || ask.tool.kind == ToolKind::Execute;
        let detail = command
            .or_else(|| {
                ask.tool
                    .input
                    .as_ref()
                    .filter(|input| !matches!(input, Value::Null))
                    .and_then(|input| serde_json::to_string_pretty(input).ok())
            })
            .or_else(|| ask.tool.title.clone())
            .map(|text| clip(&text))
            .filter(|text| !text.trim().is_empty());
        Card {
            tool: ask.tool.name.clone(),
            node: ask.node_name.clone(),
            requester: ask.requester.clone(),
            detail,
            language: execute.then_some("bash"),
        }
    }

    fn fallback(&self) -> String {
        format!("Approval needed for the '{}' tool", self.tool)
    }

    fn pending(&self, id: &str, owner: &UserId, timeout: Duration) -> Block {
        let always = clip_to(&format!("Always allow {}", self.tool), BUTTON_CHARS);
        let buttons = json!({
            "type": "actions",
            "block_id": ACTIONS_BLOCK_ID,
            "elements": [
                button(ALLOW_ONCE, id, "Approve", Some("primary")),
                button(ALLOW_ALWAYS, id, &always, None),
                button(DENY, id, "Deny", Some("danger")),
            ],
        });
        let footer = format!(
            "Waiting on <@{owner}>'s approval. Denied automatically in {}.",
            span(timeout)
        );
        self.container(
            "Approval Needed",
            &match &self.requester {
                Some(requester) => format!(
                    "The agent on {}, working for {requester}, wants to use the '{}' tool",
                    self.node, self.tool
                ),
                None => format!(
                    "The agent on {} wants to use the '{}' tool",
                    self.node, self.tool
                ),
            },
            ICON_PENDING,
            false,
            [context(&footer), buttons],
        )
    }

    fn settled(&self, outcome: &Outcome, owner: &UserId, timeout: Duration) -> Block {
        let tool = &self.tool;
        let (title, subtitle, footer, icon) = match outcome {
            Outcome::Approved { by } => (
                "Approved",
                format!("The agent ran the '{tool}' tool"),
                format!("Approved by <@{by}>."),
                ICON_PENDING,
            ),
            Outcome::AlwaysAllowed { by } => (
                "Always Allowed",
                format!("The agent ran the '{tool}' tool"),
                format!(
                    "Approved by <@{by}>, who added *{tool}* to their whitelist. It won't ask again."
                ),
                ICON_PENDING,
            ),
            Outcome::Denied { by } => (
                "Denied",
                format!("The agent was not allowed to use the '{tool}' tool"),
                format!("Denied by <@{by}>."),
                ICON_REFUSED,
            ),
            Outcome::TimedOut => (
                "Approval Timed Out",
                format!("Nobody answered, so the agent did not use the '{tool}' tool"),
                format!(
                    "No answer from <@{owner}> in {} — the '{tool}' tool was not run.",
                    span(timeout)
                ),
                ICON_REFUSED,
            ),
            Outcome::Dismissed => (
                "Approval Dismissed",
                format!("The turn ended first, so the agent did not use the '{tool}' tool"),
                format!("Dismissed before <@{owner}> answered — the '{tool}' tool was not run."),
                ICON_REFUSED,
            ),
        };
        self.container(title, &subtitle, icon, true, [context(&footer)])
    }

    fn container<const N: usize>(
        &self,
        title: &str,
        subtitle: &str,
        icon: &str,
        settled: bool,
        tail: [Value; N],
    ) -> Block {
        let mut children = Vec::new();
        if let Some(detail) = &self.detail {
            let mut preformatted = json!({
                "type": "rich_text_preformatted",
                "elements": [{"type": "text", "text": detail}],
            });
            if let Some(language) = self.language {
                preformatted["language"] = json!(language);
            }
            children.push(json!({"type": "rich_text", "elements": [preformatted]}));
        }
        children.extend(tail);
        Block::Raw(json!({
            "type": "container",
            "block_id": BLOCK_ID,
            "icon": {"type": "image", "image_url": icon, "alt_text": "Approval icon"},
            "title": {"type": "plain_text", "text": title},
            "subtitle": {"type": "plain_text", "text": clip_to(subtitle, 3000)},
            "is_collapsible": settled,
            "default_collapsed": settled,
            "has_header_divider": !settled,
            "width": "wide",
            "child_blocks": children,
        }))
    }
}

fn button(action_id: &str, value: &str, text: &str, style: Option<&str>) -> Value {
    let mut button = json!({
        "type": "button",
        "action_id": action_id,
        "value": value,
        "text": {"type": "plain_text", "text": text},
    });
    if let Some(style) = style {
        button["style"] = json!(style);
    }
    button
}

fn context(text: &str) -> Value {
    json!({"type": "context", "elements": [{"type": "mrkdwn", "text": text}]})
}

/// Long inputs, such as a whole file an edit writes, would bury the buttons.
fn clip(text: &str) -> String {
    let text = text.trim_end_matches('\n');
    let lines: Vec<&str> = text.lines().collect();
    let mut out = lines
        .iter()
        .take(DETAIL_LINES)
        .copied()
        .collect::<Vec<_>>()
        .join("\n");
    let mut cut = lines.len() > DETAIL_LINES;
    if out.chars().count() > DETAIL_CHARS {
        out = out.chars().take(DETAIL_CHARS).collect();
        cut = true;
    }
    if cut {
        out.push_str("\n…");
    }
    out
}

fn clip_to(text: &str, max: usize) -> String {
    if text.chars().count() <= max {
        return text.to_owned();
    }
    let mut out: String = text.chars().take(max - 1).collect();
    out.push('…');
    out
}

fn span(duration: Duration) -> String {
    let minutes = duration.as_secs() / 60;
    match minutes {
        0 => format!("{} seconds", duration.as_secs()),
        1 => "1 minute".to_owned(),
        _ => format!("{minutes} minutes"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_long_input_is_cut_to_twenty_lines() {
        let long: String = (1..=30).map(|n| format!("line {n}\n")).collect();
        let clipped = clip(&long);
        assert_eq!(clipped.lines().count(), 21);
        assert!(clipped.ends_with("line 20\n…"));
        assert_eq!(clip("ls -la\n"), "ls -la");
    }

    #[test]
    fn the_timeout_reads_naturally() {
        assert_eq!(span(TIMEOUT), "30 minutes");
        assert_eq!(span(Duration::from_secs(60)), "1 minute");
        assert_eq!(span(Duration::from_millis(500)), "0 seconds");
    }
}
