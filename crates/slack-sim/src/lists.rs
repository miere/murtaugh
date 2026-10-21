//! Slack Lists, as far as `slackLists.create`, `access.set` and the `items.*` methods go.
//!
//! What follows the documentation on purpose: columns, rows and select options are addressed by
//! id only; a text cell is written as `rich_text` and never as plain `text`; each written cell
//! carries exactly the one value key its column's type takes; a List is seen only by its owner,
//! those it was shared with and the members of channels it was shared with. Guessed, to be checked
//! against the real thing: a List's owner who hands ownership over keeps write access (see
//! [`crate::SlackSim::set_list_owner_keeps_access`]), and `items.list` answers the List under
//! `list` in the same shape `items.info` does.

use std::collections::BTreeMap;

use serde_json::{Map, Value, json};

use crate::BOT_USER_ID;
use crate::api::{ApiError, arg, err, refusal};
use crate::state::{State, now_secs};

const COLUMN_TYPES: &[&str] = &[
    "text",
    "message",
    "number",
    "select",
    "date",
    "user",
    "attachment",
    "checkbox",
    "email",
    "phone",
    "channel",
    "rating",
    "created_by",
    "last_edited_by",
    "created_time",
    "last_edited_time",
    "vote",
    "canvas",
    "reference",
    "link",
];
const UNEDITABLE: &[&str] = &[
    "created_by",
    "last_edited_by",
    "created_time",
    "last_edited_time",
];
const MAX_CELLS: usize = 100;

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub(crate) enum Level {
    Read,
    Write,
    Owner,
}

impl Level {
    fn parse(level: &str) -> Option<Level> {
        match level {
            "read" => Some(Level::Read),
            "write" => Some(Level::Write),
            "owner" => Some(Level::Owner),
            _ => None,
        }
    }
}

pub(crate) struct List {
    pub id: String,
    pub name: String,
    /// Columns as `slackLists.create` answers them, each with its `id`.
    pub columns: Vec<Value>,
    pub rows: Vec<Row>,
    /// Who may see or change it; its owner is the one user with [`Level::Owner`].
    pub users: BTreeMap<String, Level>,
    pub channels: BTreeMap<String, Level>,
}

#[derive(Clone)]
pub(crate) struct Row {
    pub id: String,
    pub parent: Option<String>,
    pub created_by: String,
    pub created: u64,
    /// Cells by column id, as written.
    pub cells: BTreeMap<String, Value>,
}

impl List {
    fn column(&self, id: &str) -> Option<&Value> {
        self.columns.iter().find(|c| c["id"] == id)
    }

    fn json(&self) -> Value {
        json!({
            "id": self.id,
            "name": self.name,
            "title": self.name,
            "list_metadata": {"schema": self.columns, "subtask_schema": self.columns},
        })
    }

    pub(crate) fn row_json(&self, row: &Row) -> Value {
        let fields: Vec<Value> = row
            .cells
            .iter()
            .filter_map(|(column_id, cell)| {
                let column = self.column(column_id)?;
                let mut field = cell.clone();
                field["column_id"] = json!(column_id);
                field["key"] = column["key"].clone();
                if column["type"] == "text" {
                    let text = plain(&cell["rich_text"]);
                    field["text"] = json!(text);
                    field["value"] = json!(text);
                } else {
                    field["value"] = cell
                        .as_object()
                        .and_then(|cell| cell.values().next().cloned())
                        .unwrap_or(Value::Null);
                }
                Some(field)
            })
            .collect();
        let mut item = json!({
            "id": row.id,
            "list_id": self.id,
            "date_created": row.created,
            "created_by": row.created_by,
            "updated_by": row.created_by,
            "updated_timestamp": row.created.to_string(),
            "fields": fields,
        });
        if let Some(parent) = &row.parent {
            item["parent_record_id"] = json!(parent);
        }
        item
    }
}

/// The text of a `rich_text` value, as Slack's plain fallback gives it.
fn plain(value: &Value) -> String {
    match value {
        Value::Object(obj) => {
            if obj.get("type").and_then(Value::as_str) == Some("text") {
                return obj
                    .get("text")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_owned();
            }
            obj.get("elements").map(plain).unwrap_or_default()
        }
        Value::Array(items) => items.iter().map(plain).collect(),
        _ => String::new(),
    }
}

/// The access the bot has to a List: its own, or the best of the channels it is in.
fn bot_level(st: &State, list: &List) -> Option<Level> {
    let own = list.users.get(BOT_USER_ID).copied();
    let through_channels = list
        .channels
        .iter()
        .filter(|(channel, _)| st.channels.get(*channel).is_some_and(|c| c.bot_member))
        .map(|(_, level)| *level)
        .max();
    own.max(through_channels)
}

fn not_found(id: &str) -> ApiError {
    refusal("list_not_found", format!("no List {id} the bot can see"))
}

/// The List named by `list_id`, if the bot may at least read it.
fn seen<'a>(st: &'a State, params: &Map<String, Value>) -> Result<&'a List, ApiError> {
    let id = arg(params, "list_id")?.ok_or_else(|| err("invalid_arguments", "no list_id"))?;
    let list = st.lists.get(&id).ok_or_else(|| not_found(&id))?;
    bot_level(st, list).ok_or_else(|| not_found(&id))?;
    Ok(list)
}

/// The List named by `list_id`, if the bot may change it.
fn writable(st: &mut State, params: &Map<String, Value>) -> Result<String, ApiError> {
    let list = seen(st, params)?;
    if bot_level(st, list) < Some(Level::Write) {
        return Err(refusal(
            "access_denied",
            format!("the bot may only read {}", list.id),
        ));
    }
    Ok(list.id.clone())
}

fn strings(params: &Map<String, Value>, key: &str) -> Result<Vec<String>, ApiError> {
    match params.get(key) {
        None | Some(Value::Null) => Ok(Vec::new()),
        Some(Value::Array(items)) => items
            .iter()
            .map(|item| {
                item.as_str()
                    .map(str::to_owned)
                    .ok_or_else(|| err("invalid_array_arg", format!("`{key}` holds a non-string")))
            })
            .collect(),
        Some(other) => Err(err(
            "invalid_array_arg",
            format!("`{key}` must be an array, got {other}"),
        )),
    }
}

pub(crate) fn create(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let name = arg(params, "name")?
        .filter(|name| !name.trim().is_empty())
        .ok_or_else(|| err("invalid_arguments", "a List needs a `name`"))?;
    let schema = match params.get("schema") {
        None | Some(Value::Null) => vec![json!({
            "key": "name", "name": "Name", "type": "text", "is_primary_column": true,
        })],
        Some(Value::Array(columns)) => columns.clone(),
        Some(other) => return Err(err("invalid_schema", format!("schema is {other}"))),
    };
    let mut columns = Vec::new();
    for column in &schema {
        let kind = column["type"].as_str().unwrap_or_default();
        if !COLUMN_TYPES.contains(&kind) {
            return Err(err(
                "invalid_column_type",
                format!("{kind:?} is no column type"),
            ));
        }
        if !column["key"].is_string() || !column["name"].is_string() {
            return Err(err(
                "invalid_schema",
                format!("{column} lacks a key or a name"),
            ));
        }
        if kind == "select" && !column["options"]["choices"].is_array() {
            return Err(err("invalid_schema", format!("{column} has no choices")));
        }
        let mut column = column.clone();
        column["id"] = json!(format!("Col{:08}", st.next_seq()));
        columns.push(column);
    }
    let primaries: Vec<&Value> = columns
        .iter()
        .filter(|c| c["is_primary_column"] == true)
        .collect();
    if primaries.len() != 1 || primaries[0]["type"] != "text" {
        return Err(err(
            "invalid_primary_column",
            "exactly one primary column is needed, and it must be text",
        ));
    }
    if params.get("todo_mode") == Some(&json!(true)) {
        for (key, name, kind) in [
            ("todo_completed", "Completed", "todo_completed"),
            ("todo_assignee", "Assignee", "todo_assignee"),
            ("todo_due_date", "Due date", "todo_due_date"),
        ] {
            let id = format!("Col{:08}", st.next_seq());
            columns.push(json!({"id": id, "key": key, "name": name, "type": kind}));
        }
    }
    let id = format!("F0LIST{:06}", st.next_seq());
    let list = List {
        id: id.clone(),
        name,
        columns,
        rows: Vec::new(),
        users: BTreeMap::from([(BOT_USER_ID.to_owned(), Level::Owner)]),
        channels: BTreeMap::new(),
    };
    let answer = json!({
        "list_id": id,
        "list_metadata": {"schema": list.columns, "subtask_schema": list.columns},
    });
    st.lists.insert(id, list);
    Ok(answer)
}

pub(crate) fn set_access(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let id = seen(st, params)?.id.clone();
    let level = arg(params, "access_level")?
        .as_deref()
        .and_then(Level::parse)
        .ok_or_else(|| err("invalid_arguments", "access_level is read, write or owner"))?;
    let users = strings(params, "user_ids")?;
    let channels = strings(params, "channel_ids")?;
    if users.is_empty() == channels.is_empty() {
        return Err(err(
            "invalid_parameters",
            "exactly one of user_ids and channel_ids is needed",
        ));
    }
    if let Some(user) = users.iter().find(|u| !st.users.contains_key(*u)) {
        return Err(err("user_not_found", format!("no user {user}")));
    }
    if let Some(channel) = channels.iter().find(|c| !st.channels.contains_key(*c)) {
        return Err(err("channel_not_found", format!("no channel {channel}")));
    }
    let keeps = st.list_owner_keeps_access;
    let Some(list) = st.lists.get_mut(&id) else {
        return Err(not_found(&id));
    };
    let bot = list.users.get(BOT_USER_ID).copied();
    if level == Level::Owner {
        if !channels.is_empty() || users.len() != 1 {
            return Err(err("invalid_arguments", "only one user can own a List"));
        }
        if bot != Some(Level::Owner) {
            return Err(refusal(
                "restricted_action",
                "only a List's owner can hand it over",
            ));
        }
        list.users.retain(|_, level| *level != Level::Owner);
        if keeps {
            list.users.insert(BOT_USER_ID.to_owned(), Level::Write);
        }
        list.users.insert(users[0].clone(), Level::Owner);
        return Ok(json!({}));
    }
    if bot != Some(Level::Owner) {
        return Err(refusal(
            "restricted_action",
            "only a List's owner can share it",
        ));
    }
    for user in users {
        list.users.insert(user, level);
    }
    for channel in channels {
        list.channels.insert(channel, level);
    }
    Ok(json!({}))
}

pub(crate) fn items(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let list = seen(st, params)?;
    let limit = match params.get("limit") {
        None | Some(Value::Null) => 100,
        Some(limit) => limit
            .as_u64()
            .filter(|n| (1..=1000).contains(n))
            .ok_or_else(|| err("invalid_arguments", format!("limit is {limit}")))?
            as usize,
    };
    let start = match arg(params, "cursor")? {
        None => 0,
        Some(cursor) => cursor
            .strip_prefix("row:")
            .and_then(|n| n.parse::<usize>().ok())
            .ok_or_else(|| err("invalid_cursor", format!("{cursor} was never answered")))?,
    };
    let archived = params.get("archived") == Some(&json!(true));
    let rows: Vec<&Row> = if archived {
        Vec::new()
    } else {
        list.rows.iter().collect()
    };
    let page: Vec<Value> = rows
        .iter()
        .skip(start)
        .take(limit)
        .map(|row| list.row_json(row))
        .collect();
    let next = if start + limit < rows.len() {
        format!("row:{}", start + limit)
    } else {
        String::new()
    };
    let mut answer = json!({"items": page, "response_metadata": {"next_cursor": next}});
    if params.get("include_list") == Some(&json!(true)) {
        answer["list"] = list.json();
    }
    Ok(answer)
}

pub(crate) fn info(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let list = seen(st, params)?;
    let id = arg(params, "id")?.ok_or_else(|| err("invalid_arguments", "no row id"))?;
    let row = list
        .rows
        .iter()
        .find(|row| row.id == id)
        .ok_or_else(|| refusal("record_not_found", format!("no row {id}")))?;
    let subtasks: Vec<Value> = list
        .rows
        .iter()
        .filter(|r| r.parent.as_deref() == Some(id.as_str()))
        .map(|r| list.row_json(r))
        .collect();
    Ok(json!({"list": list.json(), "record": list.row_json(row), "subtasks": subtasks}))
}

/// The value key a column of this type is written with.
fn value_key(kind: &str) -> &str {
    match kind {
        "text" => "rich_text",
        "todo_completed" => "checkbox",
        "todo_assignee" => "user",
        "todo_due_date" => "date",
        other => other,
    }
}

/// Checks one written cell against its column, as Slack does; answers the cell as stored.
fn cell(st: &State, list: &List, cell: &Value) -> Result<(String, Value), ApiError> {
    let column_id = cell["column_id"]
        .as_str()
        .ok_or_else(|| err("column_id_not_provided", format!("{cell} names no column")))?;
    let column = list
        .column(column_id)
        .ok_or_else(|| err("invalid_column_id", format!("no column {column_id}")))?;
    let kind = column["type"].as_str().unwrap_or_default();
    if UNEDITABLE.contains(&kind) {
        return Err(err("uneditable_column", format!("{column_id} is {kind}")));
    }
    let key = value_key(kind);
    let given: Vec<&String> = cell
        .as_object()
        .map(|obj| {
            obj.keys()
                .filter(|k| !matches!(k.as_str(), "column_id" | "row_id" | "row_id_to_create"))
                .collect()
        })
        .unwrap_or_default();
    if given.len() != 1 || given[0] != key {
        return Err(err(
            "invalid_input_type",
            format!("a {kind} column takes `{key}` alone, got {given:?}"),
        ));
    }
    let value = &cell[key];
    match key {
        "checkbox" if !value.is_boolean() => {
            return Err(err("invalid_input_type", "checkbox takes a boolean"));
        }
        "checkbox" => {}
        "rich_text" => {
            let blocks = value.as_array().filter(|b| !b.is_empty());
            let fine = blocks.is_some_and(|blocks| blocks.iter().all(|b| b["type"] == "rich_text"));
            if !fine {
                return Err(err("invalid_blocks", "rich_text takes rich_text blocks"));
            }
        }
        _ => {
            let Some(values) = value.as_array() else {
                return Err(err("invalid_input_type", format!("`{key}` takes an array")));
            };
            check(st, column, key, values)?;
        }
    }
    let mut stored = Map::new();
    stored.insert(key.to_owned(), value.clone());
    Ok((column_id.to_owned(), Value::Object(stored)))
}

fn check(st: &State, column: &Value, key: &str, values: &[Value]) -> Result<(), ApiError> {
    let multiple = matches!(
        column["options"]["format"].as_str(),
        Some("multi_select" | "multi_entity")
    );
    if values.len() > 1 && !multiple && matches!(key, "select" | "user" | "channel") {
        return Err(err(
            "over_cell_fields_limit",
            format!("{} takes one value", column["name"]),
        ));
    }
    for value in values {
        match key {
            "select" => {
                let choices = column["options"]["choices"].as_array();
                let known = choices.is_some_and(|c| c.iter().any(|c| c["value"] == *value));
                if !known {
                    return Err(err("invalid_option_id", format!("{value} is no option")));
                }
            }
            "user" => {
                let user = value.as_str().unwrap_or_default();
                if !st.users.contains_key(user) {
                    return Err(err("user_not_found", format!("no user {value}")));
                }
            }
            "date" => {
                let date = value.as_str().unwrap_or_default();
                let shaped = date.len() == 10
                    && date.char_indices().all(|(i, c)| {
                        if i == 4 || i == 7 {
                            c == '-'
                        } else {
                            c.is_ascii_digit()
                        }
                    });
                if !shaped {
                    return Err(err("invalid_date", format!("{value} is not YYYY-MM-DD")));
                }
            }
            "number" | "rating" if !value.is_number() => {
                return Err(err("invalid_input_type", format!("{value} is no number")));
            }
            _ => {}
        }
    }
    Ok(())
}

pub(crate) fn create_item(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let id = writable(st, params)?;
    let parent = arg(params, "parent_item_id")?;
    let fields = match params.get("initial_fields") {
        None | Some(Value::Null) => Vec::new(),
        Some(Value::Array(fields)) => fields.clone(),
        Some(other) => {
            return Err(err(
                "invalid_arguments",
                format!("initial_fields is {other}"),
            ));
        }
    };
    let list = &st.lists[&id];
    if let Some(parent) = &parent
        && !list.rows.iter().any(|row| &row.id == parent)
    {
        return Err(refusal("record_not_found", format!("no row {parent}")));
    }
    let mut cells = BTreeMap::new();
    for field in &fields {
        let (column, value) = cell(st, list, field)?;
        cells.insert(column, value);
    }
    let row = Row {
        id: format!("Rec{:08}", st.next_seq()),
        parent,
        created_by: BOT_USER_ID.to_owned(),
        created: now_secs(),
        cells,
    };
    let Some(list) = st.lists.get_mut(&id) else {
        return Err(not_found(&id));
    };
    list.rows.push(row.clone());
    Ok(json!({"item": list.row_json(&row)}))
}

pub(crate) fn update_items(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let id = writable(st, params)?;
    let cells = match params.get("cells") {
        Some(Value::Array(cells)) if (1..=MAX_CELLS).contains(&cells.len()) => cells.clone(),
        other => {
            return Err(err(
                "invalid_arguments",
                format!("cells takes 1 to {MAX_CELLS} cells, got {other:?}"),
            ));
        }
    };
    let list = &st.lists[&id];
    let mut writes = Vec::new();
    for given in &cells {
        let row = given["row_id"]
            .as_str()
            .ok_or_else(|| err("row_id_not_provided", format!("{given} names no row")))?;
        if !list.rows.iter().any(|r| r.id == row) {
            return Err(err("invalid_row_id", format!("no row {row}")));
        }
        let (column, value) = cell(st, list, given)?;
        writes.push((row.to_owned(), column, value));
    }
    let Some(list) = st.lists.get_mut(&id) else {
        return Err(not_found(&id));
    };
    for (row, column, value) in writes {
        if let Some(row) = list.rows.iter_mut().find(|r| r.id == row) {
            row.cells.insert(column, value);
        }
    }
    Ok(json!({}))
}

/// Deletes rows and their subtasks. Ids the List does not have are passed over.
pub(crate) fn delete_items(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let id = writable(st, params)?;
    let ids = strings(params, "ids")?;
    if ids.is_empty() {
        return Err(err("invalid_arguments", "no ids to delete"));
    }
    if let Some(list) = st.lists.get_mut(&id) {
        list.rows.retain(|row| {
            !ids.contains(&row.id) && !row.parent.as_ref().is_some_and(|p| ids.contains(p))
        });
    }
    Ok(json!({}))
}
