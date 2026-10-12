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

/// Slack caps the Markdown a message carries in `markdown` blocks.
const MAX_MARKDOWN_TEXT: usize = 12_000;

const SHALLOW_TYPES: &[&str] = &["image", "rich_text", "input", "file", "video", "table"];
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
    "plain_text_input",
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
    let is_chart = |block: &&Value| block["type"] == "data_visualization";
    if blocks.iter().filter(is_chart).count() > 2 {
        return Err(invalid(
            "a message holds no more than 2 data_visualization blocks [json-pointer:/blocks]"
                .into(),
        ));
    }
    let cells: usize = blocks.iter().map(data_table_chars).sum();
    if cells > DATA_TABLE_CHARS {
        return Err(invalid(format!(
            "the table cells of one message must be no more than {DATA_TABLE_CHARS} characters \
             in all [json-pointer:/blocks]"
        )));
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
        "input" => input(obj, at),
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
        "markdown" => {
            let len = string(obj, "text", at)?.chars().count();
            if len == 0 || len > MAX_MARKDOWN_TEXT {
                return Err(invalid(format!(
                    "must be between 1 and {MAX_MARKDOWN_TEXT} characters [json-pointer:{at}/text]"
                )));
            }
            Ok(())
        }
        "data_table" => data_table(obj, at),
        "data_visualization" => data_visualization(obj, at),
        k if SHALLOW_TYPES.contains(&k) => Ok(()),
        other => Err(invalid(format!(
            "unsupported block type: {other} [json-pointer:{at}/type]"
        ))),
    }
}

fn data_visualization(obj: &Map<String, Value>, at: &str) -> Result<(), Invalid> {
    only_keys(obj, &["type", "block_id", "title", "chart"], at)?;
    label(obj, "title", 50, at)?;
    let at = format!("{at}/chart");
    let chart = obj
        .get("chart")
        .ok_or_else(|| invalid(format!("missing required field: chart [json-pointer:{at}]")))?;
    let chart = object(chart, &at)?;
    let kind = string(chart, "type", &at)?;
    if kind == "pie" {
        only_keys(chart, &["type", "segments"], &at)?;
        let segments = list(chart, "segments", 12, &at)?;
        for (i, segment) in segments.iter().enumerate() {
            let at = format!("{at}/segments/{i}");
            let segment = object(segment, &at)?;
            only_keys(segment, &["label", "value"], &at)?;
            label(segment, "label", 20, &at)?;
            if !segment["value"].as_f64().is_some_and(|value| value > 0.0) {
                return Err(invalid(format!(
                    "must be a number greater than 0 [json-pointer:{at}/value]"
                )));
            }
        }
        return Ok(());
    }
    if !["bar", "area", "line"].contains(&kind) {
        return Err(invalid(format!(
            "unsupported chart type: {kind} [json-pointer:{at}/type]"
        )));
    }
    only_keys(chart, &["type", "series", "axis_config"], &at)?;
    let axis_at = format!("{at}/axis_config");
    let axis = chart.get("axis_config").ok_or_else(|| {
        invalid(format!(
            "missing required field: axis_config [json-pointer:{at}]"
        ))
    })?;
    let axis = object(axis, &axis_at)?;
    only_keys(axis, &["categories", "x_label", "y_label"], &axis_at)?;
    opt_string(axis, "x_label", 50, &axis_at)?;
    opt_string(axis, "y_label", 50, &axis_at)?;
    let mut categories = Vec::new();
    for (i, category) in list(axis, "categories", 20, &axis_at)?.iter().enumerate() {
        match category.as_str() {
            Some(name) if name.chars().count() <= 20 => categories.push(name),
            _ => {
                return Err(invalid(format!(
                    "must be a string of no more than 20 characters \
                     [json-pointer:{axis_at}/categories/{i}]"
                )));
            }
        }
    }
    let mut names = HashSet::new();
    for (i, series) in list(chart, "series", 12, &at)?.iter().enumerate() {
        let at = format!("{at}/series/{i}");
        let series = object(series, &at)?;
        only_keys(series, &["name", "data"], &at)?;
        if !names.insert(label(series, "name", 20, &at)?) {
            return Err(invalid(format!(
                "series names must be unique [json-pointer:{at}/name]"
            )));
        }
        let points = list(series, "data", 20, &at)?;
        let mut labels = Vec::new();
        for (p, point) in points.iter().enumerate() {
            let at = format!("{at}/data/{p}");
            let point = object(point, &at)?;
            only_keys(point, &["label", "value"], &at)?;
            labels.push(label(point, "label", 20, &at)?);
            if !point.get("value").is_some_and(Value::is_number) {
                return Err(invalid(format!(
                    "must provide a number [json-pointer:{at}/value]"
                )));
            }
        }
        // Every category has exactly one point, and no point names anything else.
        let matched = categories.iter().all(|category| labels.contains(category));
        if labels.len() != categories.len() || !matched {
            return Err(invalid(format!(
                "must provide one data point for each of axis_config.categories \
                 [json-pointer:{at}/data]"
            )));
        }
    }
    Ok(())
}

/// A required string of 1 to `max` characters.
fn label<'a>(
    obj: &'a Map<String, Value>,
    key: &str,
    max: usize,
    at: &str,
) -> Result<&'a str, Invalid> {
    match opt_string(obj, key, max, at)? {
        Some(text) if !text.is_empty() => Ok(text),
        _ => Err(invalid(format!(
            "must provide a string of more than 0 characters [json-pointer:{at}/{key}]"
        ))),
    }
}

/// A required array of 1 to `max` items.
fn list<'a>(
    obj: &'a Map<String, Value>,
    key: &str,
    max: usize,
    at: &str,
) -> Result<&'a Vec<Value>, Invalid> {
    match obj.get(key).and_then(Value::as_array) {
        Some(items) if (1..=max).contains(&items.len()) => Ok(items),
        _ => Err(invalid(format!(
            "must provide an array of between 1 and {max} items [json-pointer:{at}/{key}]"
        ))),
    }
}

const DATA_TABLE_CHARS: usize = 20_000;

fn data_table(obj: &Map<String, Value>, at: &str) -> Result<(), Invalid> {
    only_keys(
        obj,
        &[
            "type",
            "block_id",
            "rows",
            "caption",
            "page_size",
            "row_header_column_index",
        ],
        at,
    )?;
    string(obj, "caption", at)?;
    let rows = obj
        .get("rows")
        .and_then(Value::as_array)
        .ok_or_else(|| invalid(format!("must provide an array [json-pointer:{at}/rows]")))?;
    // The first row is the header, and a table has at least one row under it.
    if !(2..=201).contains(&rows.len()) {
        return Err(invalid(format!(
            "must provide between 2 and 201 rows [json-pointer:{at}/rows]"
        )));
    }
    let mut columns = None;
    for (r, row) in rows.iter().enumerate() {
        let at = format!("{at}/rows/{r}");
        let cells = row
            .as_array()
            .ok_or_else(|| invalid(format!("must provide an array [json-pointer:{at}]")))?;
        if !(1..=20).contains(&cells.len()) {
            return Err(invalid(format!(
                "must provide between 1 and 20 cells [json-pointer:{at}]"
            )));
        }
        if *columns.get_or_insert(cells.len()) != cells.len() {
            return Err(invalid(format!(
                "all rows must have the same number of cells [json-pointer:{at}]"
            )));
        }
        for (c, cell) in cells.iter().enumerate() {
            data_table_cell(cell, r == 0, &format!("{at}/{c}"))?;
        }
    }
    if let Some(size) = obj.get("page_size")
        && !matches!(size.as_u64(), Some(1..=100))
    {
        return Err(invalid(format!(
            "must be an integer between 1 and 100 [json-pointer:{at}/page_size]"
        )));
    }
    let columns = columns.unwrap_or(0) as u64;
    if let Some(index) = obj.get("row_header_column_index")
        && index.as_u64().is_none_or(|index| index >= columns)
    {
        return Err(invalid(format!(
            "must be the index of a column [json-pointer:{at}/row_header_column_index]"
        )));
    }
    if data_table_chars(&Value::Object(obj.clone())) > DATA_TABLE_CHARS {
        return Err(invalid(format!(
            "the cells of a table must be no more than {DATA_TABLE_CHARS} characters in all \
             [json-pointer:{at}/rows]"
        )));
    }
    Ok(())
}

fn data_table_cell(cell: &Value, header: bool, at: &str) -> Result<(), Invalid> {
    let obj = object(cell, at)?;
    let filled = |key: &str| match string(obj, key, at) {
        Ok("") => Err(invalid(format!(
            "must be more than 0 characters [json-pointer:{at}/{key}]"
        ))),
        other => other.map(drop),
    };
    match string(obj, "type", at)? {
        "raw_text" => {
            only_keys(obj, &["type", "text"], at)?;
            filled("text")
        }
        "raw_number" => {
            only_keys(obj, &["type", "value", "text"], at)?;
            if !obj.get("value").is_some_and(Value::is_number) {
                return Err(invalid(format!(
                    "must provide a number [json-pointer:{at}/value]"
                )));
            }
            filled("text")
        }
        "rich_text" if header => Err(invalid(format!(
            "rich_text cannot be used for header cells [json-pointer:{at}/type]"
        ))),
        "rich_text" => match obj.get("elements") {
            Some(Value::Array(_)) => Ok(()),
            _ => Err(invalid(format!(
                "must provide an array [json-pointer:{at}/elements]"
            ))),
        },
        "action_cell" => {
            only_keys(obj, &["type", "element", "fallback"], at)?;
            let button = obj.get("element").ok_or_else(|| {
                invalid(format!(
                    "missing required field: element [json-pointer:{at}]"
                ))
            })?;
            element(button, &format!("{at}/element"))
        }
        other => Err(invalid(format!(
            "unsupported cell type: {other} [json-pointer:{at}/type]"
        ))),
    }
}

/// How many characters a block's table cells hold; nothing for any other block.
fn data_table_chars(block: &Value) -> usize {
    if block.get("type").and_then(Value::as_str) != Some("data_table") {
        return 0;
    }
    let rows = block.get("rows").and_then(Value::as_array);
    let cells = rows.into_iter().flatten().filter_map(Value::as_array);
    cells.flatten().map(text_chars).sum()
}

/// Every `text` under a cell: its own, a button's, or each run of a rich text.
fn text_chars(value: &Value) -> usize {
    match value {
        Value::Object(obj) => obj
            .iter()
            .map(|(key, value)| match value {
                Value::String(text) if key == "text" => text.chars().count(),
                other => text_chars(other),
            })
            .sum(),
        Value::Array(items) => items.iter().map(text_chars).sum(),
        _ => 0,
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

fn input(obj: &Map<String, Value>, at: &str) -> Result<(), Invalid> {
    only_keys(
        obj,
        &[
            "type",
            "block_id",
            "label",
            "element",
            "hint",
            "optional",
            "dispatch_action",
        ],
        at,
    )?;
    let label = obj
        .get("label")
        .ok_or_else(|| invalid(format!("missing required field: label [json-pointer:{at}]")))?;
    text_object(label, &["plain_text"], 2000, &format!("{at}/label"))?;
    let element_at = format!("{at}/element");
    let element_value = obj.get("element").ok_or_else(|| {
        invalid(format!(
            "missing required field: element [json-pointer:{at}]"
        ))
    })?;
    element(element_value, &element_at)
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
