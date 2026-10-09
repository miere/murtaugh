//! Tools this gateway runs on a node's behalf. They are lent to each session as the `slack` group
//! when it opens; the node publishes the group to its agent and calls `tool.call`. It never learns
//! what any of them do, so adding one here reaches every node without any of them shipping a
//! release.

use std::collections::HashMap;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::Duration;

use async_trait::async_trait;
use murtaugh_store::{Conversation, Store};
use rax::id::{LocalFileId, RequestId, SessionId};
use rax::local_file::{ReadLocalFile, takes_local_files};
use rax::tool::{CallTool, ToolDef, ToolGroup, ToolOutcome};
use rax::{Error, ErrorKind, NodeReply};
use rax_tokio::gateway::{GatewayLink, LocalFile};
use serde_json::Value;

pub mod attach;
pub mod canvas;
pub mod send_message;
pub mod slack_message;

/// The group every Slack thread's session is lent, so the agent sees `mcp__slack__…`.
pub const NAMESPACE: &str = "slack";
/// The namespace of the deprecated catalogue sent at `initialize`, which older nodes still publish
/// under the names they always had.
pub const LEGACY_NAMESPACE: &str = "murtaugh";

#[async_trait]
pub trait Tool: Send + Sync {
    fn def(&self) -> ToolDef;
    /// The name this tool has in the deprecated `initialize` catalogue, kept so an older node's
    /// agent keeps the names its owner's prompts and whitelists already use.
    fn legacy_name(&self) -> String {
        self.def().name
    }
    /// `Err` is the tool's own answer, which the agent reads and may act on — not a failure of the
    /// call. A call that cannot run at all never reaches here.
    async fn invoke(&self, arguments: Value) -> Result<String, String>;

    /// For a tool that acts inside the calling session's conversation. Most do not.
    async fn invoke_in(&self, context: &Context, arguments: Value) -> Result<String, String> {
        let _ = context;
        self.invoke(arguments).await
    }
}

/// Where a call came from, resolved by the gateway and never taken from the node.
#[derive(Default)]
pub struct Context {
    /// The Slack thread the calling session is pinned to, if any.
    pub conversation: Option<Conversation>,
    /// The calling node's files, for a tool with an argument marked as a local file.
    pub local_files: Option<Arc<dyn LocalFiles>>,
}

/// Reads what a node offered in place of a path. `Err` is worded for the agent.
#[async_trait]
pub trait LocalFiles: Send + Sync {
    async fn read(&self, id: &str) -> Result<LocalFile, String>;
}

/// Long enough for 100 MiB over a home connection; a node that stalls must not hold the call
/// open for good.
const LOCAL_FILE_WAIT: Duration = Duration::from_secs(120);

/// The files of the node on the other end of one link.
struct NodeFiles {
    link: GatewayLink,
}

#[async_trait]
impl LocalFiles for NodeFiles {
    async fn read(&self, id: &str) -> Result<LocalFile, String> {
        let read = ReadLocalFile {
            id: LocalFileId(id.to_owned()),
            max_bytes: None,
        };
        match tokio::time::timeout(LOCAL_FILE_WAIT, self.link.read_local_file(read)).await {
            Ok(Ok(file)) => Ok(file),
            Ok(Err(err)) => Err(format!("your machine did not hand it over ({err})")),
            Err(_) => Err(format!(
                "your machine did not hand it over within {} seconds",
                LOCAL_FILE_WAIT.as_secs()
            )),
        }
    }
}

/// Whether a tool needs a node that swaps paths for local file identifiers. Any other node would
/// send the raw path, which this gateway could do nothing with.
fn takes_files(tool: &dyn Tool) -> bool {
    tool.def()
        .input_schema
        .as_ref()
        .is_some_and(takes_local_files)
}

#[derive(Clone, Default)]
pub struct Tools {
    tools: Arc<Vec<Box<dyn Tool>>>,
}

impl Tools {
    pub fn new(tools: Vec<Box<dyn Tool>>) -> Self {
        Self {
            tools: Arc::new(tools),
        }
    }

    /// The group a Slack thread's session is opened with. `None` when this gateway offers nothing,
    /// so it lends no group rather than an empty one. A tool that takes a file from the node is
    /// lent only to a node that declared `local_files`.
    pub fn group(&self, local_files: bool) -> Option<ToolGroup> {
        let tools: Vec<ToolDef> = self
            .tools
            .iter()
            .filter(|tool| local_files || !takes_files(tool.as_ref()))
            .map(|tool| tool.def())
            .collect();
        if tools.is_empty() {
            return None;
        }
        Some(ToolGroup {
            namespace: NAMESPACE.to_owned(),
            tools,
        })
    }

    /// The deprecated per-link catalogue, for nodes that do not declare `tool_groups`. It keeps
    /// the namespace and names those nodes have always published.
    pub fn catalogue(&self) -> Option<ToolGroup> {
        let tools: Vec<ToolDef> = self
            .legacy()
            .map(|tool| ToolDef {
                name: tool.legacy_name(),
                ..tool.def()
            })
            .collect();
        if tools.is_empty() {
            return None;
        }
        Some(ToolGroup {
            namespace: LEGACY_NAMESPACE.to_owned(),
            tools,
        })
    }

    /// What a node that only knows the `initialize` catalogue may call. Such a node predates
    /// local files, so a tool that takes one is not among them.
    fn legacy(&self) -> impl Iterator<Item = &Box<dyn Tool>> {
        self.tools.iter().filter(|tool| !takes_files(tool.as_ref()))
    }

    /// Runs a call if its session was lent the group it names. A call naming no group comes from
    /// a node that only knows the `initialize` catalogue, which belongs to the link as it always
    /// did.
    pub async fn serve(
        &self,
        link: &GatewayLink,
        lent: &Lent,
        node: &str,
        local_files: bool,
        id: RequestId,
        call: CallTool,
    ) {
        let selector = &link.identity().0;
        let tool = match &call.namespace {
            Some(namespace) => {
                let lent = lent.groups(selector, &call.session_id).await;
                if lent.iter().any(|group| group == namespace) && namespace == NAMESPACE {
                    self.tools
                        .iter()
                        .filter(|tool| local_files || !takes_files(tool.as_ref()))
                        .find(|tool| tool.def().name == call.name)
                } else {
                    None
                }
            }
            None => self.legacy().find(|tool| tool.legacy_name() == call.name),
        };
        let Some(tool) = tool else {
            // The discipline `resource.read` already uses: a node cannot find a tool by guessing.
            let unoffered = Error::new(
                ErrorKind::Forbidden,
                match &call.namespace {
                    Some(namespace) => format!(
                        "this session was not lent a tool named {namespace}.{}",
                        call.name
                    ),
                    None => format!("this gateway publishes no tool named {}", call.name),
                },
            );
            if let Err(err) = link.fault(id, unoffered).await {
                tracing::debug!(node = %node, error = %err, "could not refuse a tool call");
            }
            return;
        };
        let context = Context {
            conversation: lent.conversation(selector, &call.session_id).await,
            local_files: Some(Arc::new(NodeFiles { link: link.clone() })),
        };
        let outcome = match tool
            .invoke_in(&context, call.arguments.unwrap_or(Value::Null))
            .await
        {
            Ok(content) => ToolOutcome {
                content,
                is_error: false,
            },
            Err(content) => ToolOutcome {
                content,
                is_error: true,
            },
        };
        tracing::info!(node = %node, tool = %call.name, namespace = ?call.namespace, failed = outcome.is_error, "served a tool call");
        if let Err(err) = link.reply(id, NodeReply::CallTool(outcome)).await {
            tracing::debug!(node = %node, error = %err, "could not answer a tool call");
        }
    }
}

/// Which groups each session was opened with, so a call for any other is refused whatever the
/// node sends. Sessions are recorded when they open; one this process did not open — a node that
/// stayed up across a gateway restart — is resolved from the pins instead: a session pinned to a
/// Slack thread was opened with the `slack` group.
#[derive(Clone)]
pub struct Lent {
    store: Arc<dyn Store>,
    sessions: Arc<Mutex<HashMap<NodeSession, Vec<String>>>>,
}

/// A session id is the node's own, so it is only unique beside the node's selector.
type NodeSession = (String, SessionId);

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

impl Lent {
    pub fn new(store: Arc<dyn Store>) -> Self {
        Self {
            store,
            sessions: Arc::default(),
        }
    }

    pub fn opened(&self, selector: &str, session: &SessionId, groups: &[ToolGroup]) {
        let namespaces = groups.iter().map(|group| group.namespace.clone()).collect();
        lock(&self.sessions).insert((selector.to_owned(), session.clone()), namespaces);
    }

    /// Forgets every session of a node whose link ended. A durable session that comes back is
    /// found through its pin.
    pub fn detached(&self, selector: &str) {
        lock(&self.sessions).retain(|(node, _), _| node != selector);
    }

    /// The Slack thread a session is pinned to.
    pub async fn conversation(&self, selector: &str, session: &SessionId) -> Option<Conversation> {
        match self.store.pins().await {
            Ok(pins) => pins
                .into_iter()
                .find(|pin| pin.node == selector && pin.session_id == session.0)
                .map(|pin| pin.conversation),
            Err(err) => {
                tracing::warn!(error = %err, "could not read the pins to find a session's thread");
                None
            }
        }
    }

    pub async fn groups(&self, selector: &str, session: &SessionId) -> Vec<String> {
        let key = (selector.to_owned(), session.clone());
        if let Some(groups) = lock(&self.sessions).get(&key) {
            return groups.clone();
        }
        let pins = match self.store.pins().await {
            Ok(pins) => pins,
            Err(err) => {
                tracing::warn!(error = %err, "could not read the pins to place a tool call");
                return Vec::new();
            }
        };
        let pinned = pins
            .iter()
            .any(|pin| pin.node == selector && pin.session_id == session.0);
        match pinned {
            true => vec![NAMESPACE.to_owned()],
            false => Vec::new(),
        }
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used)]

    use super::*;

    /// A gateway offering nothing lends no group and publishes no catalogue, rather than an empty
    /// one a node would have to tell apart from a real offer.
    #[test]
    fn a_gateway_with_no_tools_lends_nothing() {
        assert!(Tools::default().group(true).is_none());
        assert!(Tools::default().catalogue().is_none());
    }

    #[test]
    fn the_group_names_its_namespace_and_every_tool() {
        let tools = Tools::new(vec![Box::new(slack_message::SlackReadMessage::new(None))]);
        let group = tools.group(true).expect("a group");
        assert_eq!(group.namespace, "slack");
        let names: Vec<&str> = group.tools.iter().map(|t| t.name.as_str()).collect();
        assert_eq!(names, vec!["read_message"]);
        // The kind travels with the tool, so a node never has to guess it from the name.
        assert_eq!(group.tools[0].kind, rax::tool::ToolKind::Read);
    }

    /// A node that stayed up across a gateway restart keeps the session it was opened with, so its
    /// `slack` group is found through the thread's pin; a session nothing records was lent nothing.
    #[tokio::test]
    async fn a_session_this_process_did_not_open_is_lent_slack_if_a_thread_is_pinned_to_it() {
        use murtaugh_store::{Conversation, Pin, SqliteStore, UserId};

        let dir = tempfile::tempdir().unwrap();
        let store: Arc<dyn Store> =
            Arc::new(SqliteStore::open(&dir.path().join("config.db")).unwrap());
        store
            .set_pin(&Pin {
                conversation: Conversation {
                    channel: "C1".into(),
                    thread_ts: "1.0".into(),
                },
                node: "node-a".into(),
                session_id: "s1".into(),
                user: UserId::parse("U0ALICE01").unwrap(),
                pinned_at: time::OffsetDateTime::now_utc(),
            })
            .await
            .unwrap();
        let lent = Lent::new(store);
        let s1 = SessionId("s1".into());
        assert_eq!(lent.groups("node-a", &s1).await, ["slack"]);
        assert!(lent.groups("node-b", &s1).await.is_empty());
        assert!(
            lent.groups("node-a", &SessionId("s2".into()))
                .await
                .is_empty()
        );

        // What this process opened wins, and goes when the node's link does.
        lent.opened("node-b", &s1, &[]);
        assert!(lent.groups("node-b", &s1).await.is_empty());
        lent.opened(
            "node-c",
            &s1,
            &Tools::default().group(true).into_iter().collect::<Vec<_>>(),
        );
        lent.detached("node-c");
        assert!(lent.groups("node-c", &s1).await.is_empty());
    }

    /// A node that does not swap paths for identifiers would send `attach` a raw path, so it is
    /// never offered the tool: not in the group, and not in the catalogue older nodes read.
    #[test]
    fn a_tool_that_takes_a_file_is_lent_only_to_a_node_that_hands_files_over() {
        let tools = Tools::new(vec![
            Box::new(slack_message::SlackReadMessage::new(None)),
            Box::new(attach::Attach::new(None)),
        ]);
        let names = |group: Option<ToolGroup>| -> Vec<String> {
            let tools = group.into_iter().flat_map(|group| group.tools);
            tools.map(|tool| tool.name).collect()
        };
        assert_eq!(names(tools.group(true)), ["read_message", "attach"]);
        assert_eq!(names(tools.group(false)), ["read_message"]);
        assert_eq!(names(tools.catalogue()), ["slack_read_message"]);

        let only_attach = Tools::new(vec![Box::new(attach::Attach::new(None))]);
        assert!(only_attach.group(false).is_none());
        assert!(only_attach.catalogue().is_none());
    }

    /// An older node keeps publishing `mcp__murtaugh__slack_read_message` until it upgrades.
    #[test]
    fn the_deprecated_catalogue_keeps_the_names_older_nodes_publish() {
        let tools = Tools::new(vec![
            Box::new(slack_message::SlackReadMessage::new(None)),
            Box::new(canvas::read::ReadCanvas::new(None)),
        ]);
        let catalogue = tools.catalogue().expect("a catalogue");
        assert_eq!(catalogue.namespace, "murtaugh");
        let names: Vec<&str> = catalogue.tools.iter().map(|t| t.name.as_str()).collect();
        assert_eq!(names, vec!["slack_read_message", "read_canvas"]);
    }
}
