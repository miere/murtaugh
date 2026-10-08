#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

//! `murtaugh-client workload` against a stub of Murtaugh's endpoint.

use std::io::Write;
use std::process::{Command, Stdio};
use std::sync::{Arc, Mutex};

use axum::Json;
use axum::http::{HeaderMap, StatusCode};
use axum::routing::post;
use murtaugh_client::workload::{self, Request, Target, WorkloadError};
use serde_json::{Value, json};

const TOKEN: &str = "mrtg_user_0123456789abcdef_c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0LXNlY3I";

type Seen = Arc<Mutex<Vec<(HeaderMap, Value)>>>;

/// Answers 202 for the right token, 403 `insufficient_scope` for any other.
async fn stub() -> (String, Seen) {
    let seen: Seen = Arc::default();
    let recorded = seen.clone();
    let app = axum::Router::new().route(
        workload::PATH,
        post(move |headers: HeaderMap, Json(body): Json<Value>| {
            let recorded = recorded.clone();
            async move {
                let authorized =
                    headers["authorization"].to_str().unwrap() == format!("Bearer {TOKEN}");
                recorded.lock().unwrap().push((headers, body));
                if authorized {
                    (
                        StatusCode::ACCEPTED,
                        Json(json!({"workload": "w1", "channel": "D0ALICE01", "thread_ts": "1.5"})),
                    )
                } else {
                    (
                        StatusCode::FORBIDDEN,
                        Json(json!({"error": "insufficient_scope", "message": "mint one with workloads"})),
                    )
                }
            }
        }),
    );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let gateway = format!("ws://{}", listener.local_addr().unwrap());
    tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    (gateway, seen)
}

#[tokio::test(flavor = "multi_thread")]
async fn a_workload_is_posted_with_the_token_and_its_answer_returned() {
    let (gateway, seen) = stub().await;
    let request = Request {
        prompt: "nightly report".into(),
        target: Target::Thread {
            channel: "C0123".into(),
            thread_ts: "1.0".into(),
        },
        quiet: true,
        idempotency_key: Some("nightly-1".into()),
    };
    let accepted = {
        let gateway = gateway.clone();
        tokio::task::spawn_blocking(move || workload::send(&gateway, TOKEN, &request))
            .await
            .unwrap()
            .unwrap()
    };
    assert_eq!(accepted.thread_ts, "1.5");
    let (headers, body) = seen.lock().unwrap()[0].clone();
    assert_eq!(headers["idempotency-key"], "nightly-1");
    assert_eq!(
        body,
        json!({"prompt": "nightly report", "target": {"channel": "C0123", "thread_ts": "1.0"}, "output": "quiet"})
    );

    let refused = tokio::task::spawn_blocking(move || {
        let request = Request {
            prompt: "x".into(),
            target: Target::OwnDm,
            quiet: false,
            idempotency_key: None,
        };
        workload::send(&gateway, "mrtg_user_0123456789abcdef_other", &request)
    })
    .await
    .unwrap();
    let Err(WorkloadError::Refused { status, code, .. }) = refused else {
        panic!("expected a refusal, got {refused:?}")
    };
    assert_eq!((status, code.as_str()), (403, "insufficient_scope"));
}

#[tokio::test(flavor = "multi_thread")]
async fn the_command_reads_the_prompt_from_stdin_and_prints_the_run() {
    let (gateway, seen) = stub().await;
    let dir = tempfile::tempdir().unwrap();
    let token_file = dir.path().join("ci.token");
    std::fs::write(&token_file, format!("{TOKEN}\n")).unwrap();
    let home = dir.path().to_path_buf();
    let output = {
        let (home, gateway) = (home.clone(), gateway.clone());
        tokio::task::spawn_blocking(move || run(&home, &gateway, &token_file, &["--dm"]))
            .await
            .unwrap()
    };
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let printed: Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(printed["workload"], "w1");
    let (_, body) = seen.lock().unwrap()[0].clone();
    assert_eq!(body["prompt"], "what changed today?");
    assert_eq!(body["target"], json!({"dm": "me"}));

    // A refusal is the exit status and Murtaugh's own words.
    let other = dir.path().join("other.token");
    std::fs::write(&other, "mrtg_user_0123456789abcdef_other\n").unwrap();
    let output =
        tokio::task::spawn_blocking(move || run(&home, &gateway, &other, &["--channel", "C0123"]))
            .await
            .unwrap();
    assert!(!output.status.success());
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(stderr.contains("insufficient_scope"), "{stderr}");
    assert!(stderr.contains("mint one with workloads"), "{stderr}");
    assert!(
        !stderr.contains("mrtg_user_0123456789abcdef_other"),
        "the token leaked: {stderr}"
    );
}

/// Runs the real binary with the prompt on stdin, as a script would.
fn run(
    home: &std::path::Path,
    gateway: &str,
    token_file: &std::path::Path,
    args: &[&str],
) -> std::process::Output {
    let mut child = Command::new(env!("CARGO_BIN_EXE_murtaugh-client"))
        .env("HOME", home)
        .args(["workload", "--gateway", gateway, "--token-file"])
        .arg(token_file)
        .args(args)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    child
        .stdin
        .take()
        .unwrap()
        .write_all(b"what changed today?")
        .unwrap();
    child.wait_with_output().unwrap()
}
