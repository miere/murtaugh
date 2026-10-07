//! A conversation is a Slack thread pinned to one node's session. This turns Slack messages into
//! RAX prompts and streams each turn back into the thread.

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use murtaugh_slack::{
    Block, Click, Event, FileRef, HomeClick, PostMessage, SlackClient, TaskStatus, Text, Upload,
    ViewSubmission,
};
use murtaugh_store::{Conversation, Pin, Store, UserId};
use rax::attachment::Attachment;
use rax::content::ContentBlock;
use rax::event::BackgroundEvent;
use rax::id::SessionId;
use rax::interaction::{DisplayAnswer, DisplayOutcome};
use rax::open::Subject;
use rax::session::{NewSession, Prompt, SessionRef};
use rax::tool::{ToolCall, ToolCallStatus, ToolGroup, ToolVerdict};
use rax::{ErrorKind, Event as TurnEvent, GatewayCall, GatewayReply, Open, Unhandled};
use rax_tokio::CallError;
use rax_tokio::gateway::{GatewayLink, StreamEvents};
use time::OffsetDateTime;
use tokio::sync::mpsc;
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;

use crate::access::Access;
use crate::alerts::{self, Alert, Level};
use crate::approval::{self, Approvals};
use crate::faults::{self, Refusal};
use crate::files::Files;
use crate::fleet::{Fleet, Node};
use crate::hub::{Background, FleetChange};
use crate::panel::Panel;
use crate::picker;
use crate::policy::{self, Ruling};
use crate::prompts::{self, Asked, Prompts};
use crate::render;
use crate::reply::{Reply, Target, TaskCard};
use crate::signin::{self, SignIns};
use crate::thread_commands::{self, Command};
use crate::tools::{Lent, Tools};

pub const UNAUTHORISED_REACTION: &str = "zipper_mouth_face";
/// The subtype Slack gives a DM that carries files; it is still the person talking.
const FILE_SHARE: &str = "file_share";
pub const THINKING: &str = "is thinking...";
/// The Go gateway's `request_timeout`: silence, not length, is what stops a turn.
pub const TURN_IDLE_TIMEOUT: Duration = Duration::from_secs(10 * 60);
/// How long one tool call may hold a turn open while saying nothing. An in-flight tool keeps the
/// idle clock from stopping the turn, so without a ceiling a wedged one — blocked on stdin, or on
/// a person who walked away — would hold it for as long as the node stayed up.
pub const TOOL_CEILING: Duration = Duration::from_secs(60 * 60);
/// The chunks precede the `attachment` event on the link, so the bytes are normally there already.
const ATTACHMENT_WAIT: Duration = Duration::from_secs(60);
/// Slack clears the status once a chunk lands, so it is re-asserted until the turn ends.
pub const THINKING_REFRESH: Duration = Duration::from_secs(2);
/// Long enough for a node to fetch the files a prompt links before accepting it.
pub const PROMPT_TIMEOUT: Duration = Duration::from_secs(120);
/// How often a background run that has finished checks whether the cards it raised are settled.
const SETTLE_POLL: Duration = Duration::from_millis(500);
/// How long a `Send Again` button stays live. Past this the conversation has almost certainly
/// moved on, and replaying a message into it would be a surprise rather than a retry.
const RETRY_TTL: Duration = Duration::from_secs(30 * 60);
/// A ceiling on messages kept back for a button, so a gateway nobody clicks does not hold every
/// failure it has ever seen.
const RETRIES_MOST: usize = 256;

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
    tool_ceiling: Duration,
    tools: Tools,
    lent: Lent,
    /// Messages a retryable failure left on the table, by the id their card's button carries.
    retries: Mutex<HashMap<String, Offer>>,
    /// The background run of each session that has one, by node and session: the one task that
    /// draws them keeps a session's events in the order the node sent them.
    backgrounds: Mutex<HashMap<(String, SessionId), BackgroundEvents>>,
    panel: Panel,
}

type BackgroundEvents = mpsc::UnboundedSender<Open<BackgroundEvent>>;

/// Something the gateway must say about a turn once the agent has finished saying its piece.
enum Aftermath {
    /// The turn reported a fault of its own partway through.
    Failed(rax::Error),
    /// A file the agent sent whose bytes never arrived whole.
    Unattached { name: String, reason: String },
}

/// A message held back so its card can send it again, and who may press the button.
struct Offer {
    user: UserId,
    incoming: Incoming,
    at: Instant,
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
    /// Set when the message came from the workloads endpoint rather than from Slack.
    workload: Option<WorkloadRun>,
}

/// A workload as the chat side runs it: the person whose token posted it.
#[derive(Debug, Clone)]
pub struct WorkloadRun {
    pub caller: UserId,
}

/// A workload ready to run: checked, with its thread resolved.
#[derive(Debug, Clone)]
pub struct Workload {
    pub owner: UserId,
    pub channel: String,
    /// The thread to continue, or `None` to start one with an opening message.
    pub thread_ts: Option<String>,
    pub prompt: String,
    pub direct: bool,
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
    pub tool_ceiling: Duration,
    pub tools: Tools,
    pub lent: Lent,
    pub relay: crate::relay::Relay,
    /// Shared with the relay, so a card it raised is answered by the same click handler.
    pub approvals: Approvals,
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
            tool_ceiling,
            tools,
            lent,
            relay,
            approvals,
        } = parts;
        let panel = Panel {
            slack: slack.clone(),
            store: store.clone(),
            access: access.clone(),
            fleet: fleet.clone(),
            relay,
            bot_user: bot_user.clone(),
        };
        Arc::new(Self {
            panel,
            slack,
            bot_user,
            store,
            access,
            fleet,
            files,
            team,
            turn_timings,
            approvals,
            approval_timeout,
            prompts: Prompts::default(),
            prompt_timeout,
            sign_ins,
            orphaned: Mutex::new(HashSet::new()),
            moved: Mutex::new(HashSet::new()),
            turns: Mutex::new(HashMap::new()),
            turn_idle_timeout,
            tool_ceiling,
            tools,
            lent,
            retries: Mutex::new(HashMap::new()),
            backgrounds: Mutex::new(HashMap::new()),
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
                workload: None,
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
                    workload: None,
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
        } else if click.action_id == faults::SEND_AGAIN {
            self.send_again(&click)
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

    /// Keeps a failed message so a card's button can send it again, and hands back the id that
    /// button carries. Keying by the message means a conversation that fails twice over the same
    /// message holds it once, and pressing either card's button sends that one message.
    fn offer_retry(&self, user: &UserId, incoming: &Incoming) -> String {
        let id = format!("{}/{}", incoming.channel, incoming.ts);
        let mut retries = lock(&self.retries);
        retries.retain(|_, offer| offer.at.elapsed() < RETRY_TTL);
        while retries.len() >= RETRIES_MOST {
            let oldest = retries
                .iter()
                .min_by_key(|(_, offer)| offer.at)
                .map(|(id, _)| id.clone());
            match oldest {
                Some(oldest) => drop(retries.remove(&oldest)),
                None => break,
            }
        }
        retries.insert(
            id.clone(),
            Offer {
                user: user.clone(),
                incoming: incoming.clone(),
                at: Instant::now(),
            },
        );
        id
    }

    /// Sends a failed message again from its card. The offer is taken rather than read, so one
    /// card sends one message however many times it is pressed, and the turn runs on its own: a
    /// click is answered in seconds and a turn is not.
    fn send_again(self: &Arc<Self>, click: &Click) -> Option<String> {
        let lapsed = "That message is no longer on offer. Send it again in the thread.";
        let Ok(user) = UserId::parse(&click.user) else {
            return None;
        };
        if !self.access.snapshot().may_chat(&user) {
            return None;
        }
        let taken = {
            let mut retries = lock(&self.retries);
            match retries.get(&click.value) {
                // Sending it again sends it as the person who wrote it, so only they may.
                Some(offer) if offer.user != user => {
                    return Some("That was not your message to send again.".to_owned());
                }
                Some(offer) if offer.at.elapsed() >= RETRY_TTL => {
                    retries.remove(&click.value);
                    None
                }
                Some(_) => retries.remove(&click.value).map(|offer| offer.incoming),
                None => None,
            }
        };
        let Some(incoming) = taken else {
            return Some(lapsed.to_owned());
        };
        let chat = Arc::clone(self);
        tokio::spawn(async move { chat.message(incoming, Instant::now()).await });
        Some("Sending that message again.".to_owned())
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
        if self.shut_out_of(&conversation, &user).await {
            return Some(
                "This thread runs on a machine whose owner has not let you in.".to_owned(),
            );
        }
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
        if let Ok(viewer) = UserId::parse(viewer) {
            self.panel.show(&viewer).await;
        }
    }

    pub async fn on_home_click(self: Arc<Self>, click: HomeClick) {
        self.panel.click(click).await;
    }

    pub async fn on_view_submission(self: Arc<Self>, submission: ViewSubmission) {
        self.panel.submit(submission).await;
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

    /// Hands an event to its session's background run, starting one if none is going.
    pub fn on_background(self: &Arc<Self>, background: Background) {
        let Background {
            selector,
            session_id,
            event,
        } = background;
        let key = (selector, session_id);
        let mut runs = lock(&self.backgrounds);
        let event = match runs.get(&key) {
            Some(run) => match run.send(event) {
                Ok(()) => return,
                Err(mpsc::error::SendError(event)) => event,
            },
            None => event,
        };
        let (run, events) = mpsc::unbounded_channel();
        let _ = run.send(event);
        runs.insert(key.clone(), run);
        tokio::spawn(self.clone().background(key, events));
    }

    /// What a session does between turns: a background task finishing, the answer the agent
    /// writes about it, a tool one of its sub-agents wants. Without this, all of it is lost and
    /// the agent looks as if it never woke up.
    async fn background(
        self: Arc<Self>,
        key: (String, SessionId),
        mut events: mpsc::UnboundedReceiver<Open<BackgroundEvent>>,
    ) {
        let (selector, session_id) = &key;
        loop {
            match self.pinned(selector, session_id).await {
                Some((pin, node)) => self.show_background(&pin, &node, &mut events).await,
                None => {
                    tracing::debug!(node = %selector, %session_id, "background event for a session no thread is pinned to");
                    while events.try_recv().is_ok() {}
                }
            }
            // Under the lock, so an event sent while this run was ending starts the next one here
            // rather than landing in a channel nobody reads.
            let mut runs = lock(&self.backgrounds);
            if events.is_empty() {
                runs.remove(&key);
                return;
            }
        }
    }

    /// The pin of the thread a node's session answers in, and the node while it is attached.
    async fn pinned(&self, selector: &str, session_id: &SessionId) -> Option<(Pin, Node)> {
        let pins = match self.store.pins().await {
            Ok(pins) => pins,
            Err(err) => {
                tracing::warn!(error = %err, "could not read the pins to place a background event");
                return None;
            }
        };
        let pin = pins
            .into_iter()
            .find(|pin| pin.node == selector && pin.session_id == session_id.0)?;
        let node = self.fleet.get(selector)?;
        Some((pin, node))
    }

    /// One stretch of background work, drawn as its own reply the way a turn is. It ends when the
    /// agent completes and every card it raised is settled, or when it goes quiet; nothing is
    /// cancelled then, because there is no turn to cancel.
    async fn show_background(
        &self,
        pin: &Pin,
        node: &Node,
        events: &mut mpsc::UnboundedReceiver<Open<BackgroundEvent>>,
    ) {
        let conversation = &pin.conversation;
        let recipient =
            (!is_direct(&conversation.channel)).then(|| (self.team.clone(), pin.user.to_string()));
        let thinking = Thinking::start(self.slack.clone(), conversation);
        let turn = Turn::default();
        let mut reply = Reply::new(
            self.slack.clone(),
            Target {
                channel: conversation.channel.clone(),
                thread_ts: conversation.thread_ts.clone(),
                recipient,
            },
        );
        let mut active = Instant::now();
        let mut done = false;
        let mut failure: Option<rax::Error> = None;
        let mut unattached: Vec<(String, String)> = Vec::new();
        loop {
            // An approval ends as a deny once its run does, so a run the agent finished stays up
            // until the cards its sub-agents raised are answered.
            if done && !turn.waiting() {
                break;
            }
            let due = reply.due();
            let event = tokio::select! {
                event = events.recv() => event,
                () = sleep_until_due(due) => {
                    reply.flush_due().await;
                    continue;
                }
                () = tokio::time::sleep(SETTLE_POLL), if done => continue,
                () = tokio::time::sleep_until(active + self.turn_idle_timeout) => {
                    let working = turn.waiting()
                        || turn
                            .longest_tool()
                            .is_some_and(|(_, age)| age < self.tool_ceiling);
                    if working {
                        active = Instant::now();
                        continue;
                    }
                    break;
                }
            };
            let Some(event) = event else {
                break;
            };
            active = Instant::now();
            let event = match event {
                Open::Known(event) => event,
                Open::Unknown { name, .. } => {
                    tracing::debug!(%name, "background event of a kind this gateway does not know");
                    continue;
                }
            };
            done |= matches!(
                event,
                BackgroundEvent::Complete { .. } | BackgroundEvent::Error { .. }
            );
            match self
                .show(&mut reply, node, Open::Known(as_turn(event)), &turn)
                .await
            {
                Some(Aftermath::Failed(reported)) => drop(failure.get_or_insert(reported)),
                Some(Aftermath::Unattached { name, reason }) => unattached.push((name, reason)),
                None => {}
            }
        }
        turn.token.cancel();
        thinking.stop().await;
        reply.finish().await;
        if !unattached.is_empty() {
            self.alert(conversation, &faults::unattached(&node.name, &unattached))
                .await;
        }
        if let Some(reported) = &failure {
            self.alert(conversation, &faults::turn_failed(&node.name, reported))
                .await;
        }
    }

    /// Whether a turn is running, or queued, in this conversation.
    pub fn is_busy(&self, channel: &str, thread_ts: &str) -> bool {
        let conversation = Conversation {
            channel: channel.to_owned(),
            thread_ts: thread_ts.to_owned(),
        };
        lock(&self.turns).contains_key(&conversation)
    }

    /// Runs a workload as if its owner had written the prompt in the thread: an opening message
    /// marks who sent it, and starts the thread when there is none. Returns the thread.
    pub async fn start_workload(
        self: &Arc<Self>,
        workload: Workload,
    ) -> Result<(String, String), String> {
        let first_line = workload.prompt.lines().next().unwrap_or_default();
        let opening = PostMessage {
            channel: workload.channel.clone(),
            thread_ts: workload.thread_ts.clone(),
            text: format!(
                "<@{}> sent a workload:\n>{}",
                workload.owner,
                render::escape(&alerts::clip(first_line, 200))
            ),
            blocks: Vec::new(),
        };
        let posted = self
            .slack
            .post_message(&opening)
            .await
            .map_err(|err| format!("could not post into {}: {err}", workload.channel))?;
        let thread_ts = workload
            .thread_ts
            .clone()
            .unwrap_or_else(|| posted.ts.clone());
        let incoming = Incoming {
            user: workload.owner.to_string(),
            channel: posted.channel.clone(),
            ts: posted.ts,
            thread_ts: workload.thread_ts,
            text: workload.prompt,
            files: Vec::new(),
            direct: workload.direct,
            workload: Some(WorkloadRun {
                caller: workload.owner,
            }),
        };
        let chat = self.clone();
        tokio::spawn(async move { chat.message(incoming, Instant::now()).await });
        Ok((posted.channel, thread_ts))
    }

    async fn message(&self, incoming: Incoming, received: Instant) {
        let Ok(user) = UserId::parse(&incoming.user) else {
            return;
        };
        let snapshot = self.access.snapshot();
        let conversation = Conversation {
            channel: incoming.channel.clone(),
            thread_ts: incoming
                .thread_ts
                .clone()
                .unwrap_or_else(|| incoming.ts.clone()),
        };
        // Before commands too: `/stop` and `/node` steer the owner's machine as surely as a
        // prompt does.
        if !snapshot.may_chat(&user) || self.shut_out_of(&conversation, &user).await {
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
        let (mut user, mut batch, mut received) = (user, vec![incoming], received);
        loop {
            let thinking = Thinking::start(self.slack.clone(), &conversation);
            let timing = self.converse(&user, &conversation, &batch).await;
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
            // Kept apart until the turn's machine is known, because whose words may reach it
            // depends on whose machine it is.
            let mut queued = queued;
            queued.sort_by(|a, b| a.ts.cmp(&b.ts));
            let Some(last) = queued.last() else {
                return;
            };
            let Ok(next_user) = UserId::parse(&last.user) else {
                lock(&self.turns).remove(&conversation);
                return;
            };
            (user, batch, received) = (next_user, queued, Instant::now());
        }
    }

    /// Whether the machine this thread runs on belongs to an owner who has not let this person
    /// in. Such a person cannot steer the thread either — interrupt, stop or move it — because
    /// each would act on the owner's machine for them.
    async fn shut_out_of(&self, conversation: &Conversation, user: &UserId) -> bool {
        let pinned = match self.store.pin(conversation).await {
            Ok(pinned) => pinned,
            Err(err) => {
                tracing::warn!(error = %err, "could not read a conversation's pin");
                None
            }
        };
        pinned
            .and_then(|pin| self.fleet.get(&pin.node))
            .is_some_and(|node| !node.admits(user))
    }

    /// What of these messages the node's owner lets reach it, as one prompt. The rest are marked
    /// where they were written and never leave the gateway.
    async fn admitted(&self, node: &Node, batch: &[Incoming]) -> Option<Incoming> {
        let mut kept = Vec::with_capacity(batch.len());
        for message in batch {
            if UserId::parse(&message.user).is_ok_and(|speaker| node.admits(&speaker)) {
                kept.push(message.clone());
            } else {
                self.react(&message.channel, &message.ts, UNAUTHORISED_REACTION)
                    .await;
            }
        }
        self.merge(kept)
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
        let Ok(user_id) = UserId::parse(&user) else {
            return;
        };
        if !stop || !self.access.snapshot().may_chat(&user_id) {
            return;
        }
        let thread_ts = payload["thread_ts"].as_str().map(str::to_owned);
        let note = match &thread_ts {
            None => Some(format!(
                "Slack does not run slash commands inside threads. Mention me with `{}` in the thread you want to stop.",
                Command::Stop
            )),
            Some(thread_ts) => {
                let conversation = Conversation {
                    channel: channel.clone(),
                    thread_ts: thread_ts.clone(),
                };
                if self.shut_out_of(&conversation, &user_id).await {
                    return;
                }
                self.stop(&conversation)
            }
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
        batch: &[Incoming],
    ) -> Option<TurnTiming> {
        let mut retried = false;
        loop {
            let seat = self.seat(user, conversation, batch).await?;
            let merged = self.admitted(&seat.node, batch).await?;
            let incoming = &merged;
            let text = self.strip_mention(&incoming.text);
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
                    let refusal = Refusal::Failed(err);
                    self.faulted(
                        conversation,
                        user,
                        incoming,
                        faults::unreachable(&seat.node.name, &refusal),
                        faults::worth_retrying(&refusal),
                    )
                    .await;
                    return None;
                }
            };
            let reply = match tokio::time::timeout(PROMPT_TIMEOUT, pending.reply).await {
                Ok(reply) => reply,
                Err(_) => {
                    // Silence is the one failure with nothing to read: it is always worth one
                    // more try, which reseats the conversation on whatever is up now.
                    self.faulted(
                        conversation,
                        user,
                        incoming,
                        faults::silent(&seat.node.name, PROMPT_TIMEOUT),
                        true,
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
                    let refusal = Refusal::Failed(err);
                    self.faulted(
                        conversation,
                        user,
                        incoming,
                        faults::not_taken(&seat.node.name, &refusal),
                        faults::worth_retrying(&refusal),
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

    /// `batch` is sorted and never empty; its last message is the one that started the turn.
    async fn seat(
        &self,
        user: &UserId,
        conversation: &Conversation,
        batch: &[Incoming],
    ) -> Option<Seat> {
        let incoming = batch.last()?;
        let snapshot = self.access.snapshot();
        let pinned = match self.store.pin(conversation).await {
            Ok(pinned) => pinned,
            Err(err) => {
                tracing::warn!(error = %err, "could not read a conversation's pin");
                None
            }
        };
        if let Some(pin) = &pinned {
            // A node disabled mid-conversation is treated exactly like one that went offline: the
            // pin is dropped and the thread is picked up by whichever machine `assign` finds next.
            let live = self
                .fleet
                .get(&pin.node)
                .filter(|node| node.connected && snapshot.is_enabled(&node.selector));
            if let Some(node) = live {
                let moved = lock(&self.moved).remove(conversation);
                let history = match moved {
                    true => self.history(conversation, &incoming.ts, &node).await,
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
        let Some(node) = self.fleet.assign(user, &snapshot) else {
            // Machines are up, but no owner lets these people in: that is a refusal, not an
            // outage, and says so the way every refusal does.
            if self.fleet.shuts_out(user, &snapshot) {
                for message in batch {
                    let shut_out = UserId::parse(&message.user)
                        .is_ok_and(|speaker| self.fleet.shuts_out(&speaker, &snapshot));
                    if shut_out {
                        self.react(&message.channel, &message.ts, UNAUTHORISED_REACTION)
                            .await;
                    }
                }
                return None;
            }
            let orphaned = lock(&self.orphaned).contains(conversation);
            self.alert(conversation, &no_machine(orphaned)).await;
            return None;
        };
        let orphaned = lock(&self.orphaned).remove(conversation);
        let history = if incoming.thread_ts.is_some() || orphaned {
            self.history(conversation, &incoming.ts, &node).await
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
                self.faulted(
                    conversation,
                    user,
                    incoming,
                    faults::no_session(&node.name, &refusal),
                    faults::worth_retrying(&refusal),
                )
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

    /// Every path that seats a conversation opens its session here — a new thread, a node that
    /// went away, `/node` and an `unknown_session` retry — so this is the one place its tools are
    /// chosen. A node that does not declare `tool_groups` is lent none, and keeps the catalogue it
    /// was sent at `initialize`.
    async fn open_session(
        &self,
        node: &Node,
        conversation: &Conversation,
    ) -> Result<SessionId, Refusal> {
        let tool_groups: Vec<ToolGroup> = match node.capabilities.tool_groups {
            true => self.tools.group().into_iter().collect(),
            false => Vec::new(),
        };
        let call = GatewayCall::NewSession(NewSession {
            context: vec![Open::Known(thread_link(conversation))],
            tool_groups: tool_groups.clone(),
        });
        let pending = node.link.call(call).await?;
        match pending.reply.await {
            Ok(GatewayReply::NewSession(created)) => {
                self.lent
                    .opened(&node.selector, &created.session_id, &tool_groups);
                self.unpublished(conversation, node, &created.unhandled)
                    .await;
                Ok(created.session_id)
            }
            Ok(other) => {
                tracing::warn!(reply = ?other, "a new session was answered with the wrong reply");
                Err(Refusal::Mismatched)
            }
            Err(err) => Err(Refusal::Failed(err)),
        }
    }

    /// Leaves out what people the node's owner has not let in wrote, so catching a machine up
    /// never hands it words that could not have reached it directly.
    async fn history(
        &self,
        conversation: &Conversation,
        current: &str,
        node: &Node,
    ) -> Option<String> {
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
            .filter(|message| match (&message.user, &message.bot_id) {
                (Some(user), None) if *user != self.bot_user => {
                    UserId::parse(user).is_ok_and(|speaker| node.admits(&speaker))
                }
                _ => true,
            })
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
        let turn = Turn {
            workload: incoming.workload.clone(),
            ..Turn::default()
        };
        let mut active = Instant::now();
        let mut stalled = false;
        let mut wedged = None;
        // The first fault is the diagnosis; anything after it is fallout from the same cause.
        let mut failure: Option<rax::Error> = None;
        let mut unattached: Vec<(String, String)> = Vec::new();
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
                    // A tool that is still running is the turn's activity, even though it produces
                    // no events of its own until it finishes.
                    match turn.longest_tool() {
                        Some((_, age)) if age < self.tool_ceiling => {
                            active = Instant::now();
                            continue;
                        }
                        Some((title, _)) => {
                            wedged = Some(title);
                            break;
                        }
                        None => {
                            stalled = true;
                            break;
                        }
                    }
                }
            };
            let Some(event) = event else {
                break;
            };
            active = Instant::now();
            let written = reply.has_written();
            match self.show(&mut reply, node, event, &turn).await {
                Some(Aftermath::Failed(reported)) => drop(failure.get_or_insert(reported)),
                Some(Aftermath::Unattached { name, reason }) => unattached.push((name, reason)),
                None => {}
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
        } else if let Some(title) = wedged {
            cancel_turn(node.link.clone(), session_id.clone());
            Some(format!(
                "_*{}*_ ran for {} without finishing, so I stopped this turn.",
                render::escape(&title),
                span(self.tool_ceiling)
            ))
        } else if let Some(marker) = marker {
            Some(marker.to_owned())
        } else if node.link.is_closed() {
            Some(format!(
                "_*{}*_ went offline before finishing this answer.",
                render::escape(&node.name)
            ))
        } else if !reply.has_written() && failure.is_none() && unattached.is_empty() {
            Some("done, with nothing to say.".to_owned())
        } else {
            None
        };
        reply.finish().await;
        // What went wrong is the gateway's to explain, so it follows the answer as its own card
        // rather than as a last italic line inside the agent's words.
        if !unattached.is_empty() {
            self.alert(conversation, &faults::unattached(&node.name, &unattached))
                .await;
        }
        if let Some(reported) = &failure {
            match UserId::parse(&incoming.user) {
                Ok(user) => {
                    self.faulted(
                        conversation,
                        &user,
                        incoming,
                        faults::turn_failed(&node.name, reported),
                        faults::worth_retrying_turn(reported),
                    )
                    .await;
                }
                Err(_) => {
                    self.alert(conversation, &faults::turn_failed(&node.name, reported))
                        .await;
                }
            }
        }
        if let Some(ending) = ending {
            self.notice(conversation, &ending).await;
        }
        first_output
    }

    /// Draws one event into the reply, and hands back anything the gateway must say about the
    /// turn once it ends: that is the gateway talking, not the agent, so it is never written
    /// into what the agent said.
    async fn show(
        &self,
        reply: &mut Reply,
        node: &Node,
        event: Open<TurnEvent>,
        turn: &Turn,
    ) -> Option<Aftermath> {
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
                let (heading, details, icon) = task_card(&tool_call);
                self.rule(reply.target(), node, tool_call, turn).await;
                turn.tool(&id, Some(&title), TaskStatus::InProgress);
                let card = TaskCard {
                    title: Some(&heading),
                    details: details.as_deref(),
                    icon,
                };
                reply.task(&id, card, TaskStatus::InProgress).await;
            }
            Open::Known(TurnEvent::ToolCallUpdate { tool_call_update }) => {
                let status = match tool_call_update.status {
                    ToolCallStatus::InProgress => TaskStatus::InProgress,
                    ToolCallStatus::Completed => TaskStatus::Complete,
                    ToolCallStatus::Failed | ToolCallStatus::Denied => TaskStatus::Error,
                };
                turn.tool(
                    &tool_call_update.id.0,
                    tool_call_update.title.as_deref(),
                    status,
                );
                // The agent's title for a call is its detail line; the card's own title stays.
                let details = tool_call_update
                    .title
                    .as_deref()
                    .map(|title| alerts::clip(title, TASK_DETAILS_CHARS));
                let card = TaskCard {
                    details: details.as_deref(),
                    ..TaskCard::default()
                };
                reply.task(&tool_call_update.id.0, card, status).await;
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
            Open::Known(TurnEvent::Error { error }) => return Some(Aftermath::Failed(error)),
            Open::Known(TurnEvent::Attachment { attachment }) => {
                let name = attachment
                    .filename
                    .clone()
                    .unwrap_or_else(|| "attachment".to_owned());
                if let Err(reason) = self.attach(reply.target(), node, attachment).await {
                    tracing::warn!(node = %node.name, file = %name, %reason, "could not attach a file");
                    return Some(Aftermath::Unattached { name, reason });
                }
            }
            Open::Known(TurnEvent::Complete { .. }) => {}
            other => tracing::debug!(event = ?other, "turn event not shown yet"),
        }
        None
    }

    /// Allows, denies, or puts the call to the node's owner, as their tool mode says.
    async fn rule(&self, target: &Target, node: &Node, tool: ToolCall, turn: &Turn) {
        let config = policy::rules(&*self.store, &node.owner).await;
        let decision = match policy::rule(&config, &tool.name) {
            Ruling::Decided(decision) => decision,
            Ruling::Ask => {
                let ask = approval::Ask {
                    slack: self.slack.clone(),
                    store: self.store.clone(),
                    link: node.link.clone(),
                    node_name: node.name.clone(),
                    owner: node.owner.clone(),
                    // A workload may post into a channel full of people; the card is the node
                    // owner's alone, so it goes to them.
                    channel: match &turn.workload {
                        Some(_) => node.owner.to_string(),
                        None => target.channel.clone(),
                    },
                    thread_ts: match &turn.workload {
                        Some(_) => None,
                        None => Some(target.thread_ts.clone()),
                    },
                    requester: turn
                        .workload
                        .as_ref()
                        .map(|run| format!("<@{}>'s workload", run.caller)),
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
            admits: prompts::Admits {
                owner: node.owner.clone(),
                access: node.access.clone(),
            },
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

    /// Posts a fault's card, carrying the button to send the message again when trying again has
    /// a real chance. A card with a button opens expanded, so the button is not hidden.
    async fn faulted(
        &self,
        conversation: &Conversation,
        user: &UserId,
        incoming: &Incoming,
        mut card: Alert,
        retryable: bool,
    ) {
        if retryable {
            card.actions = Some(faults::send_again(&self.offer_retry(user, incoming)));
        }
        self.alert(conversation, &card).await;
    }

    async fn alert(&self, conversation: &Conversation, alert: &Alert) {
        let message = alert.message(&conversation.channel, Some(&conversation.thread_ts));
        if let Err(err) = self.slack.post_message(&message).await {
            tracing::warn!(error = %err, "could not post an alert in a thread");
        }
    }

    /// A group the node could not publish leaves the agent without those tools for the whole
    /// session, which the people in the thread should hear rather than discover.
    async fn unpublished(&self, conversation: &Conversation, node: &Node, unhandled: &[Unhandled]) {
        for refused in unhandled {
            let Subject::ToolGroup { namespace } = &refused.subject else {
                continue;
            };
            tracing::info!(node = %node.name, %namespace, reason = ?refused.message, "node did not publish a tool group");
            let why = refused
                .message
                .as_deref()
                .map(|message| format!(": {}", render::escape(message)))
                .unwrap_or_default();
            let body = format!(
                "_*{}*_ could not take this conversation's `{}` tools{why}. The agent will work without them.",
                render::escape(&node.name),
                render::escape(namespace),
            );
            self.notice(conversation, &body).await;
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

/// A background event is drawn exactly as the same event in a turn would be.
fn as_turn(event: BackgroundEvent) -> TurnEvent {
    match event {
        BackgroundEvent::Message { content } => TurnEvent::Message { content },
        BackgroundEvent::Status { text } => TurnEvent::Status { text },
        BackgroundEvent::Complete { stop_reason } => TurnEvent::Complete { stop_reason },
        BackgroundEvent::Error { error } => TurnEvent::Error { error },
        BackgroundEvent::ToolCall { tool_call } => TurnEvent::ToolCall { tool_call },
        BackgroundEvent::ToolCallUpdate { tool_call_update } => {
            TurnEvent::ToolCallUpdate { tool_call_update }
        }
        BackgroundEvent::PlanUpdate { entries } => TurnEvent::PlanUpdate { entries },
        BackgroundEvent::Attachment { attachment } => TurnEvent::Attachment { attachment },
    }
}

/// Slack's direct-message channels are the ones whose id starts with `D`, and a reply there
/// names no recipient.
fn is_direct(channel: &str) -> bool {
    channel.starts_with('D')
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

/// Tools that run a command line, drawn with Slack's `code` icon.
const SHELL_TOOLS: [&str; 3] = ["terminal", "shell", "bash"];
/// Enough of a command to recognise it; a whole heredoc would bury the list.
const TASK_DETAILS_CHARS: usize = 300;

/// A tool call's card as (title, details, icon): the agent's plain-English `description` as the
/// title, or the tool's name when it gave none, and the agent's own title for the call — the
/// command or path — underneath.
fn task_card(tool_call: &ToolCall) -> (String, Option<String>, Option<&'static str>) {
    let title = tool_call
        .input
        .as_ref()
        .and_then(|input| input["description"].as_str())
        .map(str::trim)
        .filter(|description| !description.is_empty())
        .map_or_else(|| tool_call.name.clone(), str::to_owned);
    let details = tool_call
        .title
        .as_deref()
        .map(|title| alerts::clip(title, TASK_DETAILS_CHARS));
    let shell = SHELL_TOOLS
        .iter()
        .any(|tool| tool_call.name.eq_ignore_ascii_case(tool));
    (title, details, shell.then_some("code"))
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
/// turn is not idle. It carries the tool calls in flight for the same reason: a tool that
/// takes twenty minutes and says nothing in between is working, not silent.
#[derive(Default)]
struct Turn {
    /// A workload's approvals go to the node owner's DM rather than into its thread.
    workload: Option<WorkloadRun>,
    token: CancellationToken,
    waiting: Arc<AtomicUsize>,
    tools: Mutex<HashMap<String, InFlight>>,
}

struct InFlight {
    started: Instant,
    title: String,
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

    /// Folds one sighting of a tool call into the in-flight set: a terminal status retires it,
    /// anything else starts or keeps tracking it.
    ///
    /// The start is stamped once, on first sighting, so a call's age is measured from when it
    /// began rather than from the last update that refined its title.
    fn tool(&self, id: &str, title: Option<&str>, status: TaskStatus) {
        let mut tools = lock(&self.tools);
        match status {
            TaskStatus::Complete | TaskStatus::Error => {
                tools.remove(id);
            }
            TaskStatus::Pending | TaskStatus::InProgress => {
                let call = tools.entry(id.to_owned()).or_insert_with(|| InFlight {
                    started: Instant::now(),
                    title: id.to_owned(),
                });
                if let Some(title) = title {
                    call.title = title.to_owned();
                }
            }
        }
    }

    /// The longest-running tool call in flight, with its age — the one piece of state the idle
    /// arm needs to tell a working turn from a silent one, and from a wedged tool.
    fn longest_tool(&self) -> Option<(String, Duration)> {
        lock(&self.tools)
            .values()
            .max_by_key(|call| call.started.elapsed())
            .map(|call| (call.title.clone(), call.started.elapsed()))
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

#[cfg(test)]
mod tests {
    use rax::id::ToolCallId;
    use rax::tool::ToolKind;

    use super::*;

    fn call(name: &str, title: &str, input: serde_json::Value) -> ToolCall {
        ToolCall {
            id: ToolCallId("tc".into()),
            name: name.into(),
            title: Some(title.into()),
            kind: ToolKind::Other,
            input: Some(input),
            content: vec![],
        }
    }

    #[test]
    fn a_task_card_is_titled_by_the_description_or_else_the_tool_name() {
        let bash = call(
            "Bash",
            "ls -la",
            serde_json::json!({"command": "ls -la", "description": " List the files "}),
        );
        assert_eq!(
            task_card(&bash),
            ("List the files".into(), Some("ls -la".into()), Some("code"))
        );
        let read = call(
            "Read",
            "/tmp/INDEX.md",
            serde_json::json!({"file_path": "/tmp/INDEX.md", "description": "  "}),
        );
        assert_eq!(
            task_card(&read),
            ("Read".into(), Some("/tmp/INDEX.md".into()), None)
        );
        let shell = call("Shell", "x", serde_json::json!({}));
        assert_eq!(task_card(&shell).2, Some("code"));
    }

    #[test]
    fn a_long_command_is_clipped_in_the_details() {
        let script = "x".repeat(TASK_DETAILS_CHARS * 2);
        let details = task_card(&call("Bash", &script, serde_json::json!({}))).1;
        assert_eq!(
            details.map(|details| details.chars().count()),
            Some(TASK_DETAILS_CHARS)
        );
    }
}
