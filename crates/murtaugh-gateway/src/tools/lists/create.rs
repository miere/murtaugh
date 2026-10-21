//! Creates a List for the person who asked for it. Slack makes whoever calls `slackLists.create`
//! the List's owner, which here is the bot, so it is handed over at once: the person who started
//! the conversation becomes its owner and the bot keeps write access to fill it in.

use std::collections::HashSet;

use async_trait::async_trait;
use murtaugh_slack::{Access, CreateList as NewList, ListItems, SlackClient};
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};

use super::{Schema, client, explain, rich_text};
use crate::tools::{Context, Tool};

pub const NAME: &str = "create_list";
const TYPES: &[&str] = &[
    "text",
    "number",
    "select",
    "date",
    "user",
    "channel",
    "checkbox",
    "email",
    "phone",
    "link",
    "attachment",
];
const COLORS: &[&str] = &[
    "blue", "green", "yellow", "red", "purple", "orange", "cyan", "pink", "indigo", "brown", "gray",
];
/// Slack's limit on the options of one select column.
const MAX_CHOICES: usize = 100;

pub struct CreateList {
    slack: Option<SlackClient>,
}

impl CreateList {
    pub fn new(slack: Option<SlackClient>) -> Self {
        Self { slack }
    }
}

#[async_trait]
impl Tool for CreateList {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: format!(
                "Create a Slack List for the person in this conversation, who becomes its \
                 owner; you keep edit access to fill it in with add_list_items. Columns cannot \
                 be added or changed afterwards, so give every column it needs now. The first \
                 text column is the List's main column; one named Name is added if there is no \
                 text column. Column types: {}. A select column needs its options; set \
                 `multiple` for a select, user or channel column that holds several. \
                 `todo_mode` adds Completed, Assignee and Due date columns.",
                TYPES.join(", ")
            ),
            input_schema: Some(json!({
                "type": "object",
                "required": ["name"],
                "properties": {
                    "name": {"type": "string", "description": "The List's title."},
                    "description": {"type": "string", "description": "What the List is for."},
                    "columns": {
                        "type": "array",
                        "items": {
                            "type": "object",
                            "required": ["name", "type"],
                            "properties": {
                                "name": {"type": "string"},
                                "type": {"type": "string", "enum": TYPES},
                                "choices": {
                                    "type": "array",
                                    "items": {"type": "string"},
                                    "description": "A select column's options, in order.",
                                },
                                "multiple": {"type": "boolean"},
                            },
                        },
                    },
                    "todo_mode": {
                        "type": "boolean",
                        "description": "Track the rows as tasks.",
                    },
                },
            })),
            kind: ToolKind::Edit,
        }
    }

    async fn invoke(&self, _arguments: Value) -> Result<String, String> {
        Err("create_list needs the calling session, which this call did not carry.".into())
    }

    async fn invoke_in(&self, context: &Context, arguments: Value) -> Result<String, String> {
        let slack = client(&self.slack)?;
        let Some(owner) = &context.user else {
            return Err(
                "This session is not tied to anyone's Slack conversation, so there is nobody \
                 to give the List to. Nothing was created."
                    .into(),
            );
        };
        let name = arguments["name"]
            .as_str()
            .map(str::trim)
            .unwrap_or_default();
        if name.is_empty() {
            return Err("Give the List a name.".into());
        }
        let columns = arguments["columns"].as_array().cloned().unwrap_or_default();
        let schema = columns_schema(&columns)?;
        let description = arguments["description"]
            .as_str()
            .map(str::trim)
            .filter(|d| !d.is_empty());
        let created = slack
            .create_list(&NewList {
                name: name.to_owned(),
                description_blocks: description.map(rich_text),
                schema,
                todo_mode: arguments["todo_mode"].as_bool().unwrap_or(false),
            })
            .await
            .map_err(|err| explain(&err, name))?;
        let list = created.list_id;
        let owner = owner.to_string();
        if let Err(err) = slack
            .set_list_access(&list, Access::Owner, std::slice::from_ref(&owner))
            .await
        {
            return Err(format!(
                "The List {list} was created, but could not be handed to <@{owner}>, so only \
                 this bot can see it: {}",
                explain(&err, &list)
            ));
        }
        let mut answer = json!({"list": list, "title": name, "owner": owner});
        if let Ok(me) = slack.auth_test().await
            && !me.url.is_empty()
        {
            answer["link"] = json!(format!(
                "{}/lists/{}/{list}",
                me.url.trim_end_matches('/'),
                me.team_id
            ));
        }
        // Handing a List over must leave the bot able to fill it in; Slack does not say whether
        // it does, so this is checked rather than assumed.
        let probe = ListItems {
            list_id: list.clone(),
            limit: Some(1),
            include_list: true,
            ..ListItems::default()
        };
        match slack.list_items(&probe).await {
            Ok(page) => {
                let schema = page.list.as_ref().and_then(|l| Schema::from_list(&list, l));
                if let Some(schema) = schema {
                    answer["columns"] = schema.describe()["columns"].clone();
                }
            }
            Err(err) => {
                answer["warning"] = json!(format!(
                    "<@{owner}> owns the List now, but this bot lost access to it ({}). Ask \
                     them to share it with the bot with edit access before adding rows.",
                    explain(&err, &list)
                ));
            }
        }
        Ok(answer.to_string())
    }
}

/// The agent's columns as Slack's schema. Slack needs exactly one primary column, a text one.
fn columns_schema(columns: &[Value]) -> Result<Vec<Value>, String> {
    if columns.is_empty() {
        return Ok(Vec::new());
    }
    let mut keys = HashSet::new();
    let mut schema = Vec::new();
    let has_text = columns.iter().any(|c| c["type"] == "text");
    if !has_text {
        schema.push(json!({
            "key": unique(&mut keys, "name"), "name": "Name", "type": "text",
            "is_primary_column": true,
        }));
    }
    let mut primary = !has_text;
    for (n, column) in columns.iter().enumerate() {
        let name = column["name"].as_str().map(str::trim).unwrap_or_default();
        if name.is_empty() {
            return Err(format!("Column {} has no name.", n + 1));
        }
        let kind = column["type"].as_str().unwrap_or_default();
        if !TYPES.contains(&kind) {
            return Err(format!(
                "Column {name:?} has the type {kind:?}; the types are {}.",
                TYPES.join(", ")
            ));
        }
        let multiple = column["multiple"].as_bool().unwrap_or(false);
        let mut entry = json!({"key": unique(&mut keys, &slug(name)), "name": name, "type": kind});
        if kind == "text" && !primary {
            entry["is_primary_column"] = json!(true);
            primary = true;
        }
        match kind {
            "select" => {
                let labels: Vec<&str> = column["choices"]
                    .as_array()
                    .into_iter()
                    .flatten()
                    .filter_map(Value::as_str)
                    .map(str::trim)
                    .filter(|label| !label.is_empty())
                    .collect();
                if labels.is_empty() || labels.len() > MAX_CHOICES {
                    return Err(format!(
                        "The select column {name:?} needs 1 to {MAX_CHOICES} choices."
                    ));
                }
                let mut values = HashSet::new();
                let choices: Vec<Value> = labels
                    .iter()
                    .enumerate()
                    .map(|(i, label)| {
                        json!({
                            "value": unique(&mut values, &slug(label)),
                            "label": label,
                            "color": COLORS[i % COLORS.len()],
                        })
                    })
                    .collect();
                let format = if multiple {
                    "multi_select"
                } else {
                    "single_select"
                };
                entry["options"] = json!({"format": format, "choices": choices});
            }
            "user" | "channel" => {
                let format = if multiple {
                    "multi_entity"
                } else {
                    "single_entity"
                };
                entry["options"] = json!({"format": format});
            }
            _ => {}
        }
        schema.push(entry);
    }
    Ok(schema)
}

/// A key made of a name: lowercase ASCII letters, digits and underscores.
fn slug(name: &str) -> String {
    let mut slug = String::new();
    for c in name.chars() {
        if c.is_ascii_alphanumeric() {
            slug.push(c.to_ascii_lowercase());
        } else if !slug.ends_with('_') {
            slug.push('_');
        }
    }
    let slug = slug.trim_matches('_');
    if slug.is_empty() {
        "column".to_owned()
    } else {
        slug.to_owned()
    }
}

/// `wanted`, or `wanted_2`, `wanted_3`… if it was taken.
fn unique(taken: &mut HashSet<String>, wanted: &str) -> String {
    let mut key = wanted.to_owned();
    let mut n = 1;
    while !taken.insert(key.clone()) {
        n += 1;
        key = format!("{wanted}_{n}");
    }
    key
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used)]

    use super::*;

    #[test]
    fn the_first_text_column_is_the_primary_one() {
        let schema = columns_schema(&[
            json!({"name": "Due", "type": "date"}),
            json!({"name": "Task", "type": "text"}),
            json!({"name": "Notes", "type": "text"}),
        ])
        .unwrap();
        let primary: Vec<&Value> = schema
            .iter()
            .filter(|c| c["is_primary_column"] == true)
            .collect();
        assert_eq!(primary.len(), 1);
        assert_eq!(primary[0]["name"], "Task");
    }

    #[test]
    fn a_list_without_text_gets_a_name_column() {
        let schema = columns_schema(&[json!({"name": "Due", "type": "date"})]).unwrap();
        assert_eq!(schema[0]["name"], "Name");
        assert_eq!(schema[0]["is_primary_column"], true);
        assert_eq!(schema.len(), 2);
    }

    #[test]
    fn select_options_and_keys_stay_unique() {
        let schema = columns_schema(&[
            json!({"name": "Status", "type": "select", "choices": ["To do", "To-do", "Done"],
                   "multiple": true}),
            json!({"name": "status", "type": "text"}),
        ])
        .unwrap();
        let status = &schema[0];
        assert_eq!(status["options"]["format"], "multi_select");
        let values: Vec<&str> = status["options"]["choices"]
            .as_array()
            .unwrap()
            .iter()
            .map(|c| c["value"].as_str().unwrap())
            .collect();
        assert_eq!(values, ["to_do", "to_do_2", "done"]);
        assert_eq!(schema[1]["key"], "status_2");
    }

    #[test]
    fn a_column_slack_cannot_make_is_refused() {
        let refused = columns_schema(&[json!({"name": "Votes", "type": "vote"})]).unwrap_err();
        assert!(refused.contains("the types are"), "{refused}");
        let refused = columns_schema(&[json!({"name": "S", "type": "select"})]).unwrap_err();
        assert!(refused.contains("needs 1 to"), "{refused}");
    }

    #[tokio::test]
    async fn a_list_nobody_asked_for_is_not_made() {
        let refused = CreateList::new(None)
            .invoke_in(&Context::default(), json!({"name": "Tasks"}))
            .await
            .unwrap_err();
        assert!(refused.contains("no Slack credentials") || refused.contains("nobody"));
    }
}
