//! Questions and plans an agent puts to the people in its conversation, as cards answered in
//! place. Ported from the Go gateway: questions become radio buttons or checkboxes with
//! "Submit Answers" and "Chat About This", plans get Proceed, Revise and Cancel. Anyone who may
//! talk to the gateway may answer, as they could have typed the answer into the thread.

use std::collections::{BTreeMap, HashMap};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use murtaugh_slack::{Block, Click, PostMessage, SlackClient, UpdateMessage};
use rax::id::PromptId;
use rax::interaction::{
    DisplayAnswer, DisplayOutcome, PlanChoice, PlanRequest, Question, QuestionRequest,
};
use rax_tokio::gateway::GatewayLink;
use serde_json::{Value, json};
use tokio::sync::oneshot;
use tokio_util::sync::CancellationToken;

/// The Go gateway's bound on one question; plans share it.
pub const TIMEOUT: Duration = Duration::from_secs(10 * 60);
pub const SUBMIT: &str = "prompt_submit";
pub const CHAT: &str = "prompt_chat";
pub const PROCEED: &str = "plan_proceed";
pub const REVISE: &str = "plan_revise";
pub const CANCEL: &str = "plan_cancel";
const QUESTION_BLOCK: &str = "prompt_q:";
const QUESTION_ACTION: &str = "prompt_answer";
const OPTION_CHARS: usize = 75;
const PLAN_CHARS: usize = 11_000;
const ICON_QUESTION: &str = "https://img.icons8.com/external-flaticons-lineal-color-flat-icons/64/external-idea-gamification-flaticons-lineal-color-flat-icons.png";

/// What a card asks, as the node sent it.
pub enum Asked {
    Questions(QuestionRequest),
    Plan(PlanRequest),
}

impl Asked {
    fn id(&self) -> &PromptId {
        match self {
            Asked::Questions(request) => &request.id,
            Asked::Plan(request) => &request.id,
        }
    }
}

enum Decided {
    Answers {
        by: String,
        answers: BTreeMap<String, Vec<String>>,
    },
    Chat {
        by: String,
    },
    Plan {
        by: String,
        choice: PlanChoice,
    },
}

enum Ended {
    Decided(Decided),
    TimedOut,
    Dismissed,
}

struct Waiting {
    questions: Vec<Question>,
    decide: oneshot::Sender<Decided>,
}

/// Everything one card needs, owned so it can wait in its own task while the turn goes on.
pub struct Put {
    pub slack: SlackClient,
    pub link: GatewayLink,
    pub channel: String,
    pub thread_ts: String,
    pub asked: Asked,
    pub timeout: Duration,
    /// Cancelled when the turn ends, which dismisses a card nobody answered.
    pub turn: CancellationToken,
}

#[derive(Clone, Default)]
pub struct Prompts {
    waiting: Arc<Mutex<HashMap<String, Waiting>>>,
}

impl Prompts {
    fn waiting(&self) -> MutexGuard<'_, HashMap<String, Waiting>> {
        self.waiting
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Posts the card, waits for an answer, a timeout or the end of the turn, then answers the
    /// node and settles the card.
    pub async fn put(&self, put: Put) {
        let id = hex::encode(rand::random::<[u8; 8]>());
        let questions = match &put.asked {
            Asked::Questions(request) => request.questions.clone(),
            Asked::Plan(_) => Vec::new(),
        };
        let (decide, decided) = oneshot::channel();
        self.waiting()
            .insert(id.clone(), Waiting { questions, decide });
        let pending = match &put.asked {
            Asked::Questions(request) => question_card(request, &id),
            Asked::Plan(request) => plan_card(request, &id),
        };
        let posted = put
            .slack
            .post_message(&PostMessage {
                channel: put.channel.clone(),
                thread_ts: Some(put.thread_ts.clone()),
                text: fallback(&put.asked),
                blocks: vec![pending],
            })
            .await;
        let ended = match &posted {
            Err(err) => {
                tracing::warn!(error = %err, "could not post a prompt card");
                None
            }
            Ok(_) => Some(tokio::select! {
                decided = decided => decided.map_or(Ended::Dismissed, Ended::Decided),
                () = tokio::time::sleep(put.timeout) => Ended::TimedOut,
                () = put.turn.cancelled() => Ended::Dismissed,
            }),
        };
        self.waiting().remove(&id);
        let answer = display_answer(put.asked.id().clone(), ended.as_ref());
        if let Err(err) = put.link.answer(answer).await {
            tracing::warn!(error = %err, "could not answer a prompt");
        }
        if let (Some(ended), Ok(posted)) = (ended, posted) {
            let settled = UpdateMessage {
                channel: posted.channel,
                ts: posted.ts,
                text: fallback(&put.asked),
                blocks: vec![settled_card(&put.asked, &ended)],
            };
            if let Err(err) = put.slack.update_message(&settled).await {
                tracing::warn!(error = %err, "could not settle a prompt card");
            }
        }
    }

    /// A click on one of the cards' buttons; `may_answer` says whether the clicker could have
    /// answered in the thread. Returns the note to show the clicker alone, if any.
    pub fn click(&self, click: &Click, may_answer: bool) -> Option<String> {
        let decided = |by: String| -> Option<Decided> {
            Some(match click.action_id.as_str() {
                CHAT => Decided::Chat { by },
                PROCEED => Decided::Plan {
                    by,
                    choice: PlanChoice::Proceed,
                },
                REVISE => Decided::Plan {
                    by,
                    choice: PlanChoice::Revise,
                },
                CANCEL => Decided::Plan {
                    by,
                    choice: PlanChoice::Cancel,
                },
                _ => return None,
            })
        };
        if click.action_id != SUBMIT && decided(String::new()).is_none() {
            return None;
        }
        let mut waiting = self.waiting();
        let Some(card) = waiting.get(&click.value) else {
            return Some("That's already been answered.".into());
        };
        if !may_answer {
            return Some("You can't answer this one.".into());
        }
        let decision = if click.action_id == SUBMIT {
            let answers = submitted(&card.questions, &click.values);
            let missing = card.questions.len() - answers.len();
            if missing > 0 {
                return Some(match missing {
                    1 => "One question still needs an answer.".into(),
                    n => format!("{n} questions still need an answer."),
                });
            }
            Decided::Answers {
                by: click.user.clone(),
                answers,
            }
        } else {
            decided(click.user.clone())?
        };
        if let Some(card) = waiting.remove(&click.value) {
            let _ = card.decide.send(decision);
        }
        None
    }
}

/// Option values are indexes, so a long label survives Slack's 150-character value limit.
fn submitted(questions: &[Question], values: &Value) -> BTreeMap<String, Vec<String>> {
    let mut answers = BTreeMap::new();
    for (i, question) in questions.iter().enumerate() {
        let state = &values[format!("{QUESTION_BLOCK}{i}")][QUESTION_ACTION];
        if question.options.is_empty() {
            if let Some(typed) = state["value"]
                .as_str()
                .map(str::trim)
                .filter(|t| !t.is_empty())
            {
                answers.insert(question.key.clone(), vec![typed.to_owned()]);
            }
            continue;
        }
        let picked: Vec<&Value> = match state["selected_options"].as_array() {
            Some(many) => many.iter().collect(),
            None => state.get("selected_option").into_iter().collect(),
        };
        let labels: Vec<String> = picked
            .iter()
            .filter_map(|option| option["value"].as_str()?.parse::<usize>().ok())
            .filter_map(|index| question.options.get(index))
            .map(|option| option.label.clone())
            .collect();
        if !labels.is_empty() {
            answers.insert(question.key.clone(), labels);
        }
    }
    answers
}

fn display_answer(id: PromptId, ended: Option<&Ended>) -> DisplayAnswer {
    let mut answer = DisplayAnswer {
        id,
        outcome: DisplayOutcome::Unavailable,
        answers: BTreeMap::new(),
        choice: None,
        user_id: None,
        note: None,
        code: None,
    };
    match ended {
        None => answer.note = Some("The card could not be posted.".into()),
        Some(Ended::TimedOut) => answer.outcome = DisplayOutcome::TimedOut,
        Some(Ended::Dismissed) => answer.outcome = DisplayOutcome::Dismissed,
        Some(Ended::Decided(Decided::Answers { by, answers })) => {
            answer.outcome = DisplayOutcome::Answered;
            answer.answers = answers.clone();
            answer.user_id = Some(by.clone());
        }
        Some(Ended::Decided(Decided::Chat { by })) => {
            answer.outcome = DisplayOutcome::Chat;
            answer.user_id = Some(by.clone());
        }
        Some(Ended::Decided(Decided::Plan { by, choice })) => {
            answer.outcome = DisplayOutcome::Answered;
            answer.choice = Some(*choice);
            answer.user_id = Some(by.clone());
        }
    }
    answer
}

fn fallback(asked: &Asked) -> String {
    match asked {
        Asked::Questions(_) => "The agent has a question for you".into(),
        Asked::Plan(_) => "The agent has a plan for you to review".into(),
    }
}

fn plural(n: usize) -> &'static str {
    if n == 1 { "question" } else { "questions" }
}

fn question_label(i: usize, question: &Question) -> String {
    let text = question.question.trim();
    let text = match question.header.as_deref().map(str::trim) {
        Some(header) if !header.is_empty() => format!("{header} - {text}"),
        _ => text.to_owned(),
    };
    clip(&format!("{}. {text}", i + 1), 2000)
}

fn question_card(request: &QuestionRequest, id: &str) -> Block {
    let mut children: Vec<Value> = request
        .questions
        .iter()
        .enumerate()
        .map(|(i, question)| {
            let options: Vec<Value> = question
                .options
                .iter()
                .enumerate()
                .map(|(index, option)| {
                    let label = clip(option.label.trim(), OPTION_CHARS - 2);
                    let mut text = format!("_{label}_");
                    if let Some(description) = option
                        .description
                        .as_deref()
                        .map(str::trim)
                        .filter(|d| !d.is_empty())
                    {
                        let room = OPTION_CHARS.saturating_sub(text.chars().count() + 3);
                        if room >= 2 {
                            text.push_str(&format!(" - {}", clip(description, room)));
                        }
                    }
                    json!({"text": {"type": "mrkdwn", "text": text}, "value": index.to_string()})
                })
                .collect();
            let element = match (options.is_empty(), question.multi_select) {
                (true, _) => {
                    json!({"type": "plain_text_input", "action_id": QUESTION_ACTION, "multiline": true})
                }
                (false, true) => {
                    json!({"type": "checkboxes", "action_id": QUESTION_ACTION, "options": options})
                }
                (false, false) => {
                    json!({"type": "radio_buttons", "action_id": QUESTION_ACTION, "options": options})
                }
            };
            json!({
                "type": "input",
                "block_id": format!("{QUESTION_BLOCK}{i}"),
                "label": {"type": "plain_text", "text": question_label(i, question)},
                "optional": false,
                "element": element,
            })
        })
        .collect();
    children.push(json!({
        "type": "actions",
        "block_id": "prompt_actions",
        "elements": [
            button(SUBMIT, id, "Submit Answers", Some("primary")),
            button(CHAT, id, "Chat About This", None),
        ],
    }));
    let n = request.questions.len();
    container(
        &question_title(request),
        &format!("We need your input on the following {}.", plural(n)),
        false,
        children,
    )
}

fn question_title(request: &QuestionRequest) -> String {
    request
        .title
        .clone()
        .filter(|title| !title.trim().is_empty())
        .unwrap_or_else(|| "User Input Required".into())
}

fn plan_title(request: &PlanRequest) -> String {
    request
        .title
        .clone()
        .filter(|title| !title.trim().is_empty())
        .unwrap_or_else(|| "Plan Ready for Review".into())
}

fn plan_card(request: &PlanRequest, id: &str) -> Block {
    container(
        &plan_title(request),
        "The agent wants your go-ahead before carrying this out.",
        false,
        vec![
            json!({"type": "markdown", "text": clip(&request.plan, PLAN_CHARS)}),
            json!({
                "type": "actions",
                "block_id": "prompt_actions",
                "elements": [
                    button(PROCEED, id, "Proceed", Some("primary")),
                    button(REVISE, id, "Revise", None),
                    button(CANCEL, id, "Cancel", Some("danger")),
                ],
            }),
        ],
    )
}

fn settled_card(asked: &Asked, ended: &Ended) -> Block {
    match asked {
        Asked::Questions(request) => {
            let n = request.questions.len();
            let (subtitle, by, answers) = match ended {
                Ended::Decided(Decided::Answers { by, answers }) => (
                    format!("Answered {n} {}.", plural(n)),
                    Some(format!("Answered by <@{by}>")),
                    Some(answers),
                ),
                Ended::Decided(Decided::Chat { by }) => (
                    format!("Chose to talk the {} over instead.", plural(n)),
                    Some(format!("Raised by <@{by}>")),
                    None,
                ),
                Ended::Decided(Decided::Plan { .. }) | Ended::Dismissed => {
                    ("Dismissed before anyone answered.".into(), None, None)
                }
                Ended::TimedOut => ("Nobody answered in time.".into(), None, None),
            };
            let mut children: Vec<Value> = request
                .questions
                .iter()
                .enumerate()
                .map(|(i, question)| {
                    let mut text = format!("*{}*", question_label(i, question));
                    for label in answers
                        .and_then(|answers| answers.get(&question.key))
                        .into_iter()
                        .flatten()
                    {
                        text.push_str(&format!("\n*✓* {label}"));
                    }
                    json!({"type": "section", "text": {"type": "mrkdwn", "text": clip(&text, 3000)}})
                })
                .collect();
            children.extend(by.map(|by| context(&by)));
            container(&question_title(request), &subtitle, true, children)
        }
        Asked::Plan(request) => {
            let footer = match ended {
                Ended::Decided(Decided::Plan { by, choice }) => match choice {
                    PlanChoice::Proceed => {
                        format!("Approved by <@{by}>; the agent is carrying it out.")
                    }
                    PlanChoice::Revise => format!("<@{by}> asked for changes."),
                    PlanChoice::Cancel => format!("Cancelled by <@{by}>."),
                },
                Ended::TimedOut => "Nobody answered in time, so the agent did not go ahead.".into(),
                _ => "Dismissed before anyone answered.".into(),
            };
            container(
                &plan_title(request),
                &footer_subtitle(ended),
                true,
                vec![
                    json!({"type": "markdown", "text": clip(&request.plan, PLAN_CHARS)}),
                    context(&footer),
                ],
            )
        }
    }
}

fn footer_subtitle(ended: &Ended) -> String {
    match ended {
        Ended::Decided(Decided::Plan {
            choice: PlanChoice::Proceed,
            ..
        }) => "Approved".into(),
        Ended::Decided(Decided::Plan {
            choice: PlanChoice::Revise,
            ..
        }) => "Revision requested".into(),
        Ended::Decided(Decided::Plan {
            choice: PlanChoice::Cancel,
            ..
        }) => "Cancelled".into(),
        Ended::TimedOut => "Timed out".into(),
        _ => "Dismissed".into(),
    }
}

fn container(title: &str, subtitle: &str, settled: bool, children: Vec<Value>) -> Block {
    Block::Raw(json!({
        "type": "container",
        "block_id": "murtaugh_prompt_card",
        "icon": {"type": "image", "image_url": ICON_QUESTION, "alt_text": "Question icon"},
        "title": {"type": "plain_text", "text": clip(title, 150)},
        "subtitle": {"type": "plain_text", "text": clip(subtitle, 3000)},
        "is_collapsible": settled,
        "default_collapsed": settled,
        "has_header_divider": !settled,
        "width": "wide",
        "child_blocks": children,
    }))
}

fn button(action_id: &str, value: &str, text: &str, style: Option<&str>) -> Value {
    let mut button = json!({
        "type": "button",
        "action_id": action_id,
        "value": value,
        "text": {"type": "plain_text", "text": text},
    });
    if let Some(style) = style {
        button["style"] = json!(style);
    }
    button
}

fn context(text: &str) -> Value {
    json!({"type": "context", "elements": [{"type": "mrkdwn", "text": text}]})
}

fn clip(text: &str, max: usize) -> String {
    if text.chars().count() <= max {
        return text.to_owned();
    }
    let mut out: String = text.chars().take(max.saturating_sub(1)).collect();
    out.push('…');
    out
}
