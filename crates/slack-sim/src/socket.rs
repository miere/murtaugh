use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use axum::extract::ws::{Message, WebSocket, WebSocketUpgrade};
use axum::extract::{Query, State as AxumState};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use serde_json::{Value, json};
use tokio::sync::mpsc;

use crate::state::{Conn, Out, Pending, State};
use crate::{Inner, render};

pub(crate) const ACK_TIMEOUT: Duration = Duration::from_secs(3);
const MAX_RETRIES: u64 = 3;
const DISCONNECT_GRACE: Duration = Duration::from_secs(5);

pub(crate) async fn link(
    AxumState(inner): AxumState<Arc<Inner>>,
    Query(query): Query<HashMap<String, String>>,
    ws: WebSocketUpgrade,
) -> Response {
    let ticket = query.get("ticket").cloned().unwrap_or_default();
    {
        let mut st = inner.lock();
        if !st.tickets.remove(&ticket) {
            st.violation(
                "socket_mode",
                "invalid_ticket",
                format!("ticket `{ticket}` was never issued or was already used"),
            );
            return StatusCode::UNAUTHORIZED.into_response();
        }
    }
    ws.on_upgrade(move |socket| run(inner, socket))
}

async fn run(inner: Arc<Inner>, mut socket: WebSocket) {
    let (tx, mut rx) = mpsc::unbounded_channel();
    let (id, hello, timers) = {
        let mut st = inner.lock();
        let id = st.next_seq();
        st.conns.push(Conn {
            id,
            tx,
            disconnect_sent: false,
        });
        let hello = render::hello(st.conns.len());
        let queued: Vec<Value> = st.undelivered.drain(..).collect();
        let timers: Vec<_> = queued
            .into_iter()
            .filter_map(|envelope| dispatch(&mut st, envelope))
            .collect();
        (id, hello, timers)
    };
    for (envelope_id, attempt) in timers {
        spawn_ack_timer(&inner, envelope_id, attempt);
    }
    if socket
        .send(Message::Text(hello.to_string().into()))
        .await
        .is_err()
    {
        unregister(&inner, id);
        return;
    }
    loop {
        tokio::select! {
            _ = inner.shutdown.cancelled() => break,
            out = rx.recv() => match out {
                Some(Out::Text(text)) => {
                    if socket.send(Message::Text(text.into())).await.is_err() {
                        break;
                    }
                }
                Some(Out::Close) => {
                    let _ = socket.send(Message::Close(None)).await;
                    break;
                }
                Some(Out::Drop) | None => break,
            },
            frame = socket.recv() => match frame {
                Some(Ok(Message::Text(text))) => on_frame(&inner, text.as_str()),
                Some(Ok(Message::Binary(_))) => {
                    inner.lock().violation("socket_mode", "binary_frame", "Slack only reads text frames");
                }
                Some(Ok(Message::Close(_))) | Some(Err(_)) | None => break,
                Some(Ok(_)) => {}
            },
        }
    }
    unregister(&inner, id);
}

fn unregister(inner: &Inner, id: u64) {
    inner.lock().conns.retain(|c| c.id != id);
}

fn on_frame(inner: &Inner, text: &str) {
    let mut st = inner.lock();
    let envelope_id = serde_json::from_str::<Value>(text).ok().and_then(|v| {
        v.get("envelope_id")
            .and_then(Value::as_str)
            .map(str::to_owned)
    });
    let Some(envelope_id) = envelope_id else {
        st.violation(
            "socket_mode",
            "invalid_ack",
            format!("frame is not an ack with an envelope_id: {text}"),
        );
        return;
    };
    if st.pending.remove(&envelope_id).is_none() && !st.acked.contains(&envelope_id) {
        st.violation(
            "socket_mode",
            "unknown_envelope",
            format!("ack for envelope {envelope_id}, which was never sent"),
        );
    }
    st.acked.push(envelope_id);
}

pub(crate) fn dispatch(st: &mut State, envelope: Value) -> Option<(String, u64)> {
    let Some(conn) = st.conns.iter().rev().find(|c| !c.disconnect_sent) else {
        st.undelivered.push_back(envelope);
        return None;
    };
    let envelope_id = envelope
        .get("envelope_id")
        .and_then(Value::as_str)?
        .to_owned();
    let attempt = envelope
        .get("retry_attempt")
        .and_then(Value::as_u64)
        .unwrap_or(0);
    if conn.tx.send(Out::Text(envelope.to_string())).is_err() {
        st.undelivered.push_back(envelope);
        return None;
    }
    st.pending
        .insert(envelope_id.clone(), Pending { envelope, attempt });
    Some((envelope_id, attempt))
}

pub(crate) fn deliver(inner: &Arc<Inner>, envelope: Value) {
    let timer = dispatch(&mut inner.lock(), envelope);
    if let Some((envelope_id, attempt)) = timer {
        spawn_ack_timer(inner, envelope_id, attempt);
    }
}

fn spawn_ack_timer(inner: &Arc<Inner>, envelope_id: String, attempt: u64) {
    let inner = inner.clone();
    tokio::spawn(async move {
        tokio::select! {
            _ = inner.shutdown.cancelled() => {}
            _ = tokio::time::sleep(ACK_TIMEOUT) => ack_timed_out(&inner, &envelope_id, attempt),
        }
    });
}

fn ack_timed_out(inner: &Arc<Inner>, envelope_id: &str, attempt: u64) {
    let retry = {
        let mut st = inner.lock();
        if st
            .pending
            .get(envelope_id)
            .is_none_or(|p| p.attempt != attempt)
        {
            return;
        }
        let Some(pending) = st.pending.remove(envelope_id) else {
            return;
        };
        st.violation(
            "socket_mode",
            "ack_timeout",
            format!(
                "envelope {envelope_id} (retry_attempt {attempt}) was not acked within {}s",
                ACK_TIMEOUT.as_secs()
            ),
        );
        let retryable = pending.envelope.get("type").and_then(Value::as_str) == Some("events_api");
        if !retryable || attempt >= MAX_RETRIES {
            return;
        }
        let mut envelope = pending.envelope;
        envelope["retry_attempt"] = json!(attempt + 1);
        envelope["retry_reason"] = json!("timeout");
        envelope
    };
    deliver(inner, retry);
}

pub(crate) fn drop_all(inner: &Inner) {
    let mut st = inner.lock();
    for conn in st.conns.drain(..) {
        let _ = conn.tx.send(Out::Drop);
    }
}

pub(crate) fn send_disconnect(inner: &Arc<Inner>) {
    let text = render::disconnect("refresh_requested").to_string();
    let mut closing = Vec::new();
    {
        let mut st = inner.lock();
        for conn in st.conns.iter_mut().filter(|c| !c.disconnect_sent) {
            conn.disconnect_sent = true;
            let _ = conn.tx.send(Out::Text(text.clone()));
            closing.push(conn.tx.clone());
        }
    }
    let inner = inner.clone();
    tokio::spawn(async move {
        tokio::select! {
            _ = inner.shutdown.cancelled() => {}
            _ = tokio::time::sleep(DISCONNECT_GRACE) => {
                for tx in closing {
                    let _ = tx.send(Out::Close);
                }
            }
        }
    });
}
