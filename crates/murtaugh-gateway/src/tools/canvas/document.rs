//! A canvas's HTML read into blocks, and those blocks written out as Markdown.
//!
//! Slack serves a canvas in its own HTML dialect, so this reads that dialect rather than HTML at
//! large: links are `<lnk href>`, every list is a `<ul>` whose kind is the `data-section-style` on
//! the `<div>` around it (5 bullets, 6 numbers, 7 checklist), a checked item is `class='checked'`,
//! a code block is a `<p class="prettyprint">` with `<br>` between lines, and every block — down to
//! each list item and table cell — carries the `temp:C:…` id `canvases.edit` addresses it by.
//! Anything it does not know keeps its text and loses its markup, so nothing is silently dropped.

use std::collections::HashMap;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ListStyle {
    Bullet,
    Ordered,
    Check,
}

/// A table row: each cell's id, where it has one, beside its text.
pub type Row = Vec<(Option<String>, String)>;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Block {
    Heading {
        id: String,
        level: u8,
        text: String,
    },
    Paragraph {
        id: String,
        text: String,
    },
    Code {
        id: String,
        text: String,
    },
    Quote {
        id: String,
        text: String,
    },
    Item {
        id: String,
        style: ListStyle,
        depth: usize,
        checked: bool,
        text: String,
    },
    Table(Vec<Row>),
    Rule,
}

impl Block {
    /// The ids a delete can be aimed at. A table has none of its own, only its cells', and
    /// deleting those empties them; a divider has none at all.
    pub fn ids(&self) -> Vec<&str> {
        match self {
            Block::Heading { id, .. }
            | Block::Paragraph { id, .. }
            | Block::Code { id, .. }
            | Block::Quote { id, .. }
            | Block::Item { id, .. } => vec![id.as_str()],
            Block::Table(rows) => rows
                .iter()
                .flatten()
                .filter_map(|(id, _)| id.as_deref())
                .collect(),
            Block::Rule => Vec::new(),
        }
        .into_iter()
        .filter(|id| !id.is_empty())
        .collect()
    }

    fn heading_level(&self) -> Option<u8> {
        match self {
            Block::Heading { level, .. } => Some(*level),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Document {
    pub blocks: Vec<Block>,
}

/// Where a section sits: its heading's index, and the index just past its last block. A section
/// runs to the next heading at its own level or above, so it takes its subsections with it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Span {
    pub start: usize,
    pub end: usize,
}

impl Document {
    pub fn parse(html: &str) -> Document {
        Parser::default().run(html)
    }

    /// A short name for each heading: the tail of its id, as few characters as keep every heading
    /// in this canvas apart (never fewer than four). It is stable across edits that do not touch
    /// the heading, which an ordinal ("the third heading") would not be.
    pub fn anchors(&self) -> HashMap<String, String> {
        let ids: Vec<&str> = self
            .blocks
            .iter()
            .filter_map(|block| match block {
                Block::Heading { id, .. } => Some(id.as_str()),
                _ => None,
            })
            .collect();
        let tail = |id: &str, len: usize| -> String {
            let chars: Vec<char> = id.chars().collect();
            chars[chars.len().saturating_sub(len)..].iter().collect()
        };
        let longest = ids.iter().map(|id| id.chars().count()).max().unwrap_or(0);
        let mut len = MIN_ANCHOR.min(longest);
        while len < longest {
            let mut tails: Vec<String> = ids.iter().map(|id| tail(id, len)).collect();
            tails.sort();
            tails.dedup();
            if tails.len() == ids.len() {
                break;
            }
            len += 1;
        }
        ids.iter()
            .map(|id| ((*id).to_owned(), tail(id, len)))
            .collect()
    }

    /// The section a heading anchor names, if exactly one heading's id ends with it.
    pub fn section(&self, anchor: &str) -> Result<Span, Unresolved> {
        let anchor = anchor
            .trim()
            .trim_start_matches("{#")
            .trim_start_matches('#');
        let anchor = anchor.trim_end_matches('}');
        if anchor.is_empty() {
            return Err(Unresolved::Missing);
        }
        let matches: Vec<usize> = self
            .blocks
            .iter()
            .enumerate()
            .filter(|(_, block)| matches!(block, Block::Heading { id, .. } if id.ends_with(anchor)))
            .map(|(index, _)| index)
            .collect();
        let start = match matches.as_slice() {
            [one] => *one,
            [] => return Err(Unresolved::Missing),
            _ => return Err(Unresolved::Ambiguous),
        };
        let level = self.blocks[start].heading_level().unwrap_or(1);
        let end = self.blocks[start + 1..]
            .iter()
            .position(|block| block.heading_level().is_some_and(|l| l <= level))
            .map_or(self.blocks.len(), |offset| start + 1 + offset);
        Ok(Span { start, end })
    }

    /// The whole canvas as Markdown, each heading followed by its anchor.
    pub fn markdown(&self) -> String {
        self.markdown_of(Span {
            start: 0,
            end: self.blocks.len(),
        })
    }

    pub fn markdown_of(&self, span: Span) -> String {
        let anchors = self.anchors();
        let mut out = String::new();
        let mut previous: Option<&Block> = None;
        for block in &self.blocks[span.start..span.end.min(self.blocks.len())] {
            let same_list = matches!(
                (previous, block),
                (Some(Block::Item { style: a, .. }), Block::Item { style: b, .. }) if a == b
            );
            if previous.is_some() {
                out.push_str(if same_list { "\n" } else { "\n\n" });
            }
            write_block(&mut out, block, &anchors);
            previous = Some(block);
        }
        if !out.is_empty() {
            out.push('\n');
        }
        out
    }

    pub fn heading_text(&self, index: usize) -> Option<&str> {
        match self.blocks.get(index) {
            Some(Block::Heading { text, .. }) => Some(text),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Unresolved {
    Missing,
    Ambiguous,
}

const MIN_ANCHOR: usize = 4;

fn write_block(out: &mut String, block: &Block, anchors: &HashMap<String, String>) {
    match block {
        Block::Heading { id, level, text } => {
            out.push_str(&"#".repeat(usize::from(*level)));
            out.push(' ');
            out.push_str(text);
            if let Some(anchor) = anchors.get(id) {
                out.push_str(&format!(" {{#{anchor}}}"));
            }
        }
        Block::Paragraph { text, .. } => out.push_str(text),
        Block::Code { text, .. } => {
            out.push_str("```\n");
            out.push_str(text);
            out.push_str("\n```");
        }
        Block::Quote { text, .. } => {
            let quoted: Vec<String> = text.lines().map(|line| format!("> {line}")).collect();
            out.push_str(&quoted.join("\n"));
        }
        Block::Item {
            style,
            depth,
            checked,
            text,
            ..
        } => {
            // Slack nests a list item only under four spaces; two leave it flat.
            out.push_str(&"    ".repeat(*depth));
            out.push_str(match (style, checked) {
                (ListStyle::Bullet, _) => "- ",
                (ListStyle::Ordered, _) => "1. ",
                (ListStyle::Check, false) => "- [ ] ",
                (ListStyle::Check, true) => "- [x] ",
            });
            out.push_str(text);
        }
        Block::Table(rows) => {
            let mut lines = Vec::new();
            for (index, row) in rows.iter().enumerate() {
                let cells: Vec<String> = row
                    .iter()
                    .map(|(_, text)| text.replace('|', "\\|").replace('\n', " "))
                    .collect();
                lines.push(format!("| {} |", cells.join(" | ")));
                if index == 0 {
                    lines.push(format!("|{}|", vec!["---"; row.len().max(1)].join("|")));
                }
            }
            out.push_str(&lines.join("\n"));
        }
        Block::Rule => out.push_str("---"),
    }
}

/// Removes the `{#anchor}` a heading was read with, so Markdown an agent copied back from a read
/// does not write the anchor into the canvas as text.
pub fn strip_anchors(markdown: &str) -> String {
    markdown
        .lines()
        .map(|line| {
            let trimmed = line.trim_end();
            if !trimmed.trim_start().starts_with('#') || !trimmed.ends_with('}') {
                return line.to_owned();
            }
            match trimmed.rfind(" {#") {
                Some(at) if !trimmed[at + 3..trimmed.len() - 1].contains(char::is_whitespace) => {
                    trimmed[..at].to_owned()
                }
                _ => line.to_owned(),
            }
        })
        .collect::<Vec<_>>()
        .join("\n")
}

// ---- reading the HTML ----

#[derive(Debug)]
enum Token<'a> {
    Open {
        name: String,
        attrs: Vec<(String, String)>,
        closed: bool,
    },
    Close(String),
    Text(&'a str),
}

fn tokens(html: &str) -> Vec<Token<'_>> {
    let mut out = Vec::new();
    let mut rest = html;
    while !rest.is_empty() {
        match rest.find('<') {
            Some(0) => {
                let Some(end) = tag_end(rest) else {
                    out.push(Token::Text(rest));
                    break;
                };
                let inside = &rest[1..end];
                rest = &rest[end + 1..];
                if let Some(name) = inside.strip_prefix('/') {
                    out.push(Token::Close(name.trim().to_ascii_lowercase()));
                } else if !inside.starts_with('!') {
                    let closed = inside.trim_end().ends_with('/');
                    let inside = inside.trim_end().trim_end_matches('/');
                    let name_end = inside
                        .find(|c: char| c.is_whitespace())
                        .unwrap_or(inside.len());
                    out.push(Token::Open {
                        name: inside[..name_end].to_ascii_lowercase(),
                        attrs: attributes(&inside[name_end..]),
                        closed,
                    });
                }
            }
            Some(at) => {
                out.push(Token::Text(&rest[..at]));
                rest = &rest[at..];
            }
            None => {
                out.push(Token::Text(rest));
                break;
            }
        }
    }
    out
}

/// The `>` closing a tag, skipping any inside a quoted attribute value.
fn tag_end(tag: &str) -> Option<usize> {
    let mut quote: Option<char> = None;
    for (at, c) in tag.char_indices().skip(1) {
        match (quote, c) {
            (Some(q), c) if c == q => quote = None,
            (Some(_), _) => {}
            (None, '"' | '\'') => quote = Some(c),
            (None, '>') => return Some(at),
            (None, _) => {}
        }
    }
    None
}

fn attributes(raw: &str) -> Vec<(String, String)> {
    let mut out = Vec::new();
    let mut rest = raw.trim_start();
    while !rest.is_empty() {
        let name_end = rest
            .find(|c: char| c == '=' || c.is_whitespace())
            .unwrap_or(rest.len());
        let name = rest[..name_end].to_ascii_lowercase();
        rest = rest[name_end..].trim_start();
        let value = if let Some(after) = rest.strip_prefix('=') {
            let after = after.trim_start();
            match after.chars().next() {
                Some(q @ ('"' | '\'')) => {
                    let body = &after[1..];
                    let end = body.find(q).unwrap_or(body.len());
                    rest = body.get(end + 1..).unwrap_or("");
                    decode(&body[..end])
                }
                _ => {
                    let end = after.find(char::is_whitespace).unwrap_or(after.len());
                    rest = &after[end..];
                    decode(&after[..end])
                }
            }
        } else {
            String::new()
        };
        if !name.is_empty() {
            out.push((name, value));
        }
        rest = rest.trim_start();
    }
    out
}

fn decode(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut rest = text;
    while let Some(at) = rest.find('&') {
        out.push_str(&rest[..at]);
        rest = &rest[at..];
        let entity = rest
            .find(';')
            .filter(|end| *end <= 10)
            .and_then(|end| Some((entity(&rest[1..end])?, end)));
        match entity {
            Some((c, end)) => {
                out.push(c);
                rest = &rest[end + 1..];
            }
            None => {
                out.push('&');
                rest = &rest[1..];
            }
        }
    }
    out.push_str(rest);
    out
}

fn entity(name: &str) -> Option<char> {
    match name {
        "amp" => Some('&'),
        "lt" => Some('<'),
        "gt" => Some('>'),
        "quot" => Some('"'),
        "apos" => Some('\''),
        "nbsp" => Some(' '),
        _ => {
            let number = name.strip_prefix('#')?;
            let code = match number.strip_prefix(['x', 'X']) {
                Some(hex) => u32::from_str_radix(hex, 16).ok()?,
                None => number.parse().ok()?,
            };
            char::from_u32(code)
        }
    }
}

/// `@U…`, `@W…` or `#C…`: how a canvas writes out a person or a channel it links to.
fn is_mention(text: &str) -> bool {
    let Some(id) = text.strip_prefix('@').or_else(|| text.strip_prefix('#')) else {
        return false;
    };
    id.len() >= 2
        && id
            .bytes()
            .all(|byte| byte.is_ascii_uppercase() || byte.is_ascii_digit())
}

fn attr<'a>(attrs: &'a [(String, String)], name: &str) -> Option<&'a str> {
    attrs
        .iter()
        .find(|(key, _)| key == name)
        .map(|(_, value)| value.as_str())
}

/// The block whose text is being collected, and where it goes once it closes.
#[derive(Debug)]
enum Open {
    Heading { id: String, level: u8 },
    Paragraph { id: String, code: bool },
    Item { id: String, checked: bool },
    Cell { id: Option<String> },
}

/// An inline run being collected: where its opening Markdown starts in the block's text, how long
/// that opening is, and what closes it.
#[derive(Debug)]
struct Inline {
    start: usize,
    open_len: usize,
    close: String,
}

#[derive(Default)]
struct Parser {
    blocks: Vec<Block>,
    open: Option<Open>,
    text: String,
    /// Each inline tag still open, innermost last; `None` for one met outside any block.
    inline: Vec<Option<Inline>>,
    list_style: Option<ListStyle>,
    list_depth: usize,
    in_quote: bool,
    table: Option<Vec<Row>>,
}

impl Parser {
    fn run(mut self, html: &str) -> Document {
        for token in tokens(html) {
            match token {
                Token::Text(text) => self.text(text),
                Token::Open {
                    name,
                    attrs,
                    closed,
                } => self.open_tag(&name, &attrs, closed),
                Token::Close(name) => self.close_tag(&name),
            }
        }
        self.finish();
        Document {
            blocks: self.blocks,
        }
    }

    fn text(&mut self, raw: &str) {
        // Slack keeps indentation, in code above all, as non-breaking spaces.
        let text = decode(raw).replace('\u{a0}', " ");
        if self.open.is_some() {
            self.text.push_str(&text);
        } else if !text.trim().is_empty() {
            // Text outside any block Slack would only send in markup this does not know; keep it.
            self.blocks.push(Block::Paragraph {
                id: String::new(),
                text: text.trim().to_owned(),
            });
        }
    }

    fn open_tag(&mut self, name: &str, attrs: &[(String, String)], closed: bool) {
        let id = attr(attrs, "id").map(str::to_owned);
        match name {
            "h1" | "h2" | "h3" | "h4" | "h5" | "h6" => {
                self.finish();
                let level = name[1..].parse::<u8>().unwrap_or(3).min(3);
                self.open = Some(Open::Heading {
                    id: id.unwrap_or_default(),
                    level,
                });
            }
            "p" => {
                if matches!(self.open, Some(Open::Cell { .. })) {
                    if let (Some(Open::Cell { id: cell }), Some(id)) = (&mut self.open, id) {
                        cell.get_or_insert(id);
                    }
                    if !self.text.is_empty() {
                        self.text.push(' ');
                    }
                    return;
                }
                self.finish();
                let code = attr(attrs, "class").is_some_and(|class| class.contains("prettyprint"));
                self.open = Some(Open::Paragraph {
                    id: id.unwrap_or_default(),
                    code,
                });
            }
            "blockquote" => {
                self.finish();
                self.in_quote = true;
            }
            "div" => {
                if let Some(style) = attr(attrs, "data-section-style") {
                    self.finish();
                    self.list_style = Some(match style {
                        "6" => ListStyle::Ordered,
                        "7" => ListStyle::Check,
                        _ => ListStyle::Bullet,
                    });
                    self.list_depth = 0;
                }
            }
            "ul" | "ol" => {
                if self.list_style.is_none() {
                    self.list_style = Some(if name == "ol" {
                        ListStyle::Ordered
                    } else {
                        ListStyle::Bullet
                    });
                }
                self.list_depth += 1;
            }
            "li" => {
                self.finish();
                let checked = attr(attrs, "class").is_some_and(|class| class.contains("checked"));
                self.open = Some(Open::Item {
                    id: id.unwrap_or_default(),
                    checked,
                });
            }
            "table" => {
                self.finish();
                self.table = Some(Vec::new());
            }
            "tr" => {
                if let Some(table) = &mut self.table {
                    table.push(Vec::new());
                }
            }
            "td" | "th" => {
                self.open = Some(Open::Cell { id: None });
                self.text.clear();
            }
            "hr" => {
                self.finish();
                self.blocks.push(Block::Rule);
            }
            "br" => {
                if self.open.is_some() {
                    self.text.push('\n');
                }
            }
            "b" | "strong" => self.push_inline("**", "**"),
            "i" | "em" => self.push_inline("_", "_"),
            "del" | "s" | "strike" => self.push_inline("~~", "~~"),
            "code" => self.push_inline("`", "`"),
            "lnk" | "a" => {
                let href = attr(attrs, "href").unwrap_or_default();
                if href.is_empty() {
                    self.push_inline("", "");
                } else {
                    self.push_inline("[", &format!("]({href})"));
                }
            }
            _ => {
                // Unknown markup keeps its text; an empty element that points somewhere keeps that.
                if closed
                    && self.open.is_some()
                    && let Some(target) = attr(attrs, "href").or(attr(attrs, "src"))
                {
                    self.text.push_str(&format!("<{target}>"));
                }
            }
        }
        if closed
            && matches!(
                name,
                "b" | "strong" | "i" | "em" | "del" | "s" | "strike" | "code" | "lnk" | "a"
            )
        {
            self.close_tag(name);
        }
    }

    fn push_inline(&mut self, open: &str, close: &str) {
        if self.open.is_none() {
            self.inline.push(None);
            return;
        }
        self.inline.push(Some(Inline {
            start: self.text.len(),
            open_len: open.len(),
            close: close.to_owned(),
        }));
        self.text.push_str(open);
    }

    fn close_tag(&mut self, name: &str) {
        match name {
            "h1" | "h2" | "h3" | "h4" | "h5" | "h6" | "p" | "li" => {
                if !matches!(self.open, Some(Open::Cell { .. })) {
                    self.finish();
                }
            }
            "blockquote" => {
                self.finish();
                self.in_quote = false;
            }
            "ul" | "ol" => {
                self.finish();
                self.list_depth = self.list_depth.saturating_sub(1);
                if self.list_depth == 0 {
                    self.list_style = None;
                }
            }
            "td" | "th" => {
                if let Some(Open::Cell { id }) = self.open.take() {
                    let text = std::mem::take(&mut self.text);
                    self.inline.clear();
                    if let Some(row) = self.table.as_mut().and_then(|table| table.last_mut()) {
                        row.push((id, text.trim().to_owned()));
                    }
                }
            }
            "table" => {
                if let Some(rows) = self.table.take() {
                    self.blocks.push(Block::Table(rows));
                }
            }
            "b" | "strong" | "i" | "em" | "del" | "s" | "strike" | "code" | "lnk" | "a" => {
                if let Some(Some(run)) = self.inline.pop() {
                    // An empty run would write `****`, which reads as a rule, not as nothing.
                    if self.text.len() == run.start + run.open_len {
                        self.text.truncate(run.start);
                    } else if run.close.is_empty() && is_mention(&self.text[run.start..]) {
                        // A mention is an `<a>` with no link, and goes back the way Slack takes
                        // one in: `![](@U…)`, `![](#C…)`. Written as `<@U…>` it stays plain text.
                        let mention = format!("![]({})", &self.text[run.start..]);
                        self.text.truncate(run.start);
                        self.text.push_str(&mention);
                    } else {
                        self.text.push_str(&run.close);
                    }
                }
            }
            _ => {}
        }
    }

    /// Closes the block being collected, if any, into the document.
    fn finish(&mut self) {
        let Some(open) = self.open.take() else {
            return;
        };
        let raw = std::mem::take(&mut self.text);
        self.inline.clear();
        let text = raw.trim_matches(|c: char| c == ' ' || c == '\n').to_owned();
        let block = match open {
            Open::Heading { id, level } => Block::Heading { id, level, text },
            Open::Paragraph { id, code: true } => Block::Code { id, text },
            Open::Paragraph { id, .. } if self.in_quote => Block::Quote { id, text },
            Open::Paragraph { id, .. } => {
                // Slack keeps an empty line as an empty paragraph; it is spacing, not content.
                if text.is_empty() {
                    return;
                }
                Block::Paragraph { id, text }
            }
            Open::Item { id, checked } => Block::Item {
                id,
                style: self.list_style.unwrap_or(ListStyle::Bullet),
                depth: self.list_depth.saturating_sub(1),
                checked,
                text,
            },
            Open::Cell { id } => {
                if let Some(row) = self.table.as_mut().and_then(|table| table.last_mut()) {
                    row.push((id, text));
                }
                return;
            }
        };
        self.blocks.push(block);
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used)]

    use super::*;

    /// The playground canvas exactly as Slack served it on 2026-10-05, one of every block it has.
    const PLAYGROUND: &str = include_str!("fixtures/playground.html");

    #[test]
    fn every_kind_of_block_reads_as_markdown() {
        let markdown = Document::parse(PLAYGROUND).markdown();
        let expected = "\
# Throw Away Canvas {#3009}

## H2 Heading {#5777}

A plain paragraph.

# H1 Heading {#2b6f}

### H3 Heading {#9b22}

Formatting: **bold**, _italic single_, _italic underscore_, ~~strikethrough~~, `inline code`.

Bold italic: _**bold italic**_ end.

Links: [GFM link to Anthropic](https://www.anthropic.com) and a bare https://example.com URL.

- Unordered one
- Unordered two
    - Nested child
- Unordered three

1. Ordered one
1. Ordered two
    1. Nested ordered
1. Ordered three

- [ ] Unchecked task
- [x] Checked task

> A block quote line. A second block quote line.

```
fn main() {
    println!(\"fenced code block with a language\");
}
```

| Column A | Column B |
|---|---|
| a1 | b1 |
| a2 | b2 |

---

Final paragraph after a horizontal rule.
";
        assert_eq!(markdown, expected);
    }

    fn wrap(body: &str) -> String {
        format!("<div class=\"quip-canvas-content\">{body}</div>")
    }

    /// Two headings whose ids end alike get anchors long enough to tell them apart, and only as
    /// long as that takes.
    #[test]
    fn anchors_grow_only_as_long_as_they_must_to_stay_unique() {
        let doc = Document::parse(&wrap(
            "<h1 id=\"temp:C:GFF11abcd\">One</h1><h1 id=\"temp:C:GFF22abcd\">Two</h1>\
             <h1 id=\"temp:C:GFF000009\">Three</h1>",
        ));
        let anchors = doc.anchors();
        assert_eq!(anchors["temp:C:GFF11abcd"], "1abcd");
        assert_eq!(anchors["temp:C:GFF22abcd"], "2abcd");
        assert_eq!(anchors["temp:C:GFF000009"], "00009");
        assert!(doc.section("1abcd").is_ok());
        // The short tail both share is no longer an anchor: it would name either.
        assert_eq!(doc.section("abcd"), Err(Unresolved::Ambiguous));
    }

    #[test]
    fn a_section_takes_its_subsections_and_stops_at_its_own_level() {
        let doc = Document::parse(&wrap(
            "<h2 id=\"a0001\">A</h2><p id=\"p1\" class=\"line\">x</p>\
             <h3 id=\"b0002\">B</h3><p id=\"p2\" class=\"line\">y</p>\
             <h1 id=\"c0003\">C</h1><p id=\"p3\" class=\"line\">z</p>",
        ));
        assert_eq!(doc.section("0001"), Ok(Span { start: 0, end: 4 }));
        assert_eq!(doc.section("{#0002}"), Ok(Span { start: 2, end: 4 }));
        assert_eq!(doc.section("0003"), Ok(Span { start: 4, end: 6 }));
        assert_eq!(
            doc.markdown_of(doc.section("0002").unwrap()),
            "### B {#0002}\n\ny\n"
        );
    }

    #[test]
    fn markup_it_does_not_know_keeps_its_text_and_entities_decode() {
        let doc = Document::parse(&wrap(
            "<p id=\"p1\" class=\"line\">Tom &amp; <mention data-x='1'>@Jerry</mention> \
             &lt;3 &#x1F600; <b></b><i> </i><img src=\"https://x.test/a.png\"/></p>",
        ));
        assert_eq!(
            doc.markdown(),
            "Tom & @Jerry <3 \u{1F600} _ _<https://x.test/a.png>\n"
        );
    }

    /// Read back as the syntax Slack takes in, so an agent copying a mention keeps it a mention.
    #[test]
    fn mentions_read_as_the_markdown_that_writes_them() {
        let doc = Document::parse(&wrap(
            "<p id=\"p1\" class=\"line\">Hi <control id=\"x\" data-remapped=\"true\">\
             <a>@U0B20G0ET9T</a></control> in <control id=\"y\"><a>#C0BM1E6BKK4</a></control>\
             <control id=\"z\"><img src=\"\" alt=\"tada\" data-is-slack>:tada:</img></control></p>",
        ));
        assert_eq!(
            doc.markdown(),
            "Hi ![](@U0B20G0ET9T) in ![](#C0BM1E6BKK4):tada:\n"
        );
    }

    #[test]
    fn anchors_copied_back_into_markdown_are_not_written_as_text() {
        let markdown = "## Goals {#a1b2}\nText with {#braces} kept.\n# Plain";
        assert_eq!(
            strip_anchors(markdown),
            "## Goals\nText with {#braces} kept.\n# Plain"
        );
    }

    /// An emptied table cell and a divider have no id; they read, but nothing aims a delete at them.
    #[test]
    fn tables_offer_only_their_cells_and_dividers_nothing() {
        let doc = Document::parse(&wrap(
            "<table><tr><td></td><td><p id=\"c1\" class=\"line\">B</p></td></tr></table>\
             <hr style='width:100%'>",
        ));
        assert_eq!(doc.blocks[0].ids(), vec!["c1"]);
        assert!(doc.blocks[1].ids().is_empty());
        assert_eq!(doc.markdown(), "|  | B |\n|---|---|\n\n---\n");
    }
}
