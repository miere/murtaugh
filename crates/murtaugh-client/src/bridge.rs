//! `murtaugh-client acp`: the agent side of ACP toward an editor, and a gateway-role RAX link
//! toward Murtaugh. Each ACP session is a Murtaugh session, run on a node of its fleet; the tools
//! the editor's MCP servers offer are lent to it as one group, and called back from here.
//!
//! The bridge knows ACP, MCP and RAX, and nothing about any one editor.

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::Duration;

use agent_client_protocol::schema::v1::{
    CancelNotification, CloseSessionRequest, CloseSessionResponse, InitializeRequest,
    InitializeResponse, NewSessionRequest, NewSessionResponse, PromptRequest, PromptResponse,
    ReadTextFileRequest, ReadTextFileResponse, RequestPermissionRequest, RequestPermissionResponse,
    SessionNotification, WriteTextFileRequest, WriteTextFileResponse,
};
use agent_client_protocol::{Agent, Client, ConnectTo, ConnectionTo, Responder};
use rax::content::ContentBlock;
use rax::event::BackgroundEvent;
use rax::id::{RequestId, SessionId};
use rax::resource::ReadResource;
use rax::session::{GatewayCapabilities, Initialize, NewSession, Prompt, SessionRef};
use rax::tool::{CallTool, Decision, DeniedBy, ToolCall, ToolGroup, ToolOutcome, ToolVerdict};
use rax::{Error, ErrorKind, Event, GatewayCall, GatewayReply, NodeCall, NodeReply, Open};
use rax_tokio::CallError;
use rax_tokio::dial::DialConfig;
use rax_tokio::gateway::{DialledEvent, DialledEvents, GatewayLink, LinkEvent, StreamEvents};
use serde::Serialize;
use serde::de::DeserializeOwned;
use serde_json::{Value, json};
use tokio::sync::{OnceCell, watch};

use crate::mcp::{self, Fs, Lent, Route};

/// How long a first dial may take before `session/new` gives up on Murtaugh.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(30);
const CALL_TIMEOUT: Duration = Duration::from_secs(120);

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

#[derive(Debug, Clone)]
pub struct Options {
    pub gateway: String,
    pub token: String,
    /// The namespace the editor's tools are lent under, such as `openknowledge`.
    pub name: String,
}

/// `[a-z0-9_]` and at most 27 characters, so Murtaugh's 5-character link suffix still fits a
/// namespace's 32.
pub fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 27
        && name
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '_')
}

struct Session {
    lent: Lent,
    /// Tool calls the editor has been shown, so an update for one it never saw draws it first.
    shown: HashSet<String>,
}

struct Shared {
    options: Options,
    editor: Mutex<Option<ConnectionTo<Client>>>,
    fs: Mutex<Fs>,
    link: OnceCell<GatewayLink>,
    /// True while the link is initialized; false after a fresh link until it is again.
    ready: watch::Sender<bool>,
    sessions: Mutex<HashMap<String, Session>>,
}

type SharedRef = Arc<Shared>;

/// Serves one editor over `transport` until it hangs up.
pub async fn serve(
    options: Options,
    transport: impl ConnectTo<Agent> + 'static,
) -> Result<(), agent_client_protocol::Error> {
    let shared: SharedRef = Arc::new(Shared {
        options,
        editor: Mutex::new(None),
        fs: Mutex::new(Fs::default()),
        link: OnceCell::new(),
        ready: watch::channel(false).0,
        sessions: Mutex::new(HashMap::new()),
    });
    let result = Agent
        .builder()
        .name("murtaugh-client")
        .on_receive_request(
            {
                let shared = shared.clone();
                async move |request: InitializeRequest,
                            responder: Responder<InitializeResponse>,
                            connection: ConnectionTo<Client>| {
                    *lock(&shared.editor) = Some(connection);
                    let offered = to_value(&request);
                    let fs = &offered["clientCapabilities"]["fs"];
                    *lock(&shared.fs) = Fs {
                        read: fs["readTextFile"].as_bool() == Some(true),
                        write: fs["writeTextFile"].as_bool() == Some(true),
                    };
                    let response = from_value(json!({
                        "protocolVersion": offered["protocolVersion"].as_u64().unwrap_or(1).min(1),
                        "agentCapabilities": {
                            "loadSession": false,
                            "promptCapabilities": {"image": false, "audio": false, "embeddedContext": false},
                            "sessionCapabilities": {"close": {}},
                        },
                        "agentInfo": {"name": "murtaugh-client", "version": murtaugh_common::version::VERSION},
                        "authMethods": [],
                    }))?;
                    responder.respond(response)
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let shared = shared.clone();
                async move |request: NewSessionRequest,
                            responder: Responder<NewSessionResponse>,
                            connection: ConnectionTo<Client>| {
                    let shared = shared.clone();
                    connection.spawn(async move {
                        match new_session(&shared, request).await {
                            Ok(session_id) => responder
                                .respond(from_value(json!({"sessionId": session_id}))?),
                            Err(message) => responder.respond_with_error(failure(message)),
                        }
                    })
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let shared = shared.clone();
                async move |request: PromptRequest,
                            responder: Responder<PromptResponse>,
                            connection: ConnectionTo<Client>| {
                    let shared = shared.clone();
                    connection.spawn(async move {
                        match prompt(&shared, request).await {
                            Ok(stop_reason) => responder
                                .respond(from_value(json!({"stopReason": stop_reason}))?),
                            Err(message) => responder.respond_with_error(failure(message)),
                        }
                    })
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_request(
            {
                let shared = shared.clone();
                async move |request: CloseSessionRequest,
                            responder: Responder<CloseSessionResponse>,
                            connection: ConnectionTo<Client>| {
                    let shared = shared.clone();
                    connection.spawn(async move {
                        let session_id = to_value(&request)["sessionId"]
                            .as_str()
                            .unwrap_or_default()
                            .to_owned();
                        close_session(&shared, &session_id).await;
                        responder.respond(from_value(json!({}))?)
                    })
                }
            },
            agent_client_protocol::on_receive_request!(),
        )
        .on_receive_notification(
            {
                let shared = shared.clone();
                async move |notification: CancelNotification, connection: ConnectionTo<Client>| {
                    let shared = shared.clone();
                    let session_id = to_value(&notification)["sessionId"]
                        .as_str()
                        .unwrap_or_default()
                        .to_owned();
                    connection.spawn(async move {
                        if let Some(link) = shared.link.get() {
                            let cancel = GatewayCall::Cancel(SessionRef {
                                session_id: SessionId(session_id),
                            });
                            if let Err(err) = call(link, cancel).await {
                                tracing::debug!(error = %err, "Murtaugh did not take a cancel");
                            }
                        }
                        Ok(())
                    })
                }
            },
            agent_client_protocol::on_receive_notification!(),
        )
        .connect_to(transport)
        .await;
    let sessions: Vec<Session> = lock(&shared.sessions).drain().map(|(_, s)| s).collect();
    for session in sessions {
        session.lent.close().await;
    }
    if let Some(link) = shared.link.get() {
        link.close().await;
    }
    result
}

fn to_value(value: &impl Serialize) -> Value {
    serde_json::to_value(value).unwrap_or(Value::Null)
}

fn from_value<T: DeserializeOwned>(value: Value) -> Result<T, agent_client_protocol::Error> {
    serde_json::from_value(value).map_err(agent_client_protocol::util::internal_error)
}

fn failure(message: String) -> agent_client_protocol::Error {
    tracing::warn!(%message, "an editor request failed");
    agent_client_protocol::util::internal_error(message)
}

fn editor(shared: &Shared) -> Option<ConnectionTo<Client>> {
    lock(&shared.editor).clone()
}

/// Dials Murtaugh on the first session, and waits until the link is initialized.
async fn link(shared: &SharedRef) -> Result<GatewayLink, String> {
    let link = shared
        .link
        .get_or_try_init(|| async {
            let config = DialConfig {
                endpoints: vec![shared.options.gateway.clone()],
                token: shared.options.token.clone(),
                ..DialConfig::default()
            };
            let (link, events) = GatewayLink::dial(config).map_err(|err| err.to_string())?;
            tokio::spawn(pump(shared.clone(), link.clone(), events));
            Ok::<_, String>(link)
        })
        .await?
        .clone();
    let mut ready = shared.ready.subscribe();
    match tokio::time::timeout(CONNECT_TIMEOUT, ready.wait_for(|ready| *ready)).await {
        Ok(Ok(_)) => Ok(link),
        Ok(Err(_)) | Err(_) => Err(format!(
            "could not reach Murtaugh at {} (see the client's log for why)",
            shared.options.gateway
        )),
    }
}

/// The link's own events: every fresh link is introduced with `initialize`, and Murtaugh's calls
/// for the editor's tools and files are answered.
async fn pump(shared: SharedRef, link: GatewayLink, mut events: DialledEvents) {
    while let Some(event) = events.recv().await {
        match event {
            DialledEvent::Fresh => {
                shared.ready.send_replace(false);
                let fresh = !lock(&shared.sessions).is_empty();
                if fresh {
                    tracing::warn!("the link to Murtaugh started over; its open sessions are gone");
                }
                match initialize(&shared, &link).await {
                    Ok(()) => {
                        shared.ready.send_replace(true);
                    }
                    Err(reason) => tracing::error!(%reason, "Murtaugh did not initialize"),
                }
            }
            DialledEvent::CredentialRejected => {
                tracing::error!(
                    "Murtaugh rejected the client token; mint a new one from its Home tab"
                );
            }
            DialledEvent::Refused { status } => {
                tracing::error!(
                    status,
                    "Murtaugh refused this client; it may not serve the RAX API"
                );
            }
            DialledEvent::Link(LinkEvent::Request { id, call }) => {
                let (shared, link) = (shared.clone(), link.clone());
                tokio::spawn(async move { answer(&shared, &link, id, call).await });
            }
            DialledEvent::Link(LinkEvent::Background { session_id, event }) => {
                background(&shared, &link, &session_id.0, event).await;
            }
            DialledEvent::Link(LinkEvent::Disconnected { reason }) => {
                tracing::info!(%reason, "the link to Murtaugh dropped; resuming");
            }
            DialledEvent::Link(LinkEvent::Resumed) => {
                tracing::info!("the link to Murtaugh resumed")
            }
            DialledEvent::Link(LinkEvent::Closed | LinkEvent::Replaced) => break,
            DialledEvent::Link(other) => tracing::debug!(event = ?other, "link event"),
        }
    }
    shared.ready.send_replace(false);
}

async fn initialize(shared: &Shared, link: &GatewayLink) -> Result<(), String> {
    let fs = *lock(&shared.fs);
    let offer = GatewayCall::Initialize(Initialize {
        protocol_version: rax::PROTOCOL_VERSION,
        capabilities: GatewayCapabilities {
            question: false,
            plan: false,
            sign_in: false,
            resource_schemes: vec![],
            readable_schemes: if fs.read { vec!["file".into()] } else { vec![] },
            tools: None,
            attachment_receipts: false,
        },
    });
    match call(link, offer).await {
        Ok(GatewayReply::Initialize(initialized)) => {
            tracing::info!(
                protocol_version = initialized.protocol_version,
                "Murtaugh initialized"
            );
            Ok(())
        }
        Ok(other) => Err(format!("answered initialize with {other:?}")),
        Err(err) => Err(err.to_string()),
    }
}

async fn call(link: &GatewayLink, call: GatewayCall) -> Result<GatewayReply, CallError> {
    let pending = link.call(call).await?;
    tokio::time::timeout(CALL_TIMEOUT, pending.reply)
        .await
        .unwrap_or(Err(CallError::Closed))
}

fn reason(err: CallError) -> String {
    match err {
        CallError::Fault(error) => error.message,
        CallError::LinkReset => {
            "the link to Murtaugh started over, so this was lost; try again".to_owned()
        }
        other => other.to_string(),
    }
}

async fn new_session(shared: &SharedRef, request: NewSessionRequest) -> Result<String, String> {
    let link = link(shared).await?;
    let request = to_value(&request);
    let servers = request["mcpServers"]
        .as_array()
        .cloned()
        .unwrap_or_default();
    let fs = *lock(&shared.fs);
    let lent = Lent::connect(&servers, fs).await;
    let tool_groups = if lent.tools.is_empty() {
        vec![]
    } else {
        vec![ToolGroup {
            namespace: shared.options.name.clone(),
            tools: lent.tools.clone(),
        }]
    };
    let mut context = vec![];
    if let Some(cwd) = request["cwd"].as_str() {
        context.push(Open::Known(ContentBlock::text(format!(
            "The editor's working directory is {cwd}."
        ))));
    }
    let opening = GatewayCall::NewSession(NewSession {
        context,
        tool_groups,
    });
    let created = match call(&link, opening).await {
        Ok(GatewayReply::NewSession(created)) => created,
        Ok(other) => {
            lent.close().await;
            return Err(format!("Murtaugh answered session/new with {other:?}"));
        }
        Err(err) => {
            lent.close().await;
            return Err(reason(err));
        }
    };
    for unhandled in &created.unhandled {
        tracing::warn!(subject = ?unhandled.subject, reason = ?unhandled.reason, message = ?unhandled.message, "Murtaugh could not take part of the session");
    }
    let id = created.session_id.0.clone();
    lock(&shared.sessions).insert(
        id.clone(),
        Session {
            lent,
            shown: HashSet::new(),
        },
    );
    tracing::info!(session = %id, "session opened");
    Ok(id)
}

async fn close_session(shared: &SharedRef, session_id: &str) {
    let session = lock(&shared.sessions).remove(session_id);
    if let Some(link) = shared.link.get() {
        let close = GatewayCall::CloseSession(SessionRef {
            session_id: SessionId(session_id.to_owned()),
        });
        if let Err(err) = call(link, close).await {
            tracing::debug!(error = %err, "Murtaugh did not take a close");
        }
    }
    if let Some(session) = session {
        session.lent.close().await;
    }
}

/// Runs a turn, drawing it in the editor as it goes, and returns its stop reason.
async fn prompt(shared: &SharedRef, request: PromptRequest) -> Result<String, String> {
    let link = link(shared).await?;
    let request = to_value(&request);
    let session_id = request["sessionId"].as_str().unwrap_or_default().to_owned();
    let content: Vec<Open<ContentBlock>> = request["prompt"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|block| serde_json::from_value(block.clone()).ok())
        .collect();
    let pending = link
        .call(GatewayCall::Prompt(Prompt {
            session_id: SessionId(session_id.clone()),
            content,
        }))
        .await
        .map_err(reason)?;
    match tokio::time::timeout(CALL_TIMEOUT, pending.reply).await {
        Ok(Ok(GatewayReply::Prompt(_))) => {}
        Ok(Ok(other)) => return Err(format!("Murtaugh answered the prompt with {other:?}")),
        Ok(Err(err)) => return Err(reason(err)),
        Err(_) => return Err("Murtaugh did not take the prompt in time".to_owned()),
    }
    turn(shared, &link, &session_id, pending.events).await
}

async fn turn(
    shared: &SharedRef,
    link: &GatewayLink,
    session_id: &str,
    mut events: StreamEvents,
) -> Result<String, String> {
    let mut stop_reason = None;
    let mut failed = None;
    while let Some(event) = events.recv().await {
        let Open::Known(event) = event else {
            continue;
        };
        match event {
            Event::Complete {
                stop_reason: reason,
            } => {
                stop_reason = Some(
                    reason
                        .and_then(|reason| to_value(&reason).as_str().map(str::to_owned))
                        .unwrap_or_else(|| "end_turn".to_owned()),
                );
            }
            Event::Error { error } => failed = Some(error.message),
            Event::ToolCall { tool_call } => {
                let shared = shared.clone();
                let (link, session_id) = (link.clone(), session_id.to_owned());
                tokio::spawn(async move { ask(&shared, &link, &session_id, tool_call).await });
            }
            other => draw(shared, session_id, other),
        }
    }
    if let Some(message) = failed {
        return Err(message);
    }
    stop_reason.ok_or_else(|| {
        if link.is_closed() {
            "the link to Murtaugh closed, so this turn was lost".to_owned()
        } else {
            "the link to Murtaugh started over, so this turn was lost; send it again".to_owned()
        }
    })
}

/// What the editor draws of an event, if anything.
fn draw(shared: &Shared, session_id: &str, event: Event) {
    let update = match event {
        Event::Message { content } => {
            json!({"sessionUpdate": "agent_message_chunk", "content": to_value(&content)})
        }
        Event::Status { text } => json!({
            "sessionUpdate": "agent_thought_chunk",
            "content": {"type": "text", "text": text},
        }),
        Event::PlanUpdate { entries } => {
            json!({"sessionUpdate": "plan", "entries": to_value(&entries)})
        }
        Event::ToolCallUpdate { tool_call_update } => {
            let id = tool_call_update.id.0.clone();
            let first = lock(&shared.sessions)
                .get_mut(session_id)
                .is_some_and(|session| session.shown.insert(id.clone()));
            let status = match tool_call_update.status {
                rax::tool::ToolCallStatus::InProgress => "in_progress",
                rax::tool::ToolCallStatus::Completed => "completed",
                rax::tool::ToolCallStatus::Failed | rax::tool::ToolCallStatus::Denied => "failed",
            };
            let content: Vec<Value> = tool_call_update
                .content
                .iter()
                .map(|block| json!({"type": "content", "content": to_value(block)}))
                .collect();
            let title = tool_call_update.title.clone().unwrap_or_else(|| id.clone());
            if first {
                notify(
                    shared,
                    session_id,
                    json!({"sessionUpdate": "tool_call", "toolCallId": id, "title": title, "status": status}),
                );
            }
            let mut update =
                json!({"sessionUpdate": "tool_call_update", "toolCallId": id, "status": status});
            if !content.is_empty() {
                update["content"] = json!(content);
            }
            if let Some(output) = tool_call_update.output {
                update["rawOutput"] = output;
            }
            update
        }
        other => {
            tracing::debug!(event = ?other, "not drawn in the editor");
            return;
        }
    };
    notify(shared, session_id, update);
}

fn notify(shared: &Shared, session_id: &str, update: Value) {
    let Some(editor) = editor(shared) else {
        return;
    };
    let notification: Result<SessionNotification, _> =
        serde_json::from_value(json!({"sessionId": session_id, "update": update}));
    match notification {
        Ok(notification) => {
            if let Err(err) = editor.send_notification(notification) {
                tracing::debug!(error = %err, "could not update the editor");
            }
        }
        Err(err) => tracing::warn!(error = %err, "an update the editor would not take"),
    }
}

/// A held call goes to the person in the editor, and their answer to Murtaugh as the verdict.
async fn ask(shared: &SharedRef, link: &GatewayLink, session_id: &str, tool_call: ToolCall) {
    let id = tool_call.id.0.clone();
    let kind = to_value(&tool_call.kind);
    let title = tool_call
        .title
        .clone()
        .unwrap_or_else(|| tool_call.name.clone());
    if let Some(session) = lock(&shared.sessions).get_mut(session_id) {
        session.shown.insert(id.clone());
    }
    let mut call = json!({"toolCallId": id, "title": title, "kind": kind, "status": "pending"});
    if let Some(input) = &tool_call.input {
        call["rawInput"] = input.clone();
    }
    let mut shown = call.clone();
    shown["sessionUpdate"] = json!("tool_call");
    notify(shared, session_id, shown);
    let decision = match permission(shared, session_id, call).await {
        Ok(true) => Decision::Allow,
        Ok(false) => Decision::Deny {
            by: DeniedBy::User,
            reason: Some("The person in the editor declined this.".into()),
        },
        Err(reason) => Decision::Deny {
            by: DeniedBy::Unavailable,
            reason: Some(format!("Nobody in the editor could be asked: {reason}")),
        },
    };
    let verdict = ToolVerdict {
        id: tool_call.id,
        decision,
    };
    if let Err(err) = link.verdict(verdict).await {
        tracing::warn!(error = %err, "could not send a verdict to Murtaugh");
    }
}

async fn permission(shared: &Shared, session_id: &str, call: Value) -> Result<bool, String> {
    let editor = editor(shared).ok_or("no editor is attached")?;
    let request: RequestPermissionRequest = serde_json::from_value(json!({
        "sessionId": session_id,
        "toolCall": call,
        "options": [
            {"optionId": "allow", "name": "Allow", "kind": "allow_once"},
            {"optionId": "reject", "name": "Reject", "kind": "reject_once"},
        ],
    }))
    .map_err(|err| err.to_string())?;
    let response: RequestPermissionResponse = editor
        .send_request(request)
        .block_task()
        .await
        .map_err(|err| err.to_string())?;
    let outcome = &to_value(&response)["outcome"];
    Ok(outcome["outcome"] == "selected" && outcome["optionId"] == "allow")
}

/// Something a session did with no turn open.
async fn background(
    shared: &SharedRef,
    link: &GatewayLink,
    session_id: &str,
    event: Open<BackgroundEvent>,
) {
    let Open::Known(event) = event else {
        return;
    };
    let event = match event {
        BackgroundEvent::ToolCall { tool_call } => {
            let shared = shared.clone();
            let (link, session_id) = (link.clone(), session_id.to_owned());
            tokio::spawn(async move { ask(&shared, &link, &session_id, tool_call).await });
            return;
        }
        BackgroundEvent::Message { content } => Event::Message { content },
        BackgroundEvent::Status { text } => Event::Status { text },
        BackgroundEvent::ToolCallUpdate { tool_call_update } => {
            Event::ToolCallUpdate { tool_call_update }
        }
        BackgroundEvent::PlanUpdate { entries } => Event::PlanUpdate { entries },
        other => {
            tracing::debug!(event = ?other, "background event not drawn");
            return;
        }
    };
    draw(shared, session_id, event);
}

/// Murtaugh calls the tools the editor lent, and reads the files its links name.
async fn answer(shared: &SharedRef, link: &GatewayLink, id: RequestId, call: NodeCall) {
    let sent = match call {
        NodeCall::CallTool(call) => {
            let outcome = tool(shared, call).await;
            match outcome {
                Ok(outcome) => link.reply(id, NodeReply::CallTool(outcome)).await,
                Err(error) => link.fault(id, error).await,
            }
        }
        NodeCall::ReadResource(read) => match file(shared, &read).await {
            Ok(text) => {
                let served = link
                    .serve_resource(id, &read, text.as_bytes(), Some("text/plain".into()))
                    .await;
                if let Err(err) = served {
                    tracing::debug!(error = %err, "could not serve a file to Murtaugh");
                }
                return;
            }
            Err(error) => link.fault(id, error).await,
        },
        other => {
            let unsupported = Error::new(
                ErrorKind::Unsupported,
                format!("this client does not serve {other:?}"),
            );
            link.fault(id, unsupported).await
        }
    };
    if let Err(err) = sent {
        tracing::debug!(error = %err, "could not answer Murtaugh");
    }
}

async fn tool(shared: &Shared, call: CallTool) -> Result<ToolOutcome, Error> {
    if call.namespace.as_deref() != Some(shared.options.name.as_str()) {
        return Err(Error::new(
            ErrorKind::Forbidden,
            format!("this client lends no group named {:?}", call.namespace),
        ));
    }
    let route = lock(&shared.sessions)
        .get(&call.session_id.0)
        .map(|session| session.lent.routes.get(&call.name).cloned());
    let route = match route {
        None => {
            return Err(Error::new(
                ErrorKind::UnknownSession,
                format!("no session {}", call.session_id),
            ));
        }
        Some(None) => {
            return Err(Error::new(
                ErrorKind::Forbidden,
                format!("this session was not lent a tool named {}", call.name),
            ));
        }
        Some(Some(route)) => route,
    };
    let arguments = call.arguments.unwrap_or(Value::Null);
    let outcome = match route {
        Route::Mcp { client, name } => mcp::call(&client, &name, Some(arguments)).await,
        Route::ReadFile => {
            let mut request = json!({"sessionId": call.session_id.0, "path": arguments["path"]});
            for key in ["line", "limit"] {
                if !arguments[key].is_null() {
                    request[key] = arguments[key].clone();
                }
            }
            match read_text(shared, request).await {
                Ok(content) => ToolOutcome {
                    content,
                    is_error: false,
                },
                Err(reason) => ToolOutcome {
                    content: reason,
                    is_error: true,
                },
            }
        }
        Route::WriteFile => {
            let path = arguments["path"].as_str().unwrap_or_default().to_owned();
            let request = json!({
                "sessionId": call.session_id.0,
                "path": path,
                "content": arguments["content"],
            });
            match write_text(shared, request).await {
                Ok(()) => ToolOutcome {
                    content: format!("Wrote {path}."),
                    is_error: false,
                },
                Err(reason) => ToolOutcome {
                    content: reason,
                    is_error: true,
                },
            }
        }
    };
    Ok(outcome)
}

async fn read_text(shared: &Shared, request: Value) -> Result<String, String> {
    let editor = editor(shared).ok_or("no editor is attached")?;
    let request: ReadTextFileRequest =
        serde_json::from_value(request).map_err(|err| err.to_string())?;
    let response: ReadTextFileResponse = editor
        .send_request(request)
        .block_task()
        .await
        .map_err(|err| err.to_string())?;
    Ok(to_value(&response)["content"]
        .as_str()
        .unwrap_or_default()
        .to_owned())
}

async fn write_text(shared: &Shared, request: Value) -> Result<(), String> {
    let editor = editor(shared).ok_or("no editor is attached")?;
    let request: WriteTextFileRequest =
        serde_json::from_value(request).map_err(|err| err.to_string())?;
    let _: WriteTextFileResponse = editor
        .send_request(request)
        .block_task()
        .await
        .map_err(|err| err.to_string())?;
    Ok(())
}

/// A `file://` link the editor sent, read as the editor has it open. Anything else is not found.
async fn file(shared: &Shared, read: &ReadResource) -> Result<String, Error> {
    let not_found = |why: &str| Error::new(ErrorKind::NotFound, format!("{}: {why}", read.uri));
    let path = url::Url::parse(&read.uri)
        .ok()
        .filter(|url| url.scheme() == "file")
        .and_then(|url| url.to_file_path().ok())
        .ok_or_else(|| not_found("only file:// links are served"))?;
    if !lock(&shared.fs).read {
        return Err(not_found("the editor offers no file system"));
    }
    let session = lock(&shared.sessions).keys().next().cloned();
    let session = session.ok_or_else(|| not_found("no session is open to read it through"))?;
    read_text(
        shared,
        json!({"sessionId": session, "path": path.display().to_string()}),
    )
    .await
    .map_err(|reason| not_found(&reason))
}
