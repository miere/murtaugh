//! Edits a canvas section by section, in Markdown.
//!
//! Slack edits one block per call, and a section is many blocks, so one edit here can be several
//! `canvases.edit` calls. They are not atomic. A replace inserts the new content before deleting
//! the old, so a failure part-way leaves the old content beside the new rather than neither, and
//! the reply says exactly how far it got.
//!
//! Nothing is destroyed by omission. Rewriting the whole canvas takes `whole_canvas: true` — a
//! missing `section` is refused, not read as "everything" — and a section holding a table or a
//! divider, which Slack's API cannot remove, is refused whole rather than half deleted, with the
//! whole-canvas rewrite named as the way past it.

use std::collections::HashSet;

use async_trait::async_trait;
use murtaugh_slack::{CanvasChange, CanvasOperation, SlackClient};
use rax::tool::{ToolDef, ToolKind};
use serde_json::{Value, json};

use super::document::{Block, strip_anchors};
use super::{Canvas, canvas_id, explain};
use crate::tools::Tool;

pub const NAME: &str = "edit_canvas";

pub struct EditCanvas {
    slack: Option<SlackClient>,
}

impl EditCanvas {
    pub fn new(slack: Option<SlackClient>) -> Self {
        Self { slack }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Op {
    Append,
    Prepend,
    Replace,
    Delete,
}

impl Op {
    fn parse(text: &str) -> Result<Op, String> {
        match text {
            "append" => Ok(Op::Append),
            "prepend" => Ok(Op::Prepend),
            "replace" => Ok(Op::Replace),
            "delete" => Ok(Op::Delete),
            other => Err(format!(
                "`{other}` is not an operation. Use append, prepend, replace or delete."
            )),
        }
    }
}

/// What an edit is aimed at.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Target<'a> {
    /// A heading's anchor.
    Section(&'a str),
    /// The top or the end of the canvas: where append and prepend go with no section.
    Edge,
    /// Everything in the canvas, asked for by name.
    WholeCanvas,
}

impl<'a> Target<'a> {
    fn resolve(op: Op, section: Option<&'a str>, whole_canvas: bool) -> Result<Target<'a>, String> {
        match (op, section, whole_canvas) {
            (_, Some(_), true) => Err(
                "Give either a `section` or `whole_canvas: true`, not both. Nothing was changed."
                    .into(),
            ),
            (Op::Replace, None, true) => Ok(Target::WholeCanvas),
            (_, None, true) => {
                Err("`whole_canvas` only goes with replace. Nothing was changed.".into())
            }
            (_, Some(anchor), false) => Ok(Target::Section(anchor)),
            (Op::Append | Op::Prepend, None, false) => Ok(Target::Edge),
            (Op::Replace, None, false) => Err(
                "replace needs a `section`. To rewrite the entire canvas, which removes \
                 everything in it, pass `whole_canvas: true`. Nothing was changed."
                    .into(),
            ),
            (Op::Delete, None, false) => {
                Err("delete needs a `section`. Nothing was changed.".into())
            }
        }
    }
}

#[async_trait]
impl Tool for EditCanvas {
    fn def(&self) -> ToolDef {
        ToolDef {
            name: NAME.to_owned(),
            description: "Change a Slack canvas with Markdown. `section` is a heading anchor from \
                 read_canvas; a section is its heading and everything under it, subsections \
                 included. append adds to the end of the section (or of the canvas), prepend adds \
                 right under its heading (or at the top of the canvas), replace swaps the section, \
                 heading included, and delete removes it. A section holding a table or a divider \
                 cannot be replaced or deleted: Slack cannot remove those. Rewriting the entire \
                 canvas is replace with `whole_canvas: true`; it removes everything in the canvas, \
                 so use it only when that is what was asked for. Nest list items with four \
                 spaces. Mention a person as ![](@U…) and a channel as ![](#C…). Read the canvas \
                 first; there is no need to read it again after editing."
                .to_owned(),
            input_schema: Some(json!({
                "type": "object",
                "required": ["link", "operation"],
                "properties": {
                    "link": {
                        "type": "string",
                        "description": "The canvas link as copied from Slack, or its id (F…).",
                    },
                    "operation": {
                        "type": "string",
                        "enum": ["append", "prepend", "replace", "delete"],
                    },
                    "section": {
                        "type": "string",
                        "description": "A heading's anchor, as read_canvas showed it. replace and \
                            delete need one; append and prepend without one work on the canvas's \
                            end and top.",
                    },
                    "whole_canvas": {
                        "type": "boolean",
                        "description": "With replace and no section: rewrite the entire canvas. \
                            Everything in it is removed.",
                    },
                    "markdown": {
                        "type": "string",
                        "description": "The content to add or replace with. Not used by delete.",
                    },
                },
            })),
            kind: ToolKind::Edit,
        }
    }

    async fn invoke(&self, arguments: Value) -> Result<String, String> {
        let Some(slack) = &self.slack else {
            return Err(
                "This gateway has no Slack credentials, so it cannot edit canvases.".into(),
            );
        };
        let text = |key: &str| {
            arguments
                .get(key)
                .and_then(Value::as_str)
                .map(str::trim)
                .filter(|value| !value.is_empty())
        };
        let id = canvas_id(text("link").unwrap_or_default())?;
        let op = Op::parse(text("operation").unwrap_or_default())?;
        let whole_canvas = arguments
            .get("whole_canvas")
            .and_then(Value::as_bool)
            .unwrap_or(false);
        let target = Target::resolve(op, text("section"), whole_canvas)?;
        let markdown = match (op, text("markdown")) {
            (Op::Delete, _) => None,
            (_, Some(markdown)) => Some(strip_anchors(markdown)),
            (_, None) => return Err("This operation needs `markdown` to write.".into()),
        };

        let canvas = Canvas::load(slack, &id).await?;
        let plan = plan(&canvas, op, target, markdown.clone())?;
        for (done, change) in plan.changes.iter().enumerate() {
            if let Err(err) = slack.edit_canvas(&id, change).await {
                let why = explain(&err, &id);
                return Err(if done == 0 {
                    format!("{why} Nothing was changed.")
                } else {
                    format!(
                        "The edit stopped part-way: {done} of {} steps were applied, then: {why} \
                         Read the canvas to see where it stands.",
                        plan.changes.len()
                    )
                });
            }
        }

        let mut reply = format!("Done: {}.", plan.summary);
        if markdown.as_deref().is_some_and(has_heading) {
            reply.push_str(&new_headings(slack, &id, &canvas).await);
        }
        Ok(reply)
    }
}

struct Plan {
    changes: Vec<CanvasChange>,
    summary: String,
}

fn change(
    operation: CanvasOperation,
    section: Option<&str>,
    markdown: Option<String>,
) -> CanvasChange {
    CanvasChange {
        operation,
        section_id: section.map(str::to_owned),
        markdown,
    }
}

fn one(change: CanvasChange, summary: String) -> Plan {
    Plan {
        changes: vec![change],
        summary,
    }
}

/// The `canvases.edit` calls one edit takes, in order.
fn plan(
    canvas: &Canvas,
    op: Op,
    target: Target<'_>,
    markdown: Option<String>,
) -> Result<Plan, String> {
    let blocks = &canvas.document.blocks;
    let anchor = match target {
        Target::WholeCanvas => return replace_whole(blocks, markdown.unwrap_or_default()),
        Target::Edge => {
            return Ok(match op {
                Op::Prepend => one(
                    change(CanvasOperation::InsertAtStart, None, markdown),
                    "added to the top of the canvas".into(),
                ),
                _ => one(
                    change(CanvasOperation::InsertAtEnd, None, markdown),
                    "added to the end of the canvas".into(),
                ),
            });
        }
        Target::Section(anchor) => anchor,
    };
    let span = canvas.section(anchor)?;
    let heading = blocks[span.start].ids().first().map(|id| (*id).to_owned());
    let Some(heading) = heading else {
        return Err(format!(
            "The heading at `{anchor}` has no id Slack can edit by. Nothing was changed."
        ));
    };
    let title = canvas
        .document
        .heading_text(span.start)
        .unwrap_or_default()
        .to_owned();
    let section_blocks = &blocks[span.start..span.end];
    if matches!(op, Op::Replace | Op::Delete) {
        refuse_fixed(section_blocks, &title)?;
    }
    let deletes = || -> Vec<CanvasChange> {
        section_blocks
            .iter()
            .flat_map(Block::ids)
            .map(|id| change(CanvasOperation::Delete, Some(id), None))
            .collect()
    };
    Ok(match op {
        Op::Append => {
            // A section ends where the next heading at its level begins, so adding to its end is
            // inserting before that heading — or at the end of the canvas when there is none.
            let next = blocks
                .get(span.end)
                .and_then(|block| block.ids().first().map(|id| (*id).to_owned()));
            let change = match next {
                Some(next) => change(CanvasOperation::InsertBefore, Some(&next), markdown),
                None => change(CanvasOperation::InsertAtEnd, None, markdown),
            };
            one(change, format!("added to the end of \"{title}\""))
        }
        Op::Prepend => one(
            change(CanvasOperation::InsertAfter, Some(&heading), markdown),
            format!("added under the heading \"{title}\""),
        ),
        Op::Replace => {
            let mut changes = vec![change(
                CanvasOperation::InsertBefore,
                Some(&heading),
                markdown,
            )];
            changes.extend(deletes());
            Plan {
                changes,
                summary: format!("replaced \"{title}\""),
            }
        }
        Op::Delete => Plan {
            changes: deletes(),
            summary: format!("deleted \"{title}\""),
        },
    })
}

/// Refuses to replace or delete a section holding a table or a divider. Slack's API can empty a
/// table's cells but not remove the table, and cannot touch a divider at all, so going ahead would
/// leave the section half there.
fn refuse_fixed(blocks: &[Block], title: &str) -> Result<(), String> {
    let tables = blocks
        .iter()
        .filter(|block| matches!(block, Block::Table(_)))
        .count();
    let rules = blocks
        .iter()
        .filter(|block| matches!(block, Block::Rule))
        .count();
    let mut held = Vec::new();
    if tables > 0 {
        held.push(count(tables, "table"));
    }
    if rules > 0 {
        held.push(count(rules, "divider"));
    }
    if held.is_empty() {
        return Ok(());
    }
    Err(format!(
        "\"{title}\" holds {}, which Slack's API cannot remove, so the section cannot be \
         replaced or deleted on its own. Nothing was changed. The way past it is to rewrite the \
         entire canvas: replace with `whole_canvas: true` and the full Markdown it should hold. \
         That removes everything else in the canvas too, so check it is wanted first.",
        held.join(" and ")
    ))
}

fn count(n: usize, thing: &str) -> String {
    match n {
        1 => format!("a {thing}"),
        n => format!("{n} {thing}s"),
    }
}

/// Rewrites the whole canvas. Slack keeps a canvas's leading `#` heading through that — it is the
/// canvas's title, and a whole replace left the old one above the new (seen 2026-10-05) — so the
/// title is handled on its own: the rest goes in by a whole replace, which leaves the title, and
/// then the title block takes the new one, or goes when the new content has none.
fn replace_whole(blocks: &[Block], markdown: String) -> Result<Plan, String> {
    let summary = "replaced the whole canvas".to_owned();
    let title = match blocks.first() {
        Some(Block::Heading { id, level: 1, .. }) if !id.is_empty() => id.as_str(),
        _ => {
            return Ok(one(
                change(CanvasOperation::Replace, None, Some(markdown)),
                summary,
            ));
        }
    };
    let (new_title, rest) = split_title(&markdown);
    if rest.trim().is_empty() {
        // Slack refuses a whole replace with nothing in it.
        return Err(
            "Rewriting the whole canvas needs content besides its title. Nothing was changed."
                .into(),
        );
    }
    let mut changes = vec![change(
        CanvasOperation::Replace,
        None,
        Some(rest.to_owned()),
    )];
    changes.push(match new_title {
        Some(line) => change(CanvasOperation::Replace, Some(title), Some(line.to_owned())),
        None => change(CanvasOperation::Delete, Some(title), None),
    });
    Ok(Plan { changes, summary })
}

/// The new content's own `#` title, when its first line is one, and what follows it.
fn split_title(markdown: &str) -> (Option<&str>, &str) {
    let trimmed = markdown.trim_start();
    let (first, rest) = trimmed.split_once('\n').unwrap_or((trimmed, ""));
    if first.starts_with("# ") {
        (Some(first.trim_end()), rest)
    } else {
        (None, markdown)
    }
}

fn has_heading(markdown: &str) -> bool {
    markdown
        .lines()
        .any(|line| line.trim_start().starts_with('#'))
}

/// The headings an edit added, with their anchors, so the agent can keep editing without reading
/// the canvas again.
async fn new_headings(slack: &SlackClient, id: &str, before: &Canvas) -> String {
    let Ok(after) = Canvas::load(slack, id).await else {
        return String::new();
    };
    let old: HashSet<&str> = before.document.blocks.iter().flat_map(Block::ids).collect();
    let anchors = after.document.anchors();
    let added: Vec<String> = after
        .document
        .blocks
        .iter()
        .filter_map(|block| match block {
            Block::Heading { id, level, text } if !old.contains(id.as_str()) => Some(format!(
                "{} {text} {{#{}}}",
                "#".repeat(usize::from(*level)),
                anchors.get(id).map_or("", String::as_str)
            )),
            _ => None,
        })
        .collect();
    if added.is_empty() {
        return String::new();
    }
    format!(" New headings:\n{}", added.join("\n"))
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used)]

    use super::*;
    use crate::tools::canvas::document::Document;
    use murtaugh_slack::FileInfo;

    fn canvas(html: &str) -> Canvas {
        Canvas {
            info: FileInfo {
                id: "F1".into(),
                name: "Plan".into(),
                title: "Plan".into(),
                mimetype: murtaugh_slack::CANVAS_MIMETYPE.into(),
                size: 0,
                url_private_download: String::new(),
            },
            document: Document::parse(html),
        }
    }

    const PLAN: &str = "<div class=\"quip-canvas-content\">\
        <h1 id=\"temp:C:GFFaaaa0001\">Plan</h1>\
        <h2 id=\"temp:C:GFFbbbb0002\">Goals</h2>\
        <p id=\"temp:C:GFFcccc0003\" class=\"line\">Ship it.</p>\
        <h3 id=\"temp:C:GFFdddd0004\">Stretch</h3>\
        <p id=\"temp:C:GFFeeee0005\" class=\"line\">Ship it twice.</p>\
        <hr style='width:100%'>\
        <h2 id=\"temp:C:GFFffff0006\">Risks</h2>\
        <p id=\"temp:C:GFF11110007\" class=\"line\">None.</p>\
        </div>";

    fn steps(plan: &Plan) -> Vec<(&'static str, Option<&str>)> {
        plan.changes
            .iter()
            .map(|c| (c.operation.as_str(), c.section_id.as_deref()))
            .collect()
    }

    fn at(anchor: &str) -> Target<'_> {
        Target::Section(anchor)
    }

    #[test]
    fn appending_to_a_section_inserts_before_the_next_heading_at_its_level() {
        let canvas = canvas(PLAN);
        let plan = plan(&canvas, Op::Append, at("0002"), Some("More.".into())).unwrap();
        // Past its subsection, up to the next h2.
        assert_eq!(
            steps(&plan),
            [("insert_before", Some("temp:C:GFFffff0006"))]
        );
    }

    #[test]
    fn appending_to_the_last_section_appends_to_the_canvas() {
        let canvas = canvas(PLAN);
        let plan = plan(&canvas, Op::Append, at("0006"), Some("More.".into())).unwrap();
        assert_eq!(steps(&plan), [("insert_at_end", None)]);
    }

    #[test]
    fn prepending_to_a_section_goes_right_under_its_heading() {
        let canvas = canvas(PLAN);
        let plan = plan(&canvas, Op::Prepend, at("0002"), Some("First.".into())).unwrap();
        assert_eq!(steps(&plan), [("insert_after", Some("temp:C:GFFbbbb0002"))]);
    }

    #[test]
    fn adding_to_the_top_or_end_of_the_canvas_is_one_call() {
        let canvas = canvas(PLAN);
        for (op, expected) in [
            (Op::Append, "insert_at_end"),
            (Op::Prepend, "insert_at_start"),
        ] {
            let plan = plan(&canvas, op, Target::Edge, Some("x".into())).unwrap();
            assert_eq!(steps(&plan), [(expected, None)]);
        }
    }

    /// New content goes in before the old comes out, so a failure part-way loses nothing.
    #[test]
    fn replacing_a_section_inserts_first_then_deletes_every_block_it_had() {
        let canvas = canvas(PLAN);
        let plan = plan(
            &canvas,
            Op::Replace,
            at("0006"),
            Some("## Risks\n\nSome.".into()),
        )
        .unwrap();
        assert_eq!(
            steps(&plan),
            [
                ("insert_before", Some("temp:C:GFFffff0006")),
                ("delete", Some("temp:C:GFFffff0006")),
                ("delete", Some("temp:C:GFF11110007")),
            ]
        );
    }

    /// Going ahead would leave the divider behind with the rest gone, so nothing is touched and
    /// the agent is told the one way past it.
    #[test]
    fn a_section_holding_a_divider_is_refused_whole_and_names_the_way_past() {
        let canvas = canvas(PLAN);
        for op in [Op::Replace, Op::Delete] {
            let markdown = (op == Op::Replace).then(|| "x".to_owned());
            let refusal = plan(&canvas, op, at("0002"), markdown).err().unwrap();
            assert!(refusal.contains("a divider"), "{refusal}");
            assert!(refusal.contains("Nothing was changed"), "{refusal}");
            assert!(refusal.contains("whole_canvas: true"), "{refusal}");
        }
        // Adding to it is still fine: nothing is removed.
        assert!(plan(&canvas, Op::Append, at("0002"), Some("x".into())).is_ok());
    }

    /// A forgotten section must never read as "the whole canvas".
    #[test]
    fn the_whole_canvas_is_only_ever_replaced_when_asked_for_by_name() {
        let refusal = Target::resolve(Op::Replace, None, false).unwrap_err();
        assert!(refusal.contains("whole_canvas: true"), "{refusal}");
        assert!(Target::resolve(Op::Delete, None, false).is_err());
        assert!(Target::resolve(Op::Delete, None, true).is_err());
        assert!(Target::resolve(Op::Append, None, true).is_err());
        assert!(Target::resolve(Op::Replace, Some("a1b2"), true).is_err());
        assert_eq!(
            Target::resolve(Op::Replace, None, true),
            Ok(Target::WholeCanvas)
        );
        assert_eq!(Target::resolve(Op::Append, None, false), Ok(Target::Edge));
    }

    /// Slack keeps a canvas's leading title through a whole replace, so the title is swapped on
    /// its own after the body, or deleted when the new content has none.
    #[test]
    fn replacing_the_whole_canvas_handles_its_title_apart() {
        let canvas = canvas(PLAN);
        let whole = Target::WholeCanvas;
        let titled = plan(&canvas, Op::Replace, whole, Some("# New\n\nBody.".into())).unwrap();
        assert_eq!(
            steps(&titled),
            [("replace", None), ("replace", Some("temp:C:GFFaaaa0001"))]
        );
        assert_eq!(titled.changes[0].markdown.as_deref(), Some("\nBody."));
        assert_eq!(titled.changes[1].markdown.as_deref(), Some("# New"));

        let untitled = plan(&canvas, Op::Replace, whole, Some("Body.".into())).unwrap();
        assert_eq!(
            steps(&untitled),
            [("replace", None), ("delete", Some("temp:C:GFFaaaa0001"))]
        );

        let title_only = plan(&canvas, Op::Replace, whole, Some("# New".into()));
        assert!(title_only.err().unwrap().contains("Nothing was changed"));

        // A canvas with no title is one call.
        let bare = self::canvas(
            "<div class=\"quip-canvas-content\"><p id=\"p\" class=\"line\">x</p></div>",
        );
        let plan = plan(&bare, Op::Replace, whole, Some("# New".into())).unwrap();
        assert_eq!(steps(&plan), [("replace", None)]);
    }

    #[test]
    fn a_stale_anchor_changes_nothing_and_says_to_read_again() {
        let canvas = canvas(PLAN);
        let refusal = plan(&canvas, Op::Delete, at("9999"), None).err().unwrap();
        assert!(refusal.contains("Read it again"));
        assert!(refusal.contains("Nothing was changed"));
    }
}
