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
    #[error("{method}: HTTP failure: {source}")]
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
