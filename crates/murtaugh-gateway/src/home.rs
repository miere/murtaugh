//! The app's Home tab, a configuration panel sized to the viewer. The admin sees all of it: their
//! tool approval mode, who administers the gateway, who may connect nodes, who may use it, every
//! node and every client token. A node admin sees their tool approval mode, their own nodes and
//! their own client tokens. Anyone else allowed sees their client tokens, and the rest the footer
//! alone. Also the modals the panel opens: minting a node or a client token, and revoking a node.

use std::collections::HashMap;

use murtaugh_slack::{Block, Text};
use murtaugh_store::{ToolMode, UserId};
use serde_json::{Value, json};

use crate::access::Snapshot;
use crate::fleet::Summary;
use crate::node_access::NodeAccess;
use crate::render;
use crate::version::VERSION;

/// Home views hold at most 100 blocks; the panel above the nodes and the footer take a dozen.
const MAX_NODES: usize = 85;

pub const TOOL_MODE: &str = "tool_mode";
pub const ADMIN_SET: &str = "admin_set";
pub const NODE_ADMINS_SET: &str = "node_admins_set";
pub const ALLOWED_USERS_SET: &str = "allowed_users_set";
pub const NODE_NEW: &str = "node_new";
/// The overflow menu's action id on each node row.
pub const NODE_MENU: &str = "node_menu";
pub const CLIENT_NEW: &str = "client_new";
/// The revoke button on each client token row; its value is the selector.
pub const CLIENT_REVOKE: &str = "client_revoke";

/// The modals' callback ids.
pub const NODE_NEW_SUBMIT: &str = "node_new_submit";
pub const NODE_REVOKE_SUBMIT: &str = "node_revoke_submit";
pub const CLIENT_NEW_SUBMIT: &str = "client_new_submit";

/// Where the new node modal keeps its inputs, as block id and action id.
pub const NODE_NAME: (&str, &str) = ("node_name", "name");
pub const NODE_OWNER: (&str, &str) = ("node_owner", "owner");
const NODE_NAME_MAX: usize = 64;
/// Home views hold at most 100 blocks, which the nodes above mostly take.
const MAX_CLIENTS: usize = 20;
/// Where the new client token modal keeps its inputs, as block id and action id.
pub const CLIENT_NAME: (&str, &str) = ("client_name", "name");
pub const CLIENT_OWNER: (&str, &str) = ("client_owner", "owner");

const CLIENT_SETUP: &str = "https://github.com/miere/murtaugh-rs#murtaugh-client";

const RIGGS_SETUP: &str = "https://github.com/miere/riggs#running-riggs";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MenuAction {
    Disable,
    Enable,
    Revoke,
}

/// The selector and what to do to it, decoded from a `NODE_MENU` option's value. A selector holds
/// no colon (see `picker::chosen`), so splitting off the last one is unambiguous.
pub fn parse_menu(value: &str) -> Option<(String, MenuAction)> {
    let (selector, action) = value.rsplit_once(':')?;
    let action = match action {
        "disable" => MenuAction::Disable,
        "enable" => MenuAction::Enable,
        "revoke" => MenuAction::Revoke,
        _ => return None,
    };
    Some((selector.to_owned(), action))
}

pub fn view(
    viewer: &UserId,
    tool_mode: ToolMode,
    snapshot: &Snapshot,
    attached: &[Summary],
) -> Vec<Block> {
    let admin = snapshot.admin() == Some(viewer);
    let mut blocks = Vec::new();
    if snapshot.may_run_nodes(viewer) {
        blocks.push(Block::Raw(json!({
            "type": "header",
            "text": {"type": "plain_text", "text": "Configuration Panel"},
            "level": 1,
        })));
        blocks.push(Block::Divider);
        blocks.push(tool_mode_block(tool_mode));
        if admin {
            blocks.extend(admin_blocks(snapshot));
        }
        blocks.push(Block::Divider);
        blocks.extend(node_blocks(viewer, admin, snapshot, attached));
    }
    if snapshot.may_chat(viewer) {
        blocks.push(Block::Divider);
        blocks.extend(client_blocks(viewer, admin, snapshot));
    }
    blocks.push(Block::Divider);
    blocks.push(Block::Context(vec![Text::Mrkdwn(format!(
        "Powered by Murtaugh Slack gateway - version `{VERSION}`"
    ))]));
    tidy(blocks)
}

/// No divider first or last, and never two in a row: a section left out must not leave its
/// dividers behind.
fn tidy(blocks: Vec<Block>) -> Vec<Block> {
    let mut tidied: Vec<Block> = Vec::with_capacity(blocks.len());
    for block in blocks {
        let divider = matches!(block, Block::Divider);
        if divider
            && tidied
                .last()
                .is_none_or(|last| matches!(last, Block::Divider))
        {
            continue;
        }
        tidied.push(block);
    }
    if tidied
        .last()
        .is_some_and(|last| matches!(last, Block::Divider))
    {
        tidied.pop();
    }
    tidied
}

fn option(text: &str, value: &str) -> Value {
    json!({"text": {"type": "plain_text", "text": text}, "value": value})
}

fn plain(text: &str) -> Value {
    json!({"type": "plain_text", "text": text})
}

fn confirm(title: &str, text: &str, yes: &str, no: &str) -> Value {
    json!({
        "title": plain(title),
        "text": {"type": "mrkdwn", "text": text},
        "confirm": plain(yes),
        "deny": plain(no),
        "style": "danger",
    })
}

fn tool_mode_block(mode: ToolMode) -> Block {
    let label = |mode: ToolMode| match mode {
        ToolMode::AlwaysAllowed => "Always Allowed",
        ToolMode::AllowedWhitelist => "Whitelisted Only",
        ToolMode::Denied => "Denied",
    };
    let options: Vec<Value> = ToolMode::ALL
        .iter()
        .map(|mode| option(label(*mode), mode.as_str()))
        .collect();
    Block::Raw(json!({
        "type": "section",
        "block_id": "home_tool_mode",
        "text": {
            "type": "mrkdwn",
            "text": "*Tool Approval*\n_How much freedom should the models have._",
        },
        "accessory": {
            "type": "static_select",
            "action_id": TOOL_MODE,
            "initial_option": option(label(mode), mode.as_str()),
            "options": options,
        },
    }))
}

fn sorted<'a>(users: impl Iterator<Item = &'a UserId>) -> Vec<String> {
    let mut users: Vec<String> = users.map(ToString::to_string).collect();
    users.sort();
    users
}

fn admin_blocks(snapshot: &Snapshot) -> Vec<Block> {
    let mut admin = json!({
        "type": "users_select",
        "action_id": ADMIN_SET,
        "placeholder": plain("Select an admin"),
        "confirm": confirm(
            "Hand over the gateway?",
            "The person you pick becomes the gateway's only admin. You lose this panel straight away and can't take it back from Slack.",
            "Hand it over",
            "Keep it",
        ),
    });
    if let Some(current) = snapshot.admin() {
        admin["initial_user"] = json!(current.as_str());
    }
    let people = |action_id: &str, users: Vec<String>, confirm: Value| {
        let mut picker = json!({
            "type": "multi_users_select",
            "action_id": action_id,
            "placeholder": plain("Select people"),
            "confirm": confirm,
        });
        if !users.is_empty() {
            picker["initial_users"] = json!(users);
        }
        picker
    };
    vec![
        Block::Raw(json!({
            "type": "section",
            "block_id": "home_admin",
            "text": {"type": "mrkdwn", "text": "*Admin User*\n_Change to transfer ownership._"},
            "accessory": admin,
        })),
        Block::Raw(json!({
            "type": "section",
            "block_id": "home_node_admins",
            "text": {
                "type": "mrkdwn",
                "text": "*Node Admin Users*\n_Users who are allowed to connect their nodes to the gateway._",
            },
            "accessory": people(
                NODE_ADMINS_SET,
                sorted(snapshot.node_admins()),
                confirm(
                    "Change node admins?",
                    "People you add can connect their own machines and are allowed to use the gateway. People you remove are disconnected, and every token they hold is revoked for good.",
                    "Yes, proceed.",
                    "Maybe not!",
                ),
            ),
        })),
        Block::Raw(json!({
            "type": "section",
            "block_id": "home_allowed_users",
            "text": {
                "type": "mrkdwn",
                "text": "*Allowed Users*\n_Users who are allowed to *interact* with the gateway._",
            },
            "accessory": people(
                ALLOWED_USERS_SET,
                sorted(snapshot.allowed_users()),
                confirm(
                    "Change who can use the gateway?",
                    "People you add can talk to the gateway, on any node whose owner lets them in. People you remove can't use it anymore. If they're node admins, they lose that too, along with their tokens.",
                    "Yes, proceed.",
                    "Maybe not!",
                ),
            ),
        })),
    ]
}

fn node_blocks(
    viewer: &UserId,
    admin: bool,
    snapshot: &Snapshot,
    attached: &[Summary],
) -> Vec<Block> {
    let mut blocks = vec![Block::Raw(json!({
        "type": "section",
        "block_id": "home_nodes",
        "text": {"type": "mrkdwn", "text": "*AI Nodes*\n_Nodes that will run user's workloads._"},
        "accessory": {
            "type": "button",
            "action_id": NODE_NEW,
            "style": "primary",
            "text": plain("New node"),
            "value": "new",
        },
    }))];
    let attached: HashMap<&str, &Summary> = attached
        .iter()
        .map(|summary| (summary.selector.as_str(), summary))
        .collect();
    let mut nodes: Vec<_> = snapshot
        .live_nodes()
        .filter(|token| admin || &token.owner == viewer)
        .collect();
    nodes.sort_by(|a, b| (&a.owner, &a.name).cmp(&(&b.owner, &b.name)));
    if nodes.is_empty() {
        blocks.push(Block::Context(vec![Text::Mrkdwn(
            "No nodes yet. Press *New node* to mint one.".into(),
        )]));
        return blocks;
    }
    let shown = nodes.len().min(MAX_NODES);
    for token in &nodes[..shown] {
        let summary = attached.get(token.selector.as_str());
        // Disabled overrides whatever the socket is doing: it is not taking new work either way,
        // and that is the fact this row exists to report.
        let mut status = if token.disabled_at.is_some() {
            ":no_entry: Disabled — not routed any new work".to_owned()
        } else {
            match summary {
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
            }
        };
        // Who a node serves is what it declared when it connected, so an offline one says nothing.
        if let Some(summary) = summary {
            status.push_str(" · ");
            status.push_str(&audience(&token.owner, &summary.access));
        }
        let owner = if admin {
            format!(" · <@{}>", token.owner)
        } else {
            String::new()
        };
        let toggle = if token.disabled_at.is_some() {
            option("Enable", &format!("{}:enable", token.selector))
        } else {
            option("Disable", &format!("{}:disable", token.selector))
        };
        blocks.push(Block::Raw(json!({
            "type": "section",
            "block_id": format!("home_node:{}", token.selector),
            "text": {
                "type": "mrkdwn",
                "text": format!("*{}*{owner}\n{status}", render::escape(&token.name)),
            },
            "accessory": {
                "type": "overflow",
                "action_id": NODE_MENU,
                "options": [toggle, option("Revoke", &format!("{}:revoke", token.selector))],
            },
        })));
    }
    if nodes.len() > shown {
        blocks.push(Block::Context(vec![Text::Mrkdwn(format!(
            "…and {} more.",
            nodes.len() - shown
        ))]));
    }
    blocks
}

/// A person's own client tokens, or everyone's for the admin, each with a button to revoke it.
fn client_blocks(viewer: &UserId, admin: bool, snapshot: &Snapshot) -> Vec<Block> {
    let mut blocks = vec![Block::Raw(json!({
        "type": "section",
        "block_id": "home_clients",
        "text": {
            "type": "mrkdwn",
            "text": "*Client Tokens*\n_Credentials for your own clients, such as an editor, to use the gateway's nodes._",
        },
        "accessory": {
            "type": "button",
            "action_id": CLIENT_NEW,
            "text": plain("New client token"),
            "value": "new",
        },
    }))];
    let mut clients: Vec<_> = snapshot
        .live_clients()
        .filter(|token| admin || &token.owner == viewer)
        .collect();
    clients.sort_by(|a, b| (&a.owner, &a.name).cmp(&(&b.owner, &b.name)));
    if clients.is_empty() {
        blocks.push(Block::Context(vec![Text::Mrkdwn(
            "No client tokens yet. Press *New client token* to mint one.".into(),
        )]));
        return blocks;
    }
    let shown = clients.len().min(MAX_CLIENTS);
    for token in &clients[..shown] {
        let owner = if admin {
            format!(" · <@{}>", token.owner)
        } else {
            String::new()
        };
        blocks.push(Block::Raw(json!({
            "type": "section",
            "block_id": format!("home_client:{}", token.selector),
            "text": {
                "type": "mrkdwn",
                "text": format!("*{}*{owner}", render::escape(&token.name)),
            },
            "accessory": {
                "type": "button",
                "action_id": CLIENT_REVOKE,
                "text": plain("Revoke"),
                "style": "danger",
                "value": token.selector,
                "confirm": confirm(
                    "Revoke this client token?",
                    "Its client is disconnected within seconds, and the token stops working for good.",
                    "Revoke",
                    "Keep it",
                ),
            },
        })));
    }
    if clients.len() > shown {
        blocks.push(Block::Context(vec![Text::Mrkdwn(format!(
            "…and {} more.",
            clients.len() - shown
        ))]));
    }
    blocks
}

fn audience(owner: &UserId, access: &NodeAccess) -> String {
    match access {
        NodeAccess::AlwaysAllow => "all gateway users".to_owned(),
        NodeAccess::AllowList(people) => {
            let others = people.iter().filter(|person| *person != owner).count();
            match others {
                0 => format!("only <@{owner}>"),
                1 => format!("<@{owner}> + 1 person"),
                others => format!("<@{owner}> + {others} people"),
            }
        }
    }
}

/// The modal behind *New node*. The admin also picks whose node it is; anyone else mints for
/// themselves.
pub fn new_node_modal(viewer: &UserId, admin: bool) -> Value {
    let mut blocks = vec![json!({
        "type": "input",
        "block_id": NODE_NAME.0,
        "label": plain("Name"),
        "hint": plain("What the node is called in Slack, such as laptop."),
        "element": {
            "type": "plain_text_input",
            "action_id": NODE_NAME.1,
            "max_length": NODE_NAME_MAX,
            "placeholder": plain("laptop"),
        },
    })];
    if admin {
        blocks.push(json!({
            "type": "input",
            "block_id": NODE_OWNER.0,
            "label": plain("Owner"),
            "hint": plain("They're granted the right to connect nodes, if they don't have it yet."),
            "element": {
                "type": "users_select",
                "action_id": NODE_OWNER.1,
                "initial_user": viewer.as_str(),
            },
        }));
    }
    json!({
        "type": "modal",
        "callback_id": NODE_NEW_SUBMIT,
        "title": plain("New node"),
        "submit": plain("Mint"),
        "close": plain("Cancel"),
        "blocks": blocks,
    })
}

/// The confirmation behind *Revoke*, which cannot be undone; the selector rides along in
/// `private_metadata`.
pub fn revoke_modal(selector: &str, name: &str) -> Value {
    json!({
        "type": "modal",
        "callback_id": NODE_REVOKE_SUBMIT,
        "private_metadata": selector,
        "title": plain("Revoke this node?"),
        "submit": plain("Revoke"),
        "close": plain("Cancel"),
        "blocks": [{
            "type": "section",
            "text": {
                "type": "mrkdwn",
                "text": format!(
                    "*{}* is disconnected within seconds, and its token stops working for good. To bring the machine back, you'll need a new token.",
                    render::escape(name)
                ),
            },
        }],
    })
}

/// The modal behind *New client token*. The admin also picks whose token it is; anyone else mints
/// for themselves.
pub fn new_client_modal(viewer: &UserId, admin: bool) -> Value {
    let mut blocks = vec![json!({
        "type": "input",
        "block_id": CLIENT_NAME.0,
        "label": plain("Name"),
        "hint": plain("What the client is, such as editor."),
        "element": {
            "type": "plain_text_input",
            "action_id": CLIENT_NAME.1,
            "max_length": NODE_NAME_MAX,
            "placeholder": plain("editor"),
        },
    })];
    if admin {
        blocks.push(json!({
            "type": "input",
            "block_id": CLIENT_OWNER.0,
            "label": plain("Owner"),
            "hint": plain("They're allowed on the gateway, if they aren't yet."),
            "element": {
                "type": "users_select",
                "action_id": CLIENT_OWNER.1,
                "initial_user": viewer.as_str(),
            },
        }));
    }
    json!({
        "type": "modal",
        "callback_id": CLIENT_NEW_SUBMIT,
        "title": plain("New client token"),
        "submit": plain("Mint"),
        "close": plain("Cancel"),
        "blocks": blocks,
    })
}

/// The DM that goes with a newly minted client token, which is attached beneath it.
pub fn client_token_message(bot_user: &str, name: &str) -> String {
    format!(
        "Here's a client token for *{}*, to use <@{bot_user}>'s nodes from your own tools. It's attached. Keep it somewhere safe, because it won't be shown again. Save it with `murtaugh-client login`; the setup instructions are at <{CLIENT_SETUP}|github.com/miere/murtaugh-rs#murtaugh-client>.",
        render::escape(name)
    )
}

/// The DM that goes with a newly minted token, which is attached beneath it.
pub fn token_message(bot_user: &str, name: &str) -> String {
    format!(
        "You've been authorised to connect a node to <@{bot_user}>. The token for *{}* is attached. Keep it somewhere safe, because it won't be shown again. If you use Riggs, the setup instructions are at <{RIGGS_SETUP}|github.com/miere/riggs#running-riggs>.",
        render::escape(name)
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn menu_values_name_the_node_and_what_to_do() {
        assert_eq!(
            parse_menu("abc123:revoke"),
            Some(("abc123".to_owned(), MenuAction::Revoke))
        );
        assert_eq!(
            parse_menu("abc123:disable"),
            Some(("abc123".to_owned(), MenuAction::Disable))
        );
        assert_eq!(parse_menu("abc123:explode"), None);
        assert_eq!(parse_menu("abc123"), None);
    }

    #[test]
    fn dividers_never_lead_trail_or_double() {
        let context = || Block::Context(vec![Text::Mrkdwn("x".into())]);
        let tidied = tidy(vec![
            Block::Divider,
            context(),
            Block::Divider,
            Block::Divider,
            context(),
            Block::Divider,
        ]);
        assert_eq!(
            tidied,
            vec![context(), Block::Divider, context()],
            "{tidied:?}"
        );
    }
}
