use serde_json::{Map, Value, json};

use crate::state::{Channel, File, State, VERIFICATION_TOKEN, now_secs};
use crate::{APP_ID, BOT_ID, BOT_USER_ID, SimMessage, TEAM_ID};

pub(crate) fn file(state: &State, file: &File) -> Value {
    let url = state.file_url(file);
    let (kind, pretty) = filetype(&file.name, &file.mimetype);
    let channels: Vec<&str> = match state.channels.get(&file.channel) {
        Some(c) if c.kind == crate::ChannelKind::Public => vec![c.id.as_str()],
        _ => vec![],
    };
    let groups: Vec<&str> = match state.channels.get(&file.channel) {
        Some(c) if c.kind == crate::ChannelKind::Private => vec![c.id.as_str()],
        _ => vec![],
    };
    let ims: Vec<&str> = match state.channels.get(&file.channel) {
        Some(c) if c.kind == crate::ChannelKind::Im => vec![c.id.as_str()],
        _ => vec![],
    };
    json!({
        "id": file.id,
        "created": file.created,
        "timestamp": file.created,
        "name": file.name,
        "title": file.title.as_deref().unwrap_or(&file.name),
        "mimetype": file.mimetype,
        "filetype": kind,
        "pretty_type": pretty,
        "user": file.user,
        "user_team": TEAM_ID,
        "editable": false,
        "size": file.bytes.len(),
        "mode": "hosted",
        "is_external": false,
        "external_type": "",
        "is_public": !channels.is_empty(),
        "public_url_shared": false,
        "display_as_bot": false,
        "username": "",
        "url_private": url.replace("/download/", "/"),
        "url_private_download": url,
        "media_display_type": "unknown",
        "permalink": format!("https://slacksim.slack.com/files/{}/{}/{}", file.user, file.id, file.name),
        "permalink_public": format!("https://slack-files.com/{}-{}-0000000000", TEAM_ID, file.id),
        "channels": channels,
        "groups": groups,
        "ims": ims,
        "comments_count": 0,
        "has_rich_preview": false,
        "file_access": "visible",
    })
}

fn filetype(name: &str, mimetype: &str) -> (String, String) {
    let ext = name
        .rsplit_once('.')
        .map(|(_, e)| e.to_ascii_lowercase())
        .unwrap_or_default();
    let kind = match (ext.as_str(), mimetype) {
        ("", "text/plain") => "text".to_owned(),
        ("txt", _) => "text".to_owned(),
        ("", _) => "binary".to_owned(),
        (e, _) => e.to_owned(),
    };
    let pretty = match kind.as_str() {
        "text" => "Plain Text".to_owned(),
        "binary" => "Binary".to_owned(),
        k => k.to_ascii_uppercase(),
    };
    (kind, pretty)
}

pub(crate) fn rich_text(state: &mut State, text: &str) -> Value {
    let mut elements = Vec::new();
    let mut rest = text;
    while let Some(start) = rest.find("<@") {
        let Some(len) = rest[start..].find('>') else {
            break;
        };
        if start > 0 {
            elements.push(json!({"type": "text", "text": &rest[..start]}));
        }
        let user = &rest[start + 2..start + len];
        elements.push(json!({"type": "user", "user_id": user}));
        rest = &rest[start + len + 1..];
    }
    if !rest.is_empty() {
        elements.push(json!({"type": "text", "text": rest}));
    }
    let block_id = format!("sim{:02}", state.next_seq() % 100);
    json!([{
        "type": "rich_text",
        "block_id": block_id,
        "elements": [{"type": "rich_text_section", "elements": elements}],
    }])
}

pub(crate) fn bot_profile() -> Value {
    json!({
        "id": BOT_ID,
        "deleted": false,
        "name": "murtaugh",
        "updated": 1700000000,
        "app_id": APP_ID,
        "icons": {
            "image_36": "https://a.slack-edge.com/80588/img/plugins/app/bot_36.png",
            "image_48": "https://a.slack-edge.com/80588/img/plugins/app/bot_48.png",
            "image_72": "https://a.slack-edge.com/80588/img/plugins/app/service_72.png",
        },
        "team_id": TEAM_ID,
    })
}

pub(crate) fn message(state: &State, channel: &Channel, msg: &SimMessage) -> Value {
    let mut out = Map::new();
    out.insert("type".into(), json!("message"));
    if let Some(subtype) = &msg.subtype {
        out.insert("subtype".into(), json!(subtype));
    }
    out.insert("text".into(), json!(msg.text));
    if let Some(user) = &msg.user {
        out.insert("user".into(), json!(user));
    }
    out.insert("ts".into(), json!(msg.ts));
    if msg.bot_id.is_some() {
        out.insert("bot_id".into(), json!(BOT_ID));
        out.insert("app_id".into(), json!(APP_ID));
        out.insert("bot_profile".into(), bot_profile());
    }
    out.insert("team".into(), json!(TEAM_ID));
    if let Some(blocks) = &msg.blocks {
        out.insert("blocks".into(), blocks.clone());
    }
    if !msg.files.is_empty() {
        let files: Vec<Value> = msg
            .files
            .iter()
            .filter_map(|id| state.files.get(id))
            .map(|f| file(state, f))
            .collect();
        out.insert("files".into(), Value::Array(files));
        out.insert("upload".into(), json!(false));
        out.insert("display_as_bot".into(), json!(false));
    }
    if let Some(root) = &msg.thread_ts {
        out.insert("thread_ts".into(), json!(root));
        if *root == msg.ts {
            let replies = channel.replies(root);
            let mut users: Vec<String> = Vec::new();
            for r in &replies {
                if let Some(u) = &r.user
                    && !users.contains(u)
                {
                    users.push(u.clone());
                }
            }
            out.insert("reply_count".into(), json!(replies.len()));
            out.insert("reply_users_count".into(), json!(users.len()));
            out.insert(
                "latest_reply".into(),
                json!(replies.last().map(|r| r.ts.clone()).unwrap_or_default()),
            );
            out.insert("reply_users".into(), json!(users));
            out.insert("is_locked".into(), json!(false));
            out.insert("subscribed".into(), json!(false));
        } else if let Some(parent) = channel.find(root) {
            let parent_user = parent.user.clone().unwrap_or_else(|| BOT_USER_ID.into());
            out.insert("parent_user_id".into(), json!(parent_user));
        }
    }
    if msg.edited {
        out.insert(
            "edited".into(),
            json!({"user": msg.user.clone().unwrap_or_default(), "ts": msg.ts}),
        );
    }
    if !msg.reactions.is_empty() {
        let reactions: Vec<Value> = msg
            .reactions
            .iter()
            .map(|r| json!({"name": r.name, "users": r.users, "count": r.users.len()}))
            .collect();
        out.insert("reactions".into(), Value::Array(reactions));
    }
    Value::Object(out)
}

pub(crate) fn user_event(
    state: &mut State,
    channel: &str,
    msg: &SimMessage,
    kind: &str,
) -> Option<Value> {
    let blocks = if msg.text.is_empty() {
        None
    } else {
        Some(rich_text(state, &msg.text))
    };
    let client_msg_id = state.uuid();
    let chan = state.channels.get(channel)?;
    let mut out = Map::new();
    if msg.subtype.is_none() {
        out.insert("client_msg_id".into(), json!(client_msg_id));
    }
    out.insert("type".into(), json!(kind));
    out.insert("text".into(), json!(msg.text));
    if !msg.files.is_empty() {
        let files: Vec<Value> = msg
            .files
            .iter()
            .filter_map(|id| state.files.get(id))
            .map(|f| file(state, f))
            .collect();
        out.insert("files".into(), Value::Array(files));
        out.insert("upload".into(), json!(false));
    }
    out.insert("user".into(), json!(msg.user));
    if !msg.files.is_empty() {
        out.insert("display_as_bot".into(), json!(false));
    }
    out.insert("ts".into(), json!(msg.ts));
    if let Some(blocks) = blocks {
        out.insert("blocks".into(), blocks);
    }
    out.insert("team".into(), json!(TEAM_ID));
    if let Some(root) = &msg.thread_ts
        && *root != msg.ts
    {
        out.insert("thread_ts".into(), json!(root));
        let parent_user = chan
            .find(root)
            .and_then(|p| p.user.clone())
            .unwrap_or_else(|| BOT_USER_ID.into());
        out.insert("parent_user_id".into(), json!(parent_user));
    }
    out.insert("channel".into(), json!(chan.id));
    if let Some(subtype) = &msg.subtype {
        out.insert("subtype".into(), json!(subtype));
    }
    out.insert("event_ts".into(), json!(msg.ts));
    if kind == "message" {
        out.insert("channel_type".into(), json!(chan.event_channel_type()));
    }
    Some(Value::Object(out))
}

pub(crate) fn events_api(state: &mut State, event: Value) -> Value {
    let envelope_id = state.uuid();
    let event_id = format!("Ev0SIM{:06}", state.next_seq());
    json!({
        "envelope_id": envelope_id,
        "payload": {
            "token": VERIFICATION_TOKEN,
            "team_id": TEAM_ID,
            "context_team_id": TEAM_ID,
            "context_enterprise_id": null,
            "api_app_id": APP_ID,
            "event": event,
            "type": "event_callback",
            "event_id": event_id,
            "event_time": now_secs(),
            "authorizations": [{
                "enterprise_id": null,
                "team_id": TEAM_ID,
                "user_id": BOT_USER_ID,
                "is_bot": true,
                "is_enterprise_install": false,
            }],
            "is_ext_shared_channel": false,
            "event_context": format!("4-sim{event_id}"),
        },
        "type": "events_api",
        "accepts_response_payload": false,
        "retry_attempt": 0,
        "retry_reason": "",
    })
}

/// An ephemeral message has no timestamp Slack will echo and no `message` to attach, so the only
/// thing naming the thread is the container. Anything an app needs back must ride on the action.
pub(crate) fn ephemeral_select(
    state: &mut State,
    user: &str,
    ephemeral: &crate::SimEphemeral,
    block_id: &str,
    action_id: &str,
    option: &Value,
) -> Option<Value> {
    let envelope_id = state.uuid();
    let trigger_id = format!("{}.{}.sim", state.next_seq(), now_secs());
    let message_ts = format!("{}.000000", now_secs());
    let state: &State = state;
    let channel = state.channels.get(&ephemeral.channel)?;
    let user_name = state
        .users
        .get(user)
        .map(|u| u.name.clone())
        .unwrap_or_default();
    let mut container = Map::new();
    container.insert("type".into(), json!("message"));
    container.insert("message_ts".into(), json!(message_ts));
    container.insert("channel_id".into(), json!(channel.id));
    container.insert("is_ephemeral".into(), json!(true));
    if let Some(thread_ts) = &ephemeral.thread_ts {
        container.insert("thread_ts".into(), json!(thread_ts));
    }
    Some(json!({
        "envelope_id": envelope_id,
        "payload": {
            "type": "block_actions",
            "user": {"id": user, "username": user_name, "name": user_name, "team_id": TEAM_ID},
            "api_app_id": APP_ID,
            "token": VERIFICATION_TOKEN,
            "container": Value::Object(container),
            "trigger_id": trigger_id,
            "team": {"id": TEAM_ID, "domain": "slacksim"},
            "enterprise": null,
            "is_enterprise_install": false,
            "channel": {"id": channel.id, "name": channel.name},
            "state": {"values": {}},
            "response_url": format!("https://hooks.slack.com/actions/{}/{}/sim", TEAM_ID, now_secs()),
            "actions": [{
                "type": "static_select",
                "action_id": action_id,
                "block_id": block_id,
                "selected_option": option,
                "action_ts": format!("{}.000000", now_secs()),
            }],
        },
        "type": "interactive",
        "accepts_response_payload": false,
    }))
}

pub(crate) fn block_actions(
    state: &mut State,
    user: &str,
    channel: &str,
    msg: &SimMessage,
    block_id: &str,
    button: &Value,
    values: Value,
) -> Option<Value> {
    let envelope_id = state.uuid();
    let trigger_id = format!("{}.{}.sim", state.next_seq(), now_secs());
    let state: &State = state;
    let channel = state.channels.get(channel)?;
    let user_name = state
        .users
        .get(user)
        .map(|u| u.name.clone())
        .unwrap_or_default();
    let mut action = Map::new();
    action.insert("action_id".into(), button["action_id"].clone());
    action.insert("block_id".into(), json!(block_id));
    action.insert("text".into(), button["text"].clone());
    if let Some(value) = button.get("value") {
        action.insert("value".into(), value.clone());
    }
    if let Some(style) = button.get("style") {
        action.insert("style".into(), style.clone());
    }
    action.insert("type".into(), json!("button"));
    action.insert("action_ts".into(), json!(format!("{}.000000", now_secs())));
    Some(json!({
        "envelope_id": envelope_id,
        "payload": {
            "type": "block_actions",
            "user": {"id": user, "username": user_name, "name": user_name, "team_id": TEAM_ID},
            "api_app_id": APP_ID,
            "token": VERIFICATION_TOKEN,
            "container": {
                "type": "message",
                "message_ts": msg.ts,
                "channel_id": channel.id,
                "is_ephemeral": false,
            },
            "trigger_id": trigger_id,
            "team": {"id": TEAM_ID, "domain": "slacksim"},
            "enterprise": null,
            "is_enterprise_install": false,
            "channel": {"id": channel.id, "name": channel.name},
            "message": message(state, channel, msg),
            "state": {"values": values},
            "response_url": format!("https://hooks.slack.com/actions/{}/{}/sim", TEAM_ID, now_secs()),
            "actions": [Value::Object(action)],
        },
        "type": "interactive",
        "accepts_response_payload": false,
    }))
}

/// A choice on the app's Home tab: an overflow option, a button, a select or a user picker.
/// Unlike [`block_actions`] the container is the view, not a message: the Home tab carries no
/// channel or ts, which is what `HomeClick` (as opposed to `Click`) is built to read. `chosen` is
/// what the element type adds to the action, such as `selected_option` or `selected_users`.
pub(crate) fn home_block_actions(
    state: &mut State,
    user: &str,
    element: &Value,
    block_id: &str,
    chosen: Value,
) -> Value {
    let envelope_id = state.uuid();
    let trigger_id = format!("{}.{}.sim", state.next_seq(), now_secs());
    state.triggers.insert(trigger_id.clone(), user.to_owned());
    let view_id = format!("V{}", state.next_seq());
    let user_name = state
        .users
        .get(user)
        .map(|u| u.name.clone())
        .unwrap_or_default();
    let mut action = Map::new();
    action.insert("type".into(), element["type"].clone());
    action.insert("action_id".into(), element["action_id"].clone());
    action.insert("block_id".into(), json!(block_id));
    if let Value::Object(chosen) = chosen {
        action.extend(chosen);
    }
    action.insert("action_ts".into(), json!(format!("{}.000000", now_secs())));
    json!({
        "envelope_id": envelope_id,
        "payload": {
            "type": "block_actions",
            "user": {"id": user, "username": user_name, "name": user_name, "team_id": TEAM_ID},
            "api_app_id": APP_ID,
            "token": VERIFICATION_TOKEN,
            "container": {"type": "view", "view_id": view_id},
            "trigger_id": trigger_id,
            "team": {"id": TEAM_ID, "domain": "slacksim"},
            "enterprise": null,
            "is_enterprise_install": false,
            "view": {"id": view_id, "type": "home"},
            "state": {"values": {}},
            "response_url": format!("https://hooks.slack.com/actions/{}/{}/sim", TEAM_ID, now_secs()),
            "actions": [Value::Object(action)],
        },
        "type": "interactive",
        "accepts_response_payload": false,
    })
}

/// A modal submitted: the view as it was opened, with the inputs' `state.values` filled in.
pub(crate) fn view_submission(state: &mut State, user: &str, view: &Value, values: Value) -> Value {
    let envelope_id = state.uuid();
    let user_name = state
        .users
        .get(user)
        .map(|u| u.name.clone())
        .unwrap_or_default();
    let mut view = view.clone();
    view["state"] = json!({"values": values});
    json!({
        "envelope_id": envelope_id,
        "payload": {
            "type": "view_submission",
            "user": {"id": user, "username": user_name, "name": user_name, "team_id": TEAM_ID},
            "api_app_id": APP_ID,
            "token": VERIFICATION_TOKEN,
            "trigger_id": format!("{}.{}.sim", state.next_seq(), now_secs()),
            "team": {"id": TEAM_ID, "domain": "slacksim"},
            "enterprise": null,
            "is_enterprise_install": false,
            "view": view,
            "response_urls": [],
        },
        "type": "interactive",
        "accepts_response_payload": true,
    })
}

pub(crate) fn slash_command(
    state: &mut State,
    user: &str,
    channel: &str,
    command: &str,
    text: &str,
    thread_ts: Option<&str>,
) -> Option<Value> {
    let envelope_id = state.uuid();
    let trigger_id = format!("{}.{}.sim", state.next_seq(), now_secs());
    let state: &State = state;
    let channel = state.channels.get(channel)?;
    let user_name = state
        .users
        .get(user)
        .map(|u| u.name.clone())
        .unwrap_or_default();
    let channel_name = match channel.kind {
        crate::ChannelKind::Im => "directmessage".to_owned(),
        _ => channel.name.clone(),
    };
    let mut envelope = json!({
        "payload": {
            "token": VERIFICATION_TOKEN,
            "team_id": TEAM_ID,
            "team_domain": "slacksim",
            "channel_id": channel.id,
            "channel_name": channel_name,
            "user_id": user,
            "user_name": user_name,
            "command": command,
            "text": text,
            "api_app_id": APP_ID,
            "is_enterprise_install": "false",
            "response_url": format!("https://hooks.slack.com/commands/{}/{}/sim", TEAM_ID, now_secs()),
            "trigger_id": trigger_id,
        },
        "envelope_id": envelope_id,
        "type": "slash_commands",
        "accepts_response_payload": true,
    });
    if let Some(thread_ts) = thread_ts {
        envelope["payload"]["thread_ts"] = json!(thread_ts);
    }
    Some(envelope)
}

pub(crate) fn hello(connections: usize) -> Value {
    json!({
        "type": "hello",
        "num_connections": connections,
        "debug_info": {
            "host": "applink-slacksim",
            "build_number": 1,
            "approximate_connection_time": 18060,
        },
        "connection_info": {"app_id": APP_ID},
    })
}

pub(crate) fn disconnect(reason: &str) -> Value {
    json!({
        "type": "disconnect",
        "reason": reason,
        "debug_info": {"host": "applink-slacksim"},
    })
}
