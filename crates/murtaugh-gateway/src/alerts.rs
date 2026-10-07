//! Alert cards, ported from the Go gateway's: a collapsible container with a level icon, a title,
//! and a body of reason, text and next steps. Used for a machine that cannot take a conversation
//! and for a machine that fails one of its messages ([`crate::faults`] builds those). A node's
//! failing credential gets no card: the gateway logs it, and the node's own sign-in request is
//! what reaches the owner.

use murtaugh_slack::{Block, PostMessage};
use rax::credential::CredentialRenewal;
use serde_json::{Value, json};

/// The sign-in button on the credential cards older gateways posted; those cards still sit in DMs.
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

pub(crate) fn clip(text: &str, max: usize) -> String {
    if text.chars().count() <= max {
        return text.to_owned();
    }
    let mut out: String = text.chars().take(max.saturating_sub(1)).collect();
    out.push('…');
    out
}
