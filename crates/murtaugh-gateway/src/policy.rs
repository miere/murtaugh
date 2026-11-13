//! How a node owner's tool rules decide a call, as `rax-gateway` rules it, counting the `slack`
//! group's `attach` as a tool that only talks to people.

use murtaugh_store::UserConfig;
use rax::tool::ToolCall;

pub use rax_gateway::policy::{Ruling, rules, seed};

pub fn rule(config: &UserConfig, call: &ToolCall) -> Ruling {
    rax_gateway::policy::rule(crate::settings(), config, call)
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]

    use murtaugh_store::{ToolMode, slack_user};
    use rax::id::ToolCallId;
    use rax::tool::{Decision, ToolKind};

    use super::*;

    fn call(name: &str) -> ToolCall {
        ToolCall {
            id: ToolCallId("call-1".into()),
            name: name.into(),
            title: None,
            kind: ToolKind::default(),
            input: None,
            content: Vec::new(),
        }
    }

    #[test]
    fn attaching_to_the_thread_is_never_held_back() {
        let config = UserConfig {
            tool_mode: ToolMode::Denied,
            ..UserConfig::new(slack_user("U0OWNER01").unwrap())
        };
        assert_eq!(
            rule(&config, &call("mcp__slack__attach")),
            Ruling::Decided(Decision::Allow)
        );
        assert!(matches!(
            rule(&config, &call("Bash")),
            Ruling::Decided(Decision::Deny { .. })
        ));
    }
}
