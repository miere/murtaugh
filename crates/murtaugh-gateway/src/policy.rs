//! How a node owner's tool rules decide a call, wherever the call is shown: in a Slack thread or
//! in an editor bridged over RAX. Only the asking differs, so both ask this first.

use murtaugh_store::{Store, ToolMode, UserConfig, UserId};
use rax::tool::{Decision, DeniedBy};

/// The tools for reaching the people in the conversation. They do nothing to the owner's
/// machine, so asking the owner's permission to ask them a question helps nobody.
const TALKING_TOOLS: [&str; 4] = [
    "mcp__riggs__ask",
    "mcp__riggs__present_plan",
    "mcp__riggs__auth",
    // Lent by this gateway, in its `slack` group.
    "mcp__slack__attach",
];

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Ruling {
    /// Decided without anyone: send this verdict now.
    Decided(Decision),
    /// The owner's rules leave it to a person.
    Ask,
}

/// The owner's tool rules, or rules that ask them when the store cannot say: a call is never
/// allowed just because the rules could not be read.
pub async fn rules(store: &dyn Store, owner: &UserId) -> UserConfig {
    match store.user(owner).await {
        Ok(config) => config,
        Err(err) => {
            tracing::warn!(error = %err, "could not read the owner's tool rules; asking them");
            UserConfig {
                tool_mode: ToolMode::AllowedWhitelist,
                ..UserConfig::new(owner.clone())
            }
        }
    }
}

pub fn rule(config: &UserConfig, tool: &str) -> Ruling {
    match config.tool_mode {
        _ if talks_to_people(tool) => Ruling::Decided(Decision::Allow),
        ToolMode::AlwaysAllowed => Ruling::Decided(Decision::Allow),
        ToolMode::AllowedWhitelist if config.whitelist.contains(tool) => {
            Ruling::Decided(Decision::Allow)
        }
        ToolMode::Denied => Ruling::Decided(Decision::Deny {
            by: DeniedBy::Policy,
            reason: Some("The node's owner does not allow tools.".into()),
        }),
        ToolMode::AllowedWhitelist => Ruling::Ask,
    }
}

fn talks_to_people(tool: &str) -> bool {
    TALKING_TOOLS.contains(&tool)
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]

    use super::*;

    fn config(mode: ToolMode, whitelist: &[&str]) -> UserConfig {
        UserConfig {
            tool_mode: mode,
            whitelist: whitelist.iter().map(|tool| (*tool).to_owned()).collect(),
            ..UserConfig::new(UserId::parse("U0OWNER01").unwrap())
        }
    }

    #[test]
    fn the_owners_mode_decides_and_only_the_whitelist_mode_asks() {
        let allow = Ruling::Decided(Decision::Allow);
        assert_eq!(rule(&config(ToolMode::AlwaysAllowed, &[]), "Bash"), allow);
        assert!(matches!(
            rule(&config(ToolMode::Denied, &[]), "Bash"),
            Ruling::Decided(Decision::Deny { .. })
        ));
        let whitelisted = config(ToolMode::AllowedWhitelist, &["Read"]);
        assert_eq!(rule(&whitelisted, "Read"), allow);
        assert_eq!(rule(&whitelisted, "Bash"), Ruling::Ask);
    }

    #[test]
    fn talking_to_people_is_never_held_back() {
        assert_eq!(
            rule(&config(ToolMode::Denied, &[]), "mcp__riggs__ask"),
            Ruling::Decided(Decision::Allow)
        );
    }
}
