//! The Lists tools against the simulator, which keeps Lists the way Slack documents them: columns,
//! rows and options by id only, and a List seen only by those it is shared with.

#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use murtaugh_gateway::tools::lists::add::AddListItems;
use murtaugh_gateway::tools::lists::create::CreateList;
use murtaugh_gateway::tools::lists::delete::DeleteListItems;
use murtaugh_gateway::tools::lists::read::ReadList;
use murtaugh_gateway::tools::lists::read_item::ReadListItem;
use murtaugh_gateway::tools::lists::update::UpdateListItems;
use murtaugh_gateway::tools::{Context, Tool};
use murtaugh_slack::{SlackClient, Tokens};
use serde_json::{Value, json};
use slack_sim::{BOT_USER_ID, GENERAL, SECRET, SlackSim};

const ALICE: &str = "U0ALICE01";
const BOB: &str = "U0BOB0001";

struct Fixture {
    sim: SlackSim,
    create: CreateList,
    read: ReadList,
    read_item: ReadListItem,
    add: AddListItems,
    update: UpdateListItems,
    delete: DeleteListItems,
}

async fn fixture() -> Fixture {
    let sim = SlackSim::start().await.unwrap();
    sim.add_user(ALICE, "alice");
    sim.add_user(BOB, "bob");
    let tokens = sim.tokens();
    let slack = SlackClient::new(
        Tokens {
            app: tokens.app,
            bot: tokens.bot,
        },
        sim.api_base(),
    );
    Fixture {
        create: CreateList::new(Some(slack.clone())),
        read: ReadList::new(Some(slack.clone())),
        read_item: ReadListItem::new(Some(slack.clone())),
        add: AddListItems::new(Some(slack.clone())),
        update: UpdateListItems::new(Some(slack.clone())),
        delete: DeleteListItems::new(Some(slack)),
        sim,
    }
}

fn asked_by(user: &str) -> Context {
    Context {
        user: Some(murtaugh_store::slack_user(user).unwrap()),
        ..Context::default()
    }
}

fn parse(answer: Result<String, String>) -> Value {
    let answer = answer.unwrap_or_else(|err| panic!("the tool refused: {err}"));
    serde_json::from_str(&answer).unwrap_or_else(|_| panic!("not JSON: {answer}"))
}

fn no_violations(sim: &SlackSim) {
    assert!(sim.violations().is_empty(), "{:#?}", sim.violations());
}

/// A List made by Alice, with a select column, shared with the bot at `level`.
fn alices_list(f: &Fixture, level: &str) -> String {
    let id = f
        .sim
        .add_list(
            ALICE,
            "Launch",
            json!([
                {"key": "task", "name": "Task", "type": "text", "is_primary_column": true},
                {"key": "status", "name": "Status", "type": "select", "options": {
                    "format": "single_select",
                    "choices": [
                        {"value": "OptTodo", "label": "To do", "color": "red"},
                        {"value": "OptDone", "label": "Done", "color": "green"},
                    ],
                }},
                {"key": "owner", "name": "Owner", "type": "user"},
                {"key": "due", "name": "Due", "type": "date"},
            ]),
        )
        .unwrap();
    f.sim.share_list(&id, BOT_USER_ID, level);
    id
}

#[tokio::test]
async fn a_new_list_belongs_to_whoever_asked_and_the_bot_keeps_writing_to_it() {
    let f = fixture().await;
    let created = parse(
        f.create
            .invoke_in(
                &asked_by(ALICE),
                json!({
                    "name": "Groceries",
                    "description": "For the weekend",
                    "columns": [
                        {"name": "Item", "type": "text"},
                        {"name": "Aisle", "type": "select", "choices": ["Fruit", "Dairy"]},
                        {"name": "Bought", "type": "checkbox"},
                    ],
                }),
            )
            .await,
    );
    let list = created["list"].as_str().unwrap();
    assert_eq!(created["owner"], ALICE);
    assert_eq!(
        created["link"],
        format!("https://slacksim.slack.com/lists/T0SIM0001/{list}")
    );
    assert!(created.get("warning").is_none(), "{created}");
    let names: Vec<&str> = created["columns"]
        .as_array()
        .unwrap()
        .iter()
        .map(|c| c["name"].as_str().unwrap())
        .collect();
    assert_eq!(names, ["Item", "Aisle", "Bought"]);
    let mut access = f.sim.list_access(list);
    access.sort();
    assert_eq!(
        access,
        [
            (ALICE.to_owned(), "owner".to_owned()),
            (BOT_USER_ID.to_owned(), "write".to_owned()),
        ]
    );

    f.add
        .invoke(json!({"list": list, "items": [{"Item": "Milk", "Aisle": "dairy"}]}))
        .await
        .unwrap();
    no_violations(&f.sim);
}

/// Slack does not say whether handing a List over keeps the bot in; when it does not, the agent
/// learns it at once rather than at the first row it tries to add.
#[tokio::test]
async fn a_list_the_bot_lost_on_handing_it_over_is_reported() {
    let f = fixture().await;
    f.sim.set_list_owner_keeps_access(false);
    let created = parse(
        f.create
            .invoke_in(&asked_by(ALICE), json!({"name": "Groceries"}))
            .await,
    );
    let warning = created["warning"].as_str().unwrap();
    assert!(warning.contains("lost access"), "{warning}");
    assert_eq!(created["owner"], ALICE);
}

#[tokio::test]
async fn rows_round_trip_by_column_name_and_option_label() {
    let f = fixture().await;
    let list = alices_list(&f, "write");
    let added = parse(
        f.add
            .invoke(json!({
                "list": format!("https://slacksim.slack.com/lists/T0SIM0001/{list}"),
                "items": [
                    {"Task": "Write the post", "Status": "To do", "Owner": format!("<@{BOB}>")},
                    {"Task": "Ship", "Due": "2026-11-02"},
                ],
            }))
            .await,
    );
    let first = added["added"][0]["id"].as_str().unwrap().to_owned();
    let second = added["added"][1]["id"].as_str().unwrap().to_owned();

    let subtask = parse(
        f.add
            .invoke(json!({"list": list, "parent": first, "items": [{"Task": "Draft"}]}))
            .await,
    );
    let subtask = subtask["added"][0]["id"].as_str().unwrap().to_owned();

    f.update
        .invoke(json!({"list": list, "changes": [
            {"row": first, "column": "status", "value": "Done"},
            {"row": second, "column": "Task", "value": "Ship it"},
        ]}))
        .await
        .unwrap();

    let read = parse(f.read.invoke(json!({"list": list})).await);
    assert_eq!(read["title"], "Launch");
    assert_eq!(read["columns"][1]["choices"], json!(["To do", "Done"]));
    let rows = read["rows"].as_array().unwrap();
    assert_eq!(rows.len(), 3);
    assert_eq!(
        rows[0]["cells"],
        json!({"Task": "Write the post", "Status": "Done", "Owner": BOB})
    );
    assert_eq!(
        rows[1]["cells"],
        json!({"Task": "Ship it", "Due": "2026-11-02"})
    );
    assert_eq!(rows[2]["parent"], first.as_str());

    let item = parse(
        f.read_item
            .invoke(json!({"list": list, "row": first}))
            .await,
    );
    assert_eq!(item["row"]["cells"]["Status"], "Done");
    assert_eq!(item["subtasks"][0]["id"], subtask.as_str());

    f.delete
        .invoke(json!({"list": list, "rows": [first]}))
        .await
        .unwrap();
    let (_, rows) = f.sim.list(&list).unwrap();
    let left: Vec<&str> = rows.iter().map(|r| r["id"].as_str().unwrap()).collect();
    assert_eq!(left, [second.as_str()], "the subtask goes with its row");
    no_violations(&f.sim);
}

#[tokio::test]
async fn reading_pages_through_the_rows() {
    let f = fixture().await;
    let list = alices_list(&f, "write");
    let items: Vec<Value> = (1..=5)
        .map(|n| json!({"Task": format!("Task {n}")}))
        .collect();
    f.add
        .invoke(json!({"list": list, "items": items}))
        .await
        .unwrap();
    let first = parse(f.read.invoke(json!({"list": list, "limit": 3})).await);
    assert_eq!(first["rows"].as_array().unwrap().len(), 3);
    let cursor = first["next_cursor"].as_str().unwrap();
    let rest = parse(
        f.read
            .invoke(json!({"list": list, "limit": 3, "cursor": cursor}))
            .await,
    );
    assert_eq!(rest["rows"].as_array().unwrap().len(), 2);
    assert!(rest.get("next_cursor").is_none());
    no_violations(&f.sim);
}

/// A mistake in any row is caught before Slack is asked to write anything.
#[tokio::test]
async fn a_bad_value_adds_nothing_and_says_what_the_column_takes() {
    let f = fixture().await;
    let list = alices_list(&f, "write");
    let refused = f
        .add
        .invoke(json!({"list": list, "items": [
            {"Task": "Fine"},
            {"Task": "Not fine", "Status": "Blocked"},
        ]}))
        .await
        .unwrap_err();
    assert!(refused.starts_with("Row 2:"), "{refused}");
    assert!(refused.contains("To do, Done"), "{refused}");
    let (_, rows) = f.sim.list(&list).unwrap();
    assert!(rows.is_empty());
    no_violations(&f.sim);
}

#[tokio::test]
async fn a_list_not_shared_with_the_bot_is_explained() {
    let f = fixture().await;
    let list = f
        .sim
        .add_list(
            ALICE,
            "Private",
            json!([{"key": "t", "name": "T", "type": "text", "is_primary_column": true}]),
        )
        .unwrap();
    let refused = f.read.invoke(json!({"list": list})).await.unwrap_err();
    assert!(refused.contains("not shared with this bot"), "{refused}");

    // Sharing it with a channel the bot is not in does not help; one it is in does.
    f.sim.share_list(&list, SECRET, "write");
    assert!(f.read.invoke(json!({"list": list})).await.is_err());
    f.sim.share_list(&list, GENERAL, "read");
    parse(f.read.invoke(json!({"list": list})).await);
    let refused = f
        .add
        .invoke(json!({"list": list, "items": [{"T": "x"}]}))
        .await
        .unwrap_err();
    assert!(refused.contains("edit access"), "{refused}");
    no_violations(&f.sim);
}
