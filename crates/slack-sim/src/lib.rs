//! A fake Slack for tests. It validates requests the way Slack does, records every call, and
//! delivers Socket Mode envelopes shaped exactly like Slack's.

mod api;
mod blocks;
mod render;
mod socket;
mod state;
mod stream;
mod upload;

use std::fmt;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::Duration;

use axum::Router;
use axum::routing::{any, get, post};
use serde_json::Value;
use tokio::net::TcpListener;
use tokio::sync::watch;
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;
use url::Url;

use crate::state::{APP_TOKEN, BOT_TOKEN, Fault, File, State, new_message};

pub const TEAM_ID: &str = "T0SIM0001";
pub const APP_ID: &str = "A0SIM0001";
pub const BOT_USER_ID: &str = "U0MURTAUGH";
pub const BOT_ID: &str = "B0MURTAUGH";
pub const ALICE: &str = "U0ALICE";
pub const BOB: &str = "U0BOB";
/// Public; the bot is a member.
pub const GENERAL: &str = "C0GENERAL";
/// Public; the bot is not a member, so posting there is `not_in_channel`.
pub const RANDOM: &str = "C0RANDOM";
/// Private; the bot is a member.
pub const PRIVATE: &str = "C0PRIVATE";
/// Private; the bot is not a member, so Slack pretends it does not exist.
pub const SECRET: &str = "C0SECRET";

const WAIT_FOR_CALL: Duration = Duration::from_secs(5);

#[derive(Clone, PartialEq, Eq)]
pub struct SimTokens {
    pub app: String,
    pub bot: String,
}

impl fmt::Debug for SimTokens {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("SimTokens { .. }")
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ChannelKind {
    Public,
    Private,
    Im,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Call {
    pub method: String,
    pub params: Value,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Violation {
    pub method: String,
    pub error: String,
    pub detail: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Reaction {
    pub name: String,
    pub users: Vec<String>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct SimMessage {
    pub ts: String,
    pub user: Option<String>,
    pub bot_id: Option<String>,
    pub text: String,
    pub blocks: Option<Value>,
    pub thread_ts: Option<String>,
    pub subtype: Option<String>,
    pub files: Vec<String>,
    pub reactions: Vec<Reaction>,
    pub edited: bool,
    /// Set on a message made by `chat.startStream`; the text above is its Markdown so far.
    pub stream: Option<SimStream>,
}

/// A file as the workspace holds it, whoever shared it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SimFile {
    pub id: String,
    pub name: String,
    pub title: String,
    pub mimetype: String,
    pub bytes: Vec<u8>,
    pub user: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SimStream {
    pub open: bool,
    pub task_display_mode: String,
    pub recipient: Option<(String, String)>,
    pub plans: Vec<String>,
    pub tasks: Vec<SimTask>,
}

/// A task card, as its latest `task_update` left it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SimTask {
    pub id: String,
    pub title: String,
    pub status: String,
}

#[derive(Debug, thiserror::Error)]
pub enum SimError {
    #[error("the simulator could not bind: {0}")]
    Bind(#[from] std::io::Error),
    #[error("unknown user {0}")]
    UnknownUser(String),
    #[error("unknown channel {0}")]
    UnknownChannel(String),
    #[error("no message {ts} in {channel}")]
    UnknownMessage { channel: String, ts: String },
    #[error("no button with action_id {action_id} on {ts}")]
    UnknownAction { ts: String, action_id: String },
    #[error("no {method} call matched within {waited:?}; calls that did arrive: {arrived:#?}")]
    Timeout {
        method: String,
        waited: Duration,
        arrived: Vec<Call>,
    },
}

pub(crate) struct Inner {
    state: Mutex<State>,
    calls_tx: watch::Sender<usize>,
    shutdown: CancellationToken,
}

impl Inner {
    fn lock(&self) -> MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(PoisonError::into_inner)
    }
}

pub struct SlackSim {
    inner: Arc<Inner>,
    base: Url,
    server: JoinHandle<()>,
}

impl Drop for SlackSim {
    fn drop(&mut self) {
        self.inner.shutdown.cancel();
        self.server.abort();
    }
}

impl SlackSim {
    pub async fn start() -> Result<SlackSim, SimError> {
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let addr = listener.local_addr()?;
        let base = format!("http://{addr}/");
        let base_url = Url::parse(&base).map_err(|e| std::io::Error::other(e.to_string()))?;
        let inner = Arc::new(Inner {
            state: Mutex::new(State::new(base)),
            calls_tx: watch::channel(0).0,
            shutdown: CancellationToken::new(),
        });
        let router = Router::new()
            .route("/api/{method}", any(api::handle))
            .route("/link/", get(socket::link))
            .route("/files-pri/{team_file}/download/{name}", get(api::download))
            .route("/signin", get(api::signin))
            .route("/upload/v1/{file_id}", post(upload::receive))
            .with_state(inner.clone());
        let shutdown = inner.shutdown.clone();
        let server = tokio::spawn(async move {
            let _ = axum::serve(listener, router)
                .with_graceful_shutdown(shutdown.cancelled_owned())
                .await;
        });
        Ok(SlackSim {
            inner,
            base: base_url,
            server,
        })
    }

    pub fn api_base(&self) -> Url {
        let mut url = self.base.clone();
        url.set_path("/api/");
        url
    }

    pub fn tokens(&self) -> SimTokens {
        SimTokens {
            app: APP_TOKEN.to_owned(),
            bot: BOT_TOKEN.to_owned(),
        }
    }

    pub fn add_user(&self, id: &str, name: &str) {
        self.inner.lock().add_user(id, name);
    }

    pub fn add_channel(&self, id: &str, name: &str, kind: ChannelKind, bot_member: bool) {
        self.inner.lock().add_channel(id, name, kind, bot_member);
    }

    /// Top-level messages only, like `conversations.history`; replies live in [`SlackSim::thread`].
    pub fn messages(&self, channel: &str) -> Vec<SimMessage> {
        let st = self.inner.lock();
        st.channels
            .get(channel)
            .map(|c| {
                c.messages
                    .iter()
                    .filter(|m| m.thread_ts.as_ref().is_none_or(|t| *t == m.ts))
                    .cloned()
                    .collect()
            })
            .unwrap_or_default()
    }

    /// The parent first, then its replies; `ts` may name the parent or any reply.
    pub fn thread(&self, channel: &str, ts: &str) -> Vec<SimMessage> {
        let st = self.inner.lock();
        st.channels
            .get(channel)
            .and_then(|c| c.thread_root(ts).map(|root| c.thread(&root)))
            .unwrap_or_default()
    }

    /// Ends a stream the way Slack does on its own, so the next append is refused.
    pub fn finalize_stream(&self, channel: &str, ts: &str) {
        let mut st = self.inner.lock();
        if let Some(stream) = st
            .channels
            .get_mut(channel)
            .and_then(|c| c.find_mut(ts))
            .and_then(|m| m.stream.as_mut())
        {
            stream.open = false;
        }
    }

    pub fn reactions(&self, channel: &str, ts: &str) -> Vec<String> {
        let st = self.inner.lock();
        st.channels
            .get(channel)
            .and_then(|c| c.find(ts))
            .map(|m| m.reactions.iter().map(|r| r.name.clone()).collect())
            .unwrap_or_default()
    }

    /// The bot's DM with `user`, once anyone has opened it.
    pub fn im_channel(&self, user: &str) -> Option<String> {
        let mut st = self.inner.lock();
        let id = st.im_for(user);
        st.channels
            .get(&id)
            .is_some_and(|c| !c.messages.is_empty())
            .then_some(id)
    }

    /// Messages only one person saw, as (channel, user, text), oldest first.
    pub fn ephemerals(&self) -> Vec<(String, String, String)> {
        self.inner.lock().ephemerals.clone()
    }

    pub fn violations(&self) -> Vec<Violation> {
        self.inner.lock().violations.clone()
    }

    pub fn calls(&self) -> Vec<Call> {
        self.inner.lock().calls.clone()
    }

    /// Envelope ids acknowledged over Socket Mode, in arrival order.
    pub fn acks(&self) -> Vec<String> {
        self.inner.lock().acked.clone()
    }

    pub fn connections(&self) -> usize {
        self.inner.lock().conns.len()
    }

    /// Matches calls already recorded too, so a call made before the wait began still counts.
    pub async fn wait_for_call(
        &self,
        method: &str,
        predicate: impl Fn(&Value) -> bool,
    ) -> Result<Call, SimError> {
        let mut rx = self.inner.calls_tx.subscribe();
        let deadline = tokio::time::Instant::now() + WAIT_FOR_CALL;
        loop {
            rx.borrow_and_update();
            let found = self
                .inner
                .lock()
                .calls
                .iter()
                .find(|c| c.method == method && predicate(&c.params))
                .cloned();
            if let Some(call) = found {
                return Ok(call);
            }
            if tokio::time::timeout_at(deadline, rx.changed())
                .await
                .is_err()
            {
                return Err(SimError::Timeout {
                    method: method.to_owned(),
                    waited: WAIT_FOR_CALL,
                    arrived: self.calls(),
                });
            }
        }
    }

    /// Queues a 429 for the next call to `method`; call it again to queue more.
    pub fn rate_limit(&self, method: &str, retry_after_secs: u64) {
        self.push_fault(method, Fault::RateLimit(retry_after_secs));
    }

    /// Queues an `ok:false` with `error` for the next call to `method`.
    pub fn fail(&self, method: &str, error: &str) {
        self.push_fault(method, Fault::Fail(error.to_owned()));
    }

    fn push_fault(&self, method: &str, fault: Fault) {
        self.inner
            .lock()
            .faults
            .entry(method.to_owned())
            .or_default()
            .push_back(fault);
    }

    /// Kills every socket without a close frame, as a network failure would.
    pub fn drop_socket(&self) {
        socket::drop_all(&self.inner);
    }

    /// Sends `refresh_requested`; the old sockets close after a grace period, as Slack's do.
    pub fn send_disconnect(&self) {
        socket::send_disconnect(&self.inner);
    }

    pub fn set_replies_page_size(&self, size: usize) {
        self.inner.lock().replies_page_size = size;
    }

    /// Slack also sends a `message` event for a mention when the app subscribes to channel
    /// messages; on by default.
    /// The "is thinking..." line under a thread, while one is showing.
    pub fn file(&self, id: &str) -> Option<SimFile> {
        self.inner.lock().files.get(id).map(|file| SimFile {
            id: file.id.clone(),
            name: file.name.clone(),
            title: file.title.clone().unwrap_or_else(|| file.name.clone()),
            mimetype: file.mimetype.clone(),
            bytes: file.bytes.clone(),
            user: file.user.clone(),
        })
    }

    pub fn thread_status(&self, channel: &str, thread_ts: &str) -> Option<String> {
        self.inner
            .lock()
            .thread_statuses
            .get(&(channel.to_owned(), thread_ts.to_owned()))
            .cloned()
    }

    pub fn set_channel_message_events(&self, enabled: bool) {
        self.inner.lock().channel_message_events = enabled;
    }

    /// Prepends `<@bot>` unless the text already mentions the bot.
    pub async fn mention(
        &self,
        user: &str,
        channel: &str,
        text: &str,
        thread_ts: Option<&str>,
    ) -> Result<String, SimError> {
        let msg = new_message(Some(user), &with_mention(text));
        self.post_mention(user, channel, msg, thread_ts)
    }

    /// A file shared with a message that mentions the bot, as one Slack message.
    #[allow(clippy::too_many_arguments)]
    pub async fn mention_with_file(
        &self,
        user: &str,
        channel: &str,
        text: &str,
        name: &str,
        mimetype: &str,
        bytes: &[u8],
        thread_ts: Option<&str>,
    ) -> Result<(String, String), SimError> {
        let file_id = self.store_file(user, channel, name, mimetype, bytes)?;
        let mut msg = new_message(Some(user), &with_mention(text));
        msg.subtype = Some("file_share".to_owned());
        msg.files = vec![file_id.clone()];
        let ts = self.post_mention(user, channel, msg, thread_ts)?;
        Ok((file_id, ts))
    }

    fn post_mention(
        &self,
        user: &str,
        channel: &str,
        msg: SimMessage,
        thread_ts: Option<&str>,
    ) -> Result<String, SimError> {
        let mut envelopes = Vec::new();
        let ts = {
            let mut st = self.inner.lock();
            let msg = post_as(&mut st, user, channel, msg, thread_ts)?;
            let (kind, member) = channel_facts(&st, channel)?;
            if member
                && kind != ChannelKind::Im
                && let Some(event) = render::user_event(&mut st, channel, &msg, "app_mention")
            {
                envelopes.push(render::events_api(&mut st, event));
            }
            if member
                && (kind == ChannelKind::Im || st.channel_message_events)
                && let Some(event) = render::user_event(&mut st, channel, &msg, "message")
            {
                envelopes.push(render::events_api(&mut st, event));
            }
            msg.ts
        };
        self.deliver_all(envelopes);
        Ok(ts)
    }

    pub async fn dm(&self, user: &str, text: &str) -> Result<(String, String), SimError> {
        let channel = {
            let mut st = self.inner.lock();
            if !st.users.contains_key(user) {
                return Err(SimError::UnknownUser(user.to_owned()));
            }
            st.im_for(user)
        };
        let ts = self.say(user, &channel, text, None).await?;
        Ok((channel, ts))
    }

    pub async fn say(
        &self,
        user: &str,
        channel: &str,
        text: &str,
        thread_ts: Option<&str>,
    ) -> Result<String, SimError> {
        self.post_user_message(user, channel, new_message(Some(user), text), thread_ts)
    }

    pub async fn upload(
        &self,
        user: &str,
        channel: &str,
        name: &str,
        mimetype: &str,
        bytes: &[u8],
        thread_ts: Option<&str>,
    ) -> Result<(String, String), SimError> {
        let file_id = self.store_file(user, channel, name, mimetype, bytes)?;
        let mut msg = new_message(Some(user), "");
        msg.subtype = Some("file_share".to_owned());
        msg.files = vec![file_id.clone()];
        let ts = self.post_user_message(user, channel, msg, thread_ts)?;
        Ok((file_id, ts))
    }

    /// Clicks a button the bot posted, delivering a `block_actions` interactive envelope.
    pub async fn click(
        &self,
        user: &str,
        channel: &str,
        ts: &str,
        action_id: &str,
    ) -> Result<(), SimError> {
        self.click_with_state(
            user,
            channel,
            ts,
            action_id,
            Value::Object(Default::default()),
        )
        .await
    }

    /// Clicks a button with the message's inputs filled in: `values` is the payload's
    /// `state.values`, keyed by block id, then action id.
    pub async fn click_with_state(
        &self,
        user: &str,
        channel: &str,
        ts: &str,
        action_id: &str,
        values: Value,
    ) -> Result<(), SimError> {
        let envelope = {
            let mut st = self.inner.lock();
            if !st.users.contains_key(user) {
                return Err(SimError::UnknownUser(user.to_owned()));
            }
            let msg = st
                .channels
                .get(channel)
                .ok_or_else(|| SimError::UnknownChannel(channel.to_owned()))?
                .find(ts)
                .cloned()
                .ok_or_else(|| SimError::UnknownMessage {
                    channel: channel.to_owned(),
                    ts: ts.to_owned(),
                })?;
            let (block_id, button) =
                find_button(&msg, action_id).ok_or_else(|| SimError::UnknownAction {
                    ts: ts.to_owned(),
                    action_id: action_id.to_owned(),
                })?;
            render::block_actions(&mut st, user, channel, &msg, &block_id, &button, values)
                .ok_or_else(|| SimError::UnknownChannel(channel.to_owned()))?
        };
        socket::deliver(&self.inner, envelope);
        Ok(())
    }

    pub async fn slash(
        &self,
        user: &str,
        channel: &str,
        command: &str,
        text: &str,
    ) -> Result<(), SimError> {
        let envelope = {
            let mut st = self.inner.lock();
            if !st.users.contains_key(user) {
                return Err(SimError::UnknownUser(user.to_owned()));
            }
            render::slash_command(&mut st, user, channel, command, text)
                .ok_or_else(|| SimError::UnknownChannel(channel.to_owned()))?
        };
        socket::deliver(&self.inner, envelope);
        Ok(())
    }

    /// Wraps any event JSON in an `events_api` envelope and delivers it, for event types the
    /// drivers above do not model.
    pub async fn emit(&self, event: Value) -> String {
        let envelope = render::events_api(&mut self.inner.lock(), event);
        let id = envelope
            .get("envelope_id")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        socket::deliver(&self.inner, envelope);
        id
    }

    fn store_file(
        &self,
        user: &str,
        channel: &str,
        name: &str,
        mimetype: &str,
        bytes: &[u8],
    ) -> Result<String, SimError> {
        let mut st = self.inner.lock();
        if !st.channels.contains_key(channel) {
            return Err(SimError::UnknownChannel(channel.to_owned()));
        }
        let id = format!("F0SIM{:06}", st.next_seq());
        let file = File {
            id: id.clone(),
            name: name.to_owned(),
            title: None,
            mimetype: mimetype.to_owned(),
            bytes: bytes.to_vec(),
            user: user.to_owned(),
            created: state::now_secs(),
            channel: channel.to_owned(),
        };
        st.files.insert(id.clone(), file);
        Ok(id)
    }

    fn post_user_message(
        &self,
        user: &str,
        channel: &str,
        msg: SimMessage,
        thread_ts: Option<&str>,
    ) -> Result<String, SimError> {
        let mut envelopes = Vec::new();
        let ts = {
            let mut st = self.inner.lock();
            let msg = post_as(&mut st, user, channel, msg, thread_ts)?;
            let (_, member) = channel_facts(&st, channel)?;
            if member && let Some(event) = render::user_event(&mut st, channel, &msg, "message") {
                envelopes.push(render::events_api(&mut st, event));
            }
            msg.ts
        };
        self.deliver_all(envelopes);
        Ok(ts)
    }

    fn deliver_all(&self, envelopes: Vec<Value>) {
        for envelope in envelopes {
            socket::deliver(&self.inner, envelope);
        }
    }
}

fn with_mention(text: &str) -> String {
    let mention = format!("<@{BOT_USER_ID}>");
    if text.contains(&mention) {
        text.to_owned()
    } else {
        format!("{mention} {text}")
    }
}

fn channel_facts(st: &State, channel: &str) -> Result<(ChannelKind, bool), SimError> {
    st.channels
        .get(channel)
        .map(|c| (c.kind, c.bot_member))
        .ok_or_else(|| SimError::UnknownChannel(channel.to_owned()))
}

fn post_as(
    st: &mut State,
    user: &str,
    channel: &str,
    msg: SimMessage,
    thread_ts: Option<&str>,
) -> Result<SimMessage, SimError> {
    if !st.users.contains_key(user) {
        return Err(SimError::UnknownUser(user.to_owned()));
    }
    if !st.channels.contains_key(channel) {
        return Err(SimError::UnknownChannel(channel.to_owned()));
    }
    st.post(channel, msg, thread_ts)
        .map_err(|_| SimError::UnknownMessage {
            channel: channel.to_owned(),
            ts: thread_ts.unwrap_or_default().to_owned(),
        })
}

fn find_button(msg: &SimMessage, action_id: &str) -> Option<(String, Value)> {
    let top = msg.blocks.as_ref()?.as_array()?;
    let nested = top
        .iter()
        .filter_map(|block| block.get("child_blocks").and_then(Value::as_array))
        .flatten();
    let blocks: Vec<&Value> = top.iter().chain(nested).collect();
    blocks.iter().enumerate().find_map(|(i, block)| {
        let elements = block.get("elements")?.as_array()?;
        let button = elements.iter().find(|e| {
            e.get("type").and_then(Value::as_str) == Some("button")
                && e.get("action_id").and_then(Value::as_str) == Some(action_id)
        })?;
        let block_id = block
            .get("block_id")
            .and_then(Value::as_str)
            .map_or_else(|| format!("sim{i}"), str::to_owned);
        Some((block_id, button.clone()))
    })
}
