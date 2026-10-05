//! read_canvas and edit_canvas against the simulator, which answers `files.info`, the canvas
//! download and `canvases.edit` the way Slack was seen to.

#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use murtaugh_gateway::tools::Tool;
use murtaugh_gateway::tools::canvas::edit::EditCanvas;
use murtaugh_gateway::tools::canvas::read::ReadCanvas;
use murtaugh_slack::{SlackClient, Tokens};
use serde_json::{Value, json};
use slack_sim::{GENERAL, SECRET, SlackSim};

const PLAN: &str = "\
# Plan

## Goals

Ship it.

### Stretch

- [ ] Ship it twice
    - [ ] with ![](@U0ALICE)

## Risks

None.
";

struct Fixture {
    sim: SlackSim,
    read: ReadCanvas,
    edit: EditCanvas,
}

async fn fixture() -> Fixture {
    let sim = SlackSim::start().await.unwrap();
    let tokens = sim.tokens();
    let slack = SlackClient::new(
        Tokens {
            app: tokens.app,
            bot: tokens.bot,
        },
        sim.api_base(),
    );
    Fixture {
        read: ReadCanvas::new(Some(slack.clone())),
        edit: EditCanvas::new(Some(slack)),
        sim,
    }
}

/// The anchor a read gave the heading with this text.
fn anchor(markdown: &str, heading: &str) -> String {
    let line = markdown
        .lines()
        .find(|line| {
            line.trim_start_matches('#')
                .trim_start()
                .starts_with(heading)
        })
        .unwrap_or_else(|| panic!("no heading {heading} in:\n{markdown}"));
    let start = line.rfind("{#").unwrap() + 2;
    line[start..line.len() - 1].to_owned()
}

async fn read(f: &Fixture, id: &str) -> String {
    f.read.invoke(json!({"link": id})).await.unwrap()
}

async fn edit(f: &Fixture, args: Value) -> Result<String, String> {
    f.edit.invoke(args).await
}

#[tokio::test]
async fn a_canvas_reads_as_markdown_with_an_anchor_on_every_heading() {
    let f = fixture().await;
    let id = f.sim.add_canvas(GENERAL, "Plan", PLAN).unwrap();
    let link = format!("https://sim.slack.com/docs/T0SIM0001/{id}");
    let markdown = f.read.invoke(json!({"link": link})).await.unwrap();
    assert!(markdown.starts_with("Canvas \"Plan\""), "{markdown}");
    for heading in ["Plan", "Goals", "Stretch", "Risks"] {
        assert!(anchor(&markdown, heading).len() >= 4);
    }
    assert!(markdown.contains("- [ ] Ship it twice\n    - [ ] with ![](@U0ALICE)"));

    // One section alone, subsections included and the next sibling left out.
    let goals = anchor(&markdown, "Goals");
    let section = f
        .read
        .invoke(json!({"link": id, "section": goals}))
        .await
        .unwrap();
    assert!(section.contains("Ship it twice"));
    assert!(!section.contains("Risks"));
    assert!(f.sim.violations().is_empty(), "{:?}", f.sim.violations());
}

#[tokio::test]
async fn appending_to_a_section_lands_before_the_next_one() {
    let f = fixture().await;
    let id = f.sim.add_canvas(GENERAL, "Plan", PLAN).unwrap();
    let goals = anchor(&read(&f, &id).await, "Goals");
    let reply = edit(
        &f,
        json!({"link": id, "operation": "append", "section": goals, "markdown": "Ship it **well**."}),
    )
    .await
    .unwrap();
    assert!(reply.starts_with("Done"), "{reply}");
    let after = read(&f, &id).await;
    let well = after.find("Ship it **well**.").expect("appended text");
    assert!(well > after.find("Ship it twice").unwrap());
    assert!(well < after.find("## Risks").unwrap());
    assert!(f.sim.violations().is_empty(), "{:?}", f.sim.violations());
}

#[tokio::test]
async fn replacing_a_section_swaps_it_whole_and_names_the_new_headings() {
    let f = fixture().await;
    let id = f.sim.add_canvas(GENERAL, "Plan", PLAN).unwrap();
    let goals = anchor(&read(&f, &id).await, "Goals");
    // An agent copying a heading back with its anchor must not write the anchor as text.
    let reply = edit(
        &f,
        json!({
            "link": id,
            "operation": "replace",
            "section": goals,
            "markdown": format!("## Aims {{#{goals}}}\n\nShip it once, properly."),
        }),
    )
    .await
    .unwrap();
    let after = read(&f, &id).await;
    assert!(
        !after.contains("Goals") && !after.contains("Stretch"),
        "{after}"
    );
    assert!(after.contains("Ship it once, properly."));
    assert!(!after.contains(&format!("{{#{goals}}} {{#")), "{after}");
    // The reply gives the new heading's anchor, so a next edit needs no second read.
    let aims = anchor(&after, "Aims");
    assert!(reply.contains(&format!("## Aims {{#{aims}}}")), "{reply}");
    assert!(after.find("## Aims").unwrap() < after.find("## Risks").unwrap());
}

/// Slack cannot remove a table or a divider, so a section holding one is left untouched rather
/// than half deleted, and the whole-canvas rewrite that gets past it is spelt out.
#[tokio::test]
async fn a_section_holding_a_table_or_divider_is_refused_and_left_as_it_was() {
    let f = fixture().await;
    let id = f
        .sim
        .add_canvas(
            GENERAL,
            "Plan",
            "## Numbers\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n---\n\nText.\n\n## Keep\n\nKept.\n",
        )
        .unwrap();
    let before = f.sim.canvas_html(&id).unwrap();
    let numbers = anchor(&read(&f, &id).await, "Numbers");
    let refusal = edit(
        &f,
        json!({"link": id, "operation": "delete", "section": numbers}),
    )
    .await
    .unwrap_err();
    assert!(refusal.contains("a table and a divider"), "{refusal}");
    assert!(refusal.contains("whole_canvas: true"), "{refusal}");
    assert_eq!(f.sim.canvas_html(&id).unwrap(), before);
    assert!(
        f.sim
            .calls()
            .iter()
            .all(|call| call.method != "canvases.edit")
    );
}

/// A forgotten `section` must not wipe the canvas: rewriting it all is asked for by name.
#[tokio::test]
async fn replace_without_a_section_is_refused_unless_whole_canvas_is_named() {
    let f = fixture().await;
    let id = f.sim.add_canvas(GENERAL, "Plan", PLAN).unwrap();
    let before = f.sim.canvas_html(&id).unwrap();
    let refusal = edit(
        &f,
        json!({"link": id, "operation": "replace", "markdown": "# Fresh"}),
    )
    .await
    .unwrap_err();
    assert!(refusal.contains("whole_canvas: true"), "{refusal}");
    assert_eq!(f.sim.canvas_html(&id).unwrap(), before);
}

/// Slack keeps a canvas's title through a whole replace; the new one must not land beneath it.
#[tokio::test]
async fn a_whole_canvas_is_replaced_without_doubling_its_title() {
    let f = fixture().await;
    let id = f
        .sim
        .add_canvas(
            GENERAL,
            "Plan",
            &format!("{PLAN}\n---\n\n| a |\n|---|\n| 1 |\n"),
        )
        .unwrap();
    edit(
        &f,
        json!({
            "link": id,
            "operation": "replace",
            "whole_canvas": true,
            "markdown": "# Fresh\n\nStart over.",
        }),
    )
    .await
    .unwrap();
    let after = read(&f, &id).await;
    assert!(after.contains("# Fresh {#"), "{after}");
    // Everything else went, the table and divider a section edit cannot remove included.
    assert!(after.ends_with("Start over.\n"), "{after}");
    assert!(
        !after.contains("# Plan") && !after.contains("Goals") && !after.contains("---"),
        "{after}"
    );

    edit(
        &f,
        json!({
            "link": id,
            "operation": "replace",
            "whole_canvas": true,
            "markdown": "No title now.",
        }),
    )
    .await
    .unwrap();
    let after = read(&f, &id).await;
    assert!(!after.contains("Fresh"), "{after}");
    assert!(after.ends_with("No title now.\n"), "{after}");
    assert!(f.sim.violations().is_empty(), "{:?}", f.sim.violations());
}

/// The rule that matters most: a canvas the bot cannot see is a refusal, never an empty canvas.
#[tokio::test]
async fn a_canvas_the_bot_cannot_see_is_refused_not_empty() {
    let f = fixture().await;
    let id = f.sim.add_canvas(SECRET, "Hidden", PLAN).unwrap();
    let refusal = f.read.invoke(json!({"link": id})).await.unwrap_err();
    assert!(refusal.contains("It is not empty"), "{refusal}");
    let refusal = edit(
        &f,
        json!({"link": id, "operation": "append", "markdown": "x"}),
    )
    .await
    .unwrap_err();
    assert!(refusal.contains("It is not empty"), "{refusal}");
}

#[tokio::test]
async fn an_anchor_gone_since_the_read_changes_nothing() {
    let f = fixture().await;
    let id = f.sim.add_canvas(GENERAL, "Plan", PLAN).unwrap();
    let before = f.sim.canvas_html(&id).unwrap();
    let refusal = edit(
        &f,
        json!({"link": id, "operation": "replace", "section": "zzzz", "markdown": "x"}),
    )
    .await
    .unwrap_err();
    assert!(refusal.contains("Nothing was changed"), "{refusal}");
    assert_eq!(f.sim.canvas_html(&id).unwrap(), before);
}

#[tokio::test]
async fn an_edit_that_fails_part_way_says_how_far_it_got() {
    let f = fixture().await;
    let id = f.sim.add_canvas(GENERAL, "Plan", PLAN).unwrap();
    let goals = anchor(&read(&f, &id).await, "Goals");
    // The new content goes in; the first delete of the old is refused.
    f.sim.pass("canvases.edit");
    f.sim.fail("canvases.edit", "ratelimited");
    let refusal = edit(
        &f,
        json!({"link": id, "operation": "replace", "section": goals, "markdown": "## Aims"}),
    )
    .await
    .unwrap_err();
    assert!(refusal.contains("1 of 6 steps were applied"), "{refusal}");
    // Nothing was lost: the old section is still there beside the new heading.
    let after = read(&f, &id).await;
    assert!(
        after.contains("## Aims") && after.contains("## Goals"),
        "{after}"
    );
}

#[tokio::test]
async fn an_edit_refused_at_its_first_step_changed_nothing() {
    let f = fixture().await;
    let id = f.sim.add_canvas(GENERAL, "Plan", PLAN).unwrap();
    f.sim.fail("canvases.edit", "canvas_editing_failed");
    let refusal = edit(
        &f,
        json!({"link": id, "operation": "append", "markdown": "x"}),
    )
    .await
    .unwrap_err();
    assert!(refusal.contains("Nothing was changed"), "{refusal}");
}
