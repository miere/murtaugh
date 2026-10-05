//! Tools this gateway runs on a node's behalf. The node publishes the catalogue to its agent and
//! calls `tool.call`; it never learns what any of them do, so adding one here reaches every node
//! without any of them shipping a release.

use std::sync::Arc;

use async_trait::async_trait;
use rax::id::RequestId;
use rax::tool::{CallTool, ToolCatalogue, ToolDef, ToolOutcome};
use rax::{Error, ErrorKind, NodeReply};
use rax_tokio::gateway::GatewayLink;
use serde_json::Value;

pub mod canvas;
pub mod slack_message;

/// Qualifies every tool name on the node, so the agent sees `mcp__murtaugh__…` — the same names
/// this toolset has when an agent runs beside the gateway instead of on a node.
pub const NAMESPACE: &str = "murtaugh";

#[async_trait]
pub trait Tool: Send + Sync {
    fn def(&self) -> ToolDef;
    /// `Err` is the tool's own answer, which the agent reads and may act on — not a failure of the
    /// call. A call that cannot run at all never reaches here.
    async fn invoke(&self, arguments: Value) -> Result<String, String>;
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

    /// `None` when this gateway offers nothing, so it publishes no catalogue rather than an empty
    /// one a node would have to tell apart from a real offer.
    pub fn catalogue(&self) -> Option<ToolCatalogue> {
        if self.tools.is_empty() {
            return None;
        }
        Some(ToolCatalogue {
            namespace: NAMESPACE.to_owned(),
            tools: self.tools.iter().map(|tool| tool.def()).collect(),
        })
    }

    pub async fn serve(&self, link: &GatewayLink, node: &str, id: RequestId, call: CallTool) {
        let Some(tool) = self.tools.iter().find(|tool| tool.def().name == call.name) else {
            // The discipline `resource.read` already uses: a node cannot find a tool by guessing.
            let unoffered = Error::new(
                ErrorKind::Forbidden,
                format!("this gateway publishes no tool named {}", call.name),
            );
            if let Err(err) = link.fault(id, unoffered).await {
                tracing::debug!(node = %node, error = %err, "could not refuse a tool call");
            }
            return;
        };
        let outcome = match tool.invoke(call.arguments.unwrap_or(Value::Null)).await {
            Ok(content) => ToolOutcome {
                content,
                is_error: false,
            },
            Err(content) => ToolOutcome {
                content,
                is_error: true,
            },
        };
        tracing::info!(node = %node, tool = %call.name, failed = outcome.is_error, "served a tool call");
        if let Err(err) = link.reply(id, NodeReply::CallTool(outcome)).await {
            tracing::debug!(node = %node, error = %err, "could not answer a tool call");
        }
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used)]

    use super::*;

    /// A gateway offering nothing publishes no catalogue, rather than an empty one a node would
    /// have to tell apart from a real offer.
    #[test]
    fn a_gateway_with_no_tools_publishes_no_catalogue() {
        assert!(Tools::default().catalogue().is_none());
    }

    #[test]
    fn the_catalogue_names_its_namespace_and_every_tool() {
        let tools = Tools::new(vec![Box::new(slack_message::SlackReadMessage::new(None))]);
        let catalogue = tools.catalogue().expect("a catalogue");
        assert_eq!(catalogue.namespace, NAMESPACE);
        let names: Vec<&str> = catalogue.tools.iter().map(|t| t.name.as_str()).collect();
        assert_eq!(names, vec![slack_message::NAME]);
        // The kind travels with the tool, so a node never has to guess it from the name.
        assert_eq!(catalogue.tools[0].kind, rax::tool::ToolKind::Read);
    }
}
