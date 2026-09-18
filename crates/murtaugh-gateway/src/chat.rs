//! A conversation is a Slack thread pinned to one node's session. This turns Slack messages into
//! RAX prompts and streams each turn back into the thread.

use std::collections::HashSet;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use murtaugh_slack::{Event, FileRef, PostMessage, SlackClient, TaskStatus, Upload};
use murtaugh_store::{Conversation, Pin, Store, UserId};
use rax::attachment::Attachment;
use rax::content::ContentBlock;
use rax::id::SessionId;
use rax::interaction::{DisplayAnswer, DisplayOutcome};
use rax::session::{NewSession, Prompt};
use rax::tool::{Decision, ToolCallStatus, ToolVerdict};
use rax::{ErrorKind, Event as TurnEvent, GatewayCall, GatewayReply, Open};
use rax_tokio::CallError;
use rax_tokio::gateway::StreamEvents;
use time::OffsetDateTime;
use tokio::time::Instant;

use crate::access::Access;
use crate::files::Files;
use crate::fleet::{Fleet, Node};
use crate::hub::FleetChange;
use crate::render;
use crate::reply::{Reply, Target};

pub const UNAUTHORISED_REACTION: &str = "zipper_mouth_face";
/// The subtype Slack gives a DM that carries files; it is still the person talking.
const FILE_SHARE: &str = "file_share";
pub const THINKING: &str = "is thinking...";
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
    orphaned: Mutex<HashSet<Conversation>>,
    busy: Mutex<HashSet<Conversation>>,
}

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
            orphaned: Mutex::new(HashSet::new()),
            busy: Mutex::new(HashSet::new()),
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
        let conversation = Conversation {
            channel: incoming.channel.clone(),
            thread_ts: incoming
                .thread_ts
                .clone()
                .unwrap_or_else(|| incoming.ts.clone()),
        };
        if !lock(&self.busy).insert(conversation.clone()) {
            self.say(
                &conversation,
                "_Still on your last message. Send this again once I've answered._",
            )
            .await;
            return;
        }
        let thinking = Thinking::start(self.slack.clone(), &conversation);
        let timing = self.converse(&user, &conversation, &incoming).await;
        thinking.stop().await;
        lock(&self.busy).remove(&conversation);
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
                    self.say(
                        conversation,
                        &format!("_I couldn't reach *{}*: {err}_", seat.node.name),
                    )
                    .await;
                    return None;
                }
            };
            let reply = match tokio::time::timeout(PROMPT_TIMEOUT, pending.reply).await {
                Ok(reply) => reply,
                Err(_) => {
                    self.say(
                        conversation,
                        &format!("_*{}* did not take this message in time._", seat.node.name),
                    )
                    .await;
                    return None;
                }
            };
            match reply {
                Ok(GatewayReply::Prompt(_)) => {
                    let accepted = Instant::now();
                    let first_output = self
                        .stream(conversation, incoming, &seat.node, pending.events)
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
                    self.say(
                        conversation,
                        "_Still on your last message. Send this again once I've answered._",
                    )
                    .await;
                    return None;
                }
                Ok(other) => {
                    tracing::warn!(reply = ?other, "a prompt was answered with the wrong reply");
                    return None;
                }
                Err(err) => {
                    self.say(
                        conversation,
                        &format!("_*{}* could not take this message: {err}_", seat.node.name),
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
                return Some(Seat {
                    node,
                    session_id: SessionId(pin.session_id.clone()),
                    history: None,
                });
            }
            let _ = self.store.remove_pin(conversation).await;
            self.fleet.session_ended(&pin.node);
            lock(&self.orphaned).insert(conversation.clone());
        }
        let snapshot = self.access.snapshot();
        let Some(node) = self.fleet.assign(user, &snapshot) else {
            self.say(
                conversation,
                "_No machine can take this conversation right now: none of the nodes you may use is connected._",
            )
            .await;
            return None;
        };
        let orphaned = lock(&self.orphaned).remove(conversation);
        let history = if incoming.thread_ts.is_some() || orphaned {
            self.history(conversation, &incoming.ts).await
        } else {
            None
        };
        if history.is_some() {
            let name = render::escape(&node.name);
            let notice = if orphaned {
                format!(
                    "_The machine serving this conversation went offline. Continuing on *{name}*, catching up from this thread._"
                )
            } else {
                format!("_Picking this conversation up on *{name}*, catching up from this thread._")
            };
            self.say(conversation, &notice).await;
        }
        let session_id = match self.open_session(&node, conversation).await {
            Ok(session_id) => session_id,
            Err(reason) => {
                self.fleet.session_ended(&node.selector);
                self.say(
                    conversation,
                    &format!(
                        "_*{}* could not start a conversation: {reason}_",
                        render::escape(&node.name)
                    ),
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

    async fn open_session(
        &self,
        node: &Node,
        conversation: &Conversation,
    ) -> Result<SessionId, String> {
        let call = GatewayCall::NewSession(NewSession {
            context: vec![Open::Known(thread_link(conversation))],
        });
        let pending = node.link.call(call).await.map_err(|err| err.to_string())?;
        match pending.reply.await {
            Ok(GatewayReply::NewSession(created)) => Ok(created.session_id),
            Ok(other) => Err(format!("answered with {other:?}")),
            Err(err) => Err(err.to_string()),
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

    async fn stream(
        &self,
        conversation: &Conversation,
        incoming: &Incoming,
        node: &Node,
        mut events: StreamEvents,
    ) -> Option<Instant> {
        let recipient = (!incoming.direct).then(|| (self.team.clone(), incoming.user.clone()));
        let mut first_output = None;
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
            };
            let Some(event) = event else {
                break;
            };
            let written = reply.has_written();
            self.show(&mut reply, node, event).await;
            if !written && reply.has_written() {
                first_output.get_or_insert_with(Instant::now);
            }
        }
        if node.link.is_closed() {
            reply
                .text(&format!(
                    "\n\n_**{}** went offline before finishing this answer._",
                    node.name
                ))
                .await;
        }
        if !reply.has_written() {
            reply.text("_Done, with nothing to say._").await;
        }
        reply.finish().await;
        first_output
    }

    async fn show(&self, reply: &mut Reply, node: &Node, event: Open<TurnEvent>) {
        match event {
            Open::Known(TurnEvent::Message {
                content: Open::Known(ContentBlock::Text { text }),
            }) => reply.text(&text).await,
            Open::Known(TurnEvent::ToolCall { tool_call }) => {
                let verdict = ToolVerdict {
                    id: tool_call.id.clone(),
                    decision: Decision::Allow,
                };
                if let Err(err) = node.link.verdict(verdict).await {
                    tracing::warn!(error = %err, "could not rule on a tool call");
                }
                let title = tool_call.title.as_deref().unwrap_or(&tool_call.name);
                reply
                    .task(&tool_call.id.0, Some(title), TaskStatus::InProgress)
                    .await;
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
                self.unavailable(node, question.id).await;
            }
            Open::Known(TurnEvent::Plan { plan }) => self.unavailable(node, plan.id).await,
            Open::Known(TurnEvent::SignIn { sign_in }) => {
                self.unavailable(node, sign_in.id).await;
            }
            Open::Known(TurnEvent::Error { error }) => {
                reply
                    .text(&format!("\n\n_The turn failed: {}_", error.message))
                    .await;
            }
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

    async fn say(&self, conversation: &Conversation, text: &str) {
        let message = PostMessage {
            channel: conversation.channel.clone(),
            thread_ts: Some(conversation.thread_ts.clone()),
            text: text.to_owned(),
            blocks: Vec::new(),
        };
        if let Err(err) = self.slack.post_message(&message).await {
            tracing::warn!(error = %err, "could not post in a thread");
        }
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
