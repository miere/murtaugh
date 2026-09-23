use serde::Deserialize;
use serde_json::Value;

#[derive(Debug, Clone, PartialEq)]
#[allow(clippy::large_enum_variant)]
pub enum SocketEvent {
    Event(EventEnvelope),
    /// The interactive payload (for example `block_actions`), without the envelope.
    Interactive(Value),
    /// The slash command payload, without the envelope.
    SlashCommand(Value),
}

#[derive(Debug, Clone, PartialEq)]
pub struct EventEnvelope {
    pub event_id: String,
    pub team_id: String,
    /// Zero on first delivery; Slack raises it on each redelivery of the same event.
    pub retry_attempt: u32,
    pub retry_reason: Option<String>,
    pub event: Event,
}

#[derive(Debug, Clone, PartialEq)]
pub enum Event {
    AppMention {
        user: String,
        channel: String,
        ts: String,
        thread_ts: Option<String>,
        text: String,
        files: Vec<FileRef>,
    },
    Message {
        channel: String,
        channel_type: String,
        user: Option<String>,
        bot_id: Option<String>,
        subtype: Option<String>,
        ts: String,
        thread_ts: Option<String>,
        text: String,
        files: Vec<FileRef>,
    },
    /// Someone opened one of the app's tabs; `tab` is `home` or `messages`.
    AppHomeOpened { user: String, tab: String },
    /// Any other event type, or a known one Slack sent in a shape this crate cannot read.
    Unknown { kind: String, raw: Value },
}

/// Events may carry partial file objects, so everything but the id is optional.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct FileRef {
    pub id: String,
    pub name: Option<String>,
    pub mimetype: Option<String>,
    pub size: Option<u64>,
}

#[derive(Deserialize)]
struct AppMentionJson {
    user: String,
    channel: String,
    ts: String,
    thread_ts: Option<String>,
    #[serde(default)]
    text: String,
    #[serde(default)]
    files: Vec<FileRef>,
}

#[derive(Deserialize)]
struct AppHomeJson {
    user: String,
    #[serde(default)]
    tab: String,
}

#[derive(Deserialize)]
struct MessageJson {
    channel: String,
    #[serde(default)]
    channel_type: String,
    user: Option<String>,
    bot_id: Option<String>,
    subtype: Option<String>,
    ts: String,
    thread_ts: Option<String>,
    #[serde(default)]
    text: String,
    #[serde(default)]
    files: Vec<FileRef>,
}

impl Event {
    pub fn decode(raw: Value) -> Event {
        let kind = raw
            .get("type")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        let decoded = match kind.as_str() {
            "app_mention" => AppMentionJson::deserialize(&raw)
                .ok()
                .map(|e| Event::AppMention {
                    user: e.user,
                    channel: e.channel,
                    ts: e.ts,
                    thread_ts: e.thread_ts,
                    text: e.text,
                    files: e.files,
                }),
            "message" => MessageJson::deserialize(&raw).ok().map(|e| Event::Message {
                channel: e.channel,
                channel_type: e.channel_type,
                user: e.user,
                bot_id: e.bot_id,
                subtype: e.subtype,
                ts: e.ts,
                thread_ts: e.thread_ts,
                text: e.text,
                files: e.files,
            }),
            "app_home_opened" => {
                AppHomeJson::deserialize(&raw)
                    .ok()
                    .map(|e| Event::AppHomeOpened {
                        user: e.user,
                        tab: e.tab,
                    })
            }
            _ => None,
        };
        decoded.unwrap_or(Event::Unknown { kind, raw })
    }
}

impl EventEnvelope {
    pub(crate) fn decode(envelope: &Value) -> Option<EventEnvelope> {
        let payload = envelope.get("payload")?;
        let text = |v: Option<&Value>| v.and_then(Value::as_str).map(str::to_owned);
        Some(EventEnvelope {
            event_id: text(payload.get("event_id")).unwrap_or_default(),
            team_id: text(payload.get("team_id")).unwrap_or_default(),
            retry_attempt: envelope
                .get("retry_attempt")
                .and_then(Value::as_u64)
                .and_then(|n| u32::try_from(n).ok())
                .unwrap_or(0),
            retry_reason: text(envelope.get("retry_reason")).filter(|r| !r.is_empty()),
            event: Event::decode(payload.get("event")?.clone()),
        })
    }
}

/// One button pressed or menu chosen on a message the bot posted, taken from a `block_actions`
/// payload.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Click {
    pub user: String,
    pub channel: String,
    pub message_ts: String,
    /// The thread the clicked message sits in, if it is a reply. An ephemeral message carries no
    /// `message`, so anything that must survive one belongs in the action's own value.
    pub thread_ts: Option<String>,
    pub action_id: String,
    /// A menu keeps its value under the option it chose rather than on the action.
    pub value: String,
    /// The message's inputs as they stood: `state.values`, keyed by block id, then action id.
    pub values: Value,
}

impl Click {
    /// `None` for anything but a button or menu on a message.
    pub fn from_interactive(payload: &Value) -> Option<Click> {
        if payload["type"].as_str() != Some("block_actions") {
            return None;
        }
        let action = payload["actions"].as_array()?.first()?;
        let text = |value: &Value| value.as_str().map(str::to_owned);
        Some(Click {
            user: text(&payload["user"]["id"])?,
            channel: text(&payload["container"]["channel_id"])
                .or_else(|| text(&payload["channel"]["id"]))?,
            message_ts: text(&payload["container"]["message_ts"])?,
            thread_ts: text(&payload["message"]["thread_ts"])
                .or_else(|| text(&payload["container"]["thread_ts"])),
            action_id: text(&action["action_id"])?,
            value: text(&action["value"])
                .or_else(|| text(&action["selected_option"]["value"]))
                .unwrap_or_default(),
            values: payload["state"]["values"].clone(),
        })
    }
}

/// A button or menu chosen on the Home tab, taken from a `block_actions` payload whose container
/// is a view rather than a message — the Home tab carries no channel, which is why `Click` (built
/// for message-hosted interactions) cannot represent it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HomeClick {
    pub user: String,
    pub action_id: String,
    /// A menu keeps its value under the option it chose rather than on the action.
    pub value: String,
}

impl HomeClick {
    /// `None` for anything but a button or menu on the Home tab.
    pub fn from_interactive(payload: &Value) -> Option<HomeClick> {
        if payload["type"].as_str() != Some("block_actions") {
            return None;
        }
        if payload["container"]["type"].as_str() != Some("view") {
            return None;
        }
        let action = payload["actions"].as_array()?.first()?;
        let text = |value: &Value| value.as_str().map(str::to_owned);
        Some(HomeClick {
            user: text(&payload["user"]["id"])?,
            action_id: text(&action["action_id"])?,
            value: text(&action["value"])
                .or_else(|| text(&action["selected_option"]["value"]))
                .unwrap_or_default(),
        })
    }
}
