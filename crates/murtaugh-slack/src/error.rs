use std::time::Duration;

#[derive(Debug, thiserror::Error)]
pub enum SlackError {
    /// Slack answered `ok: false`; `error` is Slack's code, verbatim. `detail` is the sentence some
    /// methods add beside it — `canvases.edit` names the section id it could not find there.
    #[error("{method} failed: {error}")]
    Api {
        method: String,
        error: String,
        detail: Option<String>,
    },
    /// Still rate limited after the client honoured `Retry-After` on every allowed retry.
    #[error("{method} is rate limited; retry after {retry_after:?}")]
    RateLimited {
        method: String,
        retry_after: Duration,
    },
    /// The message carries every cause reqwest wraps — "error sending request" alone does not say
    /// whether it was a timeout, a reset or TLS — and no URL, since an upload URL is a credential.
    #[error("{method}: HTTP failure: {}", causes(source))]
    Http {
        method: String,
        #[source]
        source: reqwest::Error,
    },
    #[error("{method}: undecodable response: {source}")]
    Decode {
        method: String,
        #[source]
        source: serde_json::Error,
    },
    #[error("socket mode: {0}")]
    WebSocket(#[from] Box<tokio_tungstenite::tungstenite::Error>),
    /// Slack serves its HTML sign-in page, not an HTTP error, when a file fetch is not authorised.
    #[error("download of {url} failed: {reason}")]
    Download { url: String, reason: String },
    #[error("upload of {filename} failed: {reason}")]
    Upload { filename: String, reason: String },
}

impl SlackError {
    pub(crate) fn http(method: impl Into<String>, source: reqwest::Error) -> Self {
        Self::Http {
            method: method.into(),
            source: source.without_url(),
        }
    }
}

fn causes(error: &(dyn std::error::Error + 'static)) -> String {
    let mut text = error.to_string();
    let mut cause = error.source();
    while let Some(next) = cause {
        text.push_str(": ");
        text.push_str(&next.to_string());
        cause = next.source();
    }
    text
}
