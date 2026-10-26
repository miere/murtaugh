use std::collections::VecDeque;
use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use serde_json::{Value, json};
use tokio::net::TcpStream;
use tokio::sync::{mpsc, oneshot};
use tokio::task::JoinHandle;
use tokio::time::{Instant, timeout};
use tokio_tungstenite::tungstenite::{self, Message};
use tokio_tungstenite::{MaybeTlsStream, WebSocketStream};

use crate::error::SlackError;
use crate::events::{EventEnvelope, SocketEvent};
use crate::web::SlackClient;

type Ws = WebSocketStream<MaybeTlsStream<TcpStream>>;

const HELLO_TIMEOUT: Duration = Duration::from_secs(10);
const IDLE_BEFORE_PING: Duration = Duration::from_secs(20);
const CLOSE_TIMEOUT: Duration = Duration::from_secs(1);
const BACKOFF_BASE: Duration = Duration::from_millis(250);
const BACKOFF_MAX: Duration = Duration::from_secs(30);
const SHORT_LIVED: Duration = Duration::from_secs(10);

/// The ack a modal's submission waits for. Slack shows field errors, or another view, only when the
/// ack itself carries them, and gives up three seconds after the submission arrived. The first
/// answer counts; when the last clone is dropped unanswered, it acks with nothing, which closes the
/// modal.
#[derive(Clone)]
pub struct ViewAck {
    inner: Arc<Pending>,
}

struct Pending {
    envelope_id: String,
    answered: Mutex<bool>,
    answers: mpsc::UnboundedSender<String>,
}

impl ViewAck {
    /// `None` closes the modal. `Some` is what Slack acts on, such as
    /// `{"response_action": "errors", "errors": {block_id: message}}`.
    pub fn answer(&self, response: Option<Value>) {
        self.inner.send(response);
    }
}

impl Pending {
    fn send(&self, response: Option<Value>) {
        let mut answered = self.answered.lock().unwrap_or_else(PoisonError::into_inner);
        if *answered {
            return;
        }
        *answered = true;
        let mut frame = json!({"envelope_id": self.envelope_id});
        if let Some(response) = response {
            frame["payload"] = response;
        }
        // Gone only once the socket is closed, when nobody is left to show it to.
        let _ = self.answers.send(frame.to_string());
    }
}

impl Drop for Pending {
    fn drop(&mut self) {
        self.send(None);
    }
}

impl std::fmt::Debug for ViewAck {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ViewAck")
            .field("envelope_id", &self.inner.envelope_id)
            .finish()
    }
}

/// Two are equal when they answer the same submission.
impl PartialEq for ViewAck {
    fn eq(&self, other: &Self) -> bool {
        Arc::ptr_eq(&self.inner, &other.inner)
    }
}

/// A Socket Mode stream that survives `disconnect` and socket loss. A background task reads and
/// acks every envelope as it arrives, so a consumer slow to call `next` never causes redelivery.
/// The one exception is opt-in: see [`SocketMode::connect_answering_views`].
pub struct SocketMode {
    rx: mpsc::UnboundedReceiver<Result<SocketEvent, SlackError>>,
    stop: oneshot::Sender<()>,
    task: JoinHandle<()>,
    fatal: Option<SlackError>,
}

impl SocketMode {
    /// Fails if the first connection cannot be made, so a bad app token surfaces at startup.
    pub async fn connect(client: SlackClient) -> Result<SocketMode, SlackError> {
        SocketMode::open(client, false).await
    }

    /// Like [`SocketMode::connect`], except that a modal's submission is not acked on arrival: it
    /// comes as [`SocketEvent::Submission`] with the [`ViewAck`] to answer, so the app can keep the
    /// modal open with field errors. Answer within three seconds.
    pub async fn connect_answering_views(client: SlackClient) -> Result<SocketMode, SlackError> {
        SocketMode::open(client, true).await
    }

    async fn open(client: SlackClient, answer_views: bool) -> Result<SocketMode, SlackError> {
        let (ws, backlog) = dial(&client).await?;
        let (answers_tx, answers) = mpsc::unbounded_channel();
        let mut reader = Reader {
            client,
            ws: Some(ws),
            connected_at: Instant::now(),
            backlog,
            failures: 0,
            ping_sent: false,
            fatal: None,
            answer_views,
            answers_tx,
            answers,
        };
        let (tx, rx) = mpsc::unbounded_channel();
        let (stop, mut stopped) = oneshot::channel();
        let task = tokio::spawn(async move {
            loop {
                tokio::select! {
                    _ = &mut stopped => break,
                    event = reader.next() => match event {
                        Some(event) => {
                            if tx.send(Ok(event)).is_err() {
                                break;
                            }
                        }
                        None => {
                            if let Some(error) = reader.fatal.take() {
                                let _ = tx.send(Err(error));
                            }
                            return;
                        }
                    },
                }
            }
            if let Some(mut ws) = reader.ws.take() {
                let _ = timeout(CLOSE_TIMEOUT, ws.close(None)).await;
            }
        });
        Ok(SocketMode {
            rx,
            stop,
            task,
            fatal: None,
        })
    }

    /// `None` only after Slack refused to open a connection (for example a revoked app token);
    /// [`SocketMode::fatal_error`] says why.
    pub async fn next(&mut self) -> Option<SocketEvent> {
        match self.rx.recv().await? {
            Ok(event) => Some(event),
            Err(error) => {
                self.fatal = Some(error);
                None
            }
        }
    }

    pub fn fatal_error(&self) -> Option<&SlackError> {
        self.fatal.as_ref()
    }

    /// Closes the socket with a close frame, as dropping does in the background; events not yet
    /// taken with `next` are lost, but they were acked, so Slack will not resend them.
    pub async fn close(self) {
        let _ = self.stop.send(());
        let _ = timeout(CLOSE_TIMEOUT * 2, self.task).await;
    }
}

struct Reader {
    client: SlackClient,
    ws: Option<Ws>,
    connected_at: Instant,
    backlog: VecDeque<String>,
    failures: u32,
    ping_sent: bool,
    fatal: Option<SlackError>,
    /// Whether a modal's submission waits for its [`ViewAck`].
    answer_views: bool,
    answers_tx: mpsc::UnboundedSender<String>,
    /// Acks the app answered. Shared by every connection: an answer outliving the socket its
    /// submission came on goes out on the one that replaced it.
    answers: mpsc::UnboundedReceiver<String>,
}

enum Read {
    Frame(Option<Result<Message, tungstenite::Error>>),
    Silent,
    Answer(String),
}

impl Reader {
    async fn next(&mut self) -> Option<SocketEvent> {
        loop {
            if self.fatal.is_some() {
                return None;
            }
            if let Some(text) = self.backlog.pop_front() {
                if let Some(event) = self.handle(&text).await {
                    return Some(event);
                }
                continue;
            }
            let Some(ws) = self.ws.as_mut() else {
                self.redial().await;
                continue;
            };
            let read = tokio::select! {
                Some(answer) = self.answers.recv() => Read::Answer(answer),
                frame = timeout(IDLE_BEFORE_PING, ws.next()) => match frame {
                    Ok(frame) => Read::Frame(frame),
                    Err(_) => Read::Silent,
                },
            };
            let frame = match read {
                Read::Frame(frame) => frame,
                Read::Answer(answer) => {
                    if let Err(error) = ws.send(Message::Text(answer.into())).await {
                        tracing::warn!(%error, "could not answer a view submission");
                        self.lost();
                    }
                    continue;
                }
                Read::Silent if self.ping_sent => {
                    tracing::warn!("Socket Mode connection went silent; redialling");
                    self.lost();
                    continue;
                }
                Read::Silent => {
                    self.ping_sent = true;
                    if ws.send(Message::Ping(Vec::new().into())).await.is_err() {
                        self.lost();
                    }
                    continue;
                }
            };
            self.ping_sent = false;
            match frame {
                Some(Ok(Message::Text(text))) => {
                    if let Some(event) = self.handle(text.as_str()).await {
                        return Some(event);
                    }
                }
                Some(Ok(Message::Close(_))) | None => self.lost(),
                Some(Err(error)) => {
                    tracing::warn!(%error, "Socket Mode connection failed; redialling");
                    self.lost();
                }
                Some(Ok(_)) => {}
            }
        }
    }

    async fn handle(&mut self, text: &str) -> Option<SocketEvent> {
        let Ok(frame) = serde_json::from_str::<Value>(text) else {
            tracing::warn!(frame = text, "Socket Mode frame is not JSON");
            return None;
        };
        let kind = frame
            .get("type")
            .and_then(Value::as_str)
            .unwrap_or_default();
        match kind {
            "hello" => return None,
            "disconnect" => {
                let reason = frame
                    .get("reason")
                    .and_then(Value::as_str)
                    .unwrap_or_default();
                tracing::info!(reason, "Slack asked for a new Socket Mode connection");
                self.refresh().await;
                return None;
            }
            _ => {}
        }
        let Some(envelope_id) = frame.get("envelope_id").and_then(Value::as_str) else {
            tracing::warn!(kind, "Socket Mode frame has no envelope_id");
            return None;
        };
        let payload = || frame.get("payload").cloned().unwrap_or(Value::Null);
        let submission = kind == "interactive"
            && frame.pointer("/payload/type").and_then(Value::as_str) == Some("view_submission");
        if submission && self.answer_views {
            let ack = ViewAck {
                inner: Arc::new(Pending {
                    envelope_id: envelope_id.to_owned(),
                    answered: Mutex::new(false),
                    answers: self.answers_tx.clone(),
                }),
            };
            return Some(SocketEvent::Submission {
                payload: payload(),
                ack,
            });
        }
        self.ack(envelope_id).await;
        match kind {
            "events_api" => EventEnvelope::decode(&frame).map(SocketEvent::Event),
            "interactive" => Some(SocketEvent::Interactive(payload())),
            "slash_commands" => Some(SocketEvent::SlashCommand(payload())),
            other => {
                tracing::warn!(kind = other, "unknown Socket Mode envelope type");
                None
            }
        }
    }

    async fn ack(&mut self, envelope_id: &str) {
        let Some(ws) = self.ws.as_mut() else {
            return;
        };
        let ack = json!({"envelope_id": envelope_id}).to_string();
        if let Err(error) = ws.send(Message::Text(ack.into())).await {
            tracing::warn!(%error, envelope_id, "could not ack; Slack will redeliver");
            self.lost();
        }
    }

    async fn refresh(&mut self) {
        let old = self.ws.take();
        self.redial().await;
        if let Some(mut old) = old {
            let _ = timeout(CLOSE_TIMEOUT, old.close(None)).await;
        }
    }

    fn lost(&mut self) {
        self.ws = None;
        self.ping_sent = false;
        if self.connected_at.elapsed() < SHORT_LIVED {
            self.failures += 1;
        } else {
            self.failures = 0;
        }
    }

    async fn redial(&mut self) {
        loop {
            if self.failures > 0 {
                tokio::time::sleep(backoff(self.failures)).await;
            }
            match dial(&self.client).await {
                Ok((ws, early)) => {
                    self.ws = Some(ws);
                    self.connected_at = Instant::now();
                    self.backlog.extend(early);
                    return;
                }
                Err(error @ SlackError::Api { .. }) => {
                    tracing::error!(%error, "Slack refused a Socket Mode connection");
                    self.fatal = Some(error);
                    return;
                }
                Err(error) => {
                    tracing::warn!(%error, attempt = self.failures, "Socket Mode dial failed");
                    self.failures += 1;
                }
            }
        }
    }
}

fn backoff(failures: u32) -> Duration {
    let exp = BACKOFF_BASE.saturating_mul(1 << failures.saturating_sub(1).min(16));
    exp.min(BACKOFF_MAX).mul_f64(rand::random_range(0.5..=1.0))
}

async fn dial(client: &SlackClient) -> Result<(Ws, VecDeque<String>), SlackError> {
    let url = client.open_connection().await?;
    let (mut ws, _) = tokio_tungstenite::connect_async(url.as_str())
        .await
        .map_err(Box::new)?;
    let mut early = VecDeque::new();
    let deadline = Instant::now() + HELLO_TIMEOUT;
    loop {
        let frame = tokio::time::timeout_at(deadline, ws.next())
            .await
            .map_err(|_| closed("no hello within 10s"))?;
        match frame {
            Some(Ok(Message::Text(text))) => {
                let is_hello = serde_json::from_str::<Value>(text.as_str())
                    .ok()
                    .and_then(|v| v.get("type").and_then(Value::as_str).map(|t| t == "hello"))
                    .unwrap_or(false);
                if is_hello {
                    return Ok((ws, early));
                }
                early.push_back(text.as_str().to_owned());
            }
            Some(Ok(Message::Close(_))) | None => {
                return Err(closed("closed before hello"));
            }
            Some(Err(error)) => return Err(Box::new(error).into()),
            Some(Ok(_)) => {}
        }
    }
}

fn closed(why: &str) -> SlackError {
    let io = std::io::Error::new(std::io::ErrorKind::TimedOut, why.to_owned());
    Box::new(tungstenite::Error::Io(io)).into()
}
