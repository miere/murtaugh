//! Slack Lists (`slackLists.*`). Slack documents the shape of a List's schema and of its cells
//! loosely, and adds column types over time, so both stay JSON here and the caller reads what it
//! knows from them.

use serde::Deserialize;
use serde_json::{Map, Value, json};

use crate::error::SlackError;
use crate::web::{Body, SlackClient, Token};

/// A new List. `schema` is Slack's column definitions as they are sent; empty leaves Slack's
/// default of one text column.
#[derive(Debug, Clone, Default)]
pub struct CreateList {
    pub name: String,
    pub description_blocks: Option<Value>,
    pub schema: Vec<Value>,
    pub todo_mode: bool,
}

#[derive(Debug, Clone, Deserialize)]
pub struct CreatedList {
    pub list_id: String,
    /// The columns as Slack made them, ids included; only answered when a schema was sent.
    #[serde(default)]
    pub list_metadata: Option<Value>,
}

/// One page of `slackLists.items.list`.
#[derive(Debug, Clone, Default)]
pub struct ListItems {
    pub list_id: String,
    pub limit: Option<u32>,
    pub cursor: Option<String>,
    pub archived: bool,
    /// Also answer the List itself: its title and columns.
    pub include_list: bool,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ItemsPage {
    #[serde(default)]
    pub items: Vec<Value>,
    #[serde(default)]
    pub list: Option<Value>,
    #[serde(default)]
    response_metadata: Option<Cursor>,
}

#[derive(Debug, Clone, Deserialize)]
struct Cursor {
    #[serde(default)]
    next_cursor: String,
}

impl ItemsPage {
    /// Where the next page starts; `None` on the last.
    pub fn next_cursor(&self) -> Option<&str> {
        self.response_metadata
            .as_ref()
            .map(|meta| meta.next_cursor.as_str())
            .filter(|cursor| !cursor.is_empty())
    }
}

#[derive(Debug, Clone, Deserialize)]
pub struct ItemInfo {
    pub list: Value,
    pub record: Value,
    #[serde(default)]
    pub subtasks: Vec<Value>,
}

/// What `slackLists.access.set` grants.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Access {
    Read,
    Write,
    Owner,
}

impl Access {
    fn as_str(self) -> &'static str {
        match self {
            Access::Read => "read",
            Access::Write => "write",
            Access::Owner => "owner",
        }
    }
}

impl SlackClient {
    pub async fn create_list(&self, list: &CreateList) -> Result<CreatedList, SlackError> {
        let mut body = Map::new();
        body.insert("name".into(), json!(list.name));
        if let Some(blocks) = &list.description_blocks {
            body.insert("description_blocks".into(), blocks.clone());
        }
        if !list.schema.is_empty() {
            body.insert("schema".into(), Value::Array(list.schema.clone()));
        }
        if list.todo_mode {
            body.insert("todo_mode".into(), json!(true));
        }
        self.call(
            "slackLists.create",
            Token::Bot,
            Body::Json(Value::Object(body)),
        )
        .await
    }

    /// Grants `level` on a List to these users; only users can be made its owner.
    pub async fn set_list_access(
        &self,
        list_id: &str,
        level: Access,
        user_ids: &[String],
    ) -> Result<(), SlackError> {
        let body =
            json!({"list_id": list_id, "access_level": level.as_str(), "user_ids": user_ids});
        self.call::<Value>("slackLists.access.set", Token::Bot, Body::Json(body))
            .await
            .map(drop)
    }

    pub async fn list_items(&self, query: &ListItems) -> Result<ItemsPage, SlackError> {
        let mut body = Map::new();
        body.insert("list_id".into(), json!(query.list_id));
        if let Some(limit) = query.limit {
            body.insert("limit".into(), json!(limit));
        }
        if let Some(cursor) = query.cursor.as_ref().filter(|c| !c.is_empty()) {
            body.insert("cursor".into(), json!(cursor));
        }
        if query.archived {
            body.insert("archived".into(), json!(true));
        }
        if query.include_list {
            body.insert("include_list".into(), json!(true));
        }
        self.call(
            "slackLists.items.list",
            Token::Bot,
            Body::Json(Value::Object(body)),
        )
        .await
    }

    /// One row, its subtasks and the List's columns.
    pub async fn list_item(&self, list_id: &str, row_id: &str) -> Result<ItemInfo, SlackError> {
        let body = json!({"list_id": list_id, "id": row_id});
        self.call("slackLists.items.info", Token::Bot, Body::Json(body))
            .await
    }

    /// Adds a row, or a subtask of `parent`. `fields` are Slack's typed cells, each naming its
    /// `column_id`. Returns the new row.
    pub async fn create_list_item(
        &self,
        list_id: &str,
        parent: Option<&str>,
        fields: &[Value],
    ) -> Result<Value, SlackError> {
        #[derive(Deserialize)]
        struct Created {
            item: Value,
        }
        let mut body = Map::new();
        body.insert("list_id".into(), json!(list_id));
        if let Some(parent) = parent {
            body.insert("parent_item_id".into(), json!(parent));
        }
        if !fields.is_empty() {
            body.insert("initial_fields".into(), json!(fields));
        }
        let created: Created = self
            .call(
                "slackLists.items.create",
                Token::Bot,
                Body::Json(Value::Object(body)),
            )
            .await?;
        Ok(created.item)
    }

    /// Writes cells, each naming its `row_id` and `column_id`. Slack takes 1 to 100 per call.
    pub async fn update_list_cells(
        &self,
        list_id: &str,
        cells: &[Value],
    ) -> Result<(), SlackError> {
        let body = json!({"list_id": list_id, "cells": cells});
        self.call::<Value>("slackLists.items.update", Token::Bot, Body::Json(body))
            .await
            .map(drop)
    }

    pub async fn delete_list_items(&self, list_id: &str, ids: &[String]) -> Result<(), SlackError> {
        let body = json!({"list_id": list_id, "ids": ids});
        self.call::<Value>(
            "slackLists.items.deleteMultiple",
            Token::Bot,
            Body::Json(body),
        )
        .await
        .map(drop)
    }
}
