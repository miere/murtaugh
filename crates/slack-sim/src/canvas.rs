//! Canvases, as far as `canvases.edit`, `files.info` and the canvas download go.
//!
//! A canvas is a list of blocks, each with a `temp:C:…` id, served as the HTML dialect Slack's
//! canvases use. What was learnt from the real thing and is kept here on purpose: one change per
//! `canvases.edit` call; a deleted table cell empties rather than goes; a divider and a table carry
//! no id, so nothing can delete them; `replace` on a block keeps its id; `replace` with no section
//! replaces the whole canvas except a leading `#` title, which it keeps. The Markdown it accepts is the subset tests write, not all of Slack's.

use serde_json::{Map, Value, json};

use crate::api::{ApiError, arg, err, refusal};
use crate::state::State;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum ListStyle {
    Bullet,
    Ordered,
    Check,
}

impl ListStyle {
    fn section_style(self) -> u8 {
        match self {
            ListStyle::Bullet => 5,
            ListStyle::Ordered => 6,
            ListStyle::Check => 7,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Block {
    Heading {
        id: String,
        level: u8,
        html: String,
    },
    Paragraph {
        id: String,
        html: String,
        code: bool,
    },
    Quote {
        id: String,
        html: String,
    },
    Item {
        id: String,
        style: ListStyle,
        depth: usize,
        checked: bool,
        html: String,
    },
    /// Rows of cells; a deleted cell is `None`.
    Table(Vec<Vec<Option<(String, String)>>>),
    Rule,
}

impl Block {
    fn id(&self) -> Option<&str> {
        match self {
            Block::Heading { id, .. }
            | Block::Paragraph { id, .. }
            | Block::Quote { id, .. }
            | Block::Item { id, .. } => Some(id),
            Block::Table(_) | Block::Rule => None,
        }
    }

    fn set_id(&mut self, new: String) {
        match self {
            Block::Heading { id, .. }
            | Block::Paragraph { id, .. }
            | Block::Quote { id, .. }
            | Block::Item { id, .. } => *id = new,
            Block::Table(_) | Block::Rule => {}
        }
    }
}

pub(crate) struct Canvas {
    pub id: String,
    pub title: String,
    pub channel: String,
    pub blocks: Vec<Block>,
}

impl Canvas {
    /// The canvas as Slack serves its download.
    pub fn html(&self) -> String {
        let mut out = String::from("<div class=\"quip-canvas-content\">");
        let mut i = 0;
        while i < self.blocks.len() {
            match &self.blocks[i] {
                Block::Heading { id, level, html } => {
                    out.push_str(&format!("<h{level} id=\"{id}\">{html}</h{level}>"));
                }
                Block::Paragraph { id, html, code } => {
                    let class = if *code { "prettyprint line" } else { "line" };
                    out.push_str(&format!("<p id=\"{id}\" class=\"{class}\">{html}</p>"));
                }
                Block::Quote { id, html } => {
                    out.push_str(&format!(
                        "<blockquote><p id=\"{id}\" class=\"line\">{html}</p></blockquote>"
                    ));
                }
                Block::Item { style, .. } => {
                    let style = *style;
                    let start = i;
                    while matches!(self.blocks.get(i), Some(Block::Item { style: s, .. }) if *s == style)
                    {
                        i += 1;
                    }
                    out.push_str(&list_html(style, &self.blocks[start..i]));
                    continue;
                }
                Block::Table(rows) => {
                    out.push_str("<table>");
                    for row in rows {
                        out.push_str("<tr>");
                        for cell in row {
                            match cell {
                                Some((id, html)) => out.push_str(&format!(
                                    "<td><p id=\"{id}\" class=\"line\">{html}</p></td>"
                                )),
                                None => out.push_str("<td></td>"),
                            }
                        }
                        out.push_str("</tr>");
                    }
                    out.push_str("</table>");
                }
                Block::Rule => out.push_str("<hr style='width:100%'>"),
            }
            i += 1;
        }
        out.push_str("</div>");
        out
    }

    fn position(&self, section: &str) -> Option<usize> {
        self.blocks.iter().position(|b| b.id() == Some(section))
    }

    fn cell_mut(&mut self, section: &str) -> Option<&mut Option<(String, String)>> {
        self.blocks.iter_mut().find_map(|block| match block {
            Block::Table(rows) => rows
                .iter_mut()
                .flatten()
                .find(|cell| cell.as_ref().is_some_and(|(id, _)| id == section)),
            _ => None,
        })
    }
}

fn list_html(style: ListStyle, items: &[Block]) -> String {
    let mut out = format!(
        "<div data-section-style='{}' class=\"list-numbering-restart-at\" style=\"--indent0: 0\">",
        style.section_style()
    );
    let mut depth = 0;
    let mut first = true;
    for item in items {
        let Block::Item {
            id,
            depth: d,
            checked,
            html,
            ..
        } = item
        else {
            continue;
        };
        if first {
            out.push_str(&format!("<ul id='{id}-list'>"));
        }
        while depth < *d {
            out.push_str("<ul>");
            depth += 1;
        }
        while depth > *d {
            out.push_str("</ul>");
            depth -= 1;
        }
        let mut attrs = String::new();
        if first {
            attrs.push_str(" value='1'");
        }
        if *checked {
            attrs.push_str(" class='checked'");
        }
        out.push_str(&format!(
            "<li id='{id}'{attrs}><span id=\"{id}\">{html}</span><br/></li>"
        ));
        first = false;
    }
    while depth > 0 {
        out.push_str("</ul>");
        depth -= 1;
    }
    out.push_str("</ul></div>");
    out
}

pub(crate) fn new_id(st: &mut State) -> String {
    let seq = st.next_seq();
    // Slack's ids look random; a mixed sequence keeps them unique without making their tails
    // line up, which is what tells a test apart from a lucky one.
    let mut z = seq.wrapping_add(0x9e37_79b9_7f4a_7c15);
    z = (z ^ (z >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
    z = (z ^ (z >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
    z ^= z >> 31;
    format!("temp:C:GFF{seq:09x}{z:016x}")
}

/// Turns the Markdown a test writes into blocks. Headings, paragraphs, the three kinds of list,
/// quotes, fenced code, tables and dividers; inline bold, italics, strikethrough, code, links and
/// `![](@U…)` mentions.
pub(crate) fn parse_markdown(st: &mut State, markdown: &str) -> Vec<Block> {
    let mut blocks = Vec::new();
    let mut paragraph: Vec<String> = Vec::new();
    let mut lines = markdown.lines().peekable();
    let flush = |st: &mut State, paragraph: &mut Vec<String>, blocks: &mut Vec<Block>| {
        if !paragraph.is_empty() {
            blocks.push(Block::Paragraph {
                id: new_id(st),
                html: inline(&paragraph.join(" ")),
                code: false,
            });
            paragraph.clear();
        }
    };
    while let Some(line) = lines.next() {
        let trimmed = line.trim_start();
        // Slack nests a list item only under four spaces; two leave it flat.
        let depth = (line.len() - trimmed.len()) / 4;
        if trimmed.is_empty() {
            flush(st, &mut paragraph, &mut blocks);
        } else if trimmed.starts_with("```") {
            flush(st, &mut paragraph, &mut blocks);
            let mut code = Vec::new();
            for line in lines.by_ref() {
                if line.trim_start().starts_with("```") {
                    break;
                }
                code.push(escape(line));
            }
            blocks.push(Block::Paragraph {
                id: new_id(st),
                html: code.join("<br>"),
                code: true,
            });
        } else if let Some((level, text)) = heading(trimmed) {
            flush(st, &mut paragraph, &mut blocks);
            blocks.push(Block::Heading {
                id: new_id(st),
                level,
                html: inline(text),
            });
        } else if trimmed == "---" || trimmed == "***" {
            flush(st, &mut paragraph, &mut blocks);
            blocks.push(Block::Rule);
        } else if let Some(text) = trimmed.strip_prefix("> ") {
            flush(st, &mut paragraph, &mut blocks);
            blocks.push(Block::Quote {
                id: new_id(st),
                html: inline(text),
            });
        } else if let Some((style, checked, text)) = list_item(trimmed) {
            flush(st, &mut paragraph, &mut blocks);
            blocks.push(Block::Item {
                id: new_id(st),
                style,
                depth,
                checked,
                html: inline(text),
            });
        } else if trimmed.starts_with('|') {
            flush(st, &mut paragraph, &mut blocks);
            let mut rows = vec![trimmed.to_owned()];
            while let Some(next) = lines.peek().filter(|l| l.trim_start().starts_with('|')) {
                rows.push(next.trim_start().to_owned());
                lines.next();
            }
            let rows = rows
                .iter()
                .filter(|row| !row.chars().all(|c| matches!(c, '|' | '-' | ':' | ' ')))
                .map(|row| {
                    row.trim()
                        .trim_matches('|')
                        .split('|')
                        .map(|cell| Some((new_id(st), inline(cell.trim()))))
                        .collect()
                })
                .collect();
            blocks.push(Block::Table(rows));
        } else {
            paragraph.push(trimmed.to_owned());
        }
    }
    flush(st, &mut paragraph, &mut blocks);
    blocks
}

fn heading(line: &str) -> Option<(u8, &str)> {
    let hashes = line.bytes().take_while(|b| *b == b'#').count();
    if hashes == 0 {
        return None;
    }
    let text = line[hashes..].strip_prefix(' ')?;
    // Slack has three heading levels and folds deeper ones into the third.
    Some((hashes.min(3) as u8, text))
}

fn list_item(line: &str) -> Option<(ListStyle, bool, &str)> {
    if let Some(rest) = line.strip_prefix("- ").or_else(|| line.strip_prefix("* ")) {
        if let Some(text) = rest.strip_prefix("[ ] ") {
            return Some((ListStyle::Check, false, text));
        }
        if let Some(text) = rest
            .strip_prefix("[x] ")
            .or_else(|| rest.strip_prefix("[X] "))
        {
            return Some((ListStyle::Check, true, text));
        }
        return Some((ListStyle::Bullet, false, rest));
    }
    let digits = line.bytes().take_while(u8::is_ascii_digit).count();
    let text = line[digits..].strip_prefix(". ")?;
    (digits > 0).then_some((ListStyle::Ordered, false, text))
}

fn escape(text: &str) -> String {
    text.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
}

/// Inline Markdown into the canvas's tags, its links as `<lnk>` the way Slack writes them.
fn inline(text: &str) -> String {
    let mut out = String::new();
    let mut rest = text;
    while !rest.is_empty() {
        let pairs: [(&str, &str, &str); 7] = [
            ("![](", ")", "mention"),
            ("**", "**", "b"),
            ("~~", "~~", "del"),
            ("`", "`", "code"),
            ("*", "*", "i"),
            ("_", "_", "i"),
            ("[", ")", "lnk"),
        ];
        let found = pairs.iter().find_map(|(open, close, tag)| {
            let inner = rest.strip_prefix(open)?;
            let end = inner.find(close)?;
            Some((*tag, &inner[..end], &inner[end + close.len()..]))
        });
        match found {
            Some(("mention", inner, after)) => {
                out.push_str(&format!(
                    "<control id=\"mention\" data-remapped=\"true\"><a>{}</a></control>",
                    escape(inner)
                ));
                rest = after;
            }
            Some(("lnk", inner, after)) => match inner.split_once("](") {
                Some((label, href)) => {
                    out.push_str(&format!(
                        "<lnk href=\"{}\">{}</lnk>",
                        escape(href),
                        escape(label)
                    ));
                    rest = after;
                }
                None => {
                    out.push('[');
                    rest = &rest[1..];
                }
            },
            Some((tag, inner, after)) if !inner.is_empty() => {
                let inner = if tag == "code" {
                    escape(inner)
                } else {
                    self::inline(inner)
                };
                out.push_str(&format!("<{tag}>{inner}</{tag}>"));
                rest = after;
            }
            _ => {
                let mut chars = rest.chars();
                if let Some(c) = chars.next() {
                    out.push_str(&escape(&c.to_string()));
                }
                rest = chars.as_str();
            }
        }
    }
    out
}

pub(crate) fn edit(st: &mut State, params: &Map<String, Value>) -> Result<Value, ApiError> {
    let canvas_id = arg(params, "canvas_id")?.unwrap_or_default();
    let changes = params
        .get("changes")
        .and_then(Value::as_array)
        .ok_or_else(|| err("invalid_arguments", "missing required field: changes"))?;
    let [change] = changes.as_slice() else {
        return Err(err(
            "invalid_arguments",
            "no more than 1 items allowed [json-pointer:/changes]",
        ));
    };
    if !visible(st, &canvas_id) {
        return Err(refusal(
            "canvas_not_found",
            format!("no canvas `{canvas_id}`"),
        ));
    }
    let operation = change
        .get("operation")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_owned();
    let section = change
        .get("section_id")
        .and_then(Value::as_str)
        .map(str::to_owned);
    let markdown = match change.get("document_content") {
        None => None,
        Some(content) => {
            if content.get("type").and_then(Value::as_str) != Some("markdown") {
                return Err(err(
                    "invalid_arguments",
                    "document_content.type must be markdown",
                ));
            }
            Some(
                content
                    .get("markdown")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_owned(),
            )
        }
    };
    let needs_content = operation != "delete";
    let content = match (&markdown, needs_content) {
        (Some(markdown), true) if !markdown.trim().is_empty() => Some(markdown.clone()),
        (_, true) => {
            return Err(editing_failed(format!(
                "Invalid operation argument for: {operation}"
            )));
        }
        (Some(_), false) => {
            return Err(err("invalid_arguments", "delete takes no document_content"));
        }
        (None, false) => None,
    };
    let new_blocks = content.map(|markdown| parse_markdown(st, &markdown));
    let Some(canvas) = st.canvases.get_mut(&canvas_id) else {
        return Err(refusal(
            "canvas_not_found",
            format!("no canvas `{canvas_id}`"),
        ));
    };
    let missing = |section: &str| editing_failed(format!("Invalid section ID: '{section}'"));
    match (operation.as_str(), section) {
        ("insert_at_start", None) => {
            let blocks = new_blocks.unwrap_or_default();
            canvas.blocks.splice(0..0, blocks);
        }
        ("insert_at_end", None) => canvas.blocks.extend(new_blocks.unwrap_or_default()),
        ("insert_before" | "insert_after", Some(section)) => {
            let at = canvas.position(&section).ok_or_else(|| missing(&section))?;
            let at = if operation == "insert_after" {
                at + 1
            } else {
                at
            };
            canvas.blocks.splice(at..at, new_blocks.unwrap_or_default());
        }
        ("replace", None) => {
            // A leading `#` heading is the canvas's title, and a whole replace leaves it.
            let title = match canvas.blocks.first() {
                Some(title @ Block::Heading { level: 1, .. }) => Some(title.clone()),
                _ => None,
            };
            canvas.blocks = title
                .into_iter()
                .chain(new_blocks.unwrap_or_default())
                .collect();
        }
        ("replace", Some(section)) => {
            let at = canvas.position(&section).ok_or_else(|| missing(&section))?;
            let mut blocks = new_blocks.unwrap_or_default();
            if let Some(first) = blocks.iter_mut().find(|b| b.id().is_some()) {
                first.set_id(section);
            }
            canvas.blocks.splice(at..=at, blocks);
        }
        ("delete", Some(section)) => match canvas.position(&section) {
            Some(at) => {
                canvas.blocks.remove(at);
            }
            None => {
                let cell = canvas.cell_mut(&section).ok_or_else(|| missing(&section))?;
                *cell = None;
            }
        },
        ("insert_at_start" | "insert_at_end" | "insert_before" | "insert_after" | "delete", _) => {
            return Err(editing_failed(format!(
                "Invalid operation argument for: {operation}"
            )));
        }
        _ => {
            return Err(err(
                "invalid_arguments",
                format!("unknown operation `{operation}`"),
            ));
        }
    }
    Ok(json!({}))
}

/// The bot sees a canvas only in a channel it belongs to; any other is as good as missing.
pub(crate) fn visible(st: &State, canvas_id: &str) -> bool {
    st.canvases
        .get(canvas_id)
        .and_then(|canvas| st.channels.get(&canvas.channel))
        .is_some_and(|channel| channel.bot_member)
}

/// What Slack answers an edit it understood but could not apply, such as one naming a section
/// that is gone. Not a mistake in the request, so not a violation.
fn editing_failed(detail: String) -> ApiError {
    refusal("canvas_editing_failed", detail)
}

pub(crate) fn file_json(st: &State, canvas: &Canvas) -> Value {
    json!({
        "id": canvas.id,
        "name": canvas.title,
        "title": canvas.title,
        "mimetype": "application/vnd.slack-docs",
        "filetype": "quip",
        "pretty_type": "Canvas",
        "size": canvas.html().len(),
        "url_private_download": format!(
            "{}files-pri/{}-{}/download/canvas",
            st.base,
            crate::TEAM_ID,
            canvas.id
        ),
    })
}
