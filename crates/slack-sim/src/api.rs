use std::collections::VecDeque;
use std::sync::Arc;

use axum::body::Bytes;
use axum::extract::{Path, RawQuery, State as AxumState};
use axum::http::{HeaderMap, HeaderValue, StatusCode, header};
use axum::response::{IntoResponse, Response};
use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use serde_json::{Map, Value, json};

use crate::state::{APP_TOKEN, BOT_TOKEN, Fault, State, new_message};
use crate::{BOT_ID, BOT_USER_ID, Call, ChannelKind, Inner, Reaction, TEAM_ID, blocks, render};

const METHODS: &[&str] = &[
    "auth.test",
    "apps.connections.open",
    "chat.postMessage",
    "chat.update",
    "reactions.add",
    "conversations.replies",
    "files.info",
];
const JSON_METHODS: &[&str] = &[
    "auth.test",
    "chat.postMessage",
    "chat.update",
    "reactions.add",
];
const MAX_TEXT: usize = 40_000;

pub(crate) struct ApiError {
    code: String,
    detail: String,
    violation: bool,
}

fn err(code: &str, detail: impl Into<String>) -> ApiError {
    ApiError {
        code: code.to_owned(),
        detail: detail.into(),
        violation: true,
    }
}

fn benign(code: &str) -> ApiError {
    ApiError {
        code: code.to_owned(),
        detail: String::new(),
        violation: false,
    }
}

struct Request {
    params: Map<String, Value>,
    token: Option<String>,
    problems: Vec<String>,
}

fn parse_request(method: &str, headers: &HeaderMap, query: Option<&str>, body: &[u8]) -> Request {
    let mut req = Request {
        params: Map::new(),
        token: None,
        problems: Vec::new(),
    };
    if let Some(query) = query {
        for (k, v) in url::form_urlencoded::parse(query.as_bytes()) {
            if k == "token" {
                req.problems.push(
                    "token passed in the query string; Slack rejects that for new apps".into(),
                );
                continue;
            }
            req.params
                .insert(k.into_owned(), Value::String(v.into_owned()));
        }
    }
    let content_type = headers
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .to_ascii_lowercase();
    if !body.is_empty() {
        if content_type.starts_with("application/json") {
            if !content_type.contains("charset=utf-8") {
                req.problems.push(
                    "JSON body sent without `charset=utf-8`; Slack answers with a missing_charset warning"
                        .into(),
                );
            }
            if METHODS.contains(&method) && !JSON_METHODS.contains(&method) {
                req.problems.push(format!(
                    "{method} does not accept JSON bodies; Slack ignores the body entirely"
                ));
            } else {
                match serde_json::from_slice::<Value>(body) {
                    Ok(Value::Object(obj)) => req.params.extend(obj),
                    _ => req.problems.push("JSON body is not an object".into()),
                }
            }
        } else if content_type.starts_with("application/x-www-form-urlencoded") {
            for (k, v) in url::form_urlencoded::parse(body) {
                if k == "token" {
                    req.token = Some(v.into_owned());
                } else {
                    req.params
                        .insert(k.into_owned(), Value::String(v.into_owned()));
                }
            }
        } else {
            req.problems.push(format!(
                "unsupported content type `{content_type}`; Slack ignores the body"
            ));
        }
    }
    if let Some(auth) = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
    {
        match auth.strip_prefix("Bearer ") {
            Some(token) => req.token = Some(token.to_owned()),
            None => req
                .problems
                .push("Authorization header is not `Bearer <token>`".into()),
        }
    }
    req
}

pub(crate) async fn handle(
    AxumState(inner): AxumState<Arc<Inner>>,
    Path(method): Path<String>,
    headers: HeaderMap,
    RawQuery(query): RawQuery,
    body: Bytes,
) -> Response {
    let req = parse_request(&method, &headers, query.as_deref(), &body);
    let (count, outcome) = {
        let mut st = inner.lock();
        st.calls.push(Call {
            method: method.clone(),
            params: Value::Object(req.params.clone()),
        });
        for problem in &req.problems {
            st.violation(&method, "malformed_request", problem.clone());
        }
        let fault = st.faults.get_mut(&method).and_then(VecDeque::pop_front);
        let outcome = match fault {
            Some(Fault::RateLimit(secs)) => Err(Some(secs)),
            Some(Fault::Fail(code)) => Ok(Err(benign(&code))),
            None => Ok(dispatch(
                &mut st,
                &method,
                req.token.as_deref(),
                &req.params,
            )),
        };
        if let Ok(Err(e)) = &outcome
            && e.violation
        {
            let detail = e.detail.clone();
            st.violation(&method, &e.code, detail);
        }
        (st.calls.len(), outcome)
    };
    inner.calls_tx.send_replace(count);
    match outcome {
        Err(secs) => {
            let secs = secs.unwrap_or(1);
            let mut response = (
                StatusCode::TOO_MANY_REQUESTS,
                json_body(json!({"ok": false, "error": "ratelimited"})),
            )
                .into_response();
            if let Ok(value) = HeaderValue::from_str(&secs.to_string()) {
                response.headers_mut().insert(header::RETRY_AFTER, value);
            }
            response
        }
        Ok(Ok(mut value)) => {
            if let Value::Object(obj) = &mut value {
                obj.insert("ok".into(), Value::Bool(true));
            }
            json_body(value).into_response()
        }
        Ok(Err(e)) => {
            let mut body = json!({"ok": false, "error": e.code});
            if e.code.starts_with("invalid_blocks") && !e.detail.is_empty() {
                body["response_metadata"] = json!({"messages": [format!("[ERROR] {}", e.detail)]});
            }
            json_body(body).into_response()
        }
    }
}

fn json_body(value: Value) -> impl IntoResponse {
    (
        [(header::CONTENT_TYPE, "application/json; charset=utf-8")],
        value.to_string(),
    )
}

fn dispatch(
    st: &mut State,
    method: &str,
    token: Option<&str>,
    params: &Map<String, Value>,
) -> Result<Value, ApiError> {
    if !METHODS.contains(&method) {
        return Err(err("unknown_method", format!("{method} is not simulated")));
    }
    let needs_app = method == "apps.connections.open";
    match token {
        None | Some("") => return Err(err("not_authed", "no token was sent")),
        Some(t) if t == APP_TOKEN && !needs_app => {
            return Err(err(
                "not_allowed_token_type",
                "app token used for a bot method",
            ));
        }
        Some(t) if t == BOT_TOKEN && needs_app => {
            return Err(err(
                "not_allowed_token_type",
                "bot token used for apps.connections.open",
            ));
        }
        Some(t) if t != APP_TOKEN && t != BOT_TOKEN => {
            return Err(err("invalid_auth", "token is not one the simulator issued"));
        }
        Some(_) => {}
    }
    match method {
        "auth.test" => Ok(json!({
            "url": "https://slacksim.slack.com/",
            "team": "Slack Sim",
            "user": "murtaugh",
            "team_id": TEAM_ID,
            "user_id": BOT_USER_ID,
            "bot_id": BOT_ID,
            "is_enterprise_install": false,
        })),
        "apps.connections.open" => {
            let ticket = st.uuid();
            st.tickets.insert(ticket.clone());
            let base = st.base.replacen("http://", "ws://", 1);
            Ok(json!({"url": format!("{base}link/?ticket={ticket}&app_id={}", crate::APP_ID)}))
        }
        "chat.postMessage" => post_message(st, params),
        "chat.update" => update_message(st, params),
        "reactions.add" => add_reaction(st, params),
        "conversations.replies" => replies(st, params),
        _ => file_info(st, params),
    }
}

fn arg(params: &Map<String, Value>, key: &str) -> Result<Option<String>, ApiError> {
    match params.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(s)) => Ok(Some(s.clone())),
        Some(other) => Err(err(
            "invalid_arguments",
            format!("`{key}` must be a string, got {other}"),
        )),
    }
}

fn channel(st: &mut State, id: Option<String>, write: bool) -> Result<String, ApiError> {
    let id = id.ok_or_else(|| err("channel_not_found", "no channel was given"))?;
    let id = if id.starts_with('U') && st.users.contains_key(&id) && write {
        st.im_for(&id)
    } else {
        id
    };
    let chan = st
        .channels
        .get(&id)
        .ok_or_else(|| err("channel_not_found", format!("no channel {id}")))?;
    if chan.kind == ChannelKind::Private && !chan.bot_member {
        return Err(err(
            "channel_not_found",
            format!("{id} is private and the bot is not a member"),
        ));
    }
    if write && chan.kind == ChannelKind::Public && !chan.bot_member {
        return Err(err(
            "not_in_channel",
            format!("the bot is not a member of {id}"),
        ));
    }
    Ok(id)
}

struct Content {
    text: Option<String>,
    blocks: Option<Vec<Value>>,
}

fn content(params: &Map<String, Value>) -> Result<Content, ApiError> {
    let text = arg(params, "text")?.filter(|t| !t.is_empty());
    let blocks = match params.get("blocks") {
        None | Some(Value::Null) => None,
        Some(raw) => {
            let parsed = blocks::parse(raw).map_err(|e| err(e.code, e.detail))?;
            blocks::validate(&parsed).map_err(|e| err(e.code, e.detail))?;
            Some(parsed)
        }
    };
    if let Some(text) = &text {
        let len = text.chars().count();
        if len > MAX_TEXT {
            return Err(err(
                "msg_too_long",
                format!("text is {len} characters, the limit is {MAX_TEXT}"),
            ));
        }
    }
    Ok(Content { text, blocks })
}

fn post_message(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let id = channel(st, arg(params, "channel")?, true)?;
    let content = content(params)?;
    let blocks = content.blocks.filter(|b| !b.is_empty());
    if content.text.is_none() && blocks.is_none() {
        return Err(err("no_text", "neither text nor blocks were given"));
    }
    let thread_ts = arg(params, "thread_ts")?;
    let mut msg = new_message(
        Some(BOT_USER_ID),
        content.text.as_deref().unwrap_or_default(),
    );
    msg.bot_id = Some(BOT_ID.to_owned());
    msg.blocks = blocks.map(Value::Array);
    let msg = st.post(&id, msg, thread_ts.as_deref()).map_err(|code| {
        err(
            code,
            format!(
                "thread_ts {} is not a message in {id}",
                thread_ts.unwrap_or_default()
            ),
        )
    })?;
    let chan = st
        .channels
        .get(&id)
        .ok_or_else(|| err("channel_not_found", id.clone()))?;
    Ok(json!({"channel": id, "ts": msg.ts, "message": render::message(st, chan, &msg)}))
}

fn update_message(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let id = channel(st, arg(params, "channel")?, true)?;
    let ts = arg(params, "ts")?.ok_or_else(|| err("message_not_found", "no ts was given"))?;
    let content = content(params)?;
    let blocks_given = content.blocks.is_some();
    let has_blocks = content.blocks.as_ref().is_some_and(|b| !b.is_empty());
    if content.text.is_none() && !has_blocks {
        return Err(err("no_text", "neither text nor blocks were given"));
    }
    let chan = st
        .channels
        .get_mut(&id)
        .ok_or_else(|| err("channel_not_found", id.clone()))?;
    let msg = chan
        .find_mut(&ts)
        .ok_or_else(|| err("message_not_found", format!("no message {ts} in {id}")))?;
    if msg.bot_id.as_deref() != Some(BOT_ID) {
        return Err(err(
            "cant_update_message",
            format!("{ts} was not posted by the bot"),
        ));
    }
    msg.text = content.text.unwrap_or_default();
    if blocks_given {
        msg.blocks = content.blocks.filter(|b| !b.is_empty()).map(Value::Array);
    }
    msg.edited = true;
    let msg = msg.clone();
    let chan = st
        .channels
        .get(&id)
        .ok_or_else(|| err("channel_not_found", id.clone()))?;
    Ok(json!({
        "channel": id,
        "ts": ts,
        "text": msg.text,
        "message": render::message(st, chan, &msg),
    }))
}

fn add_reaction(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let id = channel(st, arg(params, "channel")?, true)?;
    let ts = arg(params, "timestamp")?
        .ok_or_else(|| err("no_item_specified", "no `timestamp` was given"))?;
    let name = arg(params, "name")?.unwrap_or_default();
    let valid = !name.is_empty()
        && name
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || "_+-'".contains(c));
    if !valid {
        return Err(err(
            "invalid_name",
            format!("`{name}` is not an emoji name (no colons, lowercase)"),
        ));
    }
    let chan = st
        .channels
        .get_mut(&id)
        .ok_or_else(|| err("channel_not_found", id.clone()))?;
    let msg = chan
        .find_mut(&ts)
        .ok_or_else(|| err("message_not_found", format!("no message {ts} in {id}")))?;
    match msg.reactions.iter_mut().find(|r| r.name == name) {
        Some(r) if r.users.iter().any(|u| u == BOT_USER_ID) => Err(benign("already_reacted")),
        Some(r) => {
            r.users.push(BOT_USER_ID.to_owned());
            Ok(json!({}))
        }
        None => {
            msg.reactions.push(Reaction {
                name,
                users: vec![BOT_USER_ID.to_owned()],
            });
            Ok(json!({}))
        }
    }
}

fn replies(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let id = channel(st, arg(params, "channel")?, false)?;
    let ts = arg(params, "ts")?.ok_or_else(|| err("invalid_arguments", "no ts was given"))?;
    let offset = match arg(params, "cursor")?.filter(|c| !c.is_empty()) {
        None => 0,
        Some(cursor) => B64
            .decode(&cursor)
            .ok()
            .and_then(|b| String::from_utf8(b).ok())
            .and_then(|s| {
                s.strip_prefix("next_ts:")
                    .and_then(|n| n.parse::<usize>().ok())
            })
            .ok_or_else(|| err("invalid_cursor", format!("`{cursor}` is not a cursor")))?,
    };
    let limit = match params.get("limit") {
        Some(Value::String(s)) => s.parse::<usize>().unwrap_or(1000),
        Some(Value::Number(n)) => n.as_u64().map_or(1000, |n| n as usize),
        _ => 1000,
    };
    let limit = limit.clamp(2, 1000).min(st.replies_page_size.max(2));
    let chan = st
        .channels
        .get(&id)
        .ok_or_else(|| err("channel_not_found", id.clone()))?;
    let root = chan
        .thread_root(&ts)
        .ok_or_else(|| err("thread_not_found", format!("no message {ts} in {id}")))?;
    let parent = chan
        .find(&root)
        .ok_or_else(|| err("thread_not_found", format!("no message {root} in {id}")))?;
    let replies = chan.replies(&root);
    let window = limit - 1;
    let end = (offset + window).min(replies.len());
    let mut messages = vec![render::message(st, chan, parent)];
    for reply in replies.iter().skip(offset).take(window) {
        messages.push(render::message(st, chan, reply));
    }
    let has_more = end < replies.len();
    let mut out = json!({"messages": messages, "has_more": has_more});
    if has_more {
        out["response_metadata"] = json!({"next_cursor": B64.encode(format!("next_ts:{end}"))});
    }
    Ok(out)
}

fn file_info(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let id = arg(params, "file")?.unwrap_or_default();
    let file = st
        .files
        .get(&id)
        .ok_or_else(|| err("file_not_found", format!("no file `{id}`")))?;
    Ok(json!({
        "file": render::file(st, file),
        "comments": [],
        "response_metadata": {"next_cursor": ""},
    }))
}

pub(crate) async fn download(
    AxumState(inner): AxumState<Arc<Inner>>,
    Path((team_file, name)): Path<(String, String)>,
    headers: HeaderMap,
) -> Response {
    let bearer = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "));
    let mut st = inner.lock();
    if bearer != Some(BOT_TOKEN) {
        st.violation(
            "files.download",
            "not_authed",
            format!(
                "{team_file}/{name} was fetched without the bot token; Slack serves its login page"
            ),
        );
        return (
            StatusCode::FOUND,
            [(
                header::LOCATION,
                format!("{}signin?redir=/files-pri/{team_file}", st.base),
            )],
        )
            .into_response();
    }
    let file_id = team_file.rsplit('-').next().unwrap_or_default();
    match st.files.get(file_id).filter(|f| f.name == name) {
        Some(file) => (
            [(header::CONTENT_TYPE, file.mimetype.clone())],
            file.bytes.clone(),
        )
            .into_response(),
        None => StatusCode::NOT_FOUND.into_response(),
    }
}

pub(crate) async fn signin() -> Response {
    (
        [(header::CONTENT_TYPE, "text/html; charset=utf-8")],
        "<!DOCTYPE html><html><head><title>Slack</title></head><body>Sign in to Slack Sim</body></html>",
    )
        .into_response()
}
