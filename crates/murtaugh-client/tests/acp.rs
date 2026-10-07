#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

//! The ACP bridge between an editor (an ACP test client), Murtaugh (a RAX acceptor playing the
//! node, as Murtaugh does) and the editor's MCP servers (a stub over HTTP).

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use agent_client_protocol::schema::v1::{
    InitializeRequest, InitializeResponse, NewSessionRequest, NewSessionResponse, PromptRequest,
    PromptResponse, ReadTextFileRequest, ReadTextFileResponse, RequestPermissionRequest,
    RequestPermissionResponse, SessionNotification, WriteTextFileRequest, WriteTextFileResponse,
};
use agent_client_protocol::{Agent, Channel, Client, ConnectionTo, Responder};
use murtaugh_client::bridge::{self, Options};
use rax::content::ContentBlock;
use rax::event::StopReason;
use rax::id::{RequestId, SessionId};
use rax::session::{
    Initialized, NewSession, NodeCapabilities, PromptAccepted, SessionCreated, ToolGate,
};
use rax::tool::{CallTool, Decision, ToolCall, ToolKind};
use rax::{Event, GatewayCall, GatewayReply, NodeCall, NodeReply};
use rax_tokio::Role;
use rax_tokio::accept::{
    AcceptConfig, Accepted, AcceptedLinks, Acceptor, Authenticator, GatewayIdentity, NodeIdentity,
};
use rax_tokio::node::{NewNodeLink, NodeEvent, NodeEvents, NodeHandle};
use rmcp::model::{
    CallToolRequestParams, CallToolResponse, CallToolResult, ListToolsResult,
    PaginatedRequestParams, ServerCapabilities, ServerConfig, Tool, ToolAnnotations,
};
use rmcp::service::RequestContext;
use rmcp::transport::streamable_http_server::{
    StreamableHttpServerConfig, StreamableHttpService, session::local::LocalSessionManager,
};
use rmcp::{ErrorData, RoleServer, ServerHandler};
use serde_json::{Value, json};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc;

const TOKEN: &str = "mrtg_user_0123456789abcdef_c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0LXNlY3I";

async fn within<F: std::future::Future>(future: F) -> F::Output {
    tokio::time::timeout(Duration::from_secs(20), future)
        .await
        .expect("timed out")
}

struct Users;

impl Authenticator for Users {
    fn authenticate(&self, _token: &str) -> Option<NodeIdentity> {
        None
    }

    fn authenticate_gateway(&self, token: &str) -> Option<GatewayIdentity> {
        (token == TOKEN).then(|| GatewayIdentity("client-1".into()))
    }
}

/// Murtaugh as the bridge meets it: an acceptor that plays the node.
struct Murtaugh {
    acceptor: Acceptor,
    links: AcceptedLinks,
}

impl Murtaugh {
    async fn start() -> Self {
        let config = AcceptConfig {
            roles: vec![Role::Gateway],
            keepalive: Duration::from_secs(1),
            ..Default::default()
        };
        let (acceptor, links) = Acceptor::bind("127.0.0.1:0", Users, config).await.unwrap();
        Self { acceptor, links }
    }

    fn url(&self, addr: Option<SocketAddr>) -> String {
        format!("ws://{}", addr.unwrap_or(self.acceptor.local_addr()))
    }

    async fn accepted(&mut self) -> Bridge {
        let Accepted::Gateway(NewNodeLink { handle, events, .. }) =
            within(self.links.recv()).await.unwrap()
        else {
            panic!("expected the bridge")
        };
        let mut bridge = Bridge { handle, events };
        let NodeEvent::Request {
            id,
            call: GatewayCall::Initialize(offer),
        } = bridge.next().await
        else {
            panic!("expected initialize")
        };
        assert!(!offer.capabilities.question && !offer.capabilities.plan);
        let reply = GatewayReply::Initialize(Initialized {
            protocol_version: rax::PROTOCOL_VERSION,
            capabilities: NodeCapabilities {
                tool_gate: ToolGate::EveryCall,
                tool_groups: true,
                interruptible: Some(true),
                ..Default::default()
            },
            metadata: Default::default(),
        });
        bridge.handle.reply(id, reply).await.unwrap();
        bridge
    }
}

/// The bridge's link, from Murtaugh's side.
struct Bridge {
    handle: NodeHandle,
    events: NodeEvents,
}

impl Bridge {
    async fn next(&mut self) -> NodeEvent {
        within(self.events.recv())
            .await
            .expect("bridge events ended")
    }

    async fn opens(&mut self, session: &str) -> NewSession {
        let NodeEvent::Request {
            id,
            call: GatewayCall::NewSession(request),
        } = self.next().await
        else {
            panic!("expected session.new")
        };
        let created = SessionCreated {
            session_id: SessionId(session.into()),
            unhandled: vec![],
        };
        self.handle
            .reply(id, GatewayReply::NewSession(created))
            .await
            .unwrap();
        request
    }

    async fn prompted(&mut self) -> (RequestId, String) {
        loop {
            if let NodeEvent::Request {
                id,
                call: GatewayCall::Prompt(prompt),
            } = self.next().await
            {
                self.handle
                    .reply(id.clone(), GatewayReply::Prompt(PromptAccepted::default()))
                    .await
                    .unwrap();
                let text = prompt
                    .content
                    .iter()
                    .filter_map(|block| match block {
                        rax::Open::Known(ContentBlock::Text { text }) => Some(text.clone()),
                        _ => None,
                    })
                    .collect();
                return (id, text);
            }
        }
    }

    async fn say_and_finish(&self, stream: &RequestId, text: &str) {
        let content = ContentBlock::text(text).into();
        self.handle
            .event(stream.clone(), Event::Message { content })
            .await
            .unwrap();
        let stop_reason = Some(StopReason::EndTurn);
        self.handle
            .event(stream.clone(), Event::Complete { stop_reason })
            .await
            .unwrap();
        self.handle.end(stream.clone()).await.unwrap();
    }

    async fn call_tool(&self, session: &str, name: &str, arguments: Value) -> NodeReply {
        self.handle
            .call(NodeCall::CallTool(CallTool {
                session_id: SessionId(session.into()),
                namespace: Some("acp".into()),
                name: name.into(),
                arguments: Some(arguments),
            }))
            .await
            .unwrap()
    }
}

#[derive(Clone)]
struct EchoServer;

impl ServerHandler for EchoServer {
    fn get_info(&self) -> ServerConfig {
        ServerConfig::new(ServerCapabilities::builder().enable_tools().build())
    }

    async fn list_tools(
        &self,
        _request: Option<PaginatedRequestParams>,
        _context: RequestContext<RoleServer>,
    ) -> Result<ListToolsResult, ErrorData> {
        let schema = json!({"type": "object", "properties": {"text": {"type": "string"}}});
        let Value::Object(schema) = schema else {
            unreachable!()
        };
        let tool = Tool::new("echo", "Says it back", schema)
            .annotate(ToolAnnotations::new().read_only(true));
        Ok(ListToolsResult::with_all_items(vec![tool]))
    }

    async fn call_tool(
        &self,
        request: CallToolRequestParams,
        _context: RequestContext<RoleServer>,
    ) -> Result<CallToolResponse, ErrorData> {
        let text = request
            .arguments
            .as_ref()
            .and_then(|arguments| arguments.get("text"))
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        Ok(
            CallToolResult::success(vec![rmcp::model::ContentBlock::text(format!(
                "echo: {text}"
            ))])
            .into(),
        )
    }
}

async fn mcp_server() -> String {
    let service = StreamableHttpService::new(
        || Ok(EchoServer),
        Arc::new(LocalSessionManager::default()),
        StreamableHttpServerConfig::default(),
    );
    let router = axum::Router::new().nest_service("/mcp", service);
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}/mcp", listener.local_addr().unwrap());
    tokio::spawn(async move { axum::serve(listener, router).await.unwrap() });
    url
}

/// The editor: an ACP client that records every update and file write, and answers permission
/// prompts with `allow`.
struct Editor {
    updates: mpsc::UnboundedReceiver<Value>,
    asked: mpsc::UnboundedReceiver<Value>,
    writes: Arc<Mutex<HashMap<String, String>>>,
    requests: mpsc::UnboundedSender<Request>,
}

enum Request {
    Initialize(Value, tokio::sync::oneshot::Sender<Value>),
    NewSession(Value, tokio::sync::oneshot::Sender<Result<Value, String>>),
    Prompt(Value, tokio::sync::oneshot::Sender<Result<Value, String>>),
}

fn editor(options: Options) -> Editor {
    let (agent_end, client_end) = Channel::duplex();
    tokio::spawn(async move { bridge::serve(options, agent_end).await });
    let (updates_tx, updates) = mpsc::unbounded_channel();
    let (asked_tx, asked) = mpsc::unbounded_channel();
    let (requests, mut incoming) = mpsc::unbounded_channel::<Request>();
    let writes: Arc<Mutex<HashMap<String, String>>> = Arc::default();
    let files = writes.clone();
    tokio::spawn(async move {
        Client
            .builder()
            .on_receive_notification(
                async move |notification: SessionNotification, _cx: ConnectionTo<Agent>| {
                    let _ = updates_tx.send(serde_json::to_value(&notification).unwrap());
                    Ok(())
                },
                agent_client_protocol::on_receive_notification!(),
            )
            .on_receive_request(
                async move |request: RequestPermissionRequest,
                            responder: Responder<RequestPermissionResponse>,
                            _cx: ConnectionTo<Agent>| {
                    let _ = asked_tx.send(serde_json::to_value(&request).unwrap());
                    responder.respond(
                        serde_json::from_value(
                            json!({"outcome": {"outcome": "selected", "optionId": "allow"}}),
                        )
                        .unwrap(),
                    )
                },
                agent_client_protocol::on_receive_request!(),
            )
            .on_receive_request(
                {
                    let files = files.clone();
                    async move |request: WriteTextFileRequest,
                                responder: Responder<WriteTextFileResponse>,
                                _cx: ConnectionTo<Agent>| {
                        let request = serde_json::to_value(&request).unwrap();
                        files.lock().unwrap().insert(
                            request["path"].as_str().unwrap().to_owned(),
                            request["content"].as_str().unwrap().to_owned(),
                        );
                        responder.respond(serde_json::from_value(json!({})).unwrap())
                    }
                },
                agent_client_protocol::on_receive_request!(),
            )
            .on_receive_request(
                async move |request: ReadTextFileRequest,
                            responder: Responder<ReadTextFileResponse>,
                            _cx: ConnectionTo<Agent>| {
                    let path = serde_json::to_value(&request).unwrap()["path"].clone();
                    responder.respond(
                        serde_json::from_value(
                            json!({"content": format!("contents of {}", path.as_str().unwrap())}),
                        )
                        .unwrap(),
                    )
                },
                agent_client_protocol::on_receive_request!(),
            )
            .connect_with(client_end, async move |cx: ConnectionTo<Agent>| {
                while let Some(request) = incoming.recv().await {
                    let cx = cx.clone();
                    tokio::spawn(async move {
                        match request {
                            Request::Initialize(body, reply) => {
                                let request: InitializeRequest =
                                    serde_json::from_value(body).unwrap();
                                let response: InitializeResponse =
                                    cx.send_request(request).block_task().await.unwrap();
                                let _ = reply.send(serde_json::to_value(&response).unwrap());
                            }
                            Request::NewSession(body, reply) => {
                                let request: NewSessionRequest =
                                    serde_json::from_value(body).unwrap();
                                let response = cx.send_request(request).block_task().await;
                                let _ = reply.send(
                                    response
                                        .map(|r: NewSessionResponse| {
                                            serde_json::to_value(&r).unwrap()
                                        })
                                        .map_err(|err| err.to_string()),
                                );
                            }
                            Request::Prompt(body, reply) => {
                                let request: PromptRequest = serde_json::from_value(body).unwrap();
                                let response = cx.send_request(request).block_task().await;
                                let _ = reply.send(
                                    response
                                        .map(|r: PromptResponse| serde_json::to_value(&r).unwrap())
                                        .map_err(|err| err.to_string()),
                                );
                            }
                        }
                    });
                }
                Ok(())
            })
            .await
    });
    Editor {
        updates,
        asked,
        writes,
        requests,
    }
}

impl Editor {
    async fn initialize(&self, fs: bool) -> Value {
        let (reply, answer) = tokio::sync::oneshot::channel();
        let body = json!({
            "protocolVersion": 1,
            "clientCapabilities": {"fs": {"readTextFile": fs, "writeTextFile": fs}},
        });
        self.requests
            .send(Request::Initialize(body, reply))
            .unwrap();
        within(answer).await.unwrap()
    }

    fn new_session(
        &self,
        mcp: Option<&str>,
    ) -> tokio::sync::oneshot::Receiver<Result<Value, String>> {
        let (reply, answer) = tokio::sync::oneshot::channel();
        let servers = match mcp {
            Some(url) => json!([{"type": "http", "name": "stub", "url": url, "headers": []}]),
            None => json!([]),
        };
        let body = json!({"cwd": "/work", "mcpServers": servers});
        self.requests
            .send(Request::NewSession(body, reply))
            .unwrap();
        answer
    }

    fn prompt(
        &self,
        session: &str,
        text: &str,
    ) -> tokio::sync::oneshot::Receiver<Result<Value, String>> {
        let (reply, answer) = tokio::sync::oneshot::channel();
        let body = json!({"sessionId": session, "prompt": [{"type": "text", "text": text}]});
        self.requests.send(Request::Prompt(body, reply)).unwrap();
        answer
    }

    async fn update(&mut self) -> Value {
        within(self.updates.recv()).await.unwrap()["update"].clone()
    }
}

fn options(url: String) -> Options {
    Options {
        gateway: url,
        token: TOKEN.into(),
        name: "acp".into(),
    }
}

async fn opened(
    editor: &Editor,
    murtaugh: &mut Murtaugh,
    mcp: Option<&str>,
) -> (Bridge, NewSession) {
    let answer = editor.new_session(mcp);
    let mut bridge = murtaugh.accepted().await;
    let request = bridge.opens("s1").await;
    let opened = within(answer).await.unwrap().unwrap();
    assert_eq!(opened["sessionId"], "s1");
    (bridge, request)
}

#[tokio::test(flavor = "multi_thread")]
async fn a_session_opens_and_a_prompt_round_trips() {
    let mut murtaugh = Murtaugh::start().await;
    let mut editor = editor(options(murtaugh.url(None)));
    let initialized = editor.initialize(false).await;
    assert_eq!(initialized["agentCapabilities"]["loadSession"], false);
    let (mut bridge, request) = opened(&editor, &mut murtaugh, None).await;
    assert!(
        request.tool_groups.is_empty(),
        "nothing to lend: {request:?}"
    );

    let answer = editor.prompt("s1", "hello");
    let (stream, text) = bridge.prompted().await;
    assert_eq!(text, "hello");
    bridge.say_and_finish(&stream, "hi there").await;
    let update = editor.update().await;
    assert_eq!(update["sessionUpdate"], "agent_message_chunk");
    assert_eq!(update["content"]["text"], "hi there");
    let done = within(answer).await.unwrap().unwrap();
    assert_eq!(done["stopReason"], "end_turn");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_tool_call_reaches_the_editors_mcp_server() {
    let mut murtaugh = Murtaugh::start().await;
    let editor = editor(options(murtaugh.url(None)));
    editor.initialize(false).await;
    let mcp = mcp_server().await;
    let (bridge, request) = opened(&editor, &mut murtaugh, Some(&mcp)).await;
    let [group] = request.tool_groups.as_slice() else {
        panic!("expected one group: {request:?}")
    };
    assert_eq!(group.namespace, "acp");
    assert_eq!(group.tools[0].name, "echo");
    assert_eq!(group.tools[0].kind, ToolKind::Read);

    let NodeReply::CallTool(outcome) = bridge
        .call_tool("s1", "echo", json!({"text": "ping"}))
        .await
    else {
        panic!("expected a tool outcome")
    };
    assert_eq!(outcome.content, "echo: ping");
    assert!(!outcome.is_error);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_write_is_applied_through_the_editors_file_system() {
    let mut murtaugh = Murtaugh::start().await;
    let editor = editor(options(murtaugh.url(None)));
    editor.initialize(true).await;
    let (bridge, request) = opened(&editor, &mut murtaugh, None).await;
    let names: Vec<&str> = request.tool_groups[0]
        .tools
        .iter()
        .map(|tool| tool.name.as_str())
        .collect();
    assert_eq!(names, ["read_text_file", "write_text_file"]);

    let NodeReply::CallTool(outcome) = bridge
        .call_tool(
            "s1",
            "write_text_file",
            json!({"path": "/work/a.rs", "content": "fn main() {}"}),
        )
        .await
    else {
        panic!("expected a tool outcome")
    };
    assert!(!outcome.is_error, "{outcome:?}");
    assert_eq!(
        editor
            .writes
            .lock()
            .unwrap()
            .get("/work/a.rs")
            .map(String::as_str),
        Some("fn main() {}")
    );
    let NodeReply::CallTool(read) = bridge
        .call_tool("s1", "read_text_file", json!({"path": "/work/a.rs"}))
        .await
    else {
        panic!("expected a tool outcome")
    };
    assert_eq!(read.content, "contents of /work/a.rs");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_held_call_is_put_to_the_person_in_the_editor() {
    let mut murtaugh = Murtaugh::start().await;
    let mut editor = editor(options(murtaugh.url(None)));
    editor.initialize(false).await;
    let (mut bridge, _) = opened(&editor, &mut murtaugh, None).await;
    let answer = editor.prompt("s1", "do it");
    let (stream, _) = bridge.prompted().await;
    let tool_call = ToolCall {
        id: "tc1".into(),
        name: "Bash".into(),
        title: Some("rm -rf build".into()),
        kind: ToolKind::Execute,
        input: Some(json!({"command": "rm -rf build"})),
        content: vec![],
    };
    bridge
        .handle
        .event(stream.clone(), Event::ToolCall { tool_call })
        .await
        .unwrap();
    let asked = within(editor.asked.recv()).await.unwrap();
    assert_eq!(asked["toolCall"]["title"], "rm -rf build");
    let verdict = loop {
        if let NodeEvent::Verdict(verdict) = bridge.next().await {
            break verdict;
        }
    };
    assert_eq!(verdict.decision, Decision::Allow);
    bridge.say_and_finish(&stream, "done").await;
    while editor.update().await["sessionUpdate"] != "agent_message_chunk" {}
    assert_eq!(
        within(answer).await.unwrap().unwrap()["stopReason"],
        "end_turn"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn a_dropped_link_resumes_and_the_session_carries_on() {
    let mut murtaugh = Murtaugh::start().await;
    let proxy = Proxy::start(murtaugh.acceptor.local_addr()).await;
    let mut editor = editor(options(murtaugh.url(Some(proxy.addr))));
    editor.initialize(false).await;
    let (mut bridge, _) = opened(&editor, &mut murtaugh, None).await;

    proxy.sever();
    loop {
        match bridge.next().await {
            NodeEvent::Resumed => break,
            NodeEvent::Disconnected { .. } => {}
            other => panic!("expected the link to resume, got {other:?}"),
        }
    }
    let answer = editor.prompt("s1", "still there?");
    let (stream, text) = bridge.prompted().await;
    assert_eq!(text, "still there?");
    bridge.say_and_finish(&stream, "yes").await;
    assert_eq!(editor.update().await["content"]["text"], "yes");
    assert_eq!(
        within(answer).await.unwrap().unwrap()["stopReason"],
        "end_turn"
    );
}

struct Proxy {
    addr: SocketAddr,
    connections: Arc<Mutex<Vec<tokio::task::JoinHandle<()>>>>,
}

impl Proxy {
    async fn start(upstream: SocketAddr) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let connections: Arc<Mutex<Vec<tokio::task::JoinHandle<()>>>> = Arc::default();
        let held = connections.clone();
        tokio::spawn(async move {
            while let Ok((mut down, _)) = listener.accept().await {
                let relay = tokio::spawn(async move {
                    let Ok(mut up) = TcpStream::connect(upstream).await else {
                        return;
                    };
                    let _ = tokio::io::copy_bidirectional(&mut down, &mut up).await;
                });
                held.lock().unwrap().push(relay);
            }
        });
        Self { addr, connections }
    }

    fn sever(&self) {
        for relay in self.connections.lock().unwrap().drain(..) {
            relay.abort();
        }
    }
}
