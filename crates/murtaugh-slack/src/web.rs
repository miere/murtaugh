use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use reqwest::header::{AUTHORIZATION, CONTENT_TYPE, HeaderMap, RETRY_AFTER};
use reqwest::{Method, StatusCode};
use serde::Deserialize;
use serde::de::DeserializeOwned;
use serde_json::{Map, Value, json};
use url::Url;

use crate::blocks::Block;
use crate::error::SlackError;
use crate::events::FileRef;
use crate::stream::{Chunk, StartStream};

pub const DEFAULT_API_BASE: &str = "https://slack.com/api/";
const DEFAULT_MAX_RETRIES: u32 = 3;
const DEFAULT_RETRY_AFTER: Duration = Duration::from_secs(1);
const REPLIES_PAGE: &str = "200";
const HTTP_TIMEOUT: Duration = Duration::from_secs(30);

#[derive(Clone, PartialEq, Eq)]
pub struct Tokens {
    pub app: String,
    pub bot: String,
}

impl fmt::Debug for Tokens {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Tokens")
            .field("app", &redact(&self.app))
            .field("bot", &redact(&self.bot))
            .finish()
    }
}

fn chunks_json(chunks: &[Chunk]) -> Result<String, SlackError> {
    serde_json::to_string(chunks).map_err(|source| SlackError::Decode {
        method: "chunks".into(),
        source,
    })
}

fn mode(mode: crate::stream::TaskDisplayMode) -> String {
    match mode {
        crate::stream::TaskDisplayMode::Plan => "plan",
        crate::stream::TaskDisplayMode::Timeline => "timeline",
    }
    .to_owned()
}

fn redact(token: &str) -> String {
    let prefix: String = token.chars().take_while(|c| *c != '-').collect();
    format!("{prefix}-[redacted]")
}

#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct Identity {
    pub team_id: String,
    pub user_id: String,
    pub bot_id: String,
}

#[derive(Debug, Clone, PartialEq)]
pub struct PostMessage {
    pub channel: String,
    pub thread_ts: Option<String>,
    pub text: String,
    pub blocks: Vec<Block>,
}

#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct Posted {
    pub channel: String,
    pub ts: String,
}

#[derive(Debug, Clone, PartialEq)]
pub struct UpdateMessage {
    pub channel: String,
    pub ts: String,
    pub text: String,
    pub blocks: Vec<Block>,
}

#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct Message {
    pub ts: String,
    #[serde(default)]
    pub text: String,
    pub user: Option<String>,
    pub bot_id: Option<String>,
    pub subtype: Option<String>,
    pub thread_ts: Option<String>,
    #[serde(default)]
    pub files: Vec<FileRef>,
}

#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct FileInfo {
    pub id: String,
    pub name: String,
    pub mimetype: String,
    pub size: u64,
    #[serde(default)]
    pub url_private_download: String,
}

#[derive(Clone, Copy)]
pub(crate) enum Token {
    App,
    Bot,
}

enum Body {
    Json(Value),
    Form(Vec<(&'static str, String)>),
}

struct Inner {
    http: reqwest::Client,
    tokens: Tokens,
    api_base: Url,
    max_retries: u32,
}

#[derive(Clone)]
pub struct SlackClient {
    inner: Arc<Inner>,
}

impl fmt::Debug for SlackClient {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("SlackClient")
            .field("api_base", &self.inner.api_base.as_str())
            .field("tokens", &self.inner.tokens)
            .finish()
    }
}

impl SlackClient {
    pub fn new(tokens: Tokens, api_base: Url) -> SlackClient {
        SlackClient::with_max_retries(tokens, api_base, DEFAULT_MAX_RETRIES)
    }

    /// `max_retries` bounds how many `Retry-After` waits one call may sit through.
    pub fn with_max_retries(tokens: Tokens, mut api_base: Url, max_retries: u32) -> SlackClient {
        if !api_base.path().ends_with('/') {
            let path = format!("{}/", api_base.path());
            api_base.set_path(&path);
        }
        let http = reqwest::Client::builder()
            .timeout(HTTP_TIMEOUT)
            .build()
            .unwrap_or_else(|_| reqwest::Client::new());
        SlackClient {
            inner: Arc::new(Inner {
                http,
                tokens,
                api_base,
                max_retries,
            }),
        }
    }

    pub async fn auth_test(&self) -> Result<Identity, SlackError> {
        self.call("auth.test", Token::Bot, Body::Form(Vec::new()))
            .await
    }

    pub async fn post_message(&self, msg: &PostMessage) -> Result<Posted, SlackError> {
        let mut body = Map::new();
        body.insert("channel".into(), json!(msg.channel));
        body.insert("text".into(), json!(msg.text));
        if let Some(thread_ts) = &msg.thread_ts {
            body.insert("thread_ts".into(), json!(thread_ts));
        }
        if !msg.blocks.is_empty() {
            body.insert("blocks".into(), blocks_json(&msg.blocks));
        }
        self.call(
            "chat.postMessage",
            Token::Bot,
            Body::Json(Value::Object(body)),
        )
        .await
    }

    /// Always sends `blocks`, even empty, because Slack keeps the old blocks when it is absent.
    pub async fn update_message(&self, msg: &UpdateMessage) -> Result<(), SlackError> {
        let body = json!({
            "channel": msg.channel,
            "ts": msg.ts,
            "text": msg.text,
            "blocks": blocks_json(&msg.blocks),
        });
        self.call::<Value>("chat.update", Token::Bot, Body::Json(body))
            .await
            .map(drop)
    }

    pub async fn add_reaction(
        &self,
        channel: &str,
        ts: &str,
        name: &str,
    ) -> Result<(), SlackError> {
        let body = json!({"channel": channel, "timestamp": ts, "name": name});
        match self
            .call::<Value>("reactions.add", Token::Bot, Body::Json(body))
            .await
        {
            Err(SlackError::Api { error, .. }) if error == "already_reacted" => Ok(()),
            other => other.map(drop),
        }
    }

    /// The whole thread, parent first; Slack repeats the parent on every page, so it is deduped.
    pub async fn replies(&self, channel: &str, ts: &str) -> Result<Vec<Message>, SlackError> {
        #[derive(Deserialize)]
        struct Page {
            messages: Vec<Message>,
            #[serde(default)]
            response_metadata: Option<Cursor>,
        }
        #[derive(Deserialize)]
        struct Cursor {
            #[serde(default)]
            next_cursor: String,
        }
        let mut out: Vec<Message> = Vec::new();
        let mut cursor = String::new();
        loop {
            let mut params = vec![
                ("channel", channel.to_owned()),
                ("ts", ts.to_owned()),
                ("limit", REPLIES_PAGE.to_owned()),
            ];
            if !cursor.is_empty() {
                params.push(("cursor", cursor.clone()));
            }
            let page: Page = self
                .call("conversations.replies", Token::Bot, Body::Form(params))
                .await?;
            for message in page.messages {
                if !out.iter().any(|m| m.ts == message.ts) {
                    out.push(message);
                }
            }
            match page.response_metadata {
                Some(Cursor { next_cursor }) if !next_cursor.is_empty() => cursor = next_cursor,
                _ => break,
            }
        }
        Ok(out)
    }

    pub async fn start_stream(&self, start: &StartStream) -> Result<Posted, SlackError> {
        let mut params = vec![
            ("channel", start.channel.clone()),
            ("thread_ts", start.thread_ts.clone()),
            ("task_display_mode", mode(start.task_display_mode)),
        ];
        if let Some((team, user)) = &start.recipient {
            params.push(("recipient_team_id", team.clone()));
            params.push(("recipient_user_id", user.clone()));
        }
        if !start.chunks.is_empty() {
            params.push(("chunks", chunks_json(&start.chunks)?));
        }
        self.call("chat.startStream", Token::Bot, Body::Form(params))
            .await
    }

    pub async fn append_stream(
        &self,
        channel: &str,
        ts: &str,
        chunks: &[Chunk],
    ) -> Result<(), SlackError> {
        let params = vec![
            ("channel", channel.to_owned()),
            ("ts", ts.to_owned()),
            ("chunks", chunks_json(chunks)?),
        ];
        self.call::<Value>("chat.appendStream", Token::Bot, Body::Form(params))
            .await
            .map(drop)
    }

    pub async fn stop_stream(&self, channel: &str, ts: &str) -> Result<(), SlackError> {
        let params = vec![("channel", channel.to_owned()), ("ts", ts.to_owned())];
        self.call::<Value>("chat.stopStream", Token::Bot, Body::Form(params))
            .await
            .map(drop)
    }

    /// Slack's "is thinking..." line under a thread; an empty status clears it. Slack also
    /// clears it by itself once the bot posts or streams into the thread.
    pub async fn set_thread_status(
        &self,
        channel: &str,
        thread_ts: &str,
        status: &str,
    ) -> Result<(), SlackError> {
        let params = vec![
            ("channel_id", channel.to_owned()),
            ("thread_ts", thread_ts.to_owned()),
            ("status", status.to_owned()),
        ];
        self.call::<Value>(
            "assistant.threads.setStatus",
            Token::Bot,
            Body::Form(params),
        )
        .await
        .map(drop)
    }

    pub async fn file_info(&self, file_id: &str) -> Result<FileInfo, SlackError> {
        #[derive(Deserialize)]
        struct Info {
            file: FileInfo,
        }
        let info: Info = self
            .call(
                "files.info",
                Token::Bot,
                Body::Form(vec![("file", file_id.to_owned())]),
            )
            .await?;
        Ok(info.file)
    }

    pub async fn download(&self, file: &FileInfo) -> Result<Vec<u8>, SlackError> {
        let failed = |reason: String| SlackError::Download {
            url: file.url_private_download.clone(),
            reason,
        };
        let url = Url::parse(&file.url_private_download)
            .map_err(|e| failed(format!("not a URL: {e}")))?;
        let http = |source| SlackError::Http {
            method: "files.download".into(),
            source,
        };
        let res = self
            .inner
            .http
            .get(url)
            .header(AUTHORIZATION, format!("Bearer {}", self.inner.tokens.bot))
            .send()
            .await
            .map_err(http)?
            .error_for_status()
            .map_err(http)?;
        let is_html = content_type(res.headers()).starts_with("text/html");
        if is_html && !file.mimetype.starts_with("text/html") {
            return Err(failed(
                "Slack answered with an HTML page, which means the token was refused".into(),
            ));
        }
        Ok(res.bytes().await.map_err(http)?.to_vec())
    }

    pub(crate) async fn open_connection(&self) -> Result<String, SlackError> {
        #[derive(Deserialize)]
        struct Opened {
            url: String,
        }
        let opened: Opened = self
            .call("apps.connections.open", Token::App, Body::Form(Vec::new()))
            .await?;
        Ok(opened.url)
    }

    async fn call<T: DeserializeOwned>(
        &self,
        method: &'static str,
        token: Token,
        body: Body,
    ) -> Result<T, SlackError> {
        let url = format!("{}{method}", self.inner.api_base);
        let token = match token {
            Token::App => &self.inner.tokens.app,
            Token::Bot => &self.inner.tokens.bot,
        };
        let (content_type, payload) = match &body {
            Body::Json(value) => ("application/json; charset=utf-8", value.to_string()),
            Body::Form(params) => (
                "application/x-www-form-urlencoded",
                url::form_urlencoded::Serializer::new(String::new())
                    .extend_pairs(params.iter().map(|(k, v)| (*k, v.as_str())))
                    .finish(),
            ),
        };
        let http = |source| SlackError::Http {
            method: method.into(),
            source,
        };
        let mut attempt = 0;
        loop {
            let res = self
                .inner
                .http
                .request(Method::POST, url.clone())
                .header(AUTHORIZATION, format!("Bearer {token}"))
                .header(CONTENT_TYPE, content_type)
                .body(payload.clone())
                .send()
                .await
                .map_err(http)?;
            if res.status() == StatusCode::TOO_MANY_REQUESTS {
                let retry_after = retry_after(res.headers());
                if attempt >= self.inner.max_retries {
                    return Err(SlackError::RateLimited {
                        method: method.into(),
                        retry_after,
                    });
                }
                attempt += 1;
                tracing::warn!(method, ?retry_after, attempt, "rate limited by Slack");
                tokio::time::sleep(retry_after).await;
                continue;
            }
            let bytes = res
                .error_for_status()
                .map_err(http)?
                .bytes()
                .await
                .map_err(http)?;
            let value: Value =
                serde_json::from_slice(&bytes).map_err(|source| SlackError::Decode {
                    method: method.into(),
                    source,
                })?;
            if value.get("ok").and_then(Value::as_bool) != Some(true) {
                let error = value
                    .get("error")
                    .and_then(Value::as_str)
                    .unwrap_or("unknown_error")
                    .to_owned();
                if let Some(messages) = value.pointer("/response_metadata/messages") {
                    tracing::warn!(method, %error, %messages, "Slack rejected the call");
                }
                return Err(SlackError::Api {
                    method: method.into(),
                    error,
                });
            }
            return serde_json::from_value(value).map_err(|source| SlackError::Decode {
                method: method.into(),
                source,
            });
        }
    }
}

fn blocks_json(blocks: &[Block]) -> Value {
    Value::Array(blocks.iter().map(Block::to_json).collect())
}

fn retry_after(headers: &HeaderMap) -> Duration {
    headers
        .get(RETRY_AFTER)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.trim().parse::<u64>().ok())
        .map_or(DEFAULT_RETRY_AFTER, Duration::from_secs)
}

fn content_type(headers: &HeaderMap) -> String {
    headers
        .get(CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .to_ascii_lowercase()
}
