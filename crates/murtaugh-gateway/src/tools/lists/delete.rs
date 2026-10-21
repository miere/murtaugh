//! Deletes rows of a List.

use async_trait::async_trait;
use murtaugh_slack::SlackClient;
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};

use super::{client, explain, list_arg};
use crate::tools::Tool;

pub const NAME: &str = "delete_list_items";
const MAX_ROWS: usize = 100;

pub struct DeleteListItems {
    slack: Option<SlackClient>,
}

impl DeleteListItems {
    pub fn new(slack: Option<SlackClient>) -> Self {
        Self { slack }
    }
}

#[async_trait]
impl Tool for DeleteListItems {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: format!(
                "Delete rows of a Slack List, with their subtasks, up to {MAX_ROWS} per call. \
                 Slack may not let them be restored: say which rows will go and have the \
                 person confirm before calling this."
            ),
            input_schema: Some(json!({
                "type": "object",
                "required": ["list", "rows"],
                "properties": {
                    "list": {
                        "type": "string",
                        "description": "The List's id (F…), or its link as copied from Slack.",
                    },
                    "rows": {
                        "type": "array",
                        "minItems": 1,
                        "maxItems": MAX_ROWS,
                        "items": {"type": "string"},
                        "description": "The ids (Rec…) of the rows to delete.",
                    },
                },
            })),
            kind: ToolKind::Edit,
        }
    }

    async fn invoke(&self, arguments: Value) -> Result<String, String> {
        let slack = client(&self.slack)?;
        let list = list_arg(&arguments)?;
        let rows: Vec<String> = arguments["rows"]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(Value::as_str)
            .map(|row| row.trim().to_owned())
            .filter(|row| !row.is_empty())
            .collect();
        if rows.is_empty() || rows.len() > MAX_ROWS {
            return Err(format!("Pass 1 to {MAX_ROWS} row ids in `rows`."));
        }
        slack
            .delete_list_items(&list, &rows)
            .await
            .map_err(|err| explain(&err, &list))?;
        Ok(format!(
            "Deleted {} rows from the List {list}: {}.",
            rows.len(),
            rows.join(", ")
        ))
    }
}
