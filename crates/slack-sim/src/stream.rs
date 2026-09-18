//! `chat.startStream`, `chat.appendStream` and `chat.stopStream`, validated like Slack.

use serde_json::{Map, Value, json};

use crate::api::{ApiError, arg, benign, channel, err};
use crate::state::{State, new_message};
use crate::{BOT_ID, BOT_USER_ID, ChannelKind, SimStream, SimTask, render};

pub(crate) const MAX_STREAM_TEXT: usize = 12_000;
const STATUSES: [&str; 4] = ["pending", "in_progress", "complete", "error"];

enum Chunk {
    Text(String),
    Task(SimTask),
    Plan(String),
}

fn chunks(params: &Map<String, Value>, required: bool) -> Result<Vec<Chunk>, ApiError> {
    let raw = match params.get("chunks") {
        None | Some(Value::Null) if required => {
            return Err(err("invalid_arguments", "no chunks were given"));
        }
        None | Some(Value::Null) => return Ok(Vec::new()),
        Some(Value::String(text)) => serde_json::from_str::<Value>(text)
            .map_err(|e| err("invalid_chunks", format!("chunks are not JSON: {e}")))?,
        Some(other) => other.clone(),
    };
    let Value::Array(items) = raw else {
        return Err(err("invalid_chunks", "chunks must be a JSON array"));
    };
    if required && items.is_empty() {
        return Err(err("invalid_arguments", "chunks is empty"));
    }
    items.iter().map(chunk).collect()
}

fn field<'a>(item: &'a Value, key: &str, kind: &str) -> Result<&'a str, ApiError> {
    item.get(key)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .ok_or_else(|| {
            err(
                "invalid_chunks",
                format!("a {kind} chunk needs a non-empty `{key}`"),
            )
        })
}

fn chunk(item: &Value) -> Result<Chunk, ApiError> {
    match item.get("type").and_then(Value::as_str) {
        Some("markdown_text") => Ok(Chunk::Text(
            field(item, "text", "markdown_text")?.to_owned(),
        )),
        Some("plan_update") => Ok(Chunk::Plan(field(item, "title", "plan_update")?.to_owned())),
        Some("task_update") => {
            let status = field(item, "status", "task_update")?;
            if !STATUSES.contains(&status) {
                return Err(err(
                    "invalid_chunks",
                    format!("task status {status:?} is not one of {STATUSES:?}"),
                ));
            }
            Ok(Chunk::Task(SimTask {
                id: field(item, "id", "task_update")?.to_owned(),
                title: field(item, "title", "task_update")?.to_owned(),
                status: status.to_owned(),
            }))
        }
        other => Err(err(
            "invalid_chunks",
            format!("unknown chunk type {other:?}"),
        )),
    }
}

fn apply(text: &mut String, stream: &mut SimStream, chunks: Vec<Chunk>) -> Result<(), ApiError> {
    let mut next = text.clone();
    for chunk in chunks {
        match chunk {
            Chunk::Text(more) => next.push_str(&more),
            Chunk::Plan(title) => stream.plans.push(title),
            Chunk::Task(task) => match stream.tasks.iter_mut().find(|t| t.id == task.id) {
                Some(known) => *known = task,
                None => stream.tasks.push(task),
            },
        }
    }
    let len = next.chars().count();
    if len > MAX_STREAM_TEXT {
        return Err(err(
            "msg_too_long",
            format!("a streamed message would be {len} characters; the limit is {MAX_STREAM_TEXT}"),
        ));
    }
    *text = next;
    Ok(())
}

pub(crate) fn start(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let id = channel(st, arg(params, "channel")?, true)?;
    let thread_ts = arg(params, "thread_ts")?
        .ok_or_else(|| err("invalid_arguments", "chat.startStream needs a thread_ts"))?;
    let mode = arg(params, "task_display_mode")?.unwrap_or_else(|| "timeline".into());
    if !["plan", "timeline"].contains(&mode.as_str()) {
        return Err(err(
            "invalid_arguments",
            format!("task_display_mode {mode:?} is not plan or timeline"),
        ));
    }
    let recipient = match (
        arg(params, "recipient_team_id")?,
        arg(params, "recipient_user_id")?,
    ) {
        (Some(team), Some(user)) => Some((team, user)),
        (None, None) => None,
        _ => {
            return Err(err(
                "invalid_arguments",
                "recipient_team_id and recipient_user_id go together",
            ));
        }
    };
    let kind = st.channels.get(&id).map(|c| c.kind);
    if kind != Some(ChannelKind::Im) && recipient.is_none() {
        return Err(err(
            "invalid_arguments",
            "streaming in a channel needs recipient_team_id and recipient_user_id",
        ));
    }
    let chunks = chunks(params, false)?;
    st.thread_statuses.remove(&(id.clone(), thread_ts.clone()));
    let mut msg = new_message(Some(BOT_USER_ID), "");
    msg.bot_id = Some(BOT_ID.to_owned());
    let mut stream = SimStream {
        open: true,
        task_display_mode: mode,
        recipient,
        plans: Vec::new(),
        tasks: Vec::new(),
    };
    apply(&mut msg.text, &mut stream, chunks)?;
    msg.stream = Some(stream);
    let msg = st.post(&id, msg, Some(&thread_ts)).map_err(|code| {
        err(
            code,
            format!("thread_ts {thread_ts} is not a message in {id}"),
        )
    })?;
    let chan = st
        .channels
        .get(&id)
        .ok_or_else(|| err("channel_not_found", id.clone()))?;
    Ok(json!({"channel": id, "ts": msg.ts, "message": render::message(st, chan, &msg)}))
}

fn open_stream<'a>(
    st: &'a mut State,
    params: &Map<String, Value>,
) -> Result<(String, String, &'a mut crate::SimMessage), ApiError> {
    let id = channel(st, arg(params, "channel")?, true)?;
    let ts = arg(params, "ts")?.ok_or_else(|| err("invalid_arguments", "no ts was given"))?;
    let msg = st
        .channels
        .get_mut(&id)
        .and_then(|c| c.find_mut(&ts))
        .ok_or_else(|| err("message_not_found", format!("no message {ts} in {id}")))?;
    match &msg.stream {
        None => Err(err(
            "message_not_in_streaming_state",
            format!("{ts} was not started with chat.startStream"),
        )),
        Some(stream) if !stream.open => Err(benign("message_not_in_streaming_state")),
        Some(_) => Ok((id, ts, msg)),
    }
}

pub(crate) fn append(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let chunks = chunks(params, true)?;
    let (id, ts, msg) = open_stream(st, params)?;
    let mut text = msg.text.clone();
    if let Some(stream) = msg.stream.as_mut() {
        apply(&mut text, stream, chunks)?;
    }
    msg.text = text;
    Ok(json!({"channel": id, "ts": ts}))
}

pub(crate) fn stop(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let (id, ts, msg) = open_stream(st, params)?;
    if let Some(stream) = msg.stream.as_mut() {
        stream.open = false;
    }
    Ok(json!({"channel": id, "ts": ts}))
}

pub(crate) fn set_status(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let id = channel(st, arg(params, "channel_id")?, true)?;
    let thread_ts = arg(params, "thread_ts")?.ok_or_else(|| {
        err(
            "invalid_arguments",
            "assistant.threads.setStatus needs a thread_ts",
        )
    })?;
    let status = match params.get("status") {
        Some(Value::String(status)) => status.clone(),
        _ => {
            return Err(err(
                "invalid_arguments",
                "status must be given, empty to clear it",
            ));
        }
    };
    let known = st
        .channels
        .get(&id)
        .is_some_and(|c| c.find(&thread_ts).is_some());
    if !known {
        return Err(err(
            "thread_not_found",
            format!("thread_ts {thread_ts} is not a message in {id}"),
        ));
    }
    let key = (id, thread_ts);
    if status.is_empty() {
        st.thread_statuses.remove(&key);
    } else {
        st.thread_statuses.insert(key, status);
    }
    Ok(json!({}))
}
