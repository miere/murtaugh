use serde::{Serialize, Serializer};
use serde_json::{Map, Value, json};

#[derive(Debug, Clone, PartialEq)]
pub enum Block {
    Section(Text),
    Context(Vec<Text>),
    Divider,
    Actions(Vec<Button>),
    /// Any block this enum does not model, sent as-is.
    Raw(Value),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Text {
    Mrkdwn(String),
    Plain(String),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Button {
    pub text: String,
    pub action_id: String,
    pub value: String,
    pub style: Option<ButtonStyle>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ButtonStyle {
    Primary,
    Danger,
}

impl Block {
    pub fn mrkdwn(text: impl Into<String>) -> Block {
        Block::Section(Text::Mrkdwn(text.into()))
    }

    pub fn plain(text: impl Into<String>) -> Block {
        Block::Section(Text::Plain(text.into()))
    }

    pub fn to_json(&self) -> Value {
        match self {
            Block::Section(text) => json!({"type": "section", "text": text.to_json()}),
            Block::Context(elements) => json!({
                "type": "context",
                "elements": elements.iter().map(Text::to_json).collect::<Vec<_>>(),
            }),
            Block::Divider => json!({"type": "divider"}),
            Block::Actions(buttons) => json!({
                "type": "actions",
                "elements": buttons.iter().map(Button::to_json).collect::<Vec<_>>(),
            }),
            Block::Raw(value) => value.clone(),
        }
    }
}

impl Text {
    pub fn to_json(&self) -> Value {
        match self {
            Text::Mrkdwn(text) => json!({"type": "mrkdwn", "text": text}),
            Text::Plain(text) => json!({"type": "plain_text", "text": text, "emoji": true}),
        }
    }
}

impl Button {
    pub fn new(
        text: impl Into<String>,
        action_id: impl Into<String>,
        value: impl Into<String>,
    ) -> Button {
        Button {
            text: text.into(),
            action_id: action_id.into(),
            value: value.into(),
            style: None,
        }
    }

    pub fn style(mut self, style: ButtonStyle) -> Button {
        self.style = Some(style);
        self
    }

    pub fn to_json(&self) -> Value {
        let mut out = Map::new();
        out.insert("type".into(), json!("button"));
        out.insert("text".into(), Text::Plain(self.text.clone()).to_json());
        out.insert("action_id".into(), json!(self.action_id));
        out.insert("value".into(), json!(self.value));
        if let Some(style) = self.style {
            let style = match style {
                ButtonStyle::Primary => "primary",
                ButtonStyle::Danger => "danger",
            };
            out.insert("style".into(), json!(style));
        }
        Value::Object(out)
    }
}

impl Serialize for Block {
    fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        self.to_json().serialize(serializer)
    }
}

pub mod mrkdwn {
    /// Escapes the three characters Slack's mrkdwn reserves for links and mentions.
    pub fn escape(text: &str) -> String {
        let mut out = String::with_capacity(text.len());
        for c in text.chars() {
            match c {
                '&' => out.push_str("&amp;"),
                '<' => out.push_str("&lt;"),
                '>' => out.push_str("&gt;"),
                c => out.push(c),
            }
        }
        out
    }
}
