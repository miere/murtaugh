//! Slack Lists. Slack addresses columns, rows and select options by id; these tools let the agent
//! use column names and option labels instead, and read cells back in the very shape they take
//! them, so what one tool reads another can write. The List's columns are fetched afresh for each
//! call: a List may change under the agent between two of them.

use murtaugh_slack::{ItemsPage, ListItems, SlackClient, SlackError};
use serde_json::{Map, Value, json};

use super::slack_file::file_id;

pub mod add;
pub mod create;
pub mod delete;
pub mod read;
pub mod read_item;
pub mod update;

/// How a column's cells are written and read.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Kind {
    Text,
    Number,
    Select,
    Date,
    User,
    Channel,
    Checkbox,
    Email,
    Phone,
    Rating,
    Link,
    Message,
    Attachment,
    /// Slack fills it in itself, such as who created the row.
    ReadOnly,
    /// A type these tools cannot write, such as a vote or a canvas.
    Other,
}

impl Kind {
    /// From Slack's column type. A todo List's own columns and older names are folded in.
    fn of(kind: &str) -> Kind {
        match kind {
            "text" | "rich_text" => Kind::Text,
            "number" => Kind::Number,
            "select" | "multi_select" => Kind::Select,
            "date" | "due_date" | "todo_due_date" => Kind::Date,
            "user" | "assignee" | "todo_assignee" => Kind::User,
            "channel" => Kind::Channel,
            "checkbox" | "completed" | "todo_completed" => Kind::Checkbox,
            "email" => Kind::Email,
            "phone" => Kind::Phone,
            "rating" => Kind::Rating,
            "link" => Kind::Link,
            "message" => Kind::Message,
            "attachment" => Kind::Attachment,
            "created_by" | "last_edited_by" | "created_time" | "last_edited_time" => Kind::ReadOnly,
            _ => Kind::Other,
        }
    }

    /// The key a cell of this kind carries its value under.
    fn key(&self) -> Option<&'static str> {
        Some(match self {
            Kind::Text => "rich_text",
            Kind::Number => "number",
            Kind::Select => "select",
            Kind::Date => "date",
            Kind::User => "user",
            Kind::Channel => "channel",
            Kind::Checkbox => "checkbox",
            Kind::Email => "email",
            Kind::Phone => "phone",
            Kind::Rating => "rating",
            Kind::Link => "link",
            Kind::Message => "message",
            Kind::Attachment => "attachment",
            Kind::ReadOnly | Kind::Other => return None,
        })
    }
}

#[derive(Debug, Clone)]
pub struct Choice {
    /// What Slack calls the option's value: its id.
    pub id: String,
    pub label: String,
}

#[derive(Debug, Clone)]
pub struct Column {
    pub id: String,
    pub key: String,
    pub name: String,
    /// Slack's own name for the type, as the agent is shown it.
    pub type_name: String,
    pub kind: Kind,
    pub choices: Vec<Choice>,
    /// Whether a cell holds more than one option, person or channel.
    pub multiple: bool,
}

impl Column {
    fn from_json(column: &Value) -> Option<Column> {
        let options = &column["options"];
        let type_name = column["type"].as_str()?.to_owned();
        let choices = options["choices"]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(|choice| {
                let id = choice["value"].as_str()?.to_owned();
                let label = choice["label"].as_str().unwrap_or(&id).to_owned();
                Some(Choice { id, label })
            })
            .collect();
        let multiple = type_name == "multi_select"
            || matches!(
                options["format"].as_str(),
                Some("multi_select" | "multi_entity")
            );
        Some(Column {
            id: column["id"].as_str()?.to_owned(),
            key: column["key"].as_str().unwrap_or_default().to_owned(),
            name: column["name"].as_str().unwrap_or_default().to_owned(),
            kind: Kind::of(&type_name),
            type_name,
            choices,
            multiple,
        })
    }

    /// How the agent is told about this column.
    fn describe(&self) -> Value {
        let mut column = json!({"name": self.name, "type": self.type_name});
        if !self.choices.is_empty() {
            let labels: Vec<&str> = self.choices.iter().map(|c| c.label.as_str()).collect();
            column["choices"] = json!(labels);
        }
        if self.multiple {
            column["multiple"] = json!(true);
        }
        if matches!(self.kind, Kind::ReadOnly | Kind::Other) {
            column["read_only"] = json!(true);
        }
        column
    }
}

/// A List's title and columns.
#[derive(Debug, Clone)]
pub struct Schema {
    pub id: String,
    pub title: String,
    pub columns: Vec<Column>,
}

impl Schema {
    /// From the `list` object Slack answers alongside rows.
    fn from_list(id: &str, list: &Value) -> Option<Schema> {
        let columns = list
            .pointer("/list_metadata/schema")
            .or_else(|| list.get("schema"))?
            .as_array()?;
        let title = ["title", "name"]
            .iter()
            .find_map(|key| list[*key].as_str())
            .unwrap_or_default();
        Some(Schema {
            id: id.to_owned(),
            title: title.to_owned(),
            columns: columns.iter().filter_map(Column::from_json).collect(),
        })
    }

    /// A column by its name, Slack key or id; a name is matched ignoring case.
    pub fn column(&self, wanted: &str) -> Result<&Column, String> {
        let wanted = wanted.trim();
        self.columns
            .iter()
            .find(|c| c.id == wanted || c.key == wanted)
            .or_else(|| {
                self.columns
                    .iter()
                    .find(|c| c.name.eq_ignore_ascii_case(wanted))
            })
            .ok_or_else(|| {
                let names: Vec<&str> = self.columns.iter().map(|c| c.name.as_str()).collect();
                format!(
                    "The List {} has no column {wanted:?}. Its columns are: {}.",
                    self.id,
                    names.join(", ")
                )
            })
    }

    pub fn describe(&self) -> Value {
        let columns: Vec<Value> = self.columns.iter().map(Column::describe).collect();
        json!({"list": self.id, "title": self.title, "columns": columns})
    }

    /// A row as the agent reads it: its id, its parent if it is a subtask, and its cells by
    /// column name, each in the shape a write takes.
    pub fn row(&self, item: &Value) -> Value {
        let mut cells = Map::new();
        for field in item["fields"].as_array().into_iter().flatten() {
            let found = field["column_id"]
                .as_str()
                .and_then(|id| self.columns.iter().find(|c| c.id == id))
                .or_else(|| {
                    let key = field["key"].as_str()?;
                    self.columns.iter().find(|c| c.key == key)
                });
            let Some(column) = found else {
                continue;
            };
            let value = read_cell(column, field);
            if !value.is_null() {
                cells.insert(column.name.clone(), value);
            }
        }
        let mut row = json!({"id": item["id"], "cells": cells});
        if let Some(parent) = item["parent_record_id"].as_str() {
            row["parent"] = json!(parent);
        }
        row
    }
}

/// The List id, from the bare id or any link Slack makes for it.
pub fn list_id(list: &str) -> Result<String, String> {
    file_id(list).map_err(|_| {
        format!(
            "`{}` is neither a Slack List link nor a List id (F…). Pass the link copied from \
             the List.",
            list.trim()
        )
    })
}

/// The Slack client, or the refusal a gateway without credentials gives.
fn client(slack: &Option<SlackClient>) -> Result<&SlackClient, String> {
    slack
        .as_ref()
        .ok_or_else(|| "This gateway has no Slack credentials, so it cannot reach Lists.".into())
}

/// The `list` argument, as a List id.
fn list_arg(arguments: &Value) -> Result<String, String> {
    list_id(arguments["list"].as_str().unwrap_or_default())
}

/// A page of rows with the List's columns. Slack answers the columns only on request, and in a
/// shape it barely documents; when they are not found there they are read through one row.
pub async fn page(slack: &SlackClient, query: ListItems) -> Result<(Schema, ItemsPage), String> {
    let id = query.list_id.clone();
    let page = slack
        .list_items(&ListItems {
            include_list: true,
            ..query
        })
        .await
        .map_err(|err| explain(&err, &id))?;
    if let Some(schema) = page.list.as_ref().and_then(|l| Schema::from_list(&id, l)) {
        return Ok((schema, page));
    }
    let Some(row) = page.items.first().and_then(|item| item["id"].as_str()) else {
        return Err(format!(
            "Slack did not say what columns the List {id} has. Nothing was changed."
        ));
    };
    let info = slack
        .list_item(&id, row)
        .await
        .map_err(|err| explain(&err, &id))?;
    let schema = Schema::from_list(&id, &info.list)
        .ok_or_else(|| format!("Slack did not say what columns the List {id} has."))?;
    Ok((schema, page))
}

/// The List's columns, for a write.
pub async fn schema(slack: &SlackClient, list: &str) -> Result<Schema, String> {
    let query = ListItems {
        list_id: list.to_owned(),
        limit: Some(1),
        ..ListItems::default()
    };
    page(slack, query).await.map(|(schema, _)| schema)
}

/// A cell's value in the shape a write takes: a string, a number, a boolean, or a list of them
/// for a cell holding several.
fn read_cell(column: &Column, field: &Value) -> Value {
    let one_or_many = |values: Vec<Value>| match values.len() {
        0 => Value::Null,
        1 if !column.multiple => values.into_iter().next().unwrap_or(Value::Null),
        _ => Value::Array(values),
    };
    let array = |key: &str| field[key].as_array().cloned().unwrap_or_default();
    match column.kind {
        Kind::Text => match field["text"].as_str() {
            Some(text) => json!(text),
            None => match plain(&field["rich_text"]) {
                text if text.is_empty() => field["value"].clone(),
                text => json!(text),
            },
        },
        Kind::Checkbox => field["checkbox"].clone(),
        Kind::Select => one_or_many(
            array("select")
                .into_iter()
                .map(|id| {
                    let label = column.choices.iter().find(|c| c.id == id).map(|c| &c.label);
                    label.map_or(id, |label| json!(label))
                })
                .collect(),
        ),
        Kind::Link => one_or_many(
            array("link")
                .into_iter()
                .map(|link| match link {
                    Value::Object(link) => link
                        .get("original_url")
                        .cloned()
                        .unwrap_or(Value::Object(link)),
                    other => other,
                })
                .collect(),
        ),
        Kind::ReadOnly | Kind::Other => {
            let typed = field.get(&column.type_name).filter(|v| !v.is_null());
            typed.unwrap_or(&field["value"]).clone()
        }
        _ => {
            let key = column.kind.key().unwrap_or_default();
            match &field[key] {
                Value::Array(values) => one_or_many(values.clone()),
                Value::Null => field["value"].clone(),
                other => other.clone(),
            }
        }
    }
}

/// The text of a `rich_text` value.
fn plain(value: &Value) -> String {
    match value {
        Value::Object(obj) => match obj.get("type").and_then(Value::as_str) {
            Some("text") | Some("link") => obj
                .get("text")
                .or_else(|| obj.get("url"))
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned(),
            Some("user") => format!("<@{}>", obj["user_id"].as_str().unwrap_or_default()),
            _ => obj.get("elements").map(plain).unwrap_or_default(),
        },
        Value::Array(items) => items.iter().map(plain).collect(),
        _ => String::new(),
    }
}

/// Text as the one `rich_text` block Slack takes for a text cell or a description.
pub fn rich_text(text: &str) -> Value {
    json!([{
        "type": "rich_text",
        "elements": [{
            "type": "rich_text_section",
            "elements": [{"type": "text", "text": text}],
        }],
    }])
}

/// The value the agent gave a column, as the typed cell Slack takes, without its `column_id`.
pub fn write_cell(column: &Column, value: &Value) -> Result<Map<String, Value>, String> {
    let name = &column.name;
    let Some(key) = column.kind.key() else {
        return Err(format!(
            "The column {name:?} ({}) cannot be written by these tools.",
            column.type_name
        ));
    };
    let empty = match value {
        Value::Null => true,
        Value::String(s) => s.trim().is_empty() && column.kind != Kind::Text,
        Value::Array(items) => items.is_empty(),
        _ => false,
    };
    if empty {
        return Err(format!(
            "No value was given for {name:?}. Slack offers no way to empty a cell through \
             these tools; leave it out instead."
        ));
    }
    let values: Vec<&Value> = match value {
        Value::Array(items) => items.iter().collect(),
        other => vec![other],
    };
    if values.len() > 1 && !column.multiple {
        return Err(format!(
            "{name:?} holds one value, but {} were given.",
            values.len()
        ));
    }
    let strings = || -> Result<Vec<String>, String> {
        values
            .iter()
            .map(|v| match v {
                Value::String(s) => Ok(s.trim().to_owned()),
                Value::Number(n) => Ok(n.to_string()),
                other => Err(format!("{name:?} takes text, not {other}.")),
            })
            .collect()
    };
    let cell = match column.kind {
        Kind::Text => {
            let text = strings()?.join("\n");
            rich_text(&text)
        }
        Kind::Checkbox => match value {
            Value::Bool(checked) => json!(checked),
            Value::String(s) if s.eq_ignore_ascii_case("true") => json!(true),
            Value::String(s) if s.eq_ignore_ascii_case("false") => json!(false),
            other => {
                return Err(format!(
                    "{name:?} is a checkbox: pass true or false, not {other}."
                ));
            }
        },
        Kind::Number | Kind::Rating => {
            let numbers: Result<Vec<Value>, String> = values
                .iter()
                .map(|v| match v {
                    Value::Number(_) => Ok((*v).clone()),
                    Value::String(s) => s
                        .trim()
                        .parse::<f64>()
                        .ok()
                        .and_then(serde_json::Number::from_f64)
                        .map(Value::Number)
                        .ok_or_else(|| format!("{name:?} takes a number, not {s:?}.")),
                    other => Err(format!("{name:?} takes a number, not {other}.")),
                })
                .collect();
            Value::Array(numbers?)
        }
        Kind::Select => {
            let ids: Result<Vec<String>, String> = strings()?
                .iter()
                .map(|wanted| {
                    column
                        .choices
                        .iter()
                        .find(|c| c.id == *wanted)
                        .or_else(|| {
                            column
                                .choices
                                .iter()
                                .find(|c| c.label.eq_ignore_ascii_case(wanted))
                        })
                        .map(|c| c.id.clone())
                        .ok_or_else(|| {
                            let labels: Vec<&str> =
                                column.choices.iter().map(|c| c.label.as_str()).collect();
                            format!(
                                "{name:?} has no option {wanted:?}. Its options are: {}.",
                                labels.join(", ")
                            )
                        })
                })
                .collect();
            json!(ids?)
        }
        Kind::Date => {
            let dates = strings()?;
            if let Some(bad) = dates.iter().find(|d| !is_date(d)) {
                return Err(format!("{name:?} takes a date as YYYY-MM-DD, not {bad:?}."));
            }
            json!(dates)
        }
        Kind::User => json!(ids(name, strings()?, "<@", &['U', 'W'], "U0123ABCD")?),
        Kind::Channel => json!(ids(name, strings()?, "<#", &['C', 'G'], "C0123ABCD")?),
        Kind::Attachment => {
            let files: Result<Vec<String>, String> =
                strings()?.iter().map(|f| file_id(f)).collect();
            json!(files?)
        }
        Kind::Email | Kind::Phone | Kind::Message => json!(strings()?),
        Kind::Link => {
            let links: Result<Vec<Value>, String> = values
                .iter()
                .map(|v| {
                    let (url, label) = match v {
                        Value::String(url) => (url.trim(), None),
                        Value::Object(link) => (
                            link.get("url").and_then(Value::as_str).unwrap_or_default(),
                            link.get("name").and_then(Value::as_str),
                        ),
                        other => return Err(format!("{name:?} takes a link, not {other}.")),
                    };
                    if url::Url::parse(url).is_err() {
                        return Err(format!("{name:?} takes a link, not {url:?}."));
                    }
                    let mut link = json!({"original_url": url, "display_as_url": label.is_none()});
                    if let Some(label) = label {
                        link["display_name"] = json!(label);
                    }
                    Ok(link)
                })
                .collect();
            Value::Array(links?)
        }
        Kind::ReadOnly | Kind::Other => unreachable!("they have no key"),
    };
    let mut written = Map::new();
    written.insert(key.to_owned(), cell);
    Ok(written)
}

/// User or channel ids, taken bare or as Slack's `<@U…>` and `<#C…|name>` markup.
fn ids(
    column: &str,
    given: Vec<String>,
    markup: &str,
    prefixes: &[char],
    example: &str,
) -> Result<Vec<String>, String> {
    given
        .into_iter()
        .map(|id| {
            let bare = id
                .strip_prefix(markup)
                .and_then(|inner| inner.strip_suffix('>'))
                .map_or(id.as_str(), |inner| {
                    inner.split('|').next().unwrap_or(inner)
                });
            let shaped = bare.len() > 1
                && bare.starts_with(prefixes)
                && bare
                    .chars()
                    .all(|c| c.is_ascii_uppercase() || c.is_ascii_digit());
            match shaped {
                true => Ok(bare.to_owned()),
                false => Err(format!(
                    "{column:?} takes Slack ids such as {example}, not {id:?}."
                )),
            }
        })
        .collect()
}

fn is_date(date: &str) -> bool {
    let mut parts = date.splitn(3, '-');
    let (Some(y), Some(m), Some(d)) = (parts.next(), parts.next(), parts.next()) else {
        return false;
    };
    let shaped = y.len() == 4 && m.len() == 2 && d.len() == 2;
    let (Ok(y), Ok(m), Ok(d)) = (y.parse::<i32>(), m.parse::<u8>(), d.parse::<u8>()) else {
        return false;
    };
    let Ok(month) = time::Month::try_from(m) else {
        return false;
    };
    shaped && time::Date::from_calendar_date(y, month, d).is_ok()
}

/// A row as the `{column: value}` object the agent gave, as Slack's typed cells.
pub fn write_row(schema: &Schema, row: &Value) -> Result<Vec<Value>, String> {
    let Some(cells) = row.as_object() else {
        return Err(format!(
            "Each row is an object of column names to values, not {row}."
        ));
    };
    cells
        .iter()
        .map(|(column, value)| {
            let column = schema.column(column)?;
            let mut cell = write_cell(column, value)?;
            cell.insert("column_id".into(), json!(column.id));
            Ok(Value::Object(cell))
        })
        .collect()
}

/// Slack's refusals in words an agent can act on.
pub fn explain(error: &SlackError, list: &str) -> String {
    let SlackError::Api { error, detail, .. } = error else {
        return format!("Could not reach Slack for the List {list}: {error}");
    };
    let detail = detail
        .as_deref()
        .map(|detail| format!(" ({detail})"))
        .unwrap_or_default();
    match error.as_str() {
        "list_not_found" | "access_denied" | "permission_denied" | "restricted_action"
        | "no_permission" => format!(
            "I cannot use the List {list}: it does not exist, or it is not shared with this bot \
             (Slack said {error}). Ask its owner to share it with the bot, with edit access to \
             change it, then try again."
        ),
        "missing_scope" | "not_allowed_token_type" => format!(
            "This bot's Slack app lacks the Lists permissions (Slack said {error}). Its admin \
             has to reinstall the app from the current manifest. Nothing was changed."
        ),
        "lists_disabled_user_team" | "unknown_method" | "paid_only" | "feature_not_enabled" => {
            format!(
                "Lists are not available in this Slack workspace (Slack said {error}); they \
                 need a paid plan."
            )
        }
        "record_not_found" | "record_deleted" | "invalid_row_id" | "row_not_found" => {
            format!("The List {list} has no such row (Slack said {error}{detail}).")
        }
        other => format!("Slack refused the List {list}: {other}{detail}."),
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used)]

    use super::*;

    fn schema() -> Schema {
        let list = json!({
            "title": "Tasks",
            "list_metadata": {"schema": [
                {"id": "Col1", "key": "task", "name": "Task", "type": "text", "is_primary_column": true},
                {"id": "Col2", "key": "status", "name": "Status", "type": "select",
                 "options": {"format": "single_select", "choices": [
                    {"value": "opt_todo", "label": "To do", "color": "red"},
                    {"value": "opt_done", "label": "Done", "color": "green"}]}},
                {"id": "Col3", "key": "people", "name": "People", "type": "user",
                 "options": {"format": "multi_entity"}},
                {"id": "Col4", "key": "due", "name": "Due", "type": "date"},
                {"id": "Col5", "key": "done", "name": "Done?", "type": "checkbox"},
                {"id": "Col6", "key": "by", "name": "Created by", "type": "created_by"},
            ]},
        });
        Schema::from_list("F1", &list).unwrap()
    }

    #[test]
    fn a_column_is_found_by_name_whatever_its_case_or_by_its_key_or_id() {
        let schema = schema();
        assert_eq!(schema.column("status").unwrap().id, "Col2");
        assert_eq!(schema.column("STATUS").unwrap().id, "Col2");
        assert_eq!(schema.column("Col3").unwrap().name, "People");
        assert_eq!(schema.column("due").unwrap().id, "Col4");
        let missing = schema.column("Owner").unwrap_err();
        assert!(missing.contains("Task, Status, People"), "{missing}");
    }

    #[test]
    fn values_are_written_as_the_typed_cells_slack_takes() {
        let schema = schema();
        let cells = write_row(
            &schema,
            &json!({
                "Task": "Ship it",
                "Status": "done",
                "People": ["<@U0ALICE01>", "U0BOB0001"],
                "Due": "2026-10-31",
                "Done?": true,
            }),
        )
        .unwrap();
        let by_column = |id: &str| cells.iter().find(|c| c["column_id"] == id).unwrap().clone();
        assert_eq!(by_column("Col1")["rich_text"], rich_text("Ship it"));
        assert_eq!(by_column("Col2")["select"], json!(["opt_done"]));
        assert_eq!(by_column("Col3")["user"], json!(["U0ALICE01", "U0BOB0001"]));
        assert_eq!(by_column("Col4")["date"], json!(["2026-10-31"]));
        assert_eq!(by_column("Col5")["checkbox"], json!(true));
    }

    #[test]
    fn a_value_the_column_cannot_take_is_refused_with_what_it_can() {
        let schema = schema();
        let refuse = |row: Value| write_row(&schema, &row).unwrap_err();
        assert!(refuse(json!({"Status": "Blocked"})).contains("To do, Done"));
        assert!(refuse(json!({"Status": ["To do", "Done"]})).contains("holds one value"));
        assert!(refuse(json!({"Due": "31/10/2026"})).contains("YYYY-MM-DD"));
        assert!(refuse(json!({"Due": "2026-02-30"})).contains("YYYY-MM-DD"));
        assert!(refuse(json!({"People": "alice"})).contains("U0123ABCD"));
        assert!(refuse(json!({"Created by": "U0ALICE01"})).contains("cannot be written"));
        assert!(refuse(json!({"Task": null})).contains("leave it out"));
        assert!(refuse(json!(["Ship it"])).contains("object of column names"));
    }

    #[test]
    fn a_row_reads_back_in_the_shape_a_write_takes() {
        let schema = schema();
        let item = json!({
            "id": "Rec1",
            "parent_record_id": "Rec0",
            "fields": [
                {"column_id": "Col1", "key": "task", "value": "Ship it", "text": "Ship it",
                 "rich_text": rich_text("Ship it")},
                {"column_id": "Col2", "key": "status", "value": "opt_done", "select": ["opt_done"]},
                {"column_id": "Col3", "key": "people", "value": "U0ALICE01", "user": ["U0ALICE01"]},
                {"column_id": "Col5", "key": "done", "value": true, "checkbox": true},
                {"column_id": "Col6", "key": "by", "value": "U0ALICE01", "created_by": "U0ALICE01"},
            ],
        });
        let row = schema.row(&item);
        assert_eq!(row["id"], "Rec1");
        assert_eq!(row["parent"], "Rec0");
        let cells = &row["cells"];
        assert_eq!(cells["Task"], "Ship it");
        assert_eq!(cells["Status"], "Done");
        assert_eq!(cells["People"], json!(["U0ALICE01"]));
        assert_eq!(cells["Done?"], true);
        assert_eq!(cells["Created by"], "U0ALICE01");
        // What was read can be written back unchanged.
        let mut writable = cells.as_object().unwrap().clone();
        writable.remove("Created by");
        assert!(write_row(&schema, &Value::Object(writable)).is_ok());
    }

    #[test]
    fn a_list_is_found_in_its_link() {
        assert_eq!(list_id("F0LIST0001").unwrap(), "F0LIST0001");
        assert_eq!(
            list_id("https://acme.slack.com/lists/T0ACME001/F0ABCDEF12").unwrap(),
            "F0ABCDEF12"
        );
        assert!(list_id("my tasks").unwrap_err().contains("List link"));
    }

    #[test]
    fn a_list_the_bot_cannot_see_is_told_apart_from_a_missing_permission() {
        let api = |error: &str| SlackError::Api {
            method: "slackLists.items.list".into(),
            error: error.into(),
            detail: None,
        };
        assert!(explain(&api("list_not_found"), "F1").contains("shared with this bot"));
        assert!(explain(&api("missing_scope"), "F1").contains("reinstall"));
        assert!(explain(&api("lists_disabled_user_team"), "F1").contains("paid plan"));
    }
}
