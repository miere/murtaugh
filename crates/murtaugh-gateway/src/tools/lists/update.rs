//! Changes cells of a List's rows.

use async_trait::async_trait;
use murtaugh_slack::SlackClient;
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};

use super::{client, explain, list_arg, schema, write_cell};
use crate::tools::Tool;

pub const NAME: &str = "update_list_items";
/// As many cells as Slack takes in one call.
const MAX_CELLS: usize = 100;

pub struct UpdateListItems {
    slack: Option<SlackClient>,
}

impl UpdateListItems {
    pub fn new(slack: Option<SlackClient>) -> Self {
        Self { slack }
    }
}

#[async_trait]
impl Tool for UpdateListItems {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: format!(
                "Change cells of a Slack List's rows, up to {MAX_CELLS} cells per call, all \
                 or none. Each change names a row id (as read_list answers it), a column name \
                 and the new value, in the shape add_list_items takes: text, a number, an \
                 option label, a date as YYYY-MM-DD, user ids (U…), true or false, a URL."
            ),
            input_schema: Some(json!({
                "type": "object",
                "required": ["list", "changes"],
                "properties": {
                    "list": {
                        "type": "string",
                        "description": "The List's id (F…), or its link as copied from Slack.",
                    },
                    "changes": {
                        "type": "array",
                        "minItems": 1,
                        "maxItems": MAX_CELLS,
                        "items": {
                            "type": "object",
                            "required": ["row", "column", "value"],
                            "properties": {
                                "row": {"type": "string", "description": "The row id (Rec…)."},
                                "column": {"type": "string", "description": "The column's name."},
                                "value": {"description": "The cell's new value."},
                            },
                        },
                    },
                },
            })),
            kind: ToolKind::Edit,
        }
    }

    async fn invoke(&self, arguments: Value) -> Result<String, String> {
        let slack = client(&self.slack)?;
        let list = list_arg(&arguments)?;
        let changes = arguments["changes"].as_array().cloned().unwrap_or_default();
        if changes.is_empty() || changes.len() > MAX_CELLS {
            return Err(format!("Pass 1 to {MAX_CELLS} cells in `changes`."));
        }
        let schema = schema(slack, &list).await?;
        let cells: Vec<Value> = changes
            .iter()
            .enumerate()
            .map(|(n, change)| {
                let row = change["row"].as_str().map(str::trim).unwrap_or_default();
                if row.is_empty() {
                    return Err(format!("Change {}: name the row by its id.", n + 1));
                }
                let column = change["column"].as_str().unwrap_or_default();
                let column = schema
                    .column(column)
                    .map_err(|err| format!("Change {}: {err}", n + 1))?;
                let mut cell = write_cell(column, &change["value"])
                    .map_err(|err| format!("Change {}: {err}", n + 1))?;
                cell.insert("row_id".into(), json!(row));
                cell.insert("column_id".into(), json!(column.id));
                Ok(Value::Object(cell))
            })
            .collect::<Result<_, String>>()?;
        slack
            .update_list_cells(&list, &cells)
            .await
            .map_err(|err| explain(&err, &list))?;
        Ok(format!("Changed {} cells of the List {list}.", cells.len()))
    }
}
