//! `murtaugh-client workload`: posts a workload to Murtaugh's HTTP API with a saved client token,
//! which must carry the `workloads` scope. Murtaugh serves it on the host it serves RAX on.

use std::time::Duration;

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use url::Url;

pub const PATH: &str = "/api/v1/workloads";
const TIMEOUT: Duration = Duration::from_secs(60);

/// Where the run answers.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Target {
    /// A new thread in a channel.
    Channel(String),
    /// An existing thread, continued as if its owner had written there.
    Thread { channel: String, thread_ts: String },
    /// The token owner's own DM.
    OwnDm,
}

#[derive(Debug, Clone)]
pub struct Request {
    pub prompt: String,
    pub target: Target,
    pub quiet: bool,
    pub idempotency_key: Option<String>,
}

/// What Murtaugh started.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Accepted {
    pub workload: String,
    pub channel: String,
    pub thread_ts: String,
}

#[derive(Debug, thiserror::Error)]
pub enum WorkloadError {
    #[error("{0} is not a Murtaugh address")]
    Address(String),
    /// Murtaugh answered, and refused.
    #[error("Murtaugh refused the workload ({status} {code}): {message}")]
    Refused {
        status: u16,
        code: String,
        message: String,
    },
    #[error("could not reach Murtaugh: {0}")]
    Unreachable(String),
}

/// The HTTP address of the endpoint on the host the profile dials RAX on: `wss` becomes `https`,
/// `ws` becomes `http`, and the path is the API's.
pub fn endpoint(gateway: &str) -> Result<Url, WorkloadError> {
    let mut url =
        Url::parse(gateway.trim()).map_err(|_| WorkloadError::Address(gateway.to_owned()))?;
    let scheme = match url.scheme() {
        "wss" | "https" => "https",
        "ws" | "http" => "http",
        _ => return Err(WorkloadError::Address(gateway.to_owned())),
    };
    url.set_scheme(scheme)
        .map_err(|()| WorkloadError::Address(gateway.to_owned()))?;
    url.set_path(PATH);
    url.set_query(None);
    Ok(url)
}

fn body(request: &Request) -> Value {
    let target = match &request.target {
        Target::Channel(channel) => json!({"channel": channel}),
        Target::Thread { channel, thread_ts } => {
            json!({"channel": channel, "thread_ts": thread_ts})
        }
        Target::OwnDm => json!({"dm": "me"}),
    };
    json!({
        "prompt": request.prompt,
        "target": target,
        "output": if request.quiet { "quiet" } else { "stream" },
    })
}

/// Posts the workload and returns what Murtaugh started. Blocking: it is one request.
pub fn send(gateway: &str, token: &str, request: &Request) -> Result<Accepted, WorkloadError> {
    let url = endpoint(gateway)?;
    let agent = ureq::Agent::config_builder()
        .timeout_global(Some(TIMEOUT))
        .http_status_as_error(false)
        .build()
        .new_agent();
    let mut call = agent
        .post(url.as_str())
        .header("Authorization", format!("Bearer {token}"))
        .header(
            "User-Agent",
            format!("murtaugh-client/{}", murtaugh_common::version::VERSION),
        );
    if let Some(key) = &request.idempotency_key {
        call = call.header("Idempotency-Key", key);
    }
    let mut response = call
        .send_json(body(request))
        .map_err(|err| WorkloadError::Unreachable(err.to_string()))?;
    let status = response.status().as_u16();
    let answer: Value = response.body_mut().read_json().unwrap_or(Value::Null);
    if status == 202 {
        return serde_json::from_value(answer.clone()).map_err(|_| WorkloadError::Refused {
            status,
            code: "unexpected_answer".into(),
            message: answer.to_string(),
        });
    }
    Err(WorkloadError::Refused {
        status,
        code: answer["error"].as_str().unwrap_or("unknown").to_owned(),
        message: answer["message"]
            .as_str()
            .map(str::to_owned)
            .unwrap_or_else(|| format!("HTTP {status}")),
    })
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]

    use super::*;

    #[test]
    fn the_endpoint_is_on_the_rax_host_over_https() {
        assert_eq!(
            endpoint("wss://murtaugh.example.com").unwrap().as_str(),
            "https://murtaugh.example.com/api/v1/workloads"
        );
        assert_eq!(
            endpoint("ws://127.0.0.1:7443/rax/v1/link")
                .unwrap()
                .as_str(),
            "http://127.0.0.1:7443/api/v1/workloads"
        );
        assert!(endpoint("ftp://nope").is_err());
    }

    #[test]
    fn the_body_names_its_target_and_output() {
        let request = Request {
            prompt: "hi".into(),
            target: Target::OwnDm,
            quiet: true,
            idempotency_key: None,
        };
        assert_eq!(
            body(&request),
            json!({"prompt": "hi", "target": {"dm": "me"}, "output": "quiet"})
        );
    }
}
