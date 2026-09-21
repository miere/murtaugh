//! Thread-scoped slash commands. Slack will not run one of its own slash commands inside a
//! thread, so a verb that only means something in a thread is typed as a mention instead.

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Command {
    Stop,
}

/// Every verb the gateway claims; anything else is left for the node.
const COMMANDS: &[Command] = &[Command::Stop];

impl Command {
    pub fn verb(self) -> &'static str {
        match self {
            Command::Stop => "stop",
        }
    }
}

impl std::fmt::Display for Command {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "/{}", self.verb())
    }
}

/// The mention is optional because a direct message has none to give, and nothing may follow the
/// verb, which leaves `/stop the deploy` a sentence and backticks a way to reach the node.
pub fn parse(text: &str, bot_user: &str) -> Option<Command> {
    let body = without_leading_mention(text.trim(), bot_user).trim();
    let verb = body.strip_prefix('/')?;
    if verb.is_empty() || verb.contains(char::is_whitespace) {
        return None;
    }
    COMMANDS
        .iter()
        .copied()
        .find(|command| verb.eq_ignore_ascii_case(command.verb()))
}

/// Slack also writes the older `<@ID|name>` form.
fn without_leading_mention<'a>(text: &'a str, bot_user: &str) -> &'a str {
    let Some(rest) = text.strip_prefix("<@") else {
        return text;
    };
    let Some(close) = rest.find('>') else {
        return text;
    };
    let (mention, after) = rest.split_at(close);
    if mention.split('|').next() == Some(bot_user) {
        &after[1..]
    } else {
        text
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const BOT: &str = "U0MURTAUGH";

    fn read(text: &str) -> Option<Command> {
        parse(text, BOT)
    }

    #[test]
    fn a_mention_and_nothing_but_a_verb_is_a_command() {
        assert_eq!(read("<@U0MURTAUGH> /stop"), Some(Command::Stop));
        assert_eq!(read("  <@U0MURTAUGH>   /stop  "), Some(Command::Stop));
        assert_eq!(read("<@U0MURTAUGH> /STOP"), Some(Command::Stop));
        assert_eq!(read("<@U0MURTAUGH|murtaugh> /stop"), Some(Command::Stop));
    }

    #[test]
    fn a_direct_message_needs_no_mention_to_command() {
        assert_eq!(read("/stop"), Some(Command::Stop));
    }

    #[test]
    fn backticks_hand_the_slash_to_the_node() {
        assert_eq!(read("<@U0MURTAUGH> `/stop`"), None);
        assert_eq!(read("`/stop`"), None);
    }

    #[test]
    fn a_verb_with_anything_after_it_is_a_sentence() {
        assert_eq!(read("<@U0MURTAUGH> /stop the deploy"), None);
        assert_eq!(read("<@U0MURTAUGH> please /stop"), None);
        assert_eq!(read("<@U0MURTAUGH> /stop."), None);
    }

    #[test]
    fn a_mention_of_somebody_else_commands_nothing() {
        assert_eq!(read("<@U0ALICE01> /stop"), None);
    }

    #[test]
    fn an_unclaimed_verb_is_left_for_the_node() {
        assert_eq!(read("<@U0MURTAUGH> /code-review"), None);
        assert_eq!(read("<@U0MURTAUGH> /"), None);
        assert_eq!(read("<@U0MURTAUGH>"), None);
        assert_eq!(read("<@U0MURTAUGH> stop"), None);
    }
}
