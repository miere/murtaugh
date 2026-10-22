use std::sync::Arc;

use rax_gateway::Principals;
use rax_gateway::store::{UserId, UserIdError};
use time::OffsetDateTime;

/// Murtaugh's people are Slack users. Only `U…` and `W…` ids parse, so a channel or bot id can
/// never be granted.
#[derive(Debug, Clone, Copy, Default)]
pub struct SlackIds;

impl Principals for SlackIds {
    fn noun(&self) -> &str {
        "Slack user ID"
    }

    fn parse(&self, raw: &str) -> Result<UserId, UserIdError> {
        slack_user(raw)
    }
}

pub fn slack_ids() -> Arc<dyn Principals> {
    Arc::new(SlackIds)
}

pub fn slack_user(raw: &str) -> Result<UserId, UserIdError> {
    let raw = raw.trim();
    let valid = raw.len() >= 9
        && matches!(raw.as_bytes()[0], b'U' | b'W')
        && raw
            .bytes()
            .all(|byte| byte.is_ascii_uppercase() || byte.is_ascii_digit());
    if valid {
        UserId::parse(raw)
    } else {
        Err(UserIdError::new(
            raw,
            format!("{raw:?} is not a Slack user id; they look like U0123ABCD"),
        ))
    }
}

/// A Slack thread; a top-level message is a thread of its own ts.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct Conversation {
    pub channel: String,
    pub thread_ts: String,
}

/// Fixed when the conversation starts, and dropped when its node goes away.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Pin {
    pub conversation: Conversation,
    pub node: String,
    pub session_id: String,
    pub user: UserId,
    pub pinned_at: OffsetDateTime,
}
