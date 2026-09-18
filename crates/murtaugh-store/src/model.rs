use std::fmt;

use serde::{Deserialize, Serialize};
use time::OffsetDateTime;

/// A Slack user id. Only `U…` and `W…` ids parse, so a channel or bot id can never be granted.
#[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(try_from = "String", into = "String")]
pub struct UserId(String);

#[derive(Debug, thiserror::Error)]
#[error("{0:?} is not a Slack user id; they look like U0123ABCD")]
pub struct UserIdError(pub String);

impl UserId {
    pub fn parse(raw: &str) -> Result<Self, UserIdError> {
        let raw = raw.trim();
        let valid = raw.len() >= 9
            && matches!(raw.as_bytes()[0], b'U' | b'W')
            && raw
                .bytes()
                .all(|byte| byte.is_ascii_uppercase() || byte.is_ascii_digit());
        if valid {
            Ok(Self(raw.to_owned()))
        } else {
            Err(UserIdError(raw.to_owned()))
        }
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl fmt::Display for UserId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl TryFrom<String> for UserId {
    type Error = UserIdError;

    fn try_from(raw: String) -> Result<Self, Self::Error> {
        Self::parse(&raw)
    }
}

impl From<UserId> for String {
    fn from(user: UserId) -> Self {
        user.0
    }
}

/// Lets a person run their own nodes on this gateway. Every node they run shares it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Grant {
    pub user: UserId,
    pub approved_by: UserId,
    pub approved_at: OffsetDateTime,
}

/// Settings that belong to one person. `allowed` pre-authorises them on the admin's nodes.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UserConfig {
    pub user: UserId,
    pub allowed: bool,
}

/// Only the secret's hash is kept, so a leaked database cannot be replayed as a node.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NodeToken {
    pub selector: String,
    pub secret_hash: String,
    pub owner: UserId,
    pub name: String,
    pub created_at: OffsetDateTime,
    pub revoked_at: Option<OffsetDateTime>,
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
