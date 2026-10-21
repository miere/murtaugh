//! Reads one row of a List, with its subtasks.

use async_trait::async_trait;
use murtaugh_slack::SlackClient;
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};

use super::{Schema, client, explain, list_arg};
use crate::tools::Tool;

pub const NAME: &str = "read_list_item";

pub struct ReadListItem {
    slack: Option<SlackClient>,
}

impl ReadListItem {
    pub fn new(slack: Option<SlackClient>) -> Self {
        Self { slack }
    }
}

#[async_trait]
impl Tool for ReadListItem {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: "Read one row of a Slack List, with its subtasks and the List's \
                 columns. Cells come by column name, in the shape update_list_items takes."
                .to_owned(),
            input_schema: Some(json!({
                "type": "object",
                "required": ["list", "row"],
                "properties": {
                    "list": {
                        "type": "string",
                        "description": "The List's id (F…), or its link as copied from Slack.",
                    },
                    "row": {
                        "type": "string",
                        "description": "The row's id (Rec…), as read_list answers it.",
                    },
                },
            })),
            kind: ToolKind::Read,
        }
    }

    async fn invoke(&self, arguments: Value) -> Result<String, String> {
        let slack = client(&self.slack)?;
        let list = list_arg(&arguments)?;
        let row = arguments["row"].as_str().unwrap_or_default().trim();
        if row.is_empty() {
            return Err("Say which row to read: its id, as read_list answers it.".into());
        }
        let info = slack
            .list_item(&list, row)
            .await
            .map_err(|err| explain(&err, &list))?;
        let schema = Schema::from_list(&list, &info.list)
            .ok_or_else(|| format!("Slack did not say what columns the List {list} has."))?;
        let mut answer = schema.describe();
        answer["row"] = schema.row(&info.record);
        let subtasks: Vec<Value> = info.subtasks.iter().map(|s| schema.row(s)).collect();
        answer["subtasks"] = json!(subtasks);
        Ok(answer.to_string())
    }
}
