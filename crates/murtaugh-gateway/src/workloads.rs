//! `POST /api/v1/workloads`: runs a prompt on one of the caller's nodes and answers in Slack. It
//! returns as soon as the run is accepted, and fails closed: every check below must pass first.
//!
//! The caller is whoever owns the client token, which must carry the `workloads` scope. The run is
//! theirs exactly as if they had written the prompt in Slack: their nodes, their node owners'
//! tool rules, and approvals for the node's owner alone.

use std::collections::HashMap;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::{Duration, Instant};

use axum::Router;
use axum::body::Bytes;
use axum::extract::State;
use axum::http::{HeaderMap, HeaderValue, StatusCode, header};
use axum::response::{IntoResponse, Response};
use axum::routing::post;
use murtaugh_slack::{SlackClient, SlackError};
use murtaugh_store::{Scope, UserId, UserToken};
use serde::{Deserialize, Serialize};
use serde_json::json;

use crate::access::Access;
use crate::chat::{Chat, Workload};
use crate::fleet::Fleet;

pub const PATH: &str = "/api/v1/workloads";
/// The most a prompt may be. A prompt is a request, not a document: files belong in Slack.
pub const PROMPT_MAX: usize = 32 * 1024;
/// A body over this is refused before it is read as JSON.
const BODY_MAX: usize = 256 * 1024;
/// How long an `Idempotency-Key` names the run it started.
pub const IDEMPOTENCY_TTL: Duration = Duration::from_secs(24 * 60 * 60);
/// When a caller with no node free should try again.
const RETRY_AFTER_SECS: u64 = 30;

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

#[derive(Clone)]
pub struct Workloads {
    inner: Arc<Inner>,
}

struct Inner {
    access: Access,
    fleet: Fleet,
    slack: SlackClient,
    chat: Arc<Chat>,
    /// The runs each token's keys started, so a retried request starts nothing new.
    started: Mutex<HashMap<(String, String), (Instant, Accepted)>>,
}

impl Workloads {
    pub fn new(access: Access, fleet: Fleet, slack: SlackClient, chat: Arc<Chat>) -> Self {
        Self {
            inner: Arc::new(Inner {
                access,
                fleet,
                slack,
                chat,
                started: Mutex::new(HashMap::new()),
            }),
        }
    }

    pub fn router(self) -> Router {
        Router::new()
            .route(PATH, post(accept))
            .layer(axum::extract::DefaultBodyLimit::max(BODY_MAX))
            .with_state(self)
    }
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    prompt: String,
    target: Target,
    #[serde(default)]
    output: Output,
}

#[derive(Debug, Deserialize)]
#[serde(untagged)]
enum Target {
    Thread { channel: String, thread_ts: String },
    Channel { channel: String },
    Dm { dm: String },
}

#[derive(Debug, Default, Clone, Copy, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
enum Output {
    #[default]
    Stream,
    Quiet,
}

#[derive(Debug, Clone, Serialize)]
struct Accepted {
    workload: String,
    channel: String,
    thread_ts: String,
}

/// A refusal: a status and the one JSON shape every refusal has.
struct Refusal {
    status: StatusCode,
    code: &'static str,
    message: String,
    headers: Vec<(header::HeaderName, String)>,
}

impl Refusal {
    fn new(status: StatusCode, code: &'static str, message: impl Into<String>) -> Self {
        Self {
            status,
            code,
            message: message.into(),
            headers: Vec::new(),
        }
    }

    fn with(mut self, name: header::HeaderName, value: impl Into<String>) -> Self {
        self.headers.push((name, value.into()));
        self
    }
}

impl IntoResponse for Refusal {
    fn into_response(self) -> Response {
        let body = axum::Json(json!({"error": self.code, "message": self.message}));
        let mut response = (self.status, body).into_response();
        for (name, value) in self.headers {
            if let Ok(value) = HeaderValue::from_str(&value) {
                response.headers_mut().insert(name, value);
            }
        }
        response
    }
}

async fn accept(
    State(workloads): State<Workloads>,
    headers: HeaderMap,
    body: Bytes,
) -> Result<Response, Refusal> {
    let inner = &workloads.inner;
    let token = caller(inner, &headers)?;
    let key = headers
        .get("idempotency-key")
        .and_then(|value| value.to_str().ok())
        .map(str::trim)
        .filter(|key| !key.is_empty())
        .map(|key| (token.selector.clone(), key.to_owned()));
    if let Some(key) = &key
        && let Some(accepted) = inner.remembered(key)
    {
        return Ok((StatusCode::ACCEPTED, axum::Json(accepted)).into_response());
    }
    let request = parse(&body)?;
    let (channel, thread_ts, direct) = target(inner, &token, request.target).await?;
    if let Some(thread_ts) = &thread_ts
        && inner.chat.is_busy(&channel, thread_ts)
    {
        return Err(Refusal::new(
            StatusCode::CONFLICT,
            "busy",
            "that thread already has a turn running; try again when it ends",
        ));
    }
    let snapshot = inner.access.snapshot();
    if inner.fleet.choices(&token.owner, &snapshot).is_empty() {
        return Err(Refusal::new(
            StatusCode::SERVICE_UNAVAILABLE,
            "no_node",
            "none of the machines you may use is connected",
        )
        .with(header::RETRY_AFTER, RETRY_AFTER_SECS.to_string()));
    }
    let workload = Workload {
        owner: token.owner.clone(),
        channel,
        thread_ts,
        prompt: request.prompt,
        direct,
        quiet: request.output == Output::Quiet,
    };
    let (channel, thread_ts) = inner
        .chat
        .start_workload(workload)
        .await
        .map_err(|reason| Refusal::new(StatusCode::FORBIDDEN, "bot_cannot_post", reason))?;
    let accepted = Accepted {
        workload: hex::encode(rand::random::<[u8; 8]>()),
        channel,
        thread_ts,
    };
    tracing::info!(owner = %token.owner, client = %token.name, workload = %accepted.workload, channel = %accepted.channel, "accepted a workload");
    if let Some(key) = key {
        inner.remember(key, accepted.clone());
    }
    Ok((StatusCode::ACCEPTED, axum::Json(accepted)).into_response())
}

impl Inner {
    fn remembered(&self, key: &(String, String)) -> Option<Accepted> {
        let mut started = lock(&self.started);
        started.retain(|_, (at, _)| at.elapsed() < IDEMPOTENCY_TTL);
        started.get(key).map(|(_, accepted)| accepted.clone())
    }

    fn remember(&self, key: (String, String), accepted: Accepted) {
        lock(&self.started).insert(key, (Instant::now(), accepted));
    }
}

/// The token's owner, or `401` for no live token and `403` for one without the scope.
fn caller(inner: &Inner, headers: &HeaderMap) -> Result<UserToken, Refusal> {
    let unauthorized = || {
        Refusal::new(
            StatusCode::UNAUTHORIZED,
            "unauthorized",
            "send a live client token as `Authorization: Bearer mrtg_user_…`",
        )
        .with(header::WWW_AUTHENTICATE, "Bearer")
    };
    let presented = headers
        .get(header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .and_then(rax::transport::bearer_token)
        .ok_or_else(unauthorized)?;
    let token = inner
        .access
        .verify_client(presented)
        .ok_or_else(unauthorized)?;
    if !token.allows(Scope::Workloads) {
        return Err(Refusal::new(
            StatusCode::FORBIDDEN,
            "insufficient_scope",
            "this token does not open the workloads API; mint one with the `workloads` scope",
        )
        .with(
            header::WWW_AUTHENTICATE,
            r#"Bearer error="insufficient_scope", scope="workloads""#,
        ));
    }
    Ok(token)
}

fn parse(body: &[u8]) -> Result<Request, Refusal> {
    let request: Request = serde_json::from_slice(body).map_err(|err| {
        Refusal::new(
            StatusCode::BAD_REQUEST,
            "invalid_request",
            format!("the body is not a workload: {err}"),
        )
    })?;
    if request.prompt.trim().is_empty() {
        return Err(Refusal::new(
            StatusCode::BAD_REQUEST,
            "invalid_request",
            "`prompt` is empty",
        ));
    }
    if request.prompt.len() > PROMPT_MAX {
        return Err(Refusal::new(
            StatusCode::PAYLOAD_TOO_LARGE,
            "prompt_too_large",
            format!("`prompt` is over {PROMPT_MAX} bytes"),
        ));
    }
    Ok(request)
}

/// Where the run answers, once the bot is known to be able to post there: the channel, the thread
/// to continue if any, and whether it is a DM.
async fn target(
    inner: &Inner,
    token: &UserToken,
    target: Target,
) -> Result<(String, Option<String>, bool), Refusal> {
    let cannot_post = |channel: &str, why: String| {
        Refusal::new(
            StatusCode::FORBIDDEN,
            "bot_cannot_post",
            format!("the bot cannot post to {channel}: {why}"),
        )
    };
    let (channel, thread_ts) = match target {
        Target::Dm { dm } => {
            // `me` is the token's owner, for a caller that knows its token but not its Slack id.
            let is_owner = dm == "me" || UserId::parse(&dm).ok().as_ref() == Some(&token.owner);
            if !is_owner {
                return Err(Refusal::new(
                    StatusCode::FORBIDDEN,
                    "dm_not_owner",
                    "a workload may only be sent to the token owner's own DM",
                ));
            }
            let channel = inner
                .slack
                .open_dm(token.owner.as_str())
                .await
                .map_err(|err| cannot_post(&dm, slack_reason(&err)))?;
            return Ok((channel, None, true));
        }
        Target::Channel { channel } => (channel, None),
        Target::Thread { channel, thread_ts } => (channel, Some(thread_ts)),
    };
    let info = inner
        .slack
        .conversation_info(&channel)
        .await
        .map_err(|err| cannot_post(&channel, slack_reason(&err)))?;
    if info.is_archived {
        return Err(cannot_post(&channel, "it is archived".into()));
    }
    if !info.is_member && !info.is_im {
        return Err(cannot_post(
            &channel,
            "it is not a member; invite it".into(),
        ));
    }
    if let Some(thread_ts) = &thread_ts {
        match inner.slack.replies(&channel, thread_ts).await {
            Ok(messages) if !messages.is_empty() => {}
            Ok(_) | Err(SlackError::Api { .. }) => {
                return Err(Refusal::new(
                    StatusCode::NOT_FOUND,
                    "thread_not_found",
                    format!("there is no thread {thread_ts} in {channel}"),
                ));
            }
            Err(err) => return Err(cannot_post(&channel, slack_reason(&err))),
        }
    }
    Ok((channel, thread_ts, info.is_im))
}

fn slack_reason(err: &SlackError) -> String {
    match err {
        SlackError::Api { error, .. } => format!("Slack answered `{error}`"),
        other => other.to_string(),
    }
}
