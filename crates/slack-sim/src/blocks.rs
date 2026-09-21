use std::collections::HashSet;

use serde_json::{Map, Value};

pub(crate) struct Invalid {
    pub code: &'static str,
    pub detail: String,
}

fn invalid(detail: String) -> Invalid {
    Invalid {
        code: "invalid_blocks",
        detail,
    }
}

const SHALLOW_TYPES: &[&str] = &[
    "image",
    "rich_text",
    "input",
    "file",
    "video",
    "markdown",
    "table",
];
const ELEMENT_TYPES: &[&str] = &[
    "button",
    "checkboxes",
    "datepicker",
    "datetimepicker",
    "image",
    "multi_static_select",
    "multi_external_select",
    "multi_users_select",
    "multi_conversations_select",
    "multi_channels_select",
    "overflow",
    "radio_buttons",
    "static_select",
    "external_select",
    "users_select",
    "conversations_select",
    "channels_select",
    "timepicker",
    "workflow_button",
];

pub(crate) fn parse(raw: &Value) -> Result<Vec<Value>, Invalid> {
    let parsed = match raw {
        Value::String(s) => serde_json::from_str::<Value>(s).map_err(|e| Invalid {
            code: "invalid_blocks_format",
            detail: format!("blocks is not valid JSON: {e}"),
        })?,
        other => other.clone(),
    };
    match parsed {
        Value::Array(items) => Ok(items),
        _ => Err(Invalid {
            code: "invalid_blocks_format",
            detail: "blocks must be a JSON array".into(),
        }),
    }
}

pub(crate) fn validate(blocks: &[Value]) -> Result<(), Invalid> {
    if blocks.len() > 50 {
        return Err(invalid(
            "must provide an array of no more than 50 items [json-pointer:/blocks]".into(),
        ));
    }
    for (i, block) in blocks.iter().enumerate() {
        validate_block(block, &format!("/blocks/{i}"))?;
    }
    Ok(())
}

fn object<'a>(value: &'a Value, at: &str) -> Result<&'a Map<String, Value>, Invalid> {
    value
        .as_object()
        .ok_or_else(|| invalid(format!("must be an object [json-pointer:{at}]")))
}

fn only_keys(obj: &Map<String, Value>, allowed: &[&str], at: &str) -> Result<(), Invalid> {
    match obj.keys().find(|k| !allowed.contains(&k.as_str())) {
        Some(key) => Err(invalid(format!(
            "invalid additional property: {key} [json-pointer:{at}]"
        ))),
        None => Ok(()),
    }
}

fn string<'a>(obj: &'a Map<String, Value>, key: &str, at: &str) -> Result<&'a str, Invalid> {
    obj.get(key)
        .and_then(Value::as_str)
        .ok_or_else(|| invalid(format!("must provide a string [json-pointer:{at}/{key}]")))
}

fn opt_string<'a>(
    obj: &'a Map<String, Value>,
    key: &str,
    max: usize,
    at: &str,
) -> Result<Option<&'a str>, Invalid> {
    match obj.get(key) {
        None => Ok(None),
        Some(Value::String(s)) if s.chars().count() <= max => Ok(Some(s)),
        Some(Value::String(_)) => Err(invalid(format!(
            "must be less than {} characters [json-pointer:{at}/{key}]",
            max + 1
        ))),
        Some(_) => Err(invalid(format!(
            "must provide a string [json-pointer:{at}/{key}]"
        ))),
    }
}

fn validate_block(block: &Value, at: &str) -> Result<(), Invalid> {
    let obj = object(block, at)?;
    let kind = string(obj, "type", at)?;
    opt_string(obj, "block_id", 255, at)?;
    match kind {
        "section" => section(obj, at),
        "divider" => only_keys(obj, &["type", "block_id"], at),
        "context" => context(obj, at),
        "actions" => actions(obj, at),
        "container" => container(obj, at),
        "header" => {
            only_keys(obj, &["type", "block_id", "text", "level"], at)?;
            let text = obj.get("text").ok_or_else(|| {
                invalid(format!("missing required field: text [json-pointer:{at}]"))
            })?;
            // `level` is optional and sets the heading, H1 to H4.
            if let Some(level) = obj.get("level")
                && !matches!(level.as_u64(), Some(1..=4))
            {
                return Err(invalid(format!(
                    "must be an integer between 1 and 4 [json-pointer:{at}/level]"
                )));
            }
            text_object(text, &["plain_text"], 150, &format!("{at}/text"))
        }
        k if SHALLOW_TYPES.contains(&k) => Ok(()),
        other => Err(invalid(format!(
            "unsupported block type: {other} [json-pointer:{at}/type]"
        ))),
    }
}

fn text_object(value: &Value, kinds: &[&str], max: usize, at: &str) -> Result<(), Invalid> {
    let obj = object(value, at)?;
    let kind = string(obj, "type", at)?;
    if !kinds.contains(&kind) {
        return Err(invalid(format!(
            "must be one of {kinds:?}, got {kind} [json-pointer:{at}/type]"
        )));
    }
    let allowed: &[&str] = if kind == "plain_text" {
        &["type", "text", "emoji"]
    } else {
        &["type", "text", "verbatim"]
    };
    only_keys(obj, allowed, at)?;
    let text = string(obj, "text", at)?;
    let len = text.chars().count();
    if len == 0 {
        return Err(invalid(format!(
            "must be more than 0 characters [json-pointer:{at}/text]"
        )));
    }
    if len > max {
        return Err(invalid(format!(
            "must be less than {} characters [json-pointer:{at}/text]",
            max + 1
        )));
    }
    for flag in ["emoji", "verbatim"] {
        if obj.get(flag).is_some_and(|v| !v.is_boolean()) {
            return Err(invalid(format!(
                "must be a boolean [json-pointer:{at}/{flag}]"
            )));
        }
    }
    Ok(())
}

fn section(obj: &Map<String, Value>, at: &str) -> Result<(), Invalid> {
    only_keys(
        obj,
        &["type", "block_id", "text", "fields", "accessory", "expand"],
        at,
    )?;
    let text = obj.get("text");
    let fields = obj.get("fields");
    if text.is_none() && fields.is_none() {
        return Err(invalid(format!(
            "must provide either text or fields [json-pointer:{at}]"
        )));
    }
    if let Some(text) = text {
        text_object(text, &["plain_text", "mrkdwn"], 3000, &format!("{at}/text"))?;
    }
    if let Some(fields) = fields {
        let items = fields
            .as_array()
            .ok_or_else(|| invalid(format!("must be an array [json-pointer:{at}/fields]")))?;
        if items.is_empty() || items.len() > 10 {
            return Err(invalid(format!(
                "must provide between 1 and 10 items [json-pointer:{at}/fields]"
            )));
        }
        for (i, field) in items.iter().enumerate() {
            text_object(
                field,
                &["plain_text", "mrkdwn"],
                2000,
                &format!("{at}/fields/{i}"),
            )?;
        }
    }
    if let Some(accessory) = obj.get("accessory") {
        element(accessory, &format!("{at}/accessory"))?;
    }
    Ok(())
}

/// Slack does not document this block; the shape is the Go gateway's approval card, which real
/// Slack accepts.
fn container(obj: &Map<String, Value>, at: &str) -> Result<(), Invalid> {
    only_keys(
        obj,
        &[
            "type",
            "block_id",
            "icon",
            "title",
            "subtitle",
            "is_collapsible",
            "default_collapsed",
            "has_header_divider",
            "width",
            "child_blocks",
        ],
        at,
    )?;
    let title = obj
        .get("title")
        .ok_or_else(|| invalid(format!("missing required field: title [json-pointer:{at}]")))?;
    text_object(title, &["plain_text"], 150, &format!("{at}/title"))?;
    if let Some(subtitle) = obj.get("subtitle") {
        text_object(subtitle, &["plain_text"], 3000, &format!("{at}/subtitle"))?;
    }
    let children = obj
        .get("child_blocks")
        .and_then(Value::as_array)
        .ok_or_else(|| {
            invalid(format!(
                "must provide an array [json-pointer:{at}/child_blocks]"
            ))
        })?;
    for (i, child) in children.iter().enumerate() {
        validate_block(child, &format!("{at}/child_blocks/{i}"))?;
    }
    Ok(())
}

fn context(obj: &Map<String, Value>, at: &str) -> Result<(), Invalid> {
    only_keys(obj, &["type", "block_id", "elements"], at)?;
    let items = obj
        .get("elements")
        .and_then(Value::as_array)
        .ok_or_else(|| {
            invalid(format!(
                "must provide an array [json-pointer:{at}/elements]"
            ))
        })?;
    if items.is_empty() || items.len() > 10 {
        return Err(invalid(format!(
            "must provide between 1 and 10 items [json-pointer:{at}/elements]"
        )));
    }
    for (i, item) in items.iter().enumerate() {
        let here = format!("{at}/elements/{i}");
        let kind = string(object(item, &here)?, "type", &here)?;
        if kind == "image" {
            continue;
        }
        text_object(item, &["plain_text", "mrkdwn"], 3000, &here)?;
    }
    Ok(())
}

fn actions(obj: &Map<String, Value>, at: &str) -> Result<(), Invalid> {
    only_keys(obj, &["type", "block_id", "elements"], at)?;
    let items = obj
        .get("elements")
        .and_then(Value::as_array)
        .ok_or_else(|| {
            invalid(format!(
                "must provide an array [json-pointer:{at}/elements]"
            ))
        })?;
    if items.is_empty() || items.len() > 25 {
        return Err(invalid(format!(
            "must provide between 1 and 25 items [json-pointer:{at}/elements]"
        )));
    }
    let mut ids = HashSet::new();
    for (i, item) in items.iter().enumerate() {
        let here = format!("{at}/elements/{i}");
        element(item, &here)?;
        if let Some(id) = item.get("action_id").and_then(Value::as_str)
            && !ids.insert(id)
        {
            return Err(invalid(format!(
                "action_id {id} is already used in this block [json-pointer:{here}/action_id]"
            )));
        }
    }
    Ok(())
}

fn element(value: &Value, at: &str) -> Result<(), Invalid> {
    let obj = object(value, at)?;
    let kind = string(obj, "type", at)?;
    if !ELEMENT_TYPES.contains(&kind) {
        return Err(invalid(format!(
            "unsupported element type: {kind} [json-pointer:{at}/type]"
        )));
    }
    if kind != "button" {
        return Ok(());
    }
    only_keys(
        obj,
        &[
            "type",
            "text",
            "action_id",
            "value",
            "style",
            "url",
            "confirm",
            "accessibility_label",
        ],
        at,
    )?;
    let text = obj
        .get("text")
        .ok_or_else(|| invalid(format!("missing required field: text [json-pointer:{at}]")))?;
    text_object(text, &["plain_text"], 75, &format!("{at}/text"))?;
    opt_string(obj, "action_id", 255, at)?;
    opt_string(obj, "value", 2000, at)?;
    opt_string(obj, "url", 3000, at)?;
    match opt_string(obj, "style", 16, at)? {
        None | Some("primary") | Some("danger") => Ok(()),
        Some(other) => Err(invalid(format!(
            "must be one of primary, danger, got {other} [json-pointer:{at}/style]"
        ))),
    }
}
