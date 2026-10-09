//! People in a prompt are written `<@ID|Name>`. Slack sends the bare `<@ID>`, which tells an
//! agent neither who was mentioned nor which of the mentions is its own, and where the bot is
//! mentioned says whether it is being spoken to or only spoken about.

use std::collections::HashMap;
use std::ops::Range;
use std::sync::Mutex;
use std::time::Duration;

use murtaugh_slack::SlackClient;
use tokio::time::Instant;

/// How long a name is trusted. People rename themselves rarely, and a stale label still carries
/// the right id.
const NAME_FRESH: Duration = Duration::from_secs(60 * 60);
/// How long a failed lookup is left alone, so a bot token without `users:read` costs one refused
/// call every few minutes rather than one per mention.
const MISS_FRESH: Duration = Duration::from_secs(5 * 60);

pub struct Names {
    slack: SlackClient,
    bot_user: String,
    /// The bot's handle from `auth.test`, which labels it when `users.info` is refused.
    bot_handle: String,
    known: Mutex<HashMap<String, Known>>,
}

struct Known {
    name: Option<String>,
    until: Instant,
}

impl Names {
    pub fn new(slack: SlackClient, bot_user: String, bot_handle: String) -> Self {
        Self {
            slack,
            bot_user,
            bot_handle,
            known: Mutex::new(HashMap::new()),
        }
    }

    /// `text` with every mention carrying the name Slack shows for it. A mention whose name cannot
    /// be read stays as Slack sent it.
    pub async fn label(&self, text: &str) -> String {
        let mut out = String::with_capacity(text.len());
        let mut copied = 0;
        for (span, user) in mentions(text) {
            let Some(name) = self.name(user).await else {
                continue;
            };
            out.push_str(&text[copied..span.start]);
            out.push_str(&labelled(user, &name));
            copied = span.end;
        }
        out.push_str(&text[copied..]);
        out
    }

    /// What tells a session who it is here. The agent knows itself by its own name, not by the
    /// one whoever installed the app gave the bot.
    pub async fn identity(&self) -> String {
        let me = match self.name(&self.bot_user).await {
            Some(name) => labelled(&self.bot_user, &name),
            None => format!("<@{}>", self.bot_user),
        };
        format!(
            "In this Slack workspace you are {me}. A mention of a person is written <@ID|Name>: {me} is you, and any other is somebody else. To mention someone yourself, write <@ID>."
        )
    }

    async fn name(&self, user: &str) -> Option<String> {
        let now = Instant::now();
        if let Some(known) = self.lock().get(user)
            && now < known.until
        {
            return known.name.clone();
        }
        let read = match self.slack.user_name(user).await {
            Ok(name) => name.as_deref().and_then(clean),
            Err(err) => {
                tracing::warn!(error = %err, user, "could not read a mentioned person's name");
                None
            }
        };
        let name = match (read, user == self.bot_user) {
            (None, true) => clean(&self.bot_handle),
            (read, _) => read,
        };
        let fresh = match name {
            Some(_) => NAME_FRESH,
            None => MISS_FRESH,
        };
        self.lock().insert(
            user.to_owned(),
            Known {
                name: name.clone(),
                until: now + fresh,
            },
        );
        name
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, HashMap<String, Known>> {
        self.known
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
}

fn labelled(user: &str, name: &str) -> String {
    format!("<@{user}|{name}>")
}

/// A name as it can sit inside a mention: people may put the characters that end one in theirs.
fn clean(name: &str) -> Option<String> {
    let name = name
        .replace(['<', '>', '|'], " ")
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ");
    (!name.is_empty()).then_some(name)
}

/// Every mention in `text` with the id it carries. Slack also writes the older `<@ID|name>` form.
fn mentions(text: &str) -> Vec<(Range<usize>, &str)> {
    let mut found = Vec::new();
    let mut from = 0;
    while let Some(open) = text[from..].find("<@").map(|at| from + at) {
        let inner = open + 2;
        let Some(close) = text[inner..].find('>').map(|at| inner + at) else {
            break;
        };
        let user = text[inner..close].split('|').next().unwrap_or_default();
        if !user.is_empty() && user.chars().all(|c| c.is_ascii_alphanumeric()) {
            found.push((open..close + 1, user));
            from = close + 1;
        } else {
            from = inner;
        }
    }
    found
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
mod tests {
    use super::*;

    fn ids(text: &str) -> Vec<(&str, &str)> {
        mentions(text)
            .into_iter()
            .map(|(span, user)| (&text[span], user))
            .collect()
    }

    #[test]
    fn a_mention_is_found_wherever_it_sits_and_in_either_form() {
        assert_eq!(
            ids("hey <@U0ALICE01> ask <@U0BOB0001|bob>."),
            vec![
                ("<@U0ALICE01>", "U0ALICE01"),
                ("<@U0BOB0001|bob>", "U0BOB0001")
            ]
        );
    }

    #[test]
    fn what_only_looks_like_a_mention_is_left_alone() {
        assert_eq!(ids("a <@> b <@not an id> c <@U0ALICE01"), vec![]);
        assert_eq!(ids("<!channel> <#C0GENERAL> <https://x.test|x>"), vec![]);
        assert_eq!(ids("<@<@U0ALICE01>"), vec![("<@U0ALICE01>", "U0ALICE01")]);
    }

    #[test]
    fn a_name_cannot_end_the_mention_it_labels() {
        assert_eq!(
            clean("  Dev | Ops <oncall>  ").as_deref(),
            Some("Dev Ops oncall")
        );
        assert_eq!(clean(" | "), None);
    }
}
