//! Sign-ins a node needs for its agent's credentials. The credentials are the node owner's, so the
//! working card goes to the owner by DM; a thread that raised one only hears that it was sent.
//! Ported from the Go gateway's two-party auth card, with the verification code typed into the
//! card itself instead of a modal.

use std::collections::{BTreeMap, HashMap};
use std::sync::{Arc, Mutex, MutexGuard};

use murtaugh_slack::{Block, Click, PostMessage, Posted, SlackClient, UpdateMessage};
use murtaugh_store::UserId;
use rax::interaction::{DisplayAnswer, DisplayOutcome, SignInRequest, SignInSettled, SignInState};
use rax::{Error, ErrorKind};
use rax_tokio::gateway::GatewayLink;
use serde_json::{Value, json};

use crate::access::Access;

pub const APPROVE: &str = "signin_approve";
pub const DENY: &str = "signin_deny";
pub const SUBMIT_CODE: &str = "signin_code";
const OPEN: &str = "signin_open";
const CODE_BLOCK: &str = "signin_code_input";
const CODE_ACTION: &str = "signin_code_value";
const ICON: &str = "https://img.icons8.com/stickers/100/palm-scan.png";

/// The node a sign-in belongs to.
#[derive(Clone)]
pub struct Node {
    pub selector: String,
    pub name: String,
    pub owner: UserId,
    pub link: GatewayLink,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Stage {
    Approval,
    Starting,
    Pending,
    Working,
    Success,
    Denied,
    TimedOut,
    Cancelled,
    Failed,
}

impl Stage {
    fn is_terminal(self) -> bool {
        matches!(
            self,
            Stage::Success | Stage::Denied | Stage::TimedOut | Stage::Cancelled | Stage::Failed
        )
    }
}

struct Open {
    node: Node,
    request: SignInRequest,
    url: Option<String>,
    stage: Stage,
    reason: Option<String>,
    card: Posted,
    note: Option<Posted>,
}

#[derive(Clone)]
pub struct SignIns {
    slack: Option<SlackClient>,
    access: Access,
    open: Arc<Mutex<HashMap<String, Open>>>,
    /// Posts and redraws run one at a time from the latest state, so a slow redraw never
    /// overwrites a newer one.
    drawing: Arc<tokio::sync::Mutex<()>>,
}

impl SignIns {
    pub fn new(slack: Option<SlackClient>, access: Access) -> Self {
        Self {
            slack,
            access,
            open: Arc::default(),
            drawing: Arc::default(),
        }
    }

    fn open(&self) -> MutexGuard<'_, HashMap<String, Open>> {
        self.open
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Shows the owner the card; `thread` is where the sign-in was raised, if a turn raised it.
    /// A node gets one sign-in at a time, as RAX's `credential` fault requires.
    pub async fn raise(
        &self,
        node: Node,
        request: SignInRequest,
        thread: Option<(String, String)>,
    ) -> Result<(), Error> {
        let unreachable = |reason: String| Error::new(ErrorKind::Unsupported, reason);
        let slack = self
            .slack
            .as_ref()
            .ok_or_else(|| unreachable("this gateway cannot reach Slack".into()))?;
        let _drawing = self.drawing.lock().await;
        let busy = self
            .open()
            .values()
            .any(|open| open.node.selector == node.selector && !open.stage.is_terminal());
        if busy {
            return Err(Error::new(
                ErrorKind::Credential,
                "the node's owner already has a sign-in in front of them",
            ));
        }
        let stage = if request.url.is_none() && request.command.is_some() {
            Stage::Approval
        } else {
            Stage::Pending
        };
        let key = key(&node.selector, &request.id.0);
        let open = Open {
            url: request.url.clone(),
            node,
            request,
            stage,
            reason: None,
            card: Posted {
                channel: String::new(),
                ts: String::new(),
            },
            note: None,
        };
        let card = PostMessage {
            channel: open.node.owner.to_string(),
            thread_ts: None,
            text: fallback(&open),
            blocks: vec![owner_card(&open, &key)],
        };
        let note = thread.map(|(channel, thread_ts)| PostMessage {
            channel,
            thread_ts: Some(thread_ts),
            text: fallback(&open),
            blocks: vec![thread_card(&open)],
        });
        // Registered before posting, so a click on the card always finds it.
        self.open().insert(key.clone(), open);
        match slack.post_message(&card).await {
            Ok(posted) => {
                if let Some(open) = self.open().get_mut(&key) {
                    open.card = posted;
                }
            }
            Err(err) => {
                self.open().remove(&key);
                return Err(unreachable(format!(
                    "could not reach the node's owner: {err}"
                )));
            }
        }
        if let Some(note) = note {
            match slack.post_message(&note).await {
                Ok(posted) => {
                    if let Some(open) = self.open().get_mut(&key) {
                        open.note = Some(posted);
                    }
                }
                Err(err) => {
                    tracing::warn!(error = %err, "could not tell the thread about a sign-in")
                }
            }
        }
        Ok(())
    }

    /// The node's own sign-in moved on.
    pub async fn settle(&self, selector: &str, settled: SignInSettled) {
        let key = key(selector, &settled.id.0);
        let confirm = {
            let mut all = self.open();
            let Some(open) = all.get_mut(&key) else {
                return;
            };
            if settled.url.is_some() {
                open.url = settled.url.clone();
            }
            open.reason = settled.reason.clone().or(open.reason.take());
            open.stage = match settled.state {
                SignInState::Working => Stage::Working,
                SignInState::Ready => Stage::Pending,
                SignInState::Confirming => open.stage,
                SignInState::Success => Stage::Success,
                SignInState::Failed => Stage::Failed,
                SignInState::TimedOut => Stage::TimedOut,
                SignInState::Cancelled => Stage::Cancelled,
            };
            (settled.state == SignInState::Confirming)
                .then(|| (open.node.clone(), open.request.id.clone()))
        };
        if let Some((node, id)) = confirm {
            let snapshot = self.access.snapshot();
            let still = snapshot.owner(&node.selector) == Some(&node.owner)
                && snapshot.may_run_nodes(&node.owner);
            let (outcome, note) = if still {
                (DisplayOutcome::Approved, None)
            } else {
                (
                    DisplayOutcome::Denied,
                    Some("The node's owner no longer has access.".to_owned()),
                )
            };
            answer(&node.link, id, outcome, None, note).await;
        }
        self.redraw(&key).await;
    }

    /// Whether a sign-in on this node is still waiting on its owner.
    pub fn pending_for(&self, selector: &str) -> bool {
        self.open()
            .values()
            .any(|open| open.node.selector == selector && !open.stage.is_terminal())
    }

    /// A click on an owner's card. Returns the note to show the clicker alone, if any.
    pub async fn click(&self, click: &Click) -> Option<String> {
        let (action, key) = (click.action_id.as_str(), click.value.clone());
        if !matches!(action, APPROVE | DENY | SUBMIT_CODE) {
            return None;
        }
        let (link, id, outcome, code) = {
            let mut all = self.open();
            let Some(open) = all.get_mut(&key) else {
                return Some("That sign-in is already over.".into());
            };
            if open.node.owner.as_str() != click.user {
                return Some(format!("Only <@{}> can do that.", open.node.owner));
            }
            let (outcome, code, stage) = match action {
                APPROVE if open.stage == Stage::Approval => {
                    (DisplayOutcome::Approved, None, Stage::Starting)
                }
                DENY => (DisplayOutcome::Denied, None, Stage::Denied),
                SUBMIT_CODE if open.request.needs_code => {
                    let code = click.values[CODE_BLOCK][CODE_ACTION]["value"]
                        .as_str()
                        .map(str::trim)
                        .filter(|code| !code.is_empty())
                        .map(str::to_owned);
                    let Some(code) = code else {
                        return Some("Paste the verification code first.".into());
                    };
                    (DisplayOutcome::Answered, Some(code), Stage::Working)
                }
                _ => return Some("That step is already done.".into()),
            };
            open.stage = stage;
            (
                open.node.link.clone(),
                open.request.id.clone(),
                outcome,
                code,
            )
        };
        answer(&link, id, outcome, code, None).await;
        self.redraw(&key).await;
        None
    }

    async fn redraw(&self, key: &str) {
        let Some(slack) = &self.slack else {
            return;
        };
        let _drawing = self.drawing.lock().await;
        let (updates, done) = {
            let all = self.open();
            let Some(open) = all.get(key) else {
                return;
            };
            let mut updates = Vec::new();
            if !open.card.ts.is_empty() {
                updates.push(update(&open.card, fallback(open), owner_card(open, key)));
            }
            if let Some(note) = &open.note {
                updates.push(update(note, fallback(open), thread_card(open)));
            }
            (updates, open.stage.is_terminal())
        };
        if done {
            self.open().remove(key);
        }
        for update in updates {
            if let Err(err) = slack.update_message(&update).await {
                tracing::warn!(error = %err, "could not update a sign-in card");
            }
        }
    }
}

async fn answer(
    link: &GatewayLink,
    id: rax::id::PromptId,
    outcome: DisplayOutcome,
    code: Option<String>,
    note: Option<String>,
) {
    let answer = DisplayAnswer {
        id,
        outcome,
        answers: BTreeMap::new(),
        choice: None,
        user_id: None,
        note,
        code,
    };
    if let Err(err) = link.answer(answer).await {
        tracing::warn!(error = %err, "could not answer a sign-in");
    }
}

fn key(selector: &str, id: &str) -> String {
    format!("{selector}/{id}")
}

fn update(posted: &Posted, text: String, card: Block) -> UpdateMessage {
    UpdateMessage {
        channel: posted.channel.clone(),
        ts: posted.ts.clone(),
        text,
        blocks: vec![card],
    }
}

fn fallback(open: &Open) -> String {
    format!("Sign-in needed for {}", open.request.tool)
}

fn status(open: &Open) -> &'static str {
    let code = open.request.needs_code;
    match open.stage {
        Stage::Approval => {
            "This sign-in has to run the command below on the machine the agent runs on. Approve it only if you expected it; nothing runs until you do."
        }
        Stage::Starting => "Approved. Waiting for the command to offer a sign-in link.",
        Stage::Pending if code => {
            "Open the link, approve the request, then paste the verification code back here."
        }
        Stage::Pending => {
            "Open the link and finish signing in. This card completes on its own once the browser flow is done."
        }
        Stage::Working if code => "Waiting for the verification code to be accepted.",
        Stage::Working => "Waiting for the browser sign-in to complete.",
        Stage::Success => "Authentication succeeded.",
        Stage::Denied => "You declined this authentication request.",
        Stage::TimedOut => "This request expired before it was completed.",
        Stage::Cancelled => "This request was cancelled before it was completed.",
        Stage::Failed => "The authentication attempt failed.",
    }
}

fn owner_card(open: &Open, key: &str) -> Block {
    let mut children = vec![rich_text(status(open))];
    if let Some(command) = &open.request.command {
        children.push(json!({
            "type": "rich_text",
            "elements": [{
                "type": "rich_text_preformatted",
                "elements": [{"type": "text", "text": command}],
            }],
        }));
    }
    children.push(context(&format!(
        "On *{}*{}",
        open.node.name,
        open.reason
            .as_deref()
            .map(|reason| format!(" · {reason}"))
            .unwrap_or_default()
    )));
    match open.stage {
        Stage::Approval => children.push(actions(vec![
            button(APPROVE, key, "Approve", Some("primary"), None),
            button(DENY, key, "Deny", Some("danger"), None),
        ])),
        Stage::Pending => {
            let url = open.url.as_deref();
            if open.request.needs_code {
                children.push(json!({
                    "type": "input",
                    "block_id": CODE_BLOCK,
                    "label": {"type": "plain_text", "text": "Verification code"},
                    "optional": false,
                    "element": {"type": "plain_text_input", "action_id": CODE_ACTION},
                }));
                let mut row = vec![button(
                    SUBMIT_CODE,
                    key,
                    "Submit Code",
                    Some("primary"),
                    None,
                )];
                row.extend(url.map(|url| button(OPEN, key, "Open In Browser", None, Some(url))));
                row.push(button(DENY, key, "Deny", Some("danger"), None));
                children.push(actions(row));
            } else {
                let mut row: Vec<Value> = url
                    .map(|url| button(OPEN, key, "Open In Browser", Some("primary"), Some(url)))
                    .into_iter()
                    .collect();
                row.push(button(DENY, key, "Deny", Some("danger"), None));
                children.push(actions(row));
            }
        }
        _ => {}
    }
    container(open, children, open.stage.is_terminal())
}

fn thread_card(open: &Open) -> Block {
    let owner = &open.node.owner;
    let text = match open.stage {
        Stage::Success => format!("<@{owner}> completed the sign-in. Carrying on."),
        Stage::Denied => format!("<@{owner}> declined the sign-in."),
        Stage::TimedOut => "The sign-in expired before it was finished.".to_owned(),
        Stage::Cancelled => "The sign-in was cancelled before it was finished.".to_owned(),
        Stage::Failed => "The sign-in failed.".to_owned(),
        _ => format!("<@{owner}> has been sent a direct message to finish signing in."),
    };
    container(open, vec![context(&text)], open.stage.is_terminal())
}

fn container(open: &Open, children: Vec<Value>, settled: bool) -> Block {
    Block::Raw(json!({
        "type": "container",
        "block_id": "murtaugh_signin_card",
        "icon": {"type": "image", "image_url": ICON, "alt_text": "Authentication Required icon"},
        "title": {"type": "plain_text", "text": "Authentication Required"},
        "subtitle": {
            "type": "plain_text",
            "text": format!("The tool '{}' requires authentication", open.request.tool),
        },
        "is_collapsible": settled,
        "default_collapsed": settled,
        "has_header_divider": !settled,
        "width": "wide",
        "child_blocks": children,
    }))
}

fn rich_text(text: &str) -> Value {
    json!({
        "type": "rich_text",
        "elements": [{"type": "rich_text_section", "elements": [{"type": "text", "text": text}]}],
    })
}

fn context(text: &str) -> Value {
    json!({"type": "context", "elements": [{"type": "mrkdwn", "text": text}]})
}

fn actions(elements: Vec<Value>) -> Value {
    json!({"type": "actions", "block_id": "murtaugh_signin_actions", "elements": elements})
}

fn button(action_id: &str, key: &str, text: &str, style: Option<&str>, url: Option<&str>) -> Value {
    let mut button = json!({
        "type": "button",
        "action_id": action_id,
        "value": key,
        "text": {"type": "plain_text", "text": text},
    });
    if let Some(style) = style {
        button["style"] = json!(style);
    }
    if let Some(url) = url {
        button["url"] = json!(url);
    }
    button
}
