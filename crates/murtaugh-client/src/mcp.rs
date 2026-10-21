//! The tools a session lends the agent: every MCP server the editor named for it, connected here
//! on the person's machine, and its files: through the editor's own file system when it offers
//! one, and from this machine's disk under the folders `--root` shares. The agent runs on a node;
//! its calls come back over RAX and are answered from here.

use std::collections::HashMap;
use std::sync::Arc;

use rax::tool::{ToolDef, ToolKind, ToolOutcome};
use rmcp::model::{CallToolRequestParams, ContentBlock};
use rmcp::service::RunningService;
use rmcp::transport::{StreamableHttpClientTransport, TokioChildProcess};
use rmcp::{RoleClient, ServiceExt};
use serde_json::{Value, json};

pub const READ_TEXT_FILE: &str = "read_text_file";
pub const WRITE_TEXT_FILE: &str = "write_text_file";
pub const EDIT_TEXT_FILE: &str = "edit_text_file";
pub const LIST_DIRECTORY: &str = "list_directory";

type Client = RunningService<RoleClient, ()>;

/// Where a lent tool's calls go.
#[derive(Clone)]
pub enum Route {
    Mcp { client: Arc<Client>, name: String },
    ReadFile,
    WriteFile,
    EditFile,
    ListDirectory,
}

/// One session's tools, and the servers behind them, closed when the session is.
#[derive(Default)]
pub struct Lent {
    pub tools: Vec<ToolDef>,
    pub routes: HashMap<String, Route>,
    clients: Vec<Arc<Client>>,
}

impl Lent {
    /// Connects each server in the editor's `mcpServers`. A server that cannot be reached is
    /// logged and left out: the session still opens with the rest.
    pub async fn connect(servers: &[Value], fs: Fs, local: bool) -> Self {
        let mut lent = Self::default();
        for server in servers {
            let name = server["name"].as_str().unwrap_or("mcp").to_owned();
            match connect(server).await {
                Ok(client) => lent.adopt(&name, client).await,
                Err(reason) => {
                    tracing::warn!(server = %name, %reason, "could not connect an MCP server; the session opens without it");
                }
            }
        }
        let (read, write) = (fs.read || local, fs.write || local);
        if read {
            let description = if fs.read {
                "Reads a text file on the person's machine as the editor has it, unsaved changes included."
            } else {
                "Reads a text file on the person's machine."
            };
            lent.add(
                ToolDef {
                    name: READ_TEXT_FILE.into(),
                    description: description.into(),
                    input_schema: Some(json!({
                        "type": "object",
                        "properties": {
                            "path": {"type": "string", "description": "Absolute path"},
                            "line": {"type": "integer", "description": "First line, from 1"},
                            "limit": {"type": "integer", "description": "How many lines"},
                        },
                        "required": ["path"],
                    })),
                    kind: ToolKind::Read,
                },
                Route::ReadFile,
            );
        }
        if write {
            lent.add(
                ToolDef {
                    name: WRITE_TEXT_FILE.into(),
                    description:
                        "Writes a whole text file on the person's machine, creating it if needed."
                            .into(),
                    input_schema: Some(json!({
                        "type": "object",
                        "properties": {
                            "path": {"type": "string", "description": "Absolute path"},
                            "content": {"type": "string"},
                        },
                        "required": ["path", "content"],
                    })),
                    kind: ToolKind::Edit,
                },
                Route::WriteFile,
            );
        }
        if read && write {
            lent.add(
                ToolDef {
                    name: EDIT_TEXT_FILE.into(),
                    description: "Edits a text file on the person's machine by replacing old_string with new_string. old_string must appear exactly once unless replace_all is set.".into(),
                    input_schema: Some(json!({
                        "type": "object",
                        "properties": {
                            "path": {"type": "string", "description": "Absolute path"},
                            "old_string": {"type": "string", "description": "The exact text to replace"},
                            "new_string": {"type": "string", "description": "What replaces it"},
                            "replace_all": {"type": "boolean", "description": "Replace every occurrence"},
                        },
                        "required": ["path", "old_string", "new_string"],
                    })),
                    kind: ToolKind::Edit,
                },
                Route::EditFile,
            );
        }
        if local {
            lent.add(
                ToolDef {
                    name: LIST_DIRECTORY.into(),
                    description: "Lists a folder on the person's machine; folders end in `/`."
                        .into(),
                    input_schema: Some(json!({
                        "type": "object",
                        "properties": {
                            "path": {"type": "string", "description": "Absolute path"},
                        },
                        "required": ["path"],
                    })),
                    kind: ToolKind::Read,
                },
                Route::ListDirectory,
            );
        }
        lent
    }

    async fn adopt(&mut self, server: &str, client: Client) {
        let client = Arc::new(client);
        let tools = match client.list_all_tools().await {
            Ok(tools) => tools,
            Err(err) => {
                tracing::warn!(%server, error = %err, "could not list an MCP server's tools");
                return;
            }
        };
        for tool in tools {
            let annotations = tool.annotations.as_ref();
            let kind = if annotations.and_then(|a| a.read_only_hint) == Some(true) {
                ToolKind::Read
            } else if annotations.and_then(|a| a.destructive_hint) == Some(true) {
                ToolKind::Delete
            } else {
                ToolKind::Other
            };
            let def = ToolDef {
                name: tool.name.to_string(),
                description: tool
                    .description
                    .as_ref()
                    .map(|d| d.to_string())
                    .unwrap_or_default(),
                input_schema: Some(Value::Object((*tool.input_schema).clone())),
                kind,
            };
            let route = Route::Mcp {
                client: client.clone(),
                name: tool.name.to_string(),
            };
            self.add(def, route);
        }
        self.clients.push(client);
    }

    /// A second tool under a name already taken is left out: the agent sees names, not servers.
    fn add(&mut self, def: ToolDef, route: Route) {
        if self.routes.contains_key(&def.name) {
            tracing::warn!(tool = %def.name, "two tools share a name; keeping the first");
            return;
        }
        self.routes.insert(def.name.clone(), route);
        self.tools.push(def);
    }

    pub async fn close(self) {
        for client in self.clients {
            if let Ok(client) = Arc::try_unwrap(client) {
                let _ = client.cancel().await;
            }
        }
    }
}

/// What the editor's file system offers, from its `initialize`.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Fs {
    pub read: bool,
    pub write: bool,
}

async fn connect(server: &Value) -> Result<Client, String> {
    match server["type"].as_str() {
        Some("http") => {
            let url = server["url"].as_str().ok_or("an http server has no url")?;
            let transport = StreamableHttpClientTransport::from_uri(url.to_owned());
            ().serve(transport).await.map_err(|err| err.to_string())
        }
        Some(other) if other != "stdio" => Err(format!("{other} MCP servers are not supported")),
        _ => {
            let command = server["command"]
                .as_str()
                .ok_or("a stdio server has no command")?;
            let mut child = tokio::process::Command::new(command);
            for arg in server["args"].as_array().into_iter().flatten() {
                if let Some(arg) = arg.as_str() {
                    child.arg(arg);
                }
            }
            for variable in server["env"].as_array().into_iter().flatten() {
                if let (Some(name), Some(value)) =
                    (variable["name"].as_str(), variable["value"].as_str())
                {
                    child.env(name, value);
                }
            }
            let transport = TokioChildProcess::new(child).map_err(|err| err.to_string())?;
            ().serve(transport).await.map_err(|err| err.to_string())
        }
    }
}

/// Runs an MCP tool. A tool that ran and failed is an answer with `is_error`, never a fault.
pub async fn call(client: &Client, name: &str, arguments: Option<Value>) -> ToolOutcome {
    let mut params = CallToolRequestParams::new(name.to_owned());
    if let Some(Value::Object(arguments)) = arguments {
        params = params.with_arguments(arguments);
    }
    match client.call_tool(params).await {
        Ok(result) => {
            let content = result
                .content
                .iter()
                .filter_map(|block| match block {
                    ContentBlock::Text(text) => Some(text.text.clone()),
                    other => serde_json::to_string(other).ok(),
                })
                .collect::<Vec<_>>()
                .join("\n");
            ToolOutcome {
                content,
                is_error: result.is_error == Some(true),
                resources: Vec::new(),
            }
        }
        Err(err) => ToolOutcome {
            content: format!("the MCP server failed the call: {err}"),
            is_error: true,
            resources: Vec::new(),
        },
    }
}
