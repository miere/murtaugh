//! Turning a node's failure into a card a person can act on.
//!
//! A fault reaches the gateway with its `ErrorKind` intact: a taxonomy of what went wrong, and for
//! a provider or an RPC failure the status code and the producer's verdict on retrying. Rendering
//! one with `Display` throws all of that away and puts the transport crate's name in front of a
//! person, so the kind is mapped to a card's reason and next steps here instead, and the
//! producer's own words are kept underneath as the detail rather than as the headline.

use std::time::Duration;

use rax::error::ProviderFailure;
use rax::{Error, ErrorKind};
use rax_tokio::{CallError, SendError};

use crate::alerts::{Alert, Level, clip};
use crate::chat::span;
use crate::render;

/// Long enough to diagnose from, short enough that the reason and the next steps still fit the
/// card's one section beside it.
const DETAIL_LIMIT: usize = 1_200;

const RETRY: &str = "Send the message again.";
const SHORTEN: &str = "Send less at once, or attach the long part as a file.";
const UPDATE: &str = "Update the machine's node (`riggs`); it is likely older than this gateway.";
const ADMIN: &str = "Tell the gateway admin; this one is not yours to fix.";

/// A call that did not produce the reply it asked for.
#[derive(Debug)]
pub enum Refusal {
    /// The node faulted the call, or the link did.
    Failed(CallError),
    /// The node answered, but with a reply the call never asked for. What it sent belongs in the
    /// log: there is nothing in it a person could act on.
    Mismatched,
}

impl From<CallError> for Refusal {
    fn from(err: CallError) -> Self {
        Self::Failed(err)
    }
}

/// The message never got onto the machine.
pub fn unreachable(node: &str, err: &CallError) -> Alert {
    card(
        format!("Could not reach {node}"),
        format!("This message never reached {node}."),
        diagnose(err),
    )
}

/// The machine was reached and did not start a turn.
pub fn not_taken(node: &str, refusal: &Refusal) -> Alert {
    card(
        format!("{node} could not take this message"),
        format!("{node} answered the gateway, but no turn started."),
        refused(refusal),
    )
}

/// The machine took neither the message nor the trouble to refuse it.
pub fn silent(node: &str, waited: Duration) -> Alert {
    card(
        format!("{node} did not answer in time"),
        format!(
            "{node} had {} to take this message and said nothing.",
            span(waited)
        ),
        Told {
            level: Level::Warn,
            reason: "The machine is connected but not answering; it may be wedged or overloaded."
                .to_owned(),
            next_steps: Some("Send the message again, or `/node` the thread onto another machine."),
            detail: None,
        },
    )
}

/// The machine is already running a turn for this conversation, and a node never queues a second.
pub fn busy(node: &str) -> Alert {
    card(
        format!("{node} is still busy"),
        format!("{node} has not finished the last turn in this conversation."),
        Told {
            level: Level::Warn,
            reason: "A machine runs one turn per conversation at a time.".to_owned(),
            next_steps: Some("Wait for the answer, or `/stop` the turn and send this again."),
            detail: None,
        },
    )
}

/// The machine could not open a session for the conversation, so no message could follow.
pub fn no_session(node: &str, refusal: &Refusal) -> Alert {
    card(
        format!("{node} could not start this conversation"),
        format!("{node} never opened a session, so this message was not sent."),
        refused(refusal),
    )
}

/// The turn started, was answered into the thread, and then failed partway.
pub fn turn_failed(node: &str, error: &Error) -> Alert {
    card(
        format!("{node} could not finish this answer"),
        format!("The turn on {node} failed partway through."),
        reported(error),
    )
}

/// The one-line diagnosis on its own, for a surface with no room for a card: a slash command is
/// answered with a sentence, not a block.
pub fn line(refusal: &Refusal) -> String {
    refused(refusal).reason
}

/// What the gateway can say about a failure on its own, before the producer's words.
struct Told {
    level: Level,
    reason: String,
    next_steps: Option<&'static str>,
    /// The producer's own text, kept only when it says more than the reason does.
    detail: Option<String>,
}

fn card(title: String, subtitle: String, told: Told) -> Alert {
    Alert {
        level: Some(told.level),
        title,
        subtitle: Some(subtitle),
        reason: Some(told.reason),
        text: told.detail.as_deref().map(quote),
        next_steps: told.next_steps.map(str::to_owned),
        actions: None,
    }
}

/// The producer's own words, fenced: raw failure text is not mrkdwn, and a fence stops a stray
/// `*` or `<@…>` in it from rendering as one.
fn quote(text: &str) -> String {
    format!("```\n{}\n```", render::escape(&clip(text, DETAIL_LIMIT)))
}

fn refused(refusal: &Refusal) -> Told {
    match refusal {
        Refusal::Failed(err) => diagnose(err),
        Refusal::Mismatched => Told {
            level: Level::Error,
            reason: "The machine answered with something other than an accepted prompt.".to_owned(),
            next_steps: Some(UPDATE),
            detail: None,
        },
    }
}

fn diagnose(err: &CallError) -> Told {
    match err {
        CallError::Fault(fault) => reported(fault),
        CallError::LinkReset => Told {
            level: Level::Warn,
            reason: "The machine reconnected, so the message was lost on its way.".to_owned(),
            next_steps: Some(RETRY),
            detail: None,
        },
        CallError::Closed => Told {
            level: Level::Warn,
            reason: "The machine went offline before it answered.".to_owned(),
            next_steps: Some("Bring the machine back up (`riggs run`) and send this again."),
            detail: None,
        },
        CallError::Transfer(reason) => Told {
            level: Level::Error,
            reason: "A file sent with this message did not arrive whole.".to_owned(),
            next_steps: Some("Send the message again, or send it without the file."),
            detail: Some(reason.clone()),
        },
        CallError::Unsendable(SendError::TooLarge { size }) => Told {
            level: Level::Error,
            reason: format!("This message is {size} bytes, more than one frame can carry."),
            next_steps: Some(SHORTEN),
            detail: None,
        },
        CallError::Unsendable(_) => Told {
            level: Level::Error,
            reason: "The gateway could not put this message on the link to the machine.".to_owned(),
            next_steps: Some(RETRY),
            detail: None,
        },
    }
}

/// A fault the node reported itself. Its kind is the diagnosis; its message is only the detail.
fn reported(fault: &Error) -> Told {
    let detail = {
        let message = fault.message.trim();
        (!message.is_empty()).then(|| message.to_owned())
    };
    let (level, reason, next_steps) = match &fault.kind {
        ErrorKind::Cancelled => (
            Level::Warn,
            "The agent stopped this turn.".to_owned(),
            Some(RETRY),
        ),
        ErrorKind::ToolCeiling => (
            Level::Warn,
            "The agent has used every tool call one turn is allowed.".to_owned(),
            Some("Ask it to carry on in a new message, which starts a fresh turn."),
        ),
        ErrorKind::Credential => (
            Level::Error,
            "The agent on that machine cannot use its credential.".to_owned(),
            Some("Finish the sign-in card the machine sends you, then send this again."),
        ),
        ErrorKind::Unsupported => (
            Level::Error,
            "The machine does not serve what the gateway asked of it.".to_owned(),
            Some(UPDATE),
        ),
        ErrorKind::UnknownSession => (
            Level::Warn,
            "The machine no longer holds this conversation's session.".to_owned(),
            Some("Send the message again; it opens a new one."),
        ),
        ErrorKind::SessionBusy => (
            Level::Warn,
            "The machine is still finishing the last turn in this conversation.".to_owned(),
            Some("Wait for the answer, or `/stop` the turn and send this again."),
        ),
        ErrorKind::NotFound => (
            Level::Error,
            "The machine asked the gateway for something it can no longer produce.".to_owned(),
            Some(RETRY),
        ),
        ErrorKind::Forbidden => (
            Level::Error,
            "The machine asked the gateway for something it is not allowed to have.".to_owned(),
            Some(ADMIN),
        ),
        ErrorKind::TooLarge => (
            Level::Error,
            "This message is larger than the machine accepts.".to_owned(),
            Some(SHORTEN),
        ),
        ErrorKind::Provider { provider } => return provider_failed(provider, detail),
        ErrorKind::Rpc { rpc } => (
            Level::Error,
            match &rpc.method {
                Some(method) => {
                    format!(
                        "The agent failed the `{method}` call with code {}.",
                        rpc.code
                    )
                }
                None => format!("The agent failed the call with code {}.", rpc.code),
            },
            Some(RETRY),
        ),
        ErrorKind::Unknown => (
            Level::Error,
            "The machine reported a failure this gateway does not know.".to_owned(),
            Some(UPDATE),
        ),
    };
    Told {
        level,
        reason,
        next_steps,
        detail,
    }
}

/// The model provider's own failure: the one kind that arrives with a verdict on whether trying
/// again is worth anything, so the card says which it is rather than guessing.
fn provider_failed(provider: &ProviderFailure, detail: Option<String>) -> Told {
    let who = match &provider.provider {
        Some(name) => format!("The model provider `{name}`"),
        None => "The agent's model provider".to_owned(),
    };
    let status = provider
        .status_code
        .map(|code| format!(" with HTTP {code}"))
        .unwrap_or_default();
    Told {
        level: Level::Error,
        reason: format!("{who} failed the request{status} ({}).", provider.kind),
        next_steps: Some(if provider.retryable {
            "The provider called this retryable; send the message again in a moment."
        } else {
            "The provider called this permanent; the same message will fail the same way."
        }),
        detail: detail.or_else(|| provider.message.clone()),
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

    use super::*;

    fn fault(kind: ErrorKind, message: &str) -> CallError {
        CallError::Fault(Error::new(kind, message))
    }

    fn body(alert: &Alert) -> String {
        format!(
            "{}\n{}\n{}\n{}",
            alert.title,
            alert.subtitle.clone().unwrap_or_default(),
            alert.reason.clone().unwrap_or_default(),
            alert.text.clone().unwrap_or_default(),
        )
    }

    #[test]
    fn a_card_never_shows_a_person_the_transport_crates_name() {
        let every = [
            fault(ErrorKind::Unknown, "boom"),
            CallError::LinkReset,
            CallError::Closed,
            CallError::Transfer("half a file".into()),
            CallError::Unsendable(SendError::TooLarge { size: 99 }),
            CallError::Unsendable(SendError::Closed),
        ];
        for err in &every {
            let alert = unreachable("laptop", err);
            assert!(
                !body(&alert).contains("rax"),
                "{} leaked the wire's wording",
                body(&alert)
            );
        }
    }

    #[test]
    fn the_kind_decides_the_words_and_the_message_is_only_the_detail() {
        let err = fault(ErrorKind::ToolCeiling, "tool ceiling of 40 reached");
        let alert = not_taken("laptop", &Refusal::Failed(err));

        assert_eq!(alert.title, "laptop could not take this message");
        assert_eq!(alert.level, Some(Level::Warn));
        assert!(alert.reason.unwrap().contains("every tool call"));
        assert!(alert.next_steps.unwrap().contains("fresh turn"));
        assert!(alert.text.unwrap().contains("tool ceiling of 40 reached"));
    }

    #[test]
    fn a_providers_own_verdict_on_retrying_is_what_the_card_says() {
        let failing = |retryable| ProviderFailure {
            kind: "overloaded_error".into(),
            provider: Some("anthropic".into()),
            status_code: Some(529),
            message: None,
            retryable,
        };
        let transient = provider_failed(&failing(true), None);
        assert!(transient.reason.contains("HTTP 529"));
        assert!(transient.reason.contains("overloaded_error"));
        assert!(transient.next_steps.unwrap().contains("again in a moment"));

        let permanent = provider_failed(&failing(false), None);
        assert!(permanent.next_steps.unwrap().contains("fail the same way"));
    }

    #[test]
    fn a_long_fault_is_clipped_and_fenced_so_it_cannot_break_the_card() {
        let err = fault(ErrorKind::Unknown, &"x".repeat(5_000));
        let detail = not_taken("laptop", &Refusal::Failed(err)).text.unwrap();

        assert!(detail.starts_with("```\n"), "the detail was not fenced");
        assert!(detail.ends_with("\n```"));
        assert!(
            detail.chars().count() < DETAIL_LIMIT + 16,
            "the detail was not clipped"
        );
    }

    #[test]
    fn a_fault_that_carries_a_mention_cannot_notify_anyone() {
        let err = fault(ErrorKind::Unknown, "<!channel> & <@U0ALICE01> failed");
        let detail = not_taken("laptop", &Refusal::Failed(err)).text.unwrap();

        assert!(detail.contains("&lt;!channel&gt;"));
        assert!(detail.contains("&lt;@U0ALICE01&gt;"));
        assert!(detail.contains("&amp;"));
    }

    #[test]
    fn a_reply_the_call_never_asked_for_says_so_without_dumping_it() {
        let alert = not_taken("laptop", &Refusal::Mismatched);

        assert_eq!(alert.level, Some(Level::Error));
        assert!(alert.text.is_none(), "a mismatched reply was shown");
        assert!(alert.next_steps.unwrap().contains("riggs"));
    }
}
