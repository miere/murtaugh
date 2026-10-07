use std::collections::BTreeSet;
use std::fmt;
use std::str::FromStr;

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

/// Settings that belong to one person. `allowed` pre-authorises them on the admin's nodes; the
/// tool mode and whitelist govern the tools agents use on the nodes they own.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UserConfig {
    pub user: UserId,
    pub allowed: bool,
    pub tool_mode: ToolMode,
    pub whitelist: BTreeSet<String>,
}

impl UserConfig {
    pub fn new(user: UserId) -> Self {
        Self {
            user,
            allowed: false,
            tool_mode: ToolMode::default(),
            whitelist: BTreeSet::new(),
        }
    }
}

/// How a node owner's tools are ruled on. Under `AllowedWhitelist`, a tool off the whitelist
/// waits for the owner's approval.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Hash)]
pub enum ToolMode {
    #[default]
    AlwaysAllowed,
    AllowedWhitelist,
    Denied,
}

impl ToolMode {
    pub const ALL: [ToolMode; 3] = [
        ToolMode::AlwaysAllowed,
        ToolMode::AllowedWhitelist,
        ToolMode::Denied,
    ];

    pub fn as_str(self) -> &'static str {
        match self {
            ToolMode::AlwaysAllowed => "always-allowed",
            ToolMode::AllowedWhitelist => "allowed-whitelist",
            ToolMode::Denied => "denied",
        }
    }
}

impl fmt::Display for ToolMode {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

#[derive(Debug, thiserror::Error)]
#[error("{0:?} is not a tool mode; use always-allowed, allowed-whitelist or denied")]
pub struct ToolModeError(pub String);

impl FromStr for ToolMode {
    type Err = ToolModeError;

    fn from_str(raw: &str) -> Result<Self, Self::Err> {
        ToolMode::ALL
            .into_iter()
            .find(|mode| mode.as_str() == raw.trim())
            .ok_or_else(|| ToolModeError(raw.to_owned()))
    }
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
    /// Set and cleared by the node's owner (or the admin) from the Home tab; routing skips a
    /// disabled node without touching its credential or its link.
    pub disabled_at: Option<OffsetDateTime>,
}

/// A credential a person's own client presents to use the gateway's nodes over the RAX API, such as
/// an editor bridge. Unlike a node token it runs nothing: anyone allowed on the gateway may hold
/// one, and it stops working when they stop being allowed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UserToken {
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
