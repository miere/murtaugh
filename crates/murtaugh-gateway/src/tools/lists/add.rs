//! Adds rows, or subtasks of a row, to a List.

use async_trait::async_trait;
use murtaugh_slack::SlackClient;
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};

use super::{Schema, client, explain, list_arg, schema, write_row};
use crate::tools::Tool;

pub const NAME: &str = "add_list_items";
/// Slack takes one row per call, about fifty a minute.
const MAX_ROWS: usize = 50;

pub struct AddListItems {
    slack: Option<SlackClient>,
}

impl AddListItems {
    pub fn new(slack: Option<SlackClient>) -> Self {
        Self { slack }
    }
}

#[async_trait]
impl Tool for AddListItems {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: format!(
                "Add rows to a Slack List, up to {MAX_ROWS} per call. Each row is an object of \
                 column names to values: text as a string, a number, a select column's option \
                 label (a list of them if the column takes several), a date as YYYY-MM-DD, \
                 people as Slack user ids (U…), channels as ids (C…), a checkbox as true or \
                 false, a link as its URL. Columns left out stay empty. Read the List with \
                 read_list first to learn its columns and options. Pass `parent` to add the \
                 rows as subtasks of that row."
            ),
            input_schema: Some(json!({
                "type": "object",
                "required": ["list", "items"],
                "properties": {
                    "list": {
                        "type": "string",
                        "description": "The List's id (F…), or its link as copied from Slack.",
                    },
                    "items": {
                        "type": "array",
                        "minItems": 1,
                        "maxItems": MAX_ROWS,
                        "items": {"type": "object"},
                        "description": "The rows, each as {\"<column name>\": value}.",
                    },
                    "parent": {
                        "type": "string",
                        "description": "A row id (Rec…): the rows become its subtasks.",
                    },
                },
            })),
            kind: ToolKind::Edit,
        }
    }

    async fn invoke(&self, arguments: Value) -> Result<String, String> {
        let slack = client(&self.slack)?;
        let list = list_arg(&arguments)?;
        let rows = arguments["items"].as_array().cloned().unwrap_or_default();
        if rows.is_empty() || rows.len() > MAX_ROWS {
            return Err(format!("Pass 1 to {MAX_ROWS} rows in `items`."));
        }
        let parent = arguments["parent"]
            .as_str()
            .map(str::trim)
            .filter(|p| !p.is_empty());
        let schema: Schema = schema(slack, &list).await?;
        // Every row is checked before the first is written, so a mistake adds nothing.
        let written: Vec<Vec<Value>> = rows
            .iter()
            .enumerate()
            .map(|(n, row)| write_row(&schema, row).map_err(|err| format!("Row {}: {err}", n + 1)))
            .collect::<Result<_, _>>()?;
        let mut added = Vec::new();
        for fields in &written {
            match slack.create_list_item(&list, parent, fields).await {
                Ok(item) => added.push(schema.row(&item)),
                Err(err) if added.is_empty() => return Err(explain(&err, &list)),
                Err(err) => {
                    return Err(format!(
                        "Only the first {} of {} rows were added, then: {} The rows added: {}",
                        added.len(),
                        written.len(),
                        explain(&err, &list),
                        json!(added),
                    ));
                }
            }
        }
        Ok(json!({"list": list, "added": added}).to_string())
    }
}
