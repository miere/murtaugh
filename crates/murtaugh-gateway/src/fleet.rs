//! The nodes attached right now, and which one a new conversation goes to.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};

use murtaugh_store::UserId;
use rax::session::NodeCapabilities;
use rax_tokio::gateway::GatewayLink;

use crate::access::Snapshot;

#[derive(Clone)]
pub struct Node {
    pub selector: String,
    pub owner: UserId,
    pub name: String,
    pub link: GatewayLink,
    pub capabilities: NodeCapabilities,
    /// False while the socket is down and RAX holds the link for a resume. Such a node takes no
    /// new work: a turn sent to it would wait for a socket that may never come back.
    pub connected: bool,
}

struct Entry {
    node: Node,
    attachment: u64,
    sessions: usize,
}

#[derive(Clone, Default)]
pub struct Fleet {
    nodes: Arc<Mutex<HashMap<String, Entry>>>,
    attachments: Arc<AtomicU64>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Summary {
    pub selector: String,
    pub owner: UserId,
    pub name: String,
    pub sessions: usize,
    pub connected: bool,
}

impl Fleet {
    fn nodes(&self) -> MutexGuard<'_, HashMap<String, Entry>> {
        self.nodes
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// The returned attachment detaches only this link, so a late close of a node's old link
    /// cannot detach its new one.
    pub fn attach(&self, node: Node) -> u64 {
        let attachment = self.attachments.fetch_add(1, Ordering::Relaxed);
        let entry = Entry {
            node,
            attachment,
            sessions: 0,
        };
        self.nodes().insert(entry.node.selector.clone(), entry);
        attachment
    }

    pub fn detach(&self, selector: &str, attachment: u64) -> bool {
        let mut nodes = self.nodes();
        let current = nodes
            .get(selector)
            .is_some_and(|entry| entry.attachment == attachment);
        if current {
            nodes.remove(selector);
        }
        current
    }

    pub fn set_connected(&self, selector: &str, attachment: u64, connected: bool) {
        if let Some(entry) = self.nodes().get_mut(selector)
            && entry.attachment == attachment
        {
            entry.node.connected = connected;
        }
    }

    pub fn get(&self, selector: &str) -> Option<Node> {
        self.nodes().get(selector).map(|entry| entry.node.clone())
    }

    /// A person's own nodes when any is attached, otherwise the nodes of whoever lets them fall
    /// back; never a mix. Least live sessions wins, and the choice counts as a session at once.
    pub fn assign(&self, user: &UserId, access: &Snapshot) -> Option<Node> {
        let mut nodes = self.nodes();
        let pick = |owners: &[UserId], nodes: &HashMap<String, Entry>| {
            nodes
                .values()
                .filter(|entry| entry.node.connected && owners.contains(&entry.node.owner))
                .min_by(|a, b| {
                    a.sessions
                        .cmp(&b.sessions)
                        .then_with(|| a.node.selector.cmp(&b.node.selector))
                })
                .map(|entry| entry.node.selector.clone())
        };
        let own = pick(std::slice::from_ref(user), &nodes);
        let selector = own.or_else(|| pick(&access.fallback_owners(user), &nodes))?;
        let entry = nodes.get_mut(&selector)?;
        entry.sessions += 1;
        Some(entry.node.clone())
    }

    pub fn session_ended(&self, selector: &str) {
        if let Some(entry) = self.nodes().get_mut(selector) {
            entry.sessions = entry.sessions.saturating_sub(1);
        }
    }

    pub fn session_resumed(&self, selector: &str) {
        if let Some(entry) = self.nodes().get_mut(selector) {
            entry.sessions += 1;
        }
    }

    pub fn summaries(&self) -> Vec<Summary> {
        let mut summaries: Vec<Summary> = self
            .nodes()
            .values()
            .map(|entry| Summary {
                selector: entry.node.selector.clone(),
                owner: entry.node.owner.clone(),
                name: entry.node.name.clone(),
                sessions: entry.sessions,
                connected: entry.node.connected,
            })
            .collect();
        summaries.sort_by(|a, b| a.name.cmp(&b.name));
        summaries
    }
}
