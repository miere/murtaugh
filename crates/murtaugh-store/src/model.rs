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

/// A credential a person's own client presents to use the gateway's nodes on their behalf. Unlike
/// a node token it runs nothing: anyone allowed on the gateway may hold one, and it stops working
/// when they stop being allowed. Its scopes say which of the gateway's entry points it opens.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UserToken {
    pub selector: String,
    pub secret_hash: String,
    pub owner: UserId,
    pub name: String,
    pub created_at: OffsetDateTime,
    pub revoked_at: Option<OffsetDateTime>,
    /// Never empty. Each entry point checks for its own scope and ignores the rest.
    pub scopes: BTreeSet<Scope>,
}

impl UserToken {
    pub fn allows(&self, scope: Scope) -> bool {
        self.scopes.contains(&scope)
    }
}

/// An entry point of the gateway a user token may open. Named for what the gateway exposes, never
/// for the client on the other end: anything may speak RAX.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum Scope {
    /// Dialling the RAX API as a gateway, to run sessions on the gateway's nodes.
    Rax,
}

impl Scope {
    pub const ALL: [Scope; 1] = [Scope::Rax];

    /// What a token minted before scopes existed may do, which is exactly what it always could.
    pub fn legacy() -> BTreeSet<Scope> {
        BTreeSet::from([Scope::Rax])
    }

    pub fn as_str(self) -> &'static str {
        match self {
            Scope::Rax => "rax",
        }
    }

    pub fn describe(self) -> &'static str {
        match self {
            Scope::Rax => "RAX API",
        }
    }

    /// Stored and shown comma-separated, in a stable order.
    pub fn join(scopes: &BTreeSet<Scope>) -> String {
        scopes
            .iter()
            .map(|scope| scope.as_str())
            .collect::<Vec<_>>()
            .join(",")
    }

    /// The inverse of [`Scope::join`]. An empty list is an error: a token must open something.
    pub fn split(raw: &str) -> Result<BTreeSet<Scope>, ScopeError> {
        let scopes = raw
            .split(',')
            .map(str::trim)
            .filter(|part| !part.is_empty())
            .map(str::parse)
            .collect::<Result<BTreeSet<_>, _>>()?;
        if scopes.is_empty() {
            return Err(ScopeError(raw.to_owned()));
        }
        Ok(scopes)
    }
}

impl fmt::Display for Scope {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

#[derive(Debug, thiserror::Error)]
#[error("{0:?} is not a token scope; use rax")]
pub struct ScopeError(pub String);

impl FromStr for Scope {
    type Err = ScopeError;

    fn from_str(raw: &str) -> Result<Self, Self::Err> {
        match raw.trim() {
            "rax" => Ok(Scope::Rax),
            other => Err(ScopeError(other.to_owned())),
        }
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

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]

    use super::*;

    #[test]
    fn scopes_are_a_non_empty_set_stored_in_a_stable_order() {
        let scopes = Scope::split("rax, rax").unwrap();
        assert_eq!(scopes, Scope::legacy());
        assert_eq!(Scope::join(&scopes), "rax");
        assert!(Scope::split("").is_err());
        assert!(Scope::split("rax,admin").is_err());
    }
}
