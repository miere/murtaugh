//! How Murtaugh reaches a node's owner: a card in their Slack DM, for a sign-in or for a client's
//! call only they may approve.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use murtaugh_store::Store;
use rax::Error;
use rax::interaction::{SignInRequest, SignInSettled};
use rax_gateway::OwnerChannel;
use rax_gateway::embed::{NodeRef, ToolApproval, deny_unasked};

use crate::approval::{self, Approvals};
use crate::signin::SignIns;

/// How the node's owner is asked about a client's call when the client is not theirs: a card in
/// their Slack DM, answered through the same handler as a thread's cards.
#[derive(Clone)]
pub struct OwnerApproval {
    pub slack: murtaugh_slack::SlackClient,
    pub approvals: Approvals,
    pub timeout: Duration,
}

pub struct SlackOwner {
    sign_ins: SignIns,
    /// `None` where there is no Slack to ask in: a call only the owner may approve is denied.
    asking: Option<(OwnerApproval, Arc<dyn Store>)>,
}

impl SlackOwner {
    pub fn new(sign_ins: SignIns, asking: Option<(OwnerApproval, Arc<dyn Store>)>) -> Self {
        Self { sign_ins, asking }
    }
}

#[async_trait]
impl OwnerChannel for SlackOwner {
    fn signs_in(&self) -> bool {
        true
    }

    async fn ask_tool_approval(&self, ask: ToolApproval) {
        let Some((asking, store)) = self.asking.clone() else {
            deny_unasked(ask).await;
            return;
        };
        let ask = approval::Ask {
            slack: asking.slack,
            store,
            link: ask.node.link,
            node_name: ask.node.name,
            owner: ask.node.owner.clone(),
            channel: ask.node.owner.to_string(),
            thread_ts: None,
            requester: Some(format!("<@{}>'s client *{}*", ask.requester, ask.client)),
            tool: ask.tool,
            timeout: asking.timeout,
            turn: ask.turn,
        };
        tokio::spawn(async move { asking.approvals.ask(ask).await });
    }

    async fn ask_sign_in(&self, node: NodeRef, request: SignInRequest) -> Result<(), Error> {
        self.sign_ins.raise(node, request, None).await
    }

    async fn sign_in_settled(&self, selector: &str, settled: SignInSettled) {
        self.sign_ins.settle(selector, settled).await;
    }
}
