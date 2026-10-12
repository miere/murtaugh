//! Posts a table into the session's own Slack thread, as Slack's `data_table` block: sortable, and
//! paged so a long one does not fill the thread.
//!
//! The agent sends plain data and never writes a block. Slack's limits are checked here first, so
//! a table Slack would refuse is turned down in words the agent can act on — above all when the
//! trouble is how much data it sent, which it can split or summarise.
//!
//! Like every tool here it posts nowhere but the session's thread. When a reply is being written
//! there, the table is placed where the reply has got to.

use async_trait::async_trait;
use murtaugh_slack::{Block, SlackClient};
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};

use super::replying::Replying;
use super::{Context, Tool};
use crate::render;

pub const NAME: &str = "show_table";

const MAX_COLUMNS: usize = 20;
const MAX_ROWS: usize = 200;
const MAX_CHARS: usize = 20_000;
const MAX_PAGE: u64 = 100;
const PAGE: u64 = 20;
/// The rows a notification or a reading of the thread gets, in place of the block.
const FALLBACK_ROWS: usize = 20;
/// What an empty cell shows, since Slack takes none without text.
const EMPTY: &str = "-";

pub struct ShowTable {
    slack: Option<SlackClient>,
    replying: Replying,
}

impl ShowTable {
    pub fn new(slack: Option<SlackClient>, replying: Replying) -> Self {
        Self { slack, replying }
    }
}

#[async_trait]
impl Tool for ShowTable {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: format!(
                "Show a table in this conversation's Slack thread, which people can sort and page \
                 through. Use it for anything with rows and columns. It appears where your reply \
                 has got to, so introduce it first and carry on after. One table holds at most \
                 {MAX_ROWS} rows, {MAX_COLUMNS} columns and {MAX_CHARS} characters; for more, \
                 show several smaller tables or summarise."
            ),
            input_schema: Some(json!({
                "type": "object",
                "required": ["title", "columns", "rows"],
                "properties": {
                    "title": {"type": "string", "description": "What the table is, in a few words."},
                    "columns": {
                        "type": "array",
                        "items": {"type": "string"},
                        "description": "The column headers, left to right.",
                    },
                    "rows": {
                        "type": "array",
                        "items": {
                            "type": "array",
                            "items": {"type": ["string", "number", "null"]},
                        },
                        "description": "One array per row, with a cell for every column. Send a \
                            number as a number so its column sorts by value. A cell that is \
                            exactly a Markdown link, [label](https://…), is shown as a link. \
                            An empty cell is shown as a dash.",
                    },
                    "page_size": {
                        "type": "integer",
                        "description": "Rows shown per page, 1 to 100. Defaults to 20.",
                    },
                },
            })),
            kind: ToolKind::Edit,
        }
    }

    async fn invoke(&self, _arguments: Value) -> Result<String, String> {
        Err("This table has no conversation to be shown in.".into())
    }

    async fn invoke_in(&self, context: &Context, arguments: Value) -> Result<String, String> {
        let Some(slack) = &self.slack else {
            return Err("This gateway has no Slack credentials, so it cannot post.".into());
        };
        let Some(conversation) = &context.conversation else {
            return Err("This session has no Slack thread to post to.".into());
        };
        let table = Table::parse(&arguments).map_err(|why| format!("{why} Nothing was posted."))?;
        let rows = table.rows.len();
        self.replying
            .place(slack, conversation, table.fallback(), vec![table.block()])
            .await
            .map_err(|err| format!("Slack did not take the table: {err}. Nothing was posted."))?;
        Ok(format!(
            "Showed the table ({rows} row{}).",
            if rows == 1 { "" } else { "s" }
        ))
    }
}

#[derive(Debug, PartialEq)]
enum Cell {
    Text(String),
    Number { value: Value, text: String },
    Link { label: String, url: String },
}

impl Cell {
    fn parse(value: &Value) -> Option<Cell> {
        match value {
            Value::Null => Some(Cell::Text(EMPTY.to_owned())),
            Value::Number(number) => Some(Cell::Number {
                value: value.clone(),
                text: number.to_string(),
            }),
            Value::String(text) => {
                let text = text.trim();
                Some(match link(text) {
                    Some((label, url)) => Cell::Link {
                        label: label.to_owned(),
                        url: url.to_owned(),
                    },
                    None if text.is_empty() => Cell::Text(EMPTY.to_owned()),
                    None => Cell::Text(text.to_owned()),
                })
            }
            _ => None,
        }
    }

    fn text(&self) -> &str {
        match self {
            Cell::Text(text) | Cell::Number { text, .. } => text,
            Cell::Link { label, .. } => label,
        }
    }

    fn to_json(&self) -> Value {
        match self {
            Cell::Text(text) => json!({"type": "raw_text", "text": text}),
            Cell::Number { value, text } => {
                json!({"type": "raw_number", "value": value, "text": text})
            }
            Cell::Link { label, url } => json!({
                "type": "rich_text",
                "elements": [{
                    "type": "rich_text_section",
                    "elements": [{"type": "link", "text": label, "url": url}],
                }],
            }),
        }
    }
}

/// A cell that is nothing but one Markdown link.
fn link(text: &str) -> Option<(&str, &str)> {
    let (label, url) = text
        .strip_prefix('[')?
        .strip_suffix(')')?
        .split_once("](")?;
    let plain = !label.is_empty() && !label.contains(['[', ']']) && !url.contains(['(', ')', ' ']);
    (plain && (url.starts_with("https://") || url.starts_with("http://"))).then_some((label, url))
}

#[derive(Debug)]
struct Table {
    title: String,
    columns: Vec<String>,
    rows: Vec<Vec<Cell>>,
    page_size: u64,
}

impl Table {
    /// `Err` says what to change. When it is the amount of data, it says so and by how much.
    fn parse(arguments: &Value) -> Result<Table, String> {
        let title = arguments
            .get("title")
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|title| !title.is_empty())
            .ok_or("`title` is empty; say what the table is.")?;
        let columns: Vec<String> = arguments
            .get("columns")
            .and_then(Value::as_array)
            .ok_or("`columns` must be an array of column headers.")?
            .iter()
            .enumerate()
            .map(|(index, name)| {
                let name = name.as_str().map(str::trim).unwrap_or_default();
                match name.is_empty() {
                    true => Err(format!("`columns[{index}]` must be a header, in words.")),
                    false => Ok(name.to_owned()),
                }
            })
            .collect::<Result<_, _>>()?;
        if columns.is_empty() {
            return Err("`columns` is empty; a table needs at least one column.".into());
        }
        if columns.len() > MAX_COLUMNS {
            return Err(format!(
                "The table has too much data: {} columns, and Slack shows at most {MAX_COLUMNS} \
                 in one table. Send fewer columns, or split them across several tables.",
                columns.len()
            ));
        }
        let given = arguments
            .get("rows")
            .and_then(Value::as_array)
            .ok_or("`rows` must be an array of rows, each an array of cells.")?;
        if given.is_empty() {
            return Err("`rows` is empty; a table needs at least one row.".into());
        }
        if given.len() > MAX_ROWS {
            return Err(format!(
                "The table has too much data: {} rows, and Slack shows at most {MAX_ROWS} in one \
                 table. Send it as several smaller tables, or summarise it.",
                given.len()
            ));
        }
        let mut rows = Vec::with_capacity(given.len());
        for (r, row) in given.iter().enumerate() {
            let cells = row
                .as_array()
                .ok_or_else(|| format!("`rows[{r}]` must be an array of cells."))?;
            if cells.len() != columns.len() {
                return Err(format!(
                    "`rows[{r}]` has {} cells and the table has {} columns; every row needs a \
                     cell for every column.",
                    cells.len(),
                    columns.len()
                ));
            }
            let cells: Vec<Cell> = cells
                .iter()
                .enumerate()
                .map(|(c, cell)| {
                    Cell::parse(cell).ok_or_else(|| {
                        format!("`rows[{r}][{c}]` must be a string, a number or null.")
                    })
                })
                .collect::<Result<_, _>>()?;
            rows.push(cells);
        }
        let headers = columns.iter().map(|name| name.chars().count());
        let cells = rows
            .iter()
            .flatten()
            .map(|cell| cell.text().chars().count());
        let chars: usize = headers.chain(cells).sum();
        if chars > MAX_CHARS {
            return Err(format!(
                "The table has too much data: its cells hold {chars} characters, and Slack takes \
                 at most {MAX_CHARS} in one table. Send it as several smaller tables, or shorten \
                 the cells."
            ));
        }
        let page_size = match arguments.get("page_size") {
            None | Some(Value::Null) => PAGE,
            Some(size) => match size.as_u64() {
                Some(size @ 1..=MAX_PAGE) => size,
                _ => return Err(format!("`page_size` must be between 1 and {MAX_PAGE}.")),
            },
        };
        Ok(Table {
            title: title.to_owned(),
            columns,
            rows,
            page_size,
        })
    }

    fn block(&self) -> Block {
        let header: Vec<Value> = self
            .columns
            .iter()
            .map(|name| json!({"type": "raw_text", "text": name}))
            .collect();
        let rows = self
            .rows
            .iter()
            .map(|row| row.iter().map(Cell::to_json).collect());
        let rows: Vec<Vec<Value>> = std::iter::once(header).chain(rows).collect();
        Block::Raw(json!({
            "type": "data_table",
            "caption": self.title,
            "page_size": self.page_size,
            "rows": rows,
        }))
    }

    /// The table in words, for a notification and for anything that reads the thread back.
    fn fallback(&self) -> String {
        let mut out = format!(
            "*{}*\n{}",
            render::escape(&self.title),
            self.columns.join(" | ")
        );
        for row in self.rows.iter().take(FALLBACK_ROWS) {
            let cells: Vec<&str> = row.iter().map(Cell::text).collect();
            out.push('\n');
            out.push_str(&render::escape(&cells.join(" | ")));
        }
        if let Some(more) = self
            .rows
            .len()
            .checked_sub(FALLBACK_ROWS)
            .filter(|&n| n > 0)
        {
            out.push_str(&format!(
                "\n… and {more} more row{}",
                if more == 1 { "" } else { "s" }
            ));
        }
        out
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used)]

    use super::*;

    fn table(arguments: Value) -> Result<Table, String> {
        Table::parse(&arguments)
    }

    #[test]
    fn plain_data_becomes_a_data_table_with_a_header_row() {
        let table = table(json!({
            "title": "Deploys this week",
            "columns": ["Service", "Deploys", "Run"],
            "rows": [["api", 14, "[#812](https://ci.example/812)"], ["web", 9.5, null]],
        }))
        .unwrap();
        assert_eq!(
            table.block().to_json(),
            json!({
                "type": "data_table",
                "caption": "Deploys this week",
                "page_size": 20,
                "rows": [
                    [
                        {"type": "raw_text", "text": "Service"},
                        {"type": "raw_text", "text": "Deploys"},
                        {"type": "raw_text", "text": "Run"},
                    ],
                    [
                        {"type": "raw_text", "text": "api"},
                        {"type": "raw_number", "value": 14, "text": "14"},
                        {"type": "rich_text", "elements": [{
                            "type": "rich_text_section",
                            "elements": [
                                {"type": "link", "text": "#812", "url": "https://ci.example/812"},
                            ],
                        }]},
                    ],
                    [
                        {"type": "raw_text", "text": "web"},
                        {"type": "raw_number", "value": 9.5, "text": "9.5"},
                        {"type": "raw_text", "text": "-"},
                    ],
                ],
            })
        );
    }

    /// A notification cannot draw the block, and neither can the agent reading its thread back.
    #[test]
    fn the_fallback_says_the_table_in_words() {
        let rows: Vec<Value> = (1..=22).map(|n| json!([format!("<row {n}>"), n])).collect();
        let table = table(json!({"title": "A & B", "columns": ["Name", "N"], "rows": rows}));
        let text = table.unwrap().fallback();
        assert!(
            text.starts_with("*A &amp; B*\nName | N\n&lt;row 1&gt; | 1\n"),
            "{text}"
        );
        assert!(
            text.ends_with("&lt;row 20&gt; | 20\n… and 2 more rows"),
            "{text}"
        );
    }

    /// The refusal Miere asked for: when there is too much data it says so, and by how much, so
    /// the agent splits the table instead of guessing what was wrong with it.
    #[test]
    fn too_much_data_is_refused_as_too_much_data() {
        let row = |n: usize| json!([n]);
        let rows: Vec<Value> = (0..201).map(row).collect();
        let why = table(json!({"title": "T", "columns": ["N"], "rows": rows})).unwrap_err();
        assert!(why.contains("too much data: 201 rows"), "{why}");
        assert!(why.contains("several smaller tables"), "{why}");

        let columns: Vec<String> = (0..21).map(|n| format!("c{n}")).collect();
        let cells: Vec<usize> = (0..21).collect();
        let why = table(json!({"title": "T", "columns": columns, "rows": [cells]})).unwrap_err();
        assert!(why.contains("too much data: 21 columns"), "{why}");

        let wide = "x".repeat(1_000);
        let rows: Vec<Value> = (0..21).map(|_| json!([wide])).collect();
        let why = table(json!({"title": "T", "columns": ["N"], "rows": rows})).unwrap_err();
        assert!(
            why.contains("too much data: its cells hold 21001 characters"),
            "{why}"
        );
    }

    #[test]
    fn a_table_that_is_not_one_says_what_to_change() {
        for (arguments, why) in [
            (json!({"columns": ["A"], "rows": [[1]]}), "`title` is empty"),
            (json!({"title": "T", "rows": [[1]]}), "`columns` must be"),
            (
                json!({"title": "T", "columns": [], "rows": [[1]]}),
                "at least one column",
            ),
            (
                json!({"title": "T", "columns": ["A", ""], "rows": [[1, 2]]}),
                "`columns[1]`",
            ),
            (
                json!({"title": "T", "columns": ["A"], "rows": []}),
                "at least one row",
            ),
            (
                json!({"title": "T", "columns": ["A", "B"], "rows": [[1, 2], [3]]}),
                "`rows[1]` has 1",
            ),
            (
                json!({"title": "T", "columns": ["A"], "rows": [[{"a": 1}]]}),
                "`rows[0][0]`",
            ),
            (
                json!({"title": "T", "columns": ["A"], "rows": [[1]], "page_size": 0}),
                "`page_size`",
            ),
        ] {
            let refused = table(arguments.clone()).unwrap_err();
            assert!(refused.contains(why), "{arguments}: {refused}");
        }
    }

    #[test]
    fn only_a_cell_that_is_one_whole_link_becomes_a_link() {
        assert_eq!(
            link("[docs](https://example.com/a)"),
            Some(("docs", "https://example.com/a"))
        );
        for text in [
            "see [docs](https://example.com)",
            "[docs](javascript:x)",
            "[](https://x.y)",
        ] {
            assert_eq!(link(text), None, "{text}");
        }
    }
}
