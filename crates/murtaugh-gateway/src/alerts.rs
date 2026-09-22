//! Alert cards, ported from the Go gateway's: a collapsible container with a level icon, a title,
//! and a body of reason, text and next steps. Used for a machine that cannot take a conversation,
//! for a machine that fails one of its messages ([`crate::faults`] builds those), and for a node's
//! failing credential, which goes to the node's owner by DM and is edited in place when it
//! recovers.

use std::collections::HashMap;
use std::sync::{Arc, Mutex, MutexGuard};

use murtaugh_slack::{Block, PostMessage, Posted, SlackClient, UpdateMessage};
use murtaugh_store::UserId;
use rax::credential::{CredentialHealth, CredentialRenewal};
use serde_json::{Value, json};
use time::OffsetDateTime;

pub const RENEW: &str = "credential_renew";
const ICON_ERROR: &str = "https://img.icons8.com/external-flaticons-lineal-color-flat-icons/64/external-close-button-web-flaticons-lineal-color-flat-icons.png";
const ICON_WARN: &str = "https://img.icons8.com/external-flaticons-lineal-color-flat-icons/64/external-danger-electrician-flaticons-lineal-color-flat-icons-4.png";
const ICON_INFO: &str = "https://img.icons8.com/external-flaticons-lineal-color-flat-icons/64/external-help-dating-app-flaticons-lineal-color-flat-icons-4.png";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Level {
    Error,
    Warn,
    Info,
}

#[derive(Debug, Clone, Default)]
pub struct Alert {
    pub level: Option<Level>,
    pub title: String,
    pub subtitle: Option<String>,
    pub reason: Option<String>,
    pub text: Option<String>,
    pub next_steps: Option<String>,
    /// Buttons under the body, already built.
    pub actions: Option<Value>,
}

impl Alert {
    pub fn card(&self) -> Block {
        let (icon, alt) = match self.level.unwrap_or(Level::Info) {
            Level::Error => (ICON_ERROR, "Error icon"),
            Level::Warn => (ICON_WARN, "Warning icon"),
            Level::Info => (ICON_INFO, "Notice icon"),
        };
        let mut body = Vec::new();
        if let Some(reason) = &self.reason {
            body.push(format!("*Reason*: {reason}"));
        }
        body.extend(self.text.clone());
        if let Some(next) = &self.next_steps {
            body.push(format!("*Next Steps*: {next}"));
        }
        let mut children = Vec::new();
        if !body.is_empty() {
            children.push(json!({
                "type": "section",
                "text": {"type": "mrkdwn", "text": clip(&body.join("\n\n"), 3000)},
            }));
        }
        children.extend(self.actions.clone());
        let mut card = json!({
            "type": "container",
            "block_id": "murtaugh_alert_card",
            "icon": {"type": "image", "image_url": icon, "alt_text": alt},
            "title": {"type": "plain_text", "text": clip(&self.title, 150)},
            "is_collapsible": true,
            "default_collapsed": self.actions.is_none(),
            "has_header_divider": false,
            "width": "wide",
            "child_blocks": children,
        });
        if let Some(subtitle) = &self.subtitle {
            card["subtitle"] = json!({"type": "plain_text", "text": clip(subtitle, 3000)});
        }
        Block::Raw(card)
    }

    pub fn message(&self, channel: &str, thread_ts: Option<&str>) -> PostMessage {
        PostMessage {
            channel: channel.to_owned(),
            thread_ts: thread_ts.map(str::to_owned),
            text: self.title.clone(),
            blocks: vec![self.card()],
        }
    }
}

/// The node a report came from, as the owner knows it.
pub struct Reporter<'a> {
    pub selector: &'a str,
    pub name: &'a str,
    pub owner: &'a UserId,
}

struct Open {
    posted: Posted,
    since: OffsetDateTime,
}

/// Failing credentials each node has reported, and the DM card that says so.
#[derive(Clone, Default)]
pub struct Credentials {
    slack: Option<SlackClient>,
    open: Arc<Mutex<HashMap<(String, String), Open>>>,
}

impl Credentials {
    pub fn new(slack: SlackClient) -> Self {
        Self {
            slack: Some(slack),
            open: Arc::default(),
        }
    }

    fn open(&self) -> MutexGuard<'_, HashMap<(String, String), Open>> {
        self.open
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Tells the owner when a credential starts failing and again when it recovers; a report that
    /// changes nothing only refreshes the open card.
    pub async fn report(&self, from: Reporter<'_>, health: &CredentialHealth) {
        let Some(slack) = &self.slack else {
            return;
        };
        let key = (from.selector.to_owned(), health.credential.clone());
        let open = self.open().remove(&key);
        let result = match (health.degraded, open) {
            (true, Some(open)) => {
                let alert = failing(&from, health);
                slack
                    .update_message(&update(&open.posted, &alert))
                    .await
                    .map(|()| Some(open))
            }
            (true, None) => {
                let alert = failing(&from, health);
                slack
                    .post_message(&alert.message(from.owner.as_str(), None))
                    .await
                    .map(|posted| {
                        Some(Open {
                            posted,
                            since: health.since.unwrap_or_else(OffsetDateTime::now_utc),
                        })
                    })
            }
            (false, Some(open)) => {
                let alert = recovered(&from, health, open.since);
                slack
                    .update_message(&update(&open.posted, &alert))
                    .await
                    .map(|()| None)
            }
            (false, None) => Ok(None),
        };
        match result {
            Ok(Some(open)) => {
                self.open().insert(key, open);
            }
            Ok(None) => {}
            Err(err) => {
                tracing::warn!(error = %err, node = %from.name, "could not tell the owner about a credential");
            }
        }
    }
}

fn update(posted: &Posted, alert: &Alert) -> UpdateMessage {
    UpdateMessage {
        channel: posted.channel.clone(),
        ts: posted.ts.clone(),
        text: alert.title.clone(),
        blocks: vec![alert.card()],
    }
}

fn failing(from: &Reporter<'_>, health: &CredentialHealth) -> Alert {
    let text = match health.expires_at {
        Some(at) => format!("Last observed expiry: {}.", relative(at)),
        None => "The node has never been able to read this credential's expiry.".to_owned(),
    };
    Alert {
        level: Some(Level::Error),
        title: format!("A credential on {} is failing", from.name),
        subtitle: Some(health.credential.clone()),
        reason: health.reason.clone(),
        text: Some(text),
        next_steps: Some(
            "Sign in again from here; the agent on this machine cannot work until you do.".into(),
        ),
        actions: Some(json!({
            "type": "actions",
            "block_id": "murtaugh_credential_actions",
            "elements": [{
                "type": "button",
                "action_id": RENEW,
                "value": from.selector,
                "style": "primary",
                "text": {"type": "plain_text", "text": "Sign In Again"},
            }],
        })),
    }
}

fn recovered(from: &Reporter<'_>, health: &CredentialHealth, since: OffsetDateTime) -> Alert {
    let failing_for = OffsetDateTime::now_utc() - since;
    Alert {
        level: Some(Level::Info),
        title: format!("A credential on {} recovered", from.name),
        subtitle: Some(health.credential.clone()),
        text: Some(format!(
            "It was failing for {}.",
            span(failing_for.whole_seconds())
        )),
        next_steps: health
            .expires_at
            .map(|at| format!("Expiry: {}.", relative(at))),
        ..Alert::default()
    }
}

/// What to tell the owner after asking the node to renew.
pub fn renewal_note(node: &str, renewal: CredentialRenewal) -> String {
    match renewal {
        CredentialRenewal::Started => {
            format!("*{node}* started a sign-in; its card will arrive here.")
        }
        CredentialRenewal::AlreadyRunning => {
            format!("*{node}* already has a sign-in running; finish that one.")
        }
        CredentialRenewal::NothingToRenew => format!("*{node}* has nothing to sign in to."),
    }
}

fn relative(at: OffsetDateTime) -> String {
    let seconds = (at - OffsetDateTime::now_utc()).whole_seconds();
    if seconds < 0 {
        format!("{} ago (lapsed)", span(-seconds))
    } else {
        format!("in {}", span(seconds))
    }
}

fn span(seconds: i64) -> String {
    match seconds {
        s if s < 60 => format!("{s}s"),
        s if s < 3600 => format!("{}m", s / 60),
        s if s < 86_400 => format!("{}h {}m", s / 3600, s % 3600 / 60),
        s => format!("{}d {}h", s / 86_400, s % 86_400 / 3600),
    }
}

pub(crate) fn clip(text: &str, max: usize) -> String {
    if text.chars().count() <= max {
        return text.to_owned();
    }
    let mut out: String = text.chars().take(max.saturating_sub(1)).collect();
    out.push('…');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn spans_read_like_an_operator_would_say_them() {
        assert_eq!(span(42), "42s");
        assert_eq!(span(125), "2m");
        assert_eq!(span(3 * 3600 + 120), "3h 2m");
        assert_eq!(span(2 * 86_400 + 3600), "2d 1h");
    }
}
