//! A conversation is a Slack thread pinned to one node's session. This turns Slack messages into
//! RAX prompts and streams each turn back into the thread.

use std::collections::HashSet;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Duration;

use murtaugh_slack::{Event, PostMessage, SlackClient, UpdateMessage};
use murtaugh_store::{Conversation, Pin, Store, UserId};
use rax::content::ContentBlock;
use rax::id::SessionId;
use rax::interaction::{DisplayAnswer, DisplayOutcome};
use rax::session::{NewSession, Prompt};
use rax::tool::{Decision, ToolVerdict};
use rax::{ErrorKind, Event as TurnEvent, GatewayCall, GatewayReply, Open};
use rax_tokio::CallError;
use rax_tokio::gateway::StreamEvents;
use time::OffsetDateTime;
use tokio::time::Instant;

use crate::access::Access;
use crate::fleet::{Fleet, Node};
use crate::hub::FleetChange;
use crate::render::{self, MESSAGE_LIMIT};

pub const UNAUTHORISED_REACTION: &str = "zipper_mouth_face";
pub const COALESCE: Duration = Duration::from_millis(800);
/// Long enough for a node to fetch the files a prompt links before accepting it.
pub const PROMPT_TIMEOUT: Duration = Duration::from_secs(120);
const WORKING: &str = "_Working on it…_";

pub struct Chat {
    slack: SlackClient,
    bot_user: String,
    store: Arc<dyn Store>,
    access: Access,
    fleet: Fleet,
    coalesce: Duration,
    orphaned: Mutex<HashSet<Conversation>>,
    busy: Mutex<HashSet<Conversation>>,
}

struct Incoming {
    user: String,
    channel: String,
    ts: String,
    thread_ts: Option<String>,
    text: String,
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
        store: Arc<dyn Store>,
        access: Access,
        fleet: Fleet,
        coalesce: Duration,
    ) -> Arc<Self> {
        Arc::new(Self {
            slack,
            bot_user,
            store,
            access,
            fleet,
            coalesce,
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
            },
            Event::Message {
                channel,
                channel_type,
                user: Some(user),
                bot_id: None,
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
            },
            _ => return,
        };
        if incoming.user == self.bot_user {
            return;
        }
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
                    self.stream(conversation, &seat.node, pending.events).await;
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
        if orphaned {
            self.say(
                conversation,
                &format!(
                    "_The machine serving this conversation went offline. Continuing on *{}*, catching up from this thread._",
                    render::escape(&node.name)
                ),
            )
            .await;
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
        let history = if incoming.thread_ts.is_some() || orphaned {
            self.history(conversation, &incoming.ts).await
        } else {
            None
        };
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

    async fn stream(&self, conversation: &Conversation, node: &Node, mut events: StreamEvents) {
        let mut reply = Reply::new(conversation.clone());
        reply.show(&self.slack, WORKING).await;
        let mut text = String::new();
        let mut status: Option<String> = None;
        let mut dirty = false;
        let mut next_flush = Instant::now() + self.coalesce;
        loop {
            let event = tokio::select! {
                event = events.recv() => event,
                () = tokio::time::sleep_until(next_flush), if dirty => {
                    reply.show(&self.slack, &compose(&text, status.as_deref())).await;
                    dirty = false;
                    continue;
                }
            };
            let Some(event) = event else {
                break;
            };
            match event {
                Open::Known(TurnEvent::Message {
                    content: Open::Known(ContentBlock::Text { text: chunk }),
                }) => {
                    text.push_str(&chunk);
                    status = None;
                }
                Open::Known(TurnEvent::Status { text: line }) => status = Some(line),
                Open::Known(TurnEvent::ToolCall { tool_call }) => {
                    status = Some(format!(
                        "Running {}",
                        tool_call
                            .title
                            .clone()
                            .unwrap_or_else(|| tool_call.name.clone())
                    ));
                    let verdict = ToolVerdict {
                        id: tool_call.id.clone(),
                        decision: Decision::Allow,
                    };
                    if let Err(err) = node.link.verdict(verdict).await {
                        tracing::warn!(error = %err, "could not rule on a tool call");
                    }
                }
                Open::Known(TurnEvent::Question { question }) => {
                    self.unavailable(node, question.id).await;
                }
                Open::Known(TurnEvent::Plan { plan }) => self.unavailable(node, plan.id).await,
                Open::Known(TurnEvent::SignIn { sign_in }) => {
                    self.unavailable(node, sign_in.id).await;
                }
                Open::Known(TurnEvent::Error { error }) => {
                    text.push_str(&format!("\n\n_The turn failed: {}_", error.message));
                }
                Open::Known(TurnEvent::Complete { .. }) => {}
                other => tracing::debug!(event = ?other, "turn event not shown yet"),
            }
            if !dirty {
                dirty = true;
                next_flush = Instant::now() + self.coalesce;
            }
        }
        if node.link.is_closed() {
            text.push_str(&format!(
                "\n\n_*{}* went offline before finishing this answer._",
                node.name
            ));
        }
        let finished = if text.trim().is_empty() {
            "_Done, with nothing to say._".to_owned()
        } else {
            text
        };
        reply.show(&self.slack, &render::mrkdwn(&finished)).await;
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

fn compose(text: &str, status: Option<&str>) -> String {
    let mut shown = render::mrkdwn(text);
    if let Some(status) = status {
        if !shown.is_empty() {
            shown.push_str("\n\n");
        }
        shown.push_str(&format!("_{}…_", render::escape(status)));
    }
    if shown.trim().is_empty() {
        WORKING.to_owned()
    } else {
        shown
    }
}

struct Reply {
    conversation: Conversation,
    posted: Vec<(String, String)>,
}

impl Reply {
    fn new(conversation: Conversation) -> Self {
        Self {
            conversation,
            posted: Vec::new(),
        }
    }

    async fn show(&mut self, slack: &SlackClient, text: &str) {
        for (index, part) in render::split(text, MESSAGE_LIMIT).into_iter().enumerate() {
            match self.posted.get(index) {
                Some((_, shown)) if *shown == part => {}
                Some((ts, _)) => {
                    let update = UpdateMessage {
                        channel: self.conversation.channel.clone(),
                        ts: ts.clone(),
                        text: part.clone(),
                        blocks: Vec::new(),
                    };
                    match slack.update_message(&update).await {
                        Ok(()) => self.posted[index].1 = part,
                        Err(err) => tracing::warn!(error = %err, "could not update a reply"),
                    }
                }
                None => {
                    let message = PostMessage {
                        channel: self.conversation.channel.clone(),
                        thread_ts: Some(self.conversation.thread_ts.clone()),
                        text: part.clone(),
                        blocks: Vec::new(),
                    };
                    match slack.post_message(&message).await {
                        Ok(posted) => self.posted.push((posted.ts, part)),
                        Err(err) => {
                            tracing::warn!(error = %err, "could not post a reply");
                            return;
                        }
                    }
                }
            }
        }
    }
}
