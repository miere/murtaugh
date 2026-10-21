//! Reads a List: its columns and a page of its rows.

use async_trait::async_trait;
use murtaugh_slack::{ListItems, SlackClient};
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};

use super::{client, list_arg, page};
use crate::tools::Tool;

pub const NAME: &str = "read_list";
const DEFAULT_ROWS: u64 = 50;
const MAX_ROWS: u64 = 200;

pub struct ReadList {
    slack: Option<SlackClient>,
}

impl ReadList {
    pub fn new(slack: Option<SlackClient>) -> Self {
        Self { slack }
    }
}

#[async_trait]
impl Tool for ReadList {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: "Read a Slack List from its id or link: its title, its columns (with \
                 each select column's options) and a page of its rows. Each row has an id, \
                 which the other List tools take, and its cells by column name, in the same \
                 shape add_list_items and update_list_items take them. When the answer has a \
                 next_cursor, more rows follow: pass it back as `cursor` to read them. The bot \
                 sees only Lists shared with it or with a channel it is in."
                .to_owned(),
            input_schema: Some(json!({
                "type": "object",
                "required": ["list"],
                "properties": {
                    "list": {
                        "type": "string",
                        "description": "The List's id (F…), or its link as copied from Slack.",
                    },
                    "limit": {
                        "type": "integer",
                        "minimum": 1,
                        "maximum": MAX_ROWS,
                        "description": format!("How many rows to read; {DEFAULT_ROWS} if left out."),
                    },
                    "cursor": {
                        "type": "string",
                        "description": "The next_cursor a previous read answered, to read on.",
                    },
                    "archived": {
                        "type": "boolean",
                        "description": "Read the archived rows instead of the current ones.",
                    },
                },
            })),
            kind: ToolKind::Read,
        }
    }

    async fn invoke(&self, arguments: Value) -> Result<String, String> {
        let slack = client(&self.slack)?;
        let list = list_arg(&arguments)?;
        let limit = arguments["limit"]
            .as_u64()
            .unwrap_or(DEFAULT_ROWS)
            .clamp(1, MAX_ROWS);
        let query = ListItems {
            list_id: list,
            limit: Some(limit as u32),
            cursor: arguments["cursor"].as_str().map(str::to_owned),
            archived: arguments["archived"].as_bool().unwrap_or(false),
            include_list: true,
        };
        let (schema, page) = page(slack, query).await?;
        let mut answer = schema.describe();
        let rows: Vec<Value> = page.items.iter().map(|item| schema.row(item)).collect();
        answer["rows"] = json!(rows);
        if let Some(cursor) = page.next_cursor() {
            answer["next_cursor"] = json!(cursor);
        }
        Ok(answer.to_string())
    }
}
