//! The app's Home tab: this gateway's version, and the nodes the viewer runs, or every node for
//! the admin, each with whether it is connected and how many conversations it is carrying.

use std::collections::HashMap;

use murtaugh_slack::{Block, Text};
use murtaugh_store::UserId;

use crate::access::Snapshot;
use crate::fleet::Summary;
use crate::render;
use crate::version::VERSION;

/// Home views hold at most 100 blocks; the header and footer take a few.
const MAX_NODES: usize = 90;

pub fn view(viewer: &UserId, snapshot: &Snapshot, attached: &[Summary]) -> Vec<Block> {
    let admin = snapshot.admin() == Some(viewer);
    let mut blocks = vec![
        Block::Raw(serde_json::json!({
            "type": "header",
            "text": {"type": "plain_text", "text": "Murtaugh"},
        })),
        Block::Context(vec![Text::Mrkdwn(format!("Gateway version `{VERSION}`"))]),
        Block::Divider,
    ];
    if !admin && !snapshot.may_chat(viewer) {
        blocks.push(Block::mrkdwn(
            "You don't have access to this gateway yet. Ask its admin, in person, to let you in.",
        ));
        return blocks;
    }
    let attached: HashMap<&str, &Summary> = attached
        .iter()
        .map(|summary| (summary.selector.as_str(), summary))
        .collect();
    let mut nodes: Vec<_> = snapshot
        .live_nodes()
        .filter(|token| admin || &token.owner == viewer)
        .collect();
    nodes.sort_by(|a, b| (&a.owner, &a.name).cmp(&(&b.owner, &b.name)));
    let heading = if admin { "*All nodes*" } else { "*Your nodes*" };
    blocks.push(Block::mrkdwn(heading));
    if nodes.is_empty() {
        let none = if snapshot.may_chat(viewer) && !admin {
            "You have no nodes of your own; your conversations run on the admin's."
        } else {
            "No nodes yet. Mint one with `murtaugh-gateway node mint`."
        };
        blocks.push(Block::Context(vec![Text::Mrkdwn(none.into())]));
        return blocks;
    }
    let shown = nodes.len().min(MAX_NODES);
    for token in &nodes[..shown] {
        let status = match attached.get(token.selector.as_str()) {
            Some(summary) if summary.connected => format!(
                ":large_green_circle: Connected · {} live {}",
                summary.sessions,
                if summary.sessions == 1 {
                    "conversation"
                } else {
                    "conversations"
                }
            ),
            Some(_) => ":large_yellow_circle: Reconnecting".to_owned(),
            None => ":white_circle: Offline".to_owned(),
        };
        let owner = if admin {
            format!(" · <@{}>", token.owner)
        } else {
            String::new()
        };
        blocks.push(Block::mrkdwn(format!(
            "*{}*{owner}\n{status}",
            render::escape(&token.name)
        )));
    }
    if nodes.len() > shown {
        blocks.push(Block::Context(vec![Text::Mrkdwn(format!(
            "…and {} more.",
            nodes.len() - shown
        ))]));
    }
    blocks
}
