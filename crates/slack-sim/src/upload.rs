//! Slack's external upload, the only way left to share a file: `files.getUploadURLExternal`
//! reserves an id and a URL, the bytes go to that URL as a multipart `file` part, and
//! `files.completeUploadExternal` shares the file, optionally into a thread with a comment.

use std::sync::Arc;

use axum::extract::{Multipart, Path, State as AxumState};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use serde_json::{Map, Value, json};

use crate::api::{ApiError, arg, channel, err};
use crate::state::{File, State, new_message, now_secs};
use crate::{BOT_ID, BOT_USER_ID, Inner};

pub(crate) struct Reserved {
    name: String,
    length: u64,
    bytes: Option<Vec<u8>>,
}

pub(crate) fn reserve(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let name = arg(params, "filename")?
        .filter(|name| !name.is_empty())
        .ok_or_else(|| err("invalid_arguments", "`filename` is required"))?;
    let length = arg(params, "length")?
        .and_then(|length| length.parse::<u64>().ok())
        .filter(|length| *length > 0)
        .ok_or_else(|| {
            err(
                "invalid_arguments",
                "`length` must be a positive byte count",
            )
        })?;
    let id = format!("F0UP{:06}", st.next_seq());
    st.reserved.insert(
        id.clone(),
        Reserved {
            name,
            length,
            bytes: None,
        },
    );
    Ok(json!({"upload_url": format!("{}upload/v1/{id}", st.base), "file_id": id}))
}

pub(crate) async fn receive(
    AxumState(inner): AxumState<Arc<Inner>>,
    Path(id): Path<String>,
    mut multipart: Multipart,
) -> Response {
    let mut bytes = None;
    while let Ok(Some(field)) = multipart.next_field().await {
        if field.name() == Some("file") {
            bytes = field.bytes().await.ok().map(|bytes| bytes.to_vec());
        }
    }
    let mut st = inner.lock();
    let Some(length) = st.reserved.get(&id).map(|reserved| reserved.length) else {
        st.violation(
            "files.upload",
            "file_not_found",
            format!("{id} was never reserved"),
        );
        return StatusCode::NOT_FOUND.into_response();
    };
    let Some(bytes) = bytes else {
        st.violation(
            "files.upload",
            "invalid_request",
            "the body has no `file` part",
        );
        return StatusCode::BAD_REQUEST.into_response();
    };
    if bytes.len() as u64 != length {
        st.violation(
            "files.upload",
            "length_mismatch",
            format!("{} bytes arrived, {length} were declared", bytes.len()),
        );
    }
    let received = bytes.len();
    if let Some(reserved) = st.reserved.get_mut(&id) {
        reserved.bytes = Some(bytes);
    }
    (StatusCode::OK, format!("OK - {received}")).into_response()
}

pub(crate) fn complete(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let raw =
        arg(params, "files")?.ok_or_else(|| err("invalid_arguments", "`files` is required"))?;
    let entries: Vec<Value> = serde_json::from_str(&raw).map_err(|e| {
        err(
            "invalid_arguments",
            format!("`files` is not a JSON array: {e}"),
        )
    })?;
    let thread_ts = arg(params, "thread_ts")?;
    let target = match arg(params, "channel_id")? {
        Some(id) => Some(channel(st, Some(id), true)?),
        None if thread_ts.is_some() => {
            return Err(err("invalid_arguments", "`thread_ts` needs a `channel_id`"));
        }
        None => None,
    };
    let comment = arg(params, "initial_comment")?.unwrap_or_default();
    let mut shared = Vec::new();
    for entry in entries {
        let id = entry["id"]
            .as_str()
            .ok_or_else(|| err("invalid_arguments", "every file needs an `id`"))?;
        let bytes = st
            .reserved
            .get(id)
            .and_then(|reserved| reserved.bytes.clone())
            .ok_or_else(|| err("file_not_found", format!("{id} was never uploaded")))?;
        let Some(reserved) = st.reserved.remove(id) else {
            continue;
        };
        let title = entry["title"].as_str().map(str::to_owned);
        shared.push(
            json!({"id": id, "title": title.clone().unwrap_or_else(|| reserved.name.clone())}),
        );
        let file = File {
            id: id.to_owned(),
            mimetype: mimetype(&reserved.name).to_owned(),
            name: reserved.name,
            title,
            bytes,
            user: BOT_USER_ID.to_owned(),
            created: now_secs(),
            channel: target.clone().unwrap_or_default(),
        };
        st.files.insert(id.to_owned(), file);
    }
    if let Some(target) = target {
        let mut msg = new_message(Some(BOT_USER_ID), &comment);
        msg.bot_id = Some(BOT_ID.to_owned());
        msg.subtype = Some("file_share".to_owned());
        msg.files = shared
            .iter()
            .filter_map(|file| file["id"].as_str().map(str::to_owned))
            .collect();
        st.post(&target, msg, thread_ts.as_deref())
            .map_err(|code| err(code, format!("cannot share into {target}")))?;
        if let Some(thread) = thread_ts {
            st.thread_statuses.remove(&(target, thread));
        }
    }
    Ok(json!({"files": shared}))
}

/// Slack sniffs the type itself; the extension is close enough for a fake.
fn mimetype(name: &str) -> &'static str {
    let ext = name
        .rsplit_once('.')
        .map(|(_, ext)| ext.to_ascii_lowercase());
    match ext.as_deref() {
        Some("txt") => "text/plain",
        Some("md") => "text/markdown",
        Some("csv") => "text/csv",
        Some("json") => "application/json",
        Some("png") => "image/png",
        Some("jpg" | "jpeg") => "image/jpeg",
        Some("pdf") => "application/pdf",
        _ => "application/octet-stream",
    }
}
