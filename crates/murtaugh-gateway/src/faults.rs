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
use serde_json::{Value, json};

use crate::alerts::{Alert, Level, clip};
use crate::chat::span;
use crate::render;

/// The button that sends a failed message again; its value is the id of the message kept back.
pub const SEND_AGAIN: &str = "message_send_again";

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
pub fn unreachable(node: &str, refusal: &Refusal) -> Alert {
    card(
        format!("Could not reach {node}"),
        format!("This message never reached {node}."),
        refused(refusal),
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

/// Files the agent meant to send that never arrived. One card for the turn however many failed:
/// a card each would bury the answer they belong to.
pub fn unattached(node: &str, failures: &[(String, String)]) -> Alert {
    let (title, subtitle) = match failures {
        [(name, _)] => (
            format!("{node} could not attach {name}"),
            "The answer is above; the file is not.".to_owned(),
        ),
        _ => (
            format!("{node} could not attach {} files", failures.len()),
            "The answer is above; the files are not.".to_owned(),
        ),
    };
    let detail = failures
        .iter()
        .map(|(name, reason)| format!("{name}: {reason}"))
        .collect::<Vec<_>>()
        .join("\n");
    card(
        title,
        subtitle,
        Told {
            level: Level::Warn,
            reason: "The gateway did not get the file's bytes from the machine.".to_owned(),
            next_steps: Some("Ask the agent to send it again."),
            detail: Some(detail),
        },
    )
}

/// The one-line diagnosis on its own, for a surface with no room for a card: a slash command is
/// answered with a sentence, not a block.
pub fn line(refusal: &Refusal) -> String {
    refused(refusal).reason
}

/// Whether sending the very same message again has a real chance, so the card is worth a button.
///
/// A provider failure is the only one that answers this outright, and its answer is taken. The
/// rest turn on whether the obstacle was the message itself: a ceiling the same message hits
/// again, or a credential nobody has signed in to yet, is not retried by pressing a button.
pub fn worth_retrying(refusal: &Refusal) -> bool {
    match refusal {
        // Whatever the machine sent instead, sending the message again will not change it.
        Refusal::Mismatched => false,
        Refusal::Failed(err) => match err {
            // The message never landed, or landed somewhere that has since forgotten it.
            // Sending it again seats the conversation afresh, which is the whole cure.
            CallError::LinkReset | CallError::Closed | CallError::Transfer(_) => true,
            CallError::Unsendable(SendError::TooLarge { .. }) => false,
            CallError::Unsendable(_) => true,
            CallError::Fault(fault) => worth_retrying_turn(fault),
        },
    }
}

/// As [`worth_retrying`], for a fault the node reported itself rather than a call that failed.
pub fn worth_retrying_turn(fault: &Error) -> bool {
    match &fault.kind {
        ErrorKind::Cancelled | ErrorKind::UnknownSession | ErrorKind::NotFound => true,
        ErrorKind::Provider { provider } => provider.retryable,
        // Each of these is the message's own doing, a standing refusal, or a state a button
        // cannot move: the same message would fail the same way.
        ErrorKind::ToolCeiling
        | ErrorKind::Credential
        | ErrorKind::Unsupported
        | ErrorKind::SessionBusy
        | ErrorKind::Forbidden
        | ErrorKind::TooLarge
        | ErrorKind::Rejected
        | ErrorKind::Rpc { .. }
        | ErrorKind::Unknown => false,
    }
}

/// The button itself. `id` is how the gateway finds the message it kept back.
pub fn send_again(id: &str) -> Value {
    json!({
        "type": "actions",
        "block_id": "murtaugh_fault_actions",
        "elements": [{
            "type": "button",
            "action_id": SEND_AGAIN,
            "value": id,
            "style": "primary",
            "text": {"type": "plain_text", "text": "Send Again"},
        }],
    })
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
        ErrorKind::Rejected => (
            Level::Error,
            "The machine's settings were refused, so it has stopped.".to_owned(),
            Some("Ask the machine's owner to fix its settings."),
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
            Refusal::Failed(fault(ErrorKind::Unknown, "boom")),
            Refusal::Failed(CallError::LinkReset),
            Refusal::Failed(CallError::Closed),
            Refusal::Failed(CallError::Transfer("half a file".into())),
            Refusal::Failed(CallError::Unsendable(SendError::TooLarge { size: 99 })),
            Refusal::Failed(CallError::Unsendable(SendError::Closed)),
            Refusal::Mismatched,
        ];
        for refusal in &every {
            for alert in [
                unreachable("laptop", refusal),
                not_taken("laptop", refusal),
                no_session("laptop", refusal),
            ] {
                assert!(
                    !body(&alert).contains("rax"),
                    "{} leaked the wire's wording",
                    body(&alert)
                );
            }
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
    fn only_a_failure_a_second_try_could_clear_is_worth_a_button() {
        let worth = |kind| worth_retrying(&Refusal::Failed(fault(kind, "")));

        // Nothing about the message caused these, so the same message may well land.
        assert!(worth(ErrorKind::Cancelled));
        assert!(worth(ErrorKind::UnknownSession));
        assert!(worth_retrying(&Refusal::Failed(CallError::LinkReset)));
        assert!(worth_retrying(&Refusal::Failed(CallError::Closed)));

        // These would fail the same way, so a button would only lie about the odds.
        assert!(!worth(ErrorKind::ToolCeiling));
        assert!(!worth(ErrorKind::Credential));
        assert!(!worth(ErrorKind::TooLarge));
        assert!(!worth(ErrorKind::Forbidden));
        assert!(!worth_retrying(&Refusal::Mismatched));
        assert!(!worth_retrying(&Refusal::Failed(CallError::Unsendable(
            SendError::TooLarge { size: 99 }
        ))));
    }

    #[test]
    fn a_provider_decides_its_own_retry_and_the_button_follows() {
        let failing = |retryable| ProviderFailure {
            kind: "overloaded_error".into(),
            provider: None,
            status_code: None,
            message: None,
            retryable,
        };
        let refusal = |retryable| {
            Refusal::Failed(fault(
                ErrorKind::Provider {
                    provider: failing(retryable),
                },
                "",
            ))
        };
        assert!(worth_retrying(&refusal(true)));
        assert!(!worth_retrying(&refusal(false)));
    }

    #[test]
    fn every_file_that_went_missing_is_named_on_one_card() {
        let failures = [
            (
                "chart.png".to_owned(),
                "3 bytes arrived, 9 were announced".to_owned(),
            ),
            ("notes.txt".to_owned(), "the transfer timed out".to_owned()),
        ];

        let one = unattached("laptop", &failures[..1]);
        assert_eq!(one.title, "laptop could not attach chart.png");
        assert!(one.subtitle.unwrap().contains("the file is not"));

        let both = unattached("laptop", &failures);
        assert_eq!(both.title, "laptop could not attach 2 files");
        let detail = both.text.unwrap();
        assert!(detail.contains("chart.png: 3 bytes arrived"));
        assert!(detail.contains("notes.txt: the transfer timed out"));
        // The answer the files belonged to already landed, so this is not an error.
        assert_eq!(both.level, Some(Level::Warn));
    }

    #[test]
    fn a_reply_the_call_never_asked_for_says_so_without_dumping_it() {
        let alert = not_taken("laptop", &Refusal::Mismatched);

        assert_eq!(alert.level, Some(Level::Error));
        assert!(alert.text.is_none(), "a mismatched reply was shown");
        assert!(alert.next_steps.unwrap().contains("riggs"));
    }
}
