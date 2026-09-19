//! One turn's reply, streamed into its thread with Slack's streaming messages: the agent's words
//! as Markdown, and a task card per tool call. Ported from the Go gateway's stream writer.

use std::collections::HashMap;
use std::time::Duration;

use murtaugh_slack::{
    Chunk, PostMessage, STREAM_FINALIZED, STREAMING_UNSUPPORTED, SlackClient, SlackError,
    StartStream, TaskDisplayMode, TaskStatus, UpdateMessage,
};
use tokio::time::Instant;

use crate::render::{self, MESSAGE_LIMIT};

/// Slack caps a streamed message; the margin leaves room for a code fence's closer.
pub const STREAM_BUDGET: usize = 12_000 - 64;
const STREAM_MIN_ROOM: usize = 512;
pub const FLUSH_INTERVAL: Duration = Duration::from_millis(250);
const FLUSH_MIN_CHARS: usize = 24;
pub const TASK_INTERVAL: Duration = Duration::from_secs(1);
const PLAN_TITLE: &str = "Task list";
/// What Slack says to an append on a stream left idle too long, as one waiting on a tool
/// approval is; the message itself stays.
const STREAM_EXPIRED: &str = "message_not_found";
const TASK_TITLE: &str = "Tool call";

/// Where the reply goes and whom it answers. `recipient` is (team, user), left out in DMs.
#[derive(Debug, Clone)]
pub struct Target {
    pub channel: String,
    pub thread_ts: String,
    pub recipient: Option<(String, String)>,
}

struct Stream {
    channel: String,
    ts: String,
    spent: usize,
    plan_open: bool,
}

enum Mode {
    Streaming(Option<Stream>),
    /// For surfaces Slack cannot stream into: posts and edits whole messages instead.
    Buffered {
        text: String,
        posted: Vec<(String, String)>,
    },
}

struct Task {
    title: String,
    status: TaskStatus,
    flushed: Option<Instant>,
}

pub struct Reply {
    slack: SlackClient,
    target: Target,
    mode: Mode,
    pending: String,
    flushes: usize,
    last_flush: Instant,
    fence: Fence,
    tasks: HashMap<String, Task>,
    written: bool,
}

impl Reply {
    pub fn new(slack: SlackClient, target: Target) -> Self {
        Self {
            slack,
            target,
            mode: Mode::Streaming(None),
            pending: String::new(),
            flushes: 0,
            last_flush: Instant::now(),
            fence: Fence::default(),
            tasks: HashMap::new(),
            written: false,
        }
    }

    pub fn target(&self) -> &Target {
        &self.target
    }

    pub fn has_written(&self) -> bool {
        self.written || !self.pending.is_empty()
    }

    /// When the pending text is next due, so the caller can wake up and `flush_due`.
    pub fn due(&self) -> Option<Instant> {
        (!self.pending.is_empty()).then_some(self.last_flush + FLUSH_INTERVAL)
    }

    pub async fn text(&mut self, text: &str) {
        if text.is_empty() {
            return;
        }
        self.pending.push_str(text);
        if self.flushes == 0 {
            let all = self.pending.len();
            return self.emit(all).await;
        }
        if let Some(at) = self.pending.rfind('\n') {
            return self.emit(at + 1).await;
        }
        if self.pending.chars().count() >= FLUSH_MIN_CHARS
            || self.last_flush.elapsed() >= FLUSH_INTERVAL
        {
            self.flush_at_word().await;
        }
    }

    pub async fn flush_due(&mut self) {
        if self.due().is_some_and(|due| Instant::now() >= due) {
            self.flush_at_word().await;
        }
    }

    async fn flush_at_word(&mut self) {
        let cut = self
            .pending
            .rfind(' ')
            .map_or(self.pending.len(), |at| at + 1);
        self.emit(cut).await;
    }

    async fn emit(&mut self, cut: usize) {
        if cut == 0 {
            return;
        }
        let text: String = self.pending.drain(..cut).collect();
        self.flushes += 1;
        self.last_flush = Instant::now();
        self.written = true;
        match &mut self.mode {
            Mode::Streaming(_) => self.paint(&text).await,
            Mode::Buffered { text: all, .. } => {
                all.push_str(&text);
                self.repost().await;
            }
        }
    }

    async fn paint(&mut self, mut text: &str) {
        while !text.is_empty() {
            let spent = self.spent();
            if spent > 0 && STREAM_BUDGET.saturating_sub(spent) < STREAM_MIN_ROOM {
                self.rollover().await;
            }
            let (piece, rest) = split_at_budget(text, STREAM_BUDGET.saturating_sub(self.spent()));
            if piece.is_empty() {
                self.rollover().await;
                continue;
            }
            if !self.append(vec![Chunk::markdown(piece)]).await {
                return;
            }
            self.add_spent(piece.chars().count());
            self.fence.consume(piece);
            text = rest;
        }
    }

    fn spent(&self) -> usize {
        match &self.mode {
            Mode::Streaming(Some(stream)) => stream.spent,
            _ => 0,
        }
    }

    fn add_spent(&mut self, chars: usize) {
        if let Mode::Streaming(Some(stream)) = &mut self.mode {
            stream.spent += chars;
        }
    }

    /// Starts the stream with these chunks if there is none yet, else appends them. A stream
    /// Slack has finalized or found too long rolls over into a new message and is tried once more.
    async fn append(&mut self, chunks: Vec<Chunk>) -> bool {
        let Mode::Streaming(current) = &self.mode else {
            return false;
        };
        let Some(stream) = current else {
            return self.start(chunks).await;
        };
        let (channel, ts) = (stream.channel.clone(), stream.ts.clone());
        match self.slack.append_stream(&channel, &ts, &chunks).await {
            Ok(()) => true,
            Err(err) if closed(&err) || is(&err, "msg_too_long") => {
                self.rollover().await;
                let chunks = self.reopen_plan_for(chunks);
                let Mode::Streaming(Some(stream)) = &self.mode else {
                    return self.start(chunks).await;
                };
                let (channel, ts) = (stream.channel.clone(), stream.ts.clone());
                match self.slack.append_stream(&channel, &ts, &chunks).await {
                    Ok(()) => true,
                    Err(err) => {
                        tracing::warn!(error = %err, "could not append to a reply after rolling over");
                        false
                    }
                }
            }
            Err(err) => {
                tracing::warn!(error = %err, "could not append to a streamed reply");
                false
            }
        }
    }

    async fn start(&mut self, chunks: Vec<Chunk>) -> bool {
        let start = StartStream {
            channel: self.target.channel.clone(),
            thread_ts: self.target.thread_ts.clone(),
            recipient: self.target.recipient.clone(),
            task_display_mode: TaskDisplayMode::Plan,
            chunks: chunks.clone(),
        };
        match self.slack.start_stream(&start).await {
            Ok(posted) => {
                let plan_open = chunks
                    .iter()
                    .any(|chunk| matches!(chunk, Chunk::PlanUpdate { .. }));
                self.mode = Mode::Streaming(Some(Stream {
                    channel: posted.channel,
                    ts: posted.ts,
                    spent: 0,
                    plan_open,
                }));
                true
            }
            Err(err) if is(&err, STREAMING_UNSUPPORTED) => {
                tracing::info!("this surface cannot stream; posting the reply instead");
                let text = chunks
                    .iter()
                    .filter_map(|chunk| match chunk {
                        Chunk::MarkdownText { text } => Some(text.as_str()),
                        _ => None,
                    })
                    .collect::<String>();
                self.mode = Mode::Buffered {
                    text,
                    posted: Vec::new(),
                };
                self.repost().await;
                true
            }
            Err(err) => {
                tracing::warn!(error = %err, "could not start a streamed reply");
                false
            }
        }
    }

    async fn rollover(&mut self) {
        let Mode::Streaming(Some(stream)) = &self.mode else {
            return;
        };
        let (channel, ts) = (stream.channel.clone(), stream.ts.clone());
        if let Some(closer) = self.fence.closer()
            && let Err(err) = self
                .slack
                .append_stream(&channel, &ts, &[Chunk::markdown(closer)])
                .await
        {
            tracing::debug!(error = %err, "could not close a code fence before rolling over");
        }
        if let Err(err) = self.slack.stop_stream(&channel, &ts).await
            && !closed(&err)
        {
            tracing::debug!(error = %err, "could not stop a stream before rolling over");
        }
        self.mode = Mode::Streaming(None);
        if let Some(reopen) = self.fence.reopen()
            && self.start(vec![Chunk::markdown(reopen.clone())]).await
        {
            self.add_spent(reopen.chars().count());
        }
        self.fence.rolled_over();
    }

    fn reopen_plan_for(&mut self, chunks: Vec<Chunk>) -> Vec<Chunk> {
        let has_task = chunks
            .iter()
            .any(|chunk| matches!(chunk, Chunk::TaskUpdate { .. }));
        let open = matches!(&self.mode, Mode::Streaming(Some(stream)) if stream.plan_open);
        if !has_task || open {
            return chunks;
        }
        if let Mode::Streaming(Some(stream)) = &mut self.mode {
            stream.plan_open = true;
        }
        let mut with_plan = vec![Chunk::PlanUpdate {
            title: PLAN_TITLE.into(),
        }];
        with_plan.extend(chunks);
        with_plan
    }

    /// Settled states always go out; a running task is redrawn at most once a `TASK_INTERVAL`.
    pub async fn task(&mut self, id: &str, title: Option<&str>, status: TaskStatus) {
        if matches!(self.mode, Mode::Buffered { .. }) {
            return;
        }
        let task = self.tasks.entry(id.to_owned()).or_insert_with(|| Task {
            title: TASK_TITLE.into(),
            status,
            flushed: None,
        });
        if let Some(title) = title.map(str::trim).filter(|title| !title.is_empty()) {
            task.title = title.to_owned();
        }
        task.status = status;
        let settled = matches!(status, TaskStatus::Complete | TaskStatus::Error);
        if !settled && task.flushed.is_some_and(|at| at.elapsed() < TASK_INTERVAL) {
            return;
        }
        task.flushed = Some(Instant::now());
        let chunk = Chunk::TaskUpdate {
            id: id.to_owned(),
            title: task.title.clone(),
            status,
            details: None,
        };
        let pending = self.pending.len();
        self.emit(pending).await;
        let chunks = match &self.mode {
            Mode::Streaming(None) => vec![
                Chunk::PlanUpdate {
                    title: PLAN_TITLE.into(),
                },
                chunk,
            ],
            _ => self.reopen_plan_for(vec![chunk]),
        };
        self.written = true;
        self.append(chunks).await;
    }

    pub async fn finish(&mut self) {
        let pending = self.pending.len();
        self.emit(pending).await;
        if let Mode::Streaming(Some(stream)) = &self.mode {
            let (channel, ts) = (stream.channel.clone(), stream.ts.clone());
            if let Err(err) = self.slack.stop_stream(&channel, &ts).await
                && !closed(&err)
            {
                tracing::warn!(error = %err, "could not stop a streamed reply");
            }
        }
    }

    async fn repost(&mut self) {
        let Mode::Buffered { text, posted } = &mut self.mode else {
            return;
        };
        let rendered = render::mrkdwn(text);
        for (index, part) in render::split(&rendered, MESSAGE_LIMIT)
            .into_iter()
            .enumerate()
        {
            match posted.get(index) {
                Some((_, shown)) if *shown == part => {}
                Some((ts, _)) => {
                    let update = UpdateMessage {
                        channel: self.target.channel.clone(),
                        ts: ts.clone(),
                        text: part.clone(),
                        blocks: Vec::new(),
                    };
                    match self.slack.update_message(&update).await {
                        Ok(()) => posted[index].1 = part,
                        Err(err) => tracing::warn!(error = %err, "could not update a reply"),
                    }
                }
                None => {
                    let message = PostMessage {
                        channel: self.target.channel.clone(),
                        thread_ts: Some(self.target.thread_ts.clone()),
                        text: part.clone(),
                        blocks: Vec::new(),
                    };
                    match self.slack.post_message(&message).await {
                        Ok(done) => posted.push((done.ts, part)),
                        Err(err) => {
                            tracing::warn!(error = %err, "could not post a reply");
                            return;
                        }
                    }
                }
            }
        }
    }
}

/// The stream can take no more, though a new message would.
fn closed(err: &SlackError) -> bool {
    is(err, STREAM_FINALIZED) || is(err, STREAM_EXPIRED)
}

fn is(err: &SlackError, code: &str) -> bool {
    matches!(err, SlackError::Api { error, .. } if error == code)
}

/// Prefers a paragraph, then a line, then a word boundary, so a cut rarely lands mid-word.
fn split_at_budget(text: &str, room: usize) -> (&str, &str) {
    if room == 0 {
        return ("", text);
    }
    let Some((end, _)) = text.char_indices().nth(room) else {
        return (text, "");
    };
    let window = &text[..end];
    let cut = window
        .rfind("\n\n")
        .map(|at| at + 2)
        .or_else(|| window.rfind('\n').map(|at| at + 1))
        .or_else(|| window.rfind(' ').map(|at| at + 1))
        .filter(|&at| at > 0)
        .unwrap_or(end);
    text.split_at(cut)
}

/// Tracks whether the text so far ends inside a code fence, so a rollover can close it in the
/// old message and reopen it in the new one.
#[derive(Default)]
struct Fence {
    partial: String,
    opener: Option<String>,
}

impl Fence {
    fn consume(&mut self, text: &str) {
        self.partial.push_str(text);
        while let Some(at) = self.partial.find('\n') {
            let line = self.partial[..at].trim().to_owned();
            self.partial.drain(..=at);
            match &self.opener {
                None if line.starts_with("```") || line.starts_with("~~~") => {
                    self.opener = Some(line);
                }
                Some(opener) if line.starts_with(&opener[..3]) => self.opener = None,
                _ => {}
            }
        }
    }

    fn closer(&self) -> Option<String> {
        self.opener
            .as_ref()
            .map(|opener| format!("\n{}\n", &opener[..3]))
    }

    fn reopen(&self) -> Option<String> {
        self.opener.as_ref().map(|opener| format!("{opener}\n"))
    }

    fn rolled_over(&mut self) {
        self.partial.clear();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_budget_cut_prefers_paragraphs_then_lines_then_words() {
        assert_eq!(split_at_budget("aa\n\nbb cc", 7), ("aa\n\n", "bb cc"));
        assert_eq!(split_at_budget("aa\nbb cc", 6), ("aa\n", "bb cc"));
        assert_eq!(split_at_budget("aa bb cc", 6), ("aa bb ", "cc"));
        assert_eq!(split_at_budget("aa bb cc", 5), ("aa ", "bb cc"));
        assert_eq!(split_at_budget("abcdef", 3), ("abc", "def"));
        assert_eq!(split_at_budget("short", 10), ("short", ""));
        assert_eq!(split_at_budget("x", 0), ("", "x"));
    }

    #[test]
    fn a_fence_open_at_the_cut_is_closed_and_reopened() {
        let mut fence = Fence::default();
        fence.consume("text\n```rust\nlet a = 1;\n");
        assert_eq!(fence.closer().as_deref(), Some("\n```\n"));
        assert_eq!(fence.reopen().as_deref(), Some("```rust\n"));
        fence.consume("```\n");
        assert_eq!(fence.closer(), None);
    }
}
