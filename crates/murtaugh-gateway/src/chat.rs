//! A conversation is a Slack thread pinned to one node's session. This turns Slack messages into
//! RAX prompts and streams each turn back into the thread.

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use murtaugh_slack::{
    Block, Click, Event, FileRef, PostMessage, SlackClient, TaskStatus, Text, Upload,
};
use murtaugh_store::{Conversation, Pin, Store, ToolMode, UserConfig, UserId};
use rax::attachment::Attachment;
use rax::content::ContentBlock;
use rax::id::SessionId;
use rax::interaction::{DisplayAnswer, DisplayOutcome};
use rax::session::{NewSession, Prompt, SessionRef};
use rax::tool::{Decision, DeniedBy, ToolCall, ToolCallStatus, ToolVerdict};
use rax::{ErrorKind, Event as TurnEvent, GatewayCall, GatewayReply, Open};
use rax_tokio::CallError;
use rax_tokio::gateway::{GatewayLink, StreamEvents};
use time::OffsetDateTime;
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;

use crate::access::Access;
use crate::alerts::{self, Alert, Level};
use crate::approval::{self, Approvals};
use crate::faults::{self, Refusal};
use crate::files::Files;
use crate::fleet::{Fleet, Node};
use crate::hub::FleetChange;
use crate::picker;
use crate::prompts::{self, Asked, Prompts};
use crate::render;
use crate::reply::{Reply, Target};
use crate::signin::{self, SignIns};
use crate::thread_commands::{self, Command};

pub const UNAUTHORISED_REACTION: &str = "zipper_mouth_face";
/// The subtype Slack gives a DM that carries files; it is still the person talking.
const FILE_SHARE: &str = "file_share";
pub const THINKING: &str = "is thinking...";
/// The Go gateway's `request_timeout`: silence, not length, is what stops a turn.
pub const TURN_IDLE_TIMEOUT: Duration = Duration::from_secs(10 * 60);
/// The chunks precede the `attachment` event on the link, so the bytes are normally there already.
const ATTACHMENT_WAIT: Duration = Duration::from_secs(60);
/// Slack clears the status once a chunk lands, so it is re-asserted until the turn ends.
pub const THINKING_REFRESH: Duration = Duration::from_secs(2);
/// Long enough for a node to fetch the files a prompt links before accepting it.
pub const PROMPT_TIMEOUT: Duration = Duration::from_secs(120);

pub struct Chat {
    slack: SlackClient,
    bot_user: String,
    store: Arc<dyn Store>,
    access: Access,
    fleet: Fleet,
    files: Files,
    team: String,
    turn_timings: bool,
    approvals: Approvals,
    approval_timeout: Duration,
    prompts: Prompts,
    prompt_timeout: Duration,
    sign_ins: SignIns,
    orphaned: Mutex<HashSet<Conversation>>,
    /// Threads `/node` moved, which catch up from the thread on their next turn even though they
    /// are pinned: the machine they moved to has none of the conversation.
    moved: Mutex<HashSet<Conversation>>,
    turns: Mutex<HashMap<Conversation, Running>>,
    turn_idle_timeout: Duration,
}

#[derive(Clone)]
struct Incoming {
    user: String,
    channel: String,
    ts: String,
    thread_ts: Option<String>,
    text: String,
    files: Vec<FileRef>,
    direct: bool,
}

struct Seat {
    node: Node,
    session_id: SessionId,
    history: Option<String>,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

fn event_summary(event: &Event) -> String {
    match event {
        Event::Message {
            channel_type,
            subtype,
            bot_id,
            user,
            ..
        } => format!(
            "message channel_type={channel_type} subtype={subtype:?} bot={} user={}",
            bot_id.is_some(),
            user.is_some()
        ),
        Event::AppMention { .. } => "app_mention".to_owned(),
        Event::AppHomeOpened { tab, .. } => format!("app_home_opened tab={tab}"),
        Event::Unknown { kind, .. } => kind.clone(),
    }
}

pub fn thread_link(conversation: &Conversation) -> ContentBlock {
    ContentBlock::link(
        format!(
            "chat://slack/{}/{}",
            conversation.channel, conversation.thread_ts
        ),
        "thread",
    )
}

/// What the chat side is built from; the gateway's run loop owns every one of them.
pub struct Parts {
    pub slack: SlackClient,
    pub bot_user: String,
    pub team: String,
    pub store: Arc<dyn Store>,
    pub access: Access,
    pub fleet: Fleet,
    pub files: Files,
    pub turn_timings: bool,
    pub approval_timeout: Duration,
    pub prompt_timeout: Duration,
    pub sign_ins: SignIns,
    pub turn_idle_timeout: Duration,
}

impl Chat {
    pub fn new(parts: Parts) -> Arc<Self> {
        let Parts {
            slack,
            bot_user,
            team,
            store,
            access,
            fleet,
            files,
            turn_timings,
            approval_timeout,
            prompt_timeout,
            sign_ins,
            turn_idle_timeout,
        } = parts;
        Arc::new(Self {
            slack,
            bot_user,
            store,
            access,
            fleet,
            files,
            team,
            turn_timings,
            approvals: Approvals::default(),
            approval_timeout,
            prompts: Prompts::default(),
            prompt_timeout,
            sign_ins,
            orphaned: Mutex::new(HashSet::new()),
            moved: Mutex::new(HashSet::new()),
            turns: Mutex::new(HashMap::new()),
            turn_idle_timeout,
        })
    }

    pub async fn on_event(self: Arc<Self>, event: Event) {
        let received = Instant::now();
        let incoming = match event {
            Event::AppMention {
                user,
                channel,
                ts,
                thread_ts,
                text,
                files,
            } => Incoming {
                user,
                channel,
                ts,
                thread_ts,
                text,
                files,
                direct: false,
            },
            Event::Message {
                channel,
                channel_type,
                user: Some(user),
                subtype,
                ts,
                thread_ts,
                text,
                files,
                ..
            } if channel_type == "im" && matches!(subtype.as_deref(), None | Some(FILE_SHARE)) => {
                Incoming {
                    user,
                    channel,
                    ts,
                    thread_ts,
                    text,
                    files,
                    direct: true,
                }
            }
            Event::Message { .. } => {
                tracing::debug!(event = ?event_summary(&event), "ignored a Slack event");
                return;
            }
            Event::AppHomeOpened { user, tab } => {
                if tab == "home" {
                    self.show_home(&user).await;
                }
                return;
            }
            Event::Unknown { kind, .. } => {
                tracing::debug!(%kind, "ignored a Slack event of a kind this gateway does not handle");
                return;
            }
        };
        if incoming.user == self.bot_user {
            return;
        }
        tracing::debug!(channel = %incoming.channel, ts = %incoming.ts, user = %incoming.user, "Slack message for the agent");
        self.message(incoming, received).await;
    }

    /// A departed node's pins are dropped now; the notice waits for the next message, which is
    /// when the conversation is rebuilt from its thread.
    pub async fn on_click(self: Arc<Self>, click: Click) {
        let note = if click.action_id == alerts::RENEW {
            self.renew(&click).await
        } else if click.action_id.starts_with("signin_") {
            self.sign_ins.click(&click).await
        } else if click.action_id == picker::CHOOSE {
            self.move_thread(&click).await
        } else if click.action_id.starts_with("tool_approval") {
            self.approvals.click(&click)
        } else {
            let may_answer =
                UserId::parse(&click.user).is_ok_and(|user| self.access.snapshot().may_chat(&user));
            self.prompts.click(&click, may_answer)
        };
        let Some(note) = note else {
            return;
        };
        if let Err(err) = self
            .slack
            .post_ephemeral(
                &click.channel,
                click.thread_ts.as_deref(),
                &click.user,
                &note,
            )
            .await
        {
            tracing::warn!(error = %err, "could not answer a click");
        }
    }

    /// Someone picked the machine a thread should run on. The turn in flight is stopped rather
    /// than carried across: its answer would land in a thread the new machine knows nothing about.
    async fn move_thread(&self, click: &Click) -> Option<String> {
        let Ok(user) = UserId::parse(&click.user) else {
            return None;
        };
        let snapshot = self.access.snapshot();
        if !snapshot.may_chat(&user) {
            return None;
        }
        let Some((conversation, selector)) = picker::chosen(&click.value) else {
            return Some("That menu is out of date. Ask me `/node` again.".to_owned());
        };
        if !self
            .fleet
            .choices(&user, &snapshot)
            .iter()
            .any(|choice| choice.selector == selector)
        {
            return Some("That machine is not yours to run this on.".to_owned());
        }
        let Some(node) = self.fleet.get(&selector).filter(|node| node.connected) else {
            return Some("That machine is not connected right now.".to_owned());
        };
        let name = render::escape(&node.name);
        let pinned = match self.store.pin(&conversation).await {
            Ok(pinned) => pinned,
            Err(err) => {
                tracing::warn!(error = %err, "could not read a conversation's pin to move it");
                return Some(format!("I could not move this thread to *{name}*."));
            }
        };
        if pinned.as_ref().is_some_and(|pin| pin.node == selector) {
            return Some(format!("This thread is already running on *{name}*."));
        }
        self.stop(&conversation);
        if let Some(pin) = &pinned {
            if let Err(err) = self.store.remove_pin(&conversation).await {
                tracing::warn!(error = %err, "could not drop a pin to move a conversation");
            }
            self.fleet.session_ended(&pin.node);
        }
        let session_id = match self.open_session(&node, &conversation).await {
            Ok(session_id) => session_id,
            Err(refusal) => {
                // The old pin is already gone, so the next message is assigned a machine afresh.
                return Some(format!(
                    "*{name}* could not take this thread. {}",
                    faults::line(&refusal)
                ));
            }
        };
        self.fleet.session_resumed(&selector);
        let pin = Pin {
            conversation: conversation.clone(),
            node: selector,
            session_id: session_id.0,
            // Whose conversation it is was fixed when it started; moving it does not make it the
            // chooser's, and anyone in the thread may move it.
            user: pinned.map_or(user, |pin| pin.user),
            pinned_at: OffsetDateTime::now_utc(),
        };
        if let Err(err) = self.store.set_pin(&pin).await {
            tracing::warn!(error = %err, "could not pin a moved conversation");
        }
        lock(&self.moved).insert(conversation);
        Some(format!(
            "This thread now runs on *{name}*. It will catch up from the thread on your next message."
        ))
    }

    /// The owner asked a node to sign in again from its credential alert.
    async fn renew(&self, click: &Click) -> Option<String> {
        let Some(node) = self.fleet.get(&click.value) else {
            return Some("That machine is not connected right now.".into());
        };
        if node.owner.as_str() != click.user {
            return Some(format!("Only <@{}> can do that.", node.owner));
        }
        let asked = match node.link.call(GatewayCall::RenewCredential).await {
            Ok(pending) => pending.reply.await.map_err(|err| err.to_string()),
            Err(err) => Err(err.to_string()),
        };
        Some(match asked {
            Ok(GatewayReply::RenewCredential(renewal)) => {
                alerts::renewal_note(&render::escape(&node.name), renewal)
            }
            Ok(other) => format!("*{}* answered oddly: {other:?}", render::escape(&node.name)),
            Err(reason) => format!(
                "*{}* could not start a sign-in: {reason}",
                render::escape(&node.name)
            ),
        })
    }

    async fn show_home(&self, viewer: &str) {
        let Ok(viewer) = UserId::parse(viewer) else {
            return;
        };
        let blocks = crate::home::view(&viewer, &self.access.snapshot(), &self.fleet.summaries());
        if let Err(err) = self.slack.publish_home(viewer.as_str(), &blocks).await {
            tracing::warn!(error = %err, "could not publish the Home tab");
        }
    }

    pub async fn on_fleet(self: Arc<Self>, change: FleetChange) {
        let FleetChange::Gone { selector } = change else {
            return;
        };
        match self.store.remove_pins_on(&selector).await {
            Ok(conversations) => lock(&self.orphaned).extend(conversations),
            Err(err) => {
                tracing::warn!(node = %selector, error = %err, "could not drop the pins of a departed node");
            }
        }
    }

    async fn message(&self, incoming: Incoming, received: Instant) {
        let Ok(user) = UserId::parse(&incoming.user) else {
            return;
        };
        let snapshot = self.access.snapshot();
        if !snapshot.may_chat(&user) {
            self.react(&incoming.channel, &incoming.ts, UNAUTHORISED_REACTION)
                .await;
            return;
        }
        // Read before the message is queued behind a running turn: a moment later and `/stop`
        // cancels that turn, then runs as the prompt replacing it.
        if let Some(command) = thread_commands::parse(&incoming.text, &self.bot_user) {
            self.command(command, &incoming).await;
            return;
        }
        let conversation = Conversation {
            channel: incoming.channel.clone(),
            thread_ts: incoming
                .thread_ts
                .clone()
                .unwrap_or_else(|| incoming.ts.clone()),
        };
        let cancel = {
            let mut turns = lock(&self.turns);
            match turns.get_mut(&conversation) {
                Some(running) => {
                    running.queued.push(incoming.clone());
                    Some(running.interrupt(INTERRUPTED))
                }
                None => {
                    turns.insert(conversation.clone(), Running::default());
                    None
                }
            }
        };
        if let Some(cancel) = cancel {
            if let Some((link, session_id)) = cancel {
                cancel_turn(link, session_id);
            }
            return;
        }
        let (mut user, mut incoming, mut received) = (user, incoming, received);
        loop {
            let thinking = Thinking::start(self.slack.clone(), &conversation);
            let timing = self.converse(&user, &conversation, &incoming).await;
            thinking.stop().await;
            if self.turn_timings
                && let Some(timing) = timing
            {
                let ms = |at: Option<Instant>| at.map(|at| at.duration_since(received).as_millis());
                tracing::info!(
                    node = %timing.node,
                    accepted_ms = ?ms(Some(timing.accepted)),
                    first_output_ms = ?ms(timing.first_output),
                    finished_ms = received.elapsed().as_millis(),
                    "turn timing: from the Slack event to the node accepting, its first output, and the end"
                );
            }
            let queued = {
                let mut turns = lock(&self.turns);
                let queued = turns
                    .get_mut(&conversation)
                    .map(|running| std::mem::take(&mut running.queued))
                    .unwrap_or_default();
                if queued.is_empty() {
                    turns.remove(&conversation);
                } else {
                    turns.insert(conversation.clone(), Running::default());
                }
                queued
            };
            let Some(next) = self.merge(queued) else {
                return;
            };
            let Ok(next_user) = UserId::parse(&next.user) else {
                lock(&self.turns).remove(&conversation);
                return;
            };
            (user, incoming, received) = (next_user, next, Instant::now());
        }
    }

    /// Messages that arrived during a turn, as one: each on its own paragraph, named when more
    /// than one person spoke.
    fn merge(&self, mut queued: Vec<Incoming>) -> Option<Incoming> {
        // Events are handled concurrently, so arrival order is not Slack's order.
        queued.sort_by(|a, b| a.ts.cmp(&b.ts));
        let last = queued.last()?.clone();
        let speakers: HashSet<&str> = queued.iter().map(|i| i.user.as_str()).collect();
        let text = queued
            .iter()
            .map(|i| {
                let words = self.strip_mention(&i.text);
                if speakers.len() > 1 {
                    format!("<@{}>: {words}", i.user)
                } else {
                    words
                }
            })
            .collect::<Vec<_>>()
            .join("\n\n");
        let files = queued.iter().flat_map(|i| i.files.clone()).collect();
        Some(Incoming {
            text,
            files,
            ..last
        })
    }

    /// A path that leaves nothing in the thread posts a note, because it is then the only sign
    /// the message was read as a command rather than passed to the node as a prompt.
    async fn command(&self, command: Command, incoming: &Incoming) {
        let (note, blocks) = match &incoming.thread_ts {
            None => (
                Some(format!("`{command}` only works inside a thread.")),
                Vec::new(),
            ),
            Some(thread_ts) => {
                let conversation = Conversation {
                    channel: incoming.channel.clone(),
                    thread_ts: thread_ts.clone(),
                };
                match command {
                    Command::Stop => (self.stop(&conversation), Vec::new()),
                    Command::Node => {
                        let (note, blocks) = self.picker(&incoming.user, &conversation);
                        (Some(note), blocks)
                    }
                }
            }
        };
        let Some(note) = note else {
            return;
        };
        if let Err(err) = self
            .slack
            .post_ephemeral_blocks(
                &incoming.channel,
                incoming.thread_ts.as_deref(),
                &incoming.user,
                &note,
                &blocks,
            )
            .await
        {
            tracing::warn!(error = %err, "could not answer a thread-scoped command");
        }
    }

    /// Only the machines this person would be assigned anyway, so the menu cannot offer one the
    /// choice would then refuse.
    fn picker(&self, user: &str, conversation: &Conversation) -> (String, Vec<Block>) {
        let Ok(user) = UserId::parse(user) else {
            return ("I could not tell who you are.".to_owned(), Vec::new());
        };
        let choices = self.fleet.choices(&user, &self.access.snapshot());
        if choices.is_empty() {
            return ("No machine is connected right now.".to_owned(), Vec::new());
        }
        (
            picker::PROMPT.to_owned(),
            picker::view(conversation, &choices),
        )
    }

    /// No note once a turn is stopping: the notice after its reply says so, which tells the whole
    /// thread. Whatever queued behind it goes too, so the next message does not start now.
    fn stop(&self, conversation: &Conversation) -> Option<String> {
        let cancel = lock(&self.turns).get_mut(conversation).map(|running| {
            running.queued.clear();
            running.interrupt(STOPPED)
        });
        match cancel {
            Some(cancel) => {
                if let Some((link, session_id)) = cancel {
                    cancel_turn(link, session_id);
                }
                None
            }
            None => Some("Nothing to stop.".to_owned()),
        }
    }

    /// `/stop`, or any of the app's commands with the text `stop`. Slack will not run one inside
    /// a thread, so from a channel the useful answer is where the verb belongs.
    pub async fn on_slash(self: Arc<Self>, payload: serde_json::Value) {
        let text = |key: &str| payload[key].as_str().unwrap_or_default().trim().to_owned();
        let (command, words) = (text("command"), text("text"));
        let stop = command.eq_ignore_ascii_case("/stop")
            || words
                .split_whitespace()
                .next()
                .is_some_and(|word| word.eq_ignore_ascii_case("stop"));
        let (user, channel) = (text("user_id"), text("channel_id"));
        if !stop || !UserId::parse(&user).is_ok_and(|u| self.access.snapshot().may_chat(&u)) {
            return;
        }
        let thread_ts = payload["thread_ts"].as_str().map(str::to_owned);
        let note = match &thread_ts {
            None => Some(format!(
                "Slack does not run slash commands inside threads. Mention me with `{}` in the thread you want to stop.",
                Command::Stop
            )),
            Some(thread_ts) => self.stop(&Conversation {
                channel: channel.clone(),
                thread_ts: thread_ts.clone(),
            }),
        };
        let Some(note) = note else {
            return;
        };
        if let Err(err) = self
            .slack
            .post_ephemeral(&channel, thread_ts.as_deref(), &user, &note)
            .await
        {
            tracing::warn!(error = %err, "could not answer a slash command");
        }
    }

    async fn converse(
        &self,
        user: &UserId,
        conversation: &Conversation,
        incoming: &Incoming,
    ) -> Option<TurnTiming> {
        let text = self.strip_mention(&incoming.text);
        let mut retried = false;
        loop {
            let seat = self.seat(user, conversation, incoming).await?;
            let mut words = String::new();
            if let Some(history) = &seat.history {
                words.push_str(history);
                words.push_str("\n\n");
            }
            words.push_str(&text);
            let prompt = GatewayCall::Prompt(Prompt {
                session_id: seat.session_id.clone(),
                content: std::iter::once(ContentBlock::text(words))
                    .chain(
                        incoming
                            .files
                            .iter()
                            .map(|file| self.files.offer(&seat.node.selector, file)),
                    )
                    .map(Open::Known)
                    .collect(),
            });
            let pending = match seat.node.link.call(prompt).await {
                Ok(pending) => pending,
                Err(err) => {
                    self.alert(conversation, &faults::unreachable(&seat.node.name, &err))
                        .await;
                    return None;
                }
            };
            let reply = match tokio::time::timeout(PROMPT_TIMEOUT, pending.reply).await {
                Ok(reply) => reply,
                Err(_) => {
                    self.alert(
                        conversation,
                        &faults::silent(&seat.node.name, PROMPT_TIMEOUT),
                    )
                    .await;
                    return None;
                }
            };
            match reply {
                Ok(GatewayReply::Prompt(_)) => {
                    let accepted = Instant::now();
                    self.started(conversation, &seat.node, &seat.session_id);
                    let first_output = self
                        .stream(
                            conversation,
                            incoming,
                            &seat.node,
                            &seat.session_id,
                            pending.events,
                        )
                        .await;
                    return Some(TurnTiming {
                        node: seat.node.name.clone(),
                        accepted,
                        first_output,
                    });
                }
                Err(CallError::Fault(fault))
                    if fault.kind == ErrorKind::UnknownSession && !retried =>
                {
                    retried = true;
                    self.forget(conversation, &seat.node).await;
                }
                Err(CallError::Fault(fault)) if fault.kind == ErrorKind::SessionBusy => {
                    self.alert(conversation, &faults::busy(&seat.node.name))
                        .await;
                    return None;
                }
                Ok(other) => {
                    tracing::warn!(reply = ?other, "a prompt was answered with the wrong reply");
                    self.alert(
                        conversation,
                        &faults::not_taken(&seat.node.name, &Refusal::Mismatched),
                    )
                    .await;
                    return None;
                }
                Err(err) => {
                    self.alert(
                        conversation,
                        &faults::not_taken(&seat.node.name, &Refusal::Failed(err)),
                    )
                    .await;
                    return None;
                }
            }
        }
    }

    async fn forget(&self, conversation: &Conversation, node: &Node) {
        if let Err(err) = self.store.remove_pin(conversation).await {
            tracing::warn!(error = %err, "could not drop a pin");
        }
        self.fleet.session_ended(&node.selector);
    }

    async fn seat(
        &self,
        user: &UserId,
        conversation: &Conversation,
        incoming: &Incoming,
    ) -> Option<Seat> {
        let pinned = match self.store.pin(conversation).await {
            Ok(pinned) => pinned,
            Err(err) => {
                tracing::warn!(error = %err, "could not read a conversation's pin");
                None
            }
        };
        if let Some(pin) = &pinned {
            if let Some(node) = self.fleet.get(&pin.node).filter(|node| node.connected) {
                let moved = lock(&self.moved).remove(conversation);
                let history = match moved {
                    true => self.history(conversation, &incoming.ts).await,
                    false => None,
                };
                return Some(Seat {
                    node,
                    session_id: SessionId(pin.session_id.clone()),
                    history,
                });
            }
            let _ = self.store.remove_pin(conversation).await;
            self.fleet.session_ended(&pin.node);
            lock(&self.orphaned).insert(conversation.clone());
        }
        let snapshot = self.access.snapshot();
        let Some(node) = self.fleet.assign(user, &snapshot) else {
            let orphaned = lock(&self.orphaned).contains(conversation);
            self.alert(conversation, &no_machine(orphaned)).await;
            return None;
        };
        let orphaned = lock(&self.orphaned).remove(conversation);
        let history = if incoming.thread_ts.is_some() || orphaned {
            self.history(conversation, &incoming.ts).await
        } else {
            None
        };
        // Which of the admin's own machines took a thread is not news; a machine going offline is,
        // and whose machine picked the thread up matters once somebody else's can.
        let notice = match (history.is_some(), orphaned) {
            (false, _) => None,
            (true, true) => Some(format!(
                "the machine serving this conversation went offline. Continuing on _*{}*_, catching up from this thread.",
                render::escape(&node.name)
            )),
            (true, false) => (snapshot.admin() != Some(&node.owner)).then(|| {
                format!(
                    "picking this conversation up on _*{}*_, catching up from this thread.",
                    render::escape(&node.name)
                )
            }),
        };
        if let Some(notice) = notice {
            self.notice(conversation, &notice).await;
        }
        let session_id = match self.open_session(&node, conversation).await {
            Ok(session_id) => session_id,
            Err(refusal) => {
                self.fleet.session_ended(&node.selector);
                self.alert(conversation, &faults::no_session(&node.name, &refusal))
                    .await;
                return None;
            }
        };
        let pin = Pin {
            conversation: conversation.clone(),
            node: node.selector.clone(),
            session_id: session_id.0.clone(),
            user: user.clone(),
            pinned_at: OffsetDateTime::now_utc(),
        };
        if let Err(err) = self.store.set_pin(&pin).await {
            tracing::warn!(error = %err, "could not pin a conversation");
        }
        Some(Seat {
            node,
            session_id,
            history,
        })
    }

    async fn open_session(
        &self,
        node: &Node,
        conversation: &Conversation,
    ) -> Result<SessionId, Refusal> {
        let call = GatewayCall::NewSession(NewSession {
            context: vec![Open::Known(thread_link(conversation))],
        });
        let pending = node.link.call(call).await?;
        match pending.reply.await {
            Ok(GatewayReply::NewSession(created)) => Ok(created.session_id),
            Ok(other) => {
                tracing::warn!(reply = ?other, "a new session was answered with the wrong reply");
                Err(Refusal::Mismatched)
            }
            Err(err) => Err(Refusal::Failed(err)),
        }
    }

    async fn history(&self, conversation: &Conversation, current: &str) -> Option<String> {
        let messages = match self
            .slack
            .replies(&conversation.channel, &conversation.thread_ts)
            .await
        {
            Ok(messages) => messages,
            Err(err) => {
                tracing::warn!(error = %err, "could not read the thread to catch up");
                return None;
            }
        };
        let lines: Vec<String> = messages
            .iter()
            .filter(|message| message.ts != current)
            .filter(|message| !message.text.trim().is_empty())
            .map(|message| {
                let who = match (&message.user, &message.bot_id) {
                    (Some(user), _) if *user == self.bot_user => "you (the assistant)".to_owned(),
                    (_, Some(_)) => "a bot".to_owned(),
                    (Some(user), None) => format!("<@{user}>"),
                    (None, None) => "someone".to_owned(),
                };
                format!("{who}: {}", message.text)
            })
            .collect();
        (!lines.is_empty()).then(|| {
            format!(
                "This conversation continues a Slack thread. Earlier messages, oldest first:\n{}\n\nThe new message:",
                lines.join("\n")
            )
        })
    }

    /// Lets a message or `/stop` that arrives from now on cancel the turn, and cancels at once
    /// if one already did.
    fn started(&self, conversation: &Conversation, node: &Node, session_id: &SessionId) {
        let cancel = lock(&self.turns).get_mut(conversation).and_then(|running| {
            running.session = Some((node.link.clone(), session_id.clone()));
            running
                .marker
                .is_some()
                .then(|| running.session.clone())
                .flatten()
        });
        if let Some((link, session_id)) = cancel {
            cancel_turn(link, session_id);
        }
    }

    async fn stream(
        &self,
        conversation: &Conversation,
        incoming: &Incoming,
        node: &Node,
        session_id: &SessionId,
        mut events: StreamEvents,
    ) -> Option<Instant> {
        let recipient = (!incoming.direct).then(|| (self.team.clone(), incoming.user.clone()));
        let mut first_output = None;
        let turn = Turn::default();
        let mut active = Instant::now();
        let mut stalled = false;
        // The first fault is the diagnosis; anything after it is fallout from the same cause.
        let mut failure: Option<rax::Error> = None;
        let mut reply = Reply::new(
            self.slack.clone(),
            Target {
                channel: conversation.channel.clone(),
                thread_ts: conversation.thread_ts.clone(),
                recipient,
            },
        );
        loop {
            let due = reply.due();
            let event = tokio::select! {
                event = events.recv() => event,
                () = sleep_until_due(due) => {
                    reply.flush_due().await;
                    continue;
                }
                () = tokio::time::sleep_until(active + self.turn_idle_timeout) => {
                    if turn.waiting() || self.sign_ins.pending_for(&node.selector) {
                        active = Instant::now();
                        continue;
                    }
                    stalled = true;
                    break;
                }
            };
            let Some(event) = event else {
                break;
            };
            active = Instant::now();
            let written = reply.has_written();
            if let Some(reported) = self.show(&mut reply, node, event, &turn).await {
                failure.get_or_insert(reported);
            }
            if !written && reply.has_written() {
                first_output.get_or_insert_with(Instant::now);
            }
        }
        turn.token.cancel();
        let marker = lock(&self.turns)
            .get(conversation)
            .and_then(|running| running.marker);
        // How a turn ended is the gateway talking, not the agent, so it follows the answer as a
        // notice of its own rather than as a last italic line inside it.
        let ending = if stalled {
            cancel_turn(node.link.clone(), session_id.clone());
            Some(format!(
                "the agent went quiet for {}, so I stopped this turn.",
                span(self.turn_idle_timeout)
            ))
        } else if let Some(marker) = marker {
            Some(marker.to_owned())
        } else if node.link.is_closed() {
            Some(format!(
                "_*{}*_ went offline before finishing this answer.",
                render::escape(&node.name)
            ))
        } else if !reply.has_written() && failure.is_none() {
            Some("done, with nothing to say.".to_owned())
        } else {
            None
        };
        reply.finish().await;
        // A fault is the gateway's to explain, so it follows the answer as its own card rather
        // than as a last italic line inside the agent's words.
        if let Some(reported) = &failure {
            self.alert(conversation, &faults::turn_failed(&node.name, reported))
                .await;
        }
        if let Some(ending) = ending {
            self.notice(conversation, &ending).await;
        }
        first_output
    }

    /// Draws one event into the reply, and hands back the fault the turn reported, if it did:
    /// that one is not the agent talking, so it is not written into what the agent said.
    async fn show(
        &self,
        reply: &mut Reply,
        node: &Node,
        event: Open<TurnEvent>,
        turn: &Turn,
    ) -> Option<rax::Error> {
        match event {
            Open::Known(TurnEvent::Message {
                content: Open::Known(ContentBlock::Text { text }),
            }) => reply.text(&text).await,
            Open::Known(TurnEvent::ToolCall { tool_call }) => {
                let id = tool_call.id.0.clone();
                let title = tool_call
                    .title
                    .clone()
                    .unwrap_or_else(|| tool_call.name.clone());
                self.rule(reply.target(), node, tool_call, turn).await;
                reply.task(&id, Some(&title), TaskStatus::InProgress).await;
            }
            Open::Known(TurnEvent::ToolCallUpdate { tool_call_update }) => {
                let status = match tool_call_update.status {
                    ToolCallStatus::InProgress => TaskStatus::InProgress,
                    ToolCallStatus::Completed => TaskStatus::Complete,
                    ToolCallStatus::Failed | ToolCallStatus::Denied => TaskStatus::Error,
                };
                reply
                    .task(
                        &tool_call_update.id.0,
                        tool_call_update.title.as_deref(),
                        status,
                    )
                    .await;
            }
            Open::Known(TurnEvent::Question { question }) => {
                self.put(reply.target(), node, Asked::Questions(question), turn);
            }
            Open::Known(TurnEvent::Plan { plan }) => {
                self.put(reply.target(), node, Asked::Plan(plan), turn);
            }
            Open::Known(TurnEvent::SignIn { sign_in }) => {
                let id = sign_in.id.clone();
                let raised_by = signin::Node {
                    selector: node.selector.clone(),
                    name: node.name.clone(),
                    owner: node.owner.clone(),
                    link: node.link.clone(),
                };
                let thread = Some((
                    reply.target().channel.clone(),
                    reply.target().thread_ts.clone(),
                ));
                if let Err(err) = self.sign_ins.raise(raised_by, sign_in, thread).await {
                    tracing::warn!(error = %err.message, "could not show a sign-in");
                    self.unavailable(node, id).await;
                }
            }
            Open::Known(TurnEvent::SignInSettled { sign_in_settled }) => {
                self.sign_ins.settle(&node.selector, sign_in_settled).await;
            }
            Open::Known(TurnEvent::Error { error }) => return Some(error),
            Open::Known(TurnEvent::Attachment { attachment }) => {
                let name = attachment
                    .filename
                    .clone()
                    .unwrap_or_else(|| "attachment".to_owned());
                if let Err(reason) = self.attach(reply.target(), node, attachment).await {
                    tracing::warn!(node = %node.name, file = %name, %reason, "could not attach a file");
                    reply
                        .text(&format!("\n\n_Could not attach {name}: {reason}_\n\n"))
                        .await;
                }
            }
            Open::Known(TurnEvent::Complete { .. }) => {}
            other => tracing::debug!(event = ?other, "turn event not shown yet"),
        }
        None
    }

    /// Allows, denies, or puts the call to the node's owner, as their tool mode says.
    async fn rule(&self, target: &Target, node: &Node, tool: ToolCall, turn: &Turn) {
        let config = match self.store.user(&node.owner).await {
            Ok(config) => config,
            Err(err) => {
                tracing::warn!(error = %err, "could not read the owner's tool rules; asking them");
                UserConfig {
                    tool_mode: ToolMode::AllowedWhitelist,
                    ..UserConfig::new(node.owner.clone())
                }
            }
        };
        let decision = match config.tool_mode {
            _ if talks_to_people(&tool.name) => Decision::Allow,
            ToolMode::AlwaysAllowed => Decision::Allow,
            ToolMode::AllowedWhitelist if config.whitelist.contains(&tool.name) => Decision::Allow,
            ToolMode::Denied => Decision::Deny {
                by: DeniedBy::Policy,
                reason: Some("The node's owner does not allow tools.".into()),
            },
            ToolMode::AllowedWhitelist => {
                let ask = approval::Ask {
                    slack: self.slack.clone(),
                    store: self.store.clone(),
                    link: node.link.clone(),
                    node_name: node.name.clone(),
                    owner: node.owner.clone(),
                    channel: target.channel.clone(),
                    thread_ts: target.thread_ts.clone(),
                    tool,
                    timeout: self.approval_timeout,
                    turn: turn.token.clone(),
                };
                let approvals = self.approvals.clone();
                let waiting = turn.wait();
                tokio::spawn(async move {
                    approvals.ask(ask).await;
                    drop(waiting);
                });
                return;
            }
        };
        let verdict = ToolVerdict {
            id: tool.id,
            decision,
        };
        if let Err(err) = node.link.verdict(verdict).await {
            tracing::warn!(error = %err, "could not rule on a tool call");
        }
    }

    fn put(&self, target: &Target, node: &Node, asked: Asked, turn: &Turn) {
        let put = prompts::Put {
            slack: self.slack.clone(),
            link: node.link.clone(),
            channel: target.channel.clone(),
            thread_ts: target.thread_ts.clone(),
            asked,
            timeout: self.prompt_timeout,
            turn: turn.token.clone(),
        };
        let prompts = self.prompts.clone();
        let waiting = turn.wait();
        tokio::spawn(async move {
            prompts.put(put).await;
            drop(waiting);
        });
    }

    async fn attach(
        &self,
        target: &Target,
        node: &Node,
        attachment: Attachment,
    ) -> Result<(), String> {
        let bytes = self
            .files
            .claim(&node.selector, &attachment.transfer_id, ATTACHMENT_WAIT)
            .await?;
        if bytes.len() as u64 != attachment.size {
            return Err(format!(
                "{} bytes arrived, {} were announced",
                bytes.len(),
                attachment.size
            ));
        }
        let upload = Upload {
            channel: target.channel.clone(),
            thread_ts: Some(target.thread_ts.clone()),
            filename: attachment
                .filename
                .unwrap_or_else(|| "attachment".to_owned()),
            title: attachment.title,
            initial_comment: attachment.comment,
            bytes,
        };
        self.slack
            .upload_file(&upload)
            .await
            .map(drop)
            .map_err(|err| err.to_string())
    }

    async fn unavailable(&self, node: &Node, id: rax::id::PromptId) {
        let answer = DisplayAnswer {
            id,
            outcome: DisplayOutcome::Unavailable,
            answers: Default::default(),
            choice: None,
            user_id: None,
            note: Some("This gateway cannot show that prompt yet.".into()),
            code: None,
        };
        if let Err(err) = node.link.answer(answer).await {
            tracing::warn!(error = %err, "could not answer a prompt");
        }
    }

    fn strip_mention(&self, text: &str) -> String {
        text.replace(&format!("<@{}>", self.bot_user), "")
            .trim()
            .to_owned()
    }

    async fn react(&self, channel: &str, ts: &str, name: &str) {
        if let Err(err) = self.slack.add_reaction(channel, ts, name).await {
            tracing::warn!(error = %err, "could not react");
        }
    }

    async fn alert(&self, conversation: &Conversation, alert: &Alert) {
        let message = alert.message(&conversation.channel, Some(&conversation.thread_ts));
        if let Err(err) = self.slack.post_message(&message).await {
            tracing::warn!(error = %err, "could not post an alert in a thread");
        }
    }

    /// The gateway talking about itself rather than answering: a context block, so a note about
    /// a machine never reads like something the agent said.
    async fn notice(&self, conversation: &Conversation, body: &str) {
        let text = notice(body);
        let message = PostMessage {
            channel: conversation.channel.clone(),
            thread_ts: Some(conversation.thread_ts.clone()),
            text: text.clone(),
            blocks: vec![Block::Context(vec![Text::Mrkdwn(text)])],
        };
        if let Err(err) = self.slack.post_message(&message).await {
            tracing::warn!(error = %err, "could not post in a thread");
        }
    }
}

/// Every gateway notice wears the same label, so one is told from an answer at a glance. The
/// `text` carries it too: it is what a notification and an unformatted client show.
fn notice(body: &str) -> String {
    format!("_*Notice*_: {body}")
}

fn no_machine(orphaned: bool) -> Alert {
    let next_steps =
        "Start a node (`riggs run`) and try again, or ask the gateway admin to let you use theirs.";
    if orphaned {
        Alert {
            level: Some(Level::Warn),
            title: "Machine offline".into(),
            subtitle: Some("The machine this conversation was running on is offline.".into()),
            reason: Some(
                "It is not connected, and no other machine you may use can take the conversation over."
                    .into(),
            ),
            next_steps: Some(next_steps.into()),
            ..Alert::default()
        }
    } else {
        Alert {
            level: Some(Level::Warn),
            title: "No machine available".into(),
            subtitle: Some("No machine is available to run this conversation.".into()),
            reason: Some("None of the machines you may use is connected.".into()),
            next_steps: Some(next_steps.into()),
            ..Alert::default()
        }
    }
}

/// The node's own tools for reaching the people in the conversation. They do nothing to the
/// owner's machine, so asking the owner's permission to ask them a question helps nobody.
const TALKING_TOOLS: [&str; 4] = [
    "mcp__riggs__ask",
    "mcp__riggs__present_plan",
    "mcp__riggs__attach",
    "mcp__riggs__auth",
];

fn talks_to_people(tool: &str) -> bool {
    TALKING_TOOLS.contains(&tool)
}

const INTERRUPTED: &str = "interrupted by a newer message.";
const STOPPED: &str = "stopped.";

/// A conversation with a turn under way: the session a newer message or `/stop` cancels, and
/// the messages that arrived meanwhile, which run together once the turn ends.
#[derive(Default)]
struct Running {
    session: Option<(GatewayLink, SessionId)>,
    queued: Vec<Incoming>,
    /// Set once the turn was cancelled, to say why in the notice after its reply.
    marker: Option<&'static str>,
}

impl Running {
    /// Returns the session to cancel, unless it was already cancelled or has not started.
    fn interrupt(&mut self, marker: &'static str) -> Option<(GatewayLink, SessionId)> {
        if self.marker.is_some() {
            return None;
        }
        self.marker = Some(marker);
        self.session.clone()
    }
}

fn cancel_turn(link: GatewayLink, session_id: SessionId) {
    tokio::spawn(async move {
        let cancel = GatewayCall::Cancel(SessionRef { session_id });
        match link.call(cancel).await {
            Ok(pending) => {
                let _ = pending.reply.await;
            }
            Err(err) => tracing::debug!(error = %err, "could not cancel a turn"),
        }
    });
}

/// One turn's cards: dismissed together when it ends, and while any waits on a person the
/// turn is not idle.
#[derive(Default)]
struct Turn {
    token: CancellationToken,
    waiting: Arc<AtomicUsize>,
}

struct Waiting(Arc<AtomicUsize>);

impl Drop for Waiting {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::SeqCst);
    }
}

impl Turn {
    fn wait(&self) -> Waiting {
        self.waiting.fetch_add(1, Ordering::SeqCst);
        Waiting(self.waiting.clone())
    }

    fn waiting(&self) -> bool {
        self.waiting.load(Ordering::SeqCst) > 0
    }
}

pub(crate) fn span(duration: Duration) -> String {
    match duration.as_secs() {
        s if s >= 120 => format!("{} minutes", s / 60),
        60..=119 => "a minute".to_owned(),
        s => format!("{s} seconds"),
    }
}

struct TurnTiming {
    node: String,
    accepted: Instant,
    first_output: Option<Instant>,
}

/// Slack's "is thinking..." line under the thread, kept up for the whole turn.
struct Thinking {
    stop: tokio_util::sync::CancellationToken,
    task: tokio::task::JoinHandle<()>,
    slack: SlackClient,
    conversation: Conversation,
}

impl Thinking {
    fn start(slack: SlackClient, conversation: &Conversation) -> Self {
        let stop = tokio_util::sync::CancellationToken::new();
        let task = tokio::spawn({
            let (slack, stop) = (slack.clone(), stop.clone());
            let conversation = conversation.clone();
            async move {
                loop {
                    let shown = slack
                        .set_thread_status(&conversation.channel, &conversation.thread_ts, THINKING)
                        .await;
                    if let Err(err) = shown {
                        tracing::warn!(error = %err, "could not show that the agent is thinking");
                        return;
                    }
                    tokio::select! {
                        () = tokio::time::sleep(THINKING_REFRESH) => {}
                        () = stop.cancelled() => return,
                    }
                }
            }
        });
        Self {
            stop,
            task,
            slack,
            conversation: conversation.clone(),
        }
    }

    async fn stop(self) {
        self.stop.cancel();
        let _ = self.task.await;
        let cleared = self
            .slack
            .set_thread_status(&self.conversation.channel, &self.conversation.thread_ts, "")
            .await;
        if let Err(err) = cleared {
            tracing::debug!(error = %err, "could not clear the thinking status");
        }
    }
}

async fn sleep_until_due(due: Option<Instant>) {
    match due {
        Some(due) => tokio::time::sleep_until(due).await,
        None => std::future::pending().await,
    }
}
