#![allow(dead_code, clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use serde_json::{Value, json};
use slack_sim::SlackSim;
use tokio::net::TcpStream;
use tokio_tungstenite::tungstenite::Message;
use tokio_tungstenite::{MaybeTlsStream, WebSocketStream};

pub type Ws = WebSocketStream<MaybeTlsStream<TcpStream>>;

pub fn fixture(name: &str) -> Value {
    let path = format!("{}/tests/fixtures/{name}.json", env!("CARGO_MANIFEST_DIR"));
    let text = std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{path}: {e}"));
    serde_json::from_str(&text).unwrap()
}

pub fn assert_shape(expected: &Value, actual: &Value) {
    shape(expected, actual, "$");
}

fn shape(expected: &Value, actual: &Value, at: &str) {
    match (expected, actual) {
        (Value::Object(e), Value::Object(a)) => {
            let mut ek: Vec<_> = e.keys().collect();
            let mut ak: Vec<_> = a.keys().collect();
            ek.sort();
            ak.sort();
            assert_eq!(ek, ak, "keys differ at {at}");
            for (k, v) in e {
                shape(v, &a[k], &format!("{at}.{k}"));
            }
        }
        (Value::Array(e), Value::Array(a)) => {
            assert_eq!(
                e.is_empty(),
                a.is_empty(),
                "emptiness differs at {at}: {actual}"
            );
            for (i, (ev, av)) in e.iter().zip(a).enumerate() {
                shape(ev, av, &format!("{at}[{i}]"));
            }
        }
        (Value::String(_), Value::String(_))
        | (Value::Number(_), Value::Number(_))
        | (Value::Bool(_), Value::Bool(_))
        | (Value::Null, Value::Null) => {}
        _ => panic!("type differs at {at}: expected like {expected}, got {actual}"),
    }
}

pub async fn json_call(sim: &SlackSim, method: &str, token: Option<&str>, body: Value) -> Value {
    let mut req = reqwest::Client::new()
        .post(sim.api_base().join(method).unwrap())
        .header("content-type", "application/json; charset=utf-8")
        .body(body.to_string());
    if let Some(token) = token {
        req = req.bearer_auth(token);
    }
    json_of(req.send().await.unwrap()).await
}

pub async fn form_call(
    sim: &SlackSim,
    method: &str,
    token: Option<&str>,
    params: &[(&str, &str)],
) -> Value {
    let body = url::form_urlencoded::Serializer::new(String::new())
        .extend_pairs(params)
        .finish();
    let mut req = reqwest::Client::new()
        .post(sim.api_base().join(method).unwrap())
        .header("content-type", "application/x-www-form-urlencoded")
        .body(body);
    if let Some(token) = token {
        req = req.bearer_auth(token);
    }
    json_of(req.send().await.unwrap()).await
}

async fn json_of(res: reqwest::Response) -> Value {
    serde_json::from_slice(&res.bytes().await.unwrap()).unwrap()
}

pub async fn bot(sim: &SlackSim, method: &str, body: Value) -> Value {
    json_call(sim, method, Some(&sim.tokens().bot), body).await
}

pub async fn post(sim: &SlackSim, channel: &str, text: &str) -> String {
    let res = bot(
        sim,
        "chat.postMessage",
        json!({"channel": channel, "text": text}),
    )
    .await;
    assert_eq!(res["ok"], true, "{res}");
    res["ts"].as_str().unwrap().to_owned()
}

pub async fn connect(sim: &SlackSim) -> Ws {
    let (ws, hello) = connect_raw(sim).await;
    assert_eq!(hello["type"], "hello");
    ws
}

pub async fn connect_raw(sim: &SlackSim) -> (Ws, Value) {
    let res = form_call(sim, "apps.connections.open", Some(&sim.tokens().app), &[]).await;
    assert_eq!(res["ok"], true, "{res}");
    let (mut ws, _) = tokio_tungstenite::connect_async(res["url"].as_str().unwrap())
        .await
        .unwrap();
    let hello = recv(&mut ws).await;
    (ws, hello)
}

pub async fn recv(ws: &mut Ws) -> Value {
    recv_within(ws, Duration::from_secs(5))
        .await
        .expect("no frame arrived within 5s")
}

pub async fn recv_within(ws: &mut Ws, wait: Duration) -> Option<Value> {
    let deadline = tokio::time::Instant::now() + wait;
    loop {
        let frame = tokio::time::timeout_at(deadline, ws.next()).await.ok()??;
        match frame.ok()? {
            Message::Text(text) => return Some(serde_json::from_str(text.as_str()).unwrap()),
            Message::Close(_) => return None,
            _ => continue,
        }
    }
}

pub async fn ack(ws: &mut Ws, envelope: &Value) {
    let ack = json!({"envelope_id": envelope["envelope_id"]});
    ws.send(Message::Text(ack.to_string().into()))
        .await
        .unwrap();
}
