//! How a node owner's tool rules decide a call, as `rax-gateway` rules it, counting the `slack`
//! group's `attach` as a tool that only talks to people.

use murtaugh_store::UserConfig;

pub use rax_gateway::policy::{Ruling, rules};

pub fn rule(config: &UserConfig, tool: &str) -> Ruling {
    rax_gateway::policy::rule(crate::settings(), config, tool)
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]

    use murtaugh_store::{ToolMode, slack_user};
    use rax::tool::Decision;

    use super::*;

    #[test]
    fn attaching_to_the_thread_is_never_held_back() {
        let config = UserConfig {
            tool_mode: ToolMode::Denied,
            ..UserConfig::new(slack_user("U0OWNER01").unwrap())
        };
        assert_eq!(
            rule(&config, "mcp__slack__attach"),
            Ruling::Decided(Decision::Allow)
        );
        assert!(matches!(
            rule(&config, "Bash"),
            Ruling::Decided(Decision::Deny { .. })
        ));
    }
}
