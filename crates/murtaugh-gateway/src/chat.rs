//! A conversation is a Slack thread pinned to one node's session. This turns Slack messages into
//! RAX prompts and streams each turn back into the thread.

use std::collections::HashSet;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use murtaugh_slack::{Event, PostMessage, SlackClient, TaskStatus};
use murtaugh_store::{Conversation, Pin, Store, UserId};
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
use crate::fleet::{Fleet, Node};
use crate::hub::FleetChange;
use crate::render;
use crate::reply::{Reply, Target};

pub const UNAUTHORISED_REACTION: &str = "zipper_mouth_face";
/// Long enough for a node to fetch the files a prompt links before accepting it.
pub const PROMPT_TIMEOUT: Duration = Duration::from_secs(120);

pub struct Chat {
    slack: SlackClient,
    bot_user: String,
    store: Arc<dyn Store>,
    access: Access,
    fleet: Fleet,
    team: String,
    orphaned: Mutex<HashSet<Conversation>>,
    busy: Mutex<HashSet<Conversation>>,
}

struct Incoming {
    user: String,
    channel: String,
    ts: String,
    thread_ts: Option<String>,
    text: String,
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

impl Chat {
    pub fn new(
        slack: SlackClient,
        bot_user: String,
        team: String,
        store: Arc<dyn Store>,
        access: Access,
        fleet: Fleet,
    ) -> Arc<Self> {
        Arc::new(Self {
            slack,
            bot_user,
            store,
            access,
            fleet,
            team,
            orphaned: Mutex::new(HashSet::new()),
            busy: Mutex::new(HashSet::new()),
        })
    }

    pub async fn on_event(self: Arc<Self>, event: Event) {
        let incoming = match event {
            Event::AppMention {
                user,
                channel,
                ts,
                thread_ts,
                text,
                ..
            } => Incoming {
                user,
                channel,
                ts,
                thread_ts,
                text,
                direct: false,
            },
            Event::Message {
                channel,
                channel_type,
                user: Some(user),
                subtype: None,
                ts,
                thread_ts,
                text,
                ..
            } if channel_type == "im" => Incoming {
                user,
                channel,
                ts,
                thread_ts,
                text,
                direct: true,
            },
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
        self.message(incoming).await;
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

    async fn message(&self, incoming: Incoming) {
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
        self.converse(&user, &conversation, &incoming).await;
        lock(&self.busy).remove(&conversation);
    }

    async fn converse(&self, user: &UserId, conversation: &Conversation, incoming: &Incoming) {
        let text = self.strip_mention(&incoming.text);
        let mut retried = false;
        loop {
            let Some(seat) = self.seat(user, conversation, incoming).await else {
                return;
            };
            let mut words = String::new();
            if let Some(history) = &seat.history {
                words.push_str(history);
                words.push_str("\n\n");
            }
            words.push_str(&text);
            let prompt = GatewayCall::Prompt(Prompt {
                session_id: seat.session_id.clone(),
                content: vec![ContentBlock::text(words).into()],
            });
            let pending = match seat.node.link.call(prompt).await {
                Ok(pending) => pending,
                Err(err) => {
                    self.say(
                        conversation,
                        &format!("_I couldn't reach *{}*: {err}_", seat.node.name),
                    )
                    .await;
                    return;
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
                    return;
                }
            };
            match reply {
                Ok(GatewayReply::Prompt(_)) => {
                    self.stream(conversation, incoming, &seat.node, pending.events)
                        .await;
                    return;
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
                    return;
                }
                Ok(other) => {
                    tracing::warn!(reply = ?other, "a prompt was answered with the wrong reply");
                    return;
                }
                Err(err) => {
                    self.say(
                        conversation,
                        &format!("_*{}* could not take this message: {err}_", seat.node.name),
                    )
                    .await;
                    return;
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
    ) {
        let recipient = (!incoming.direct).then(|| (self.team.clone(), incoming.user.clone()));
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
                Open::Known(TurnEvent::Complete { .. }) => {}
                other => tracing::debug!(event = ?other, "turn event not shown yet"),
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

async fn sleep_until_due(due: Option<Instant>) {
    match due {
        Some(due) => tokio::time::sleep_until(due).await,
        None => std::future::pending().await,
    }
}
