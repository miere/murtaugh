#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

//! Runs only with `FIRESTORE_EMULATOR_HOST` set, like the Go gateway's CI did; every test gets a
//! collection of its own so they never see each other's documents.

mod contract;

use std::time::Duration;

use murtaugh_store::{
    Conversation, FirestoreOptions, FirestoreStore, GatewayStore, Leader, Pin, Store, StoreError,
    slack_user,
};
use rax_gateway::store::firestore::FirestoreStore as LooseStore;
use serde_json::json;
use time::OffsetDateTime;

fn options() -> Option<FirestoreOptions> {
    let host = std::env::var("FIRESTORE_EMULATOR_HOST").ok()?;
    let mut options = FirestoreOptions::new(format!("test-{:016x}", rand::random::<u64>()));
    options.project_id = Some("demo-murtaugh".into());
    options.emulator_host = Some(host);
    Some(options)
}

async fn open() -> Option<FirestoreStore> {
    Some(FirestoreStore::open(options()?).await.unwrap())
}

macro_rules! contract {
    ($($name:ident),+ $(,)?) => {$(
        #[tokio::test]
        async fn $name() {
            let Some(store) = open().await else {
                eprintln!("skipped: FIRESTORE_EMULATOR_HOST is not set");
                return;
            };
            contract::$name(&store).await;
        }
    )+};
}

contract!(
    the_admin_is_unset_until_assigned,
    approving_twice_keeps_the_first_approval_and_revoking_removes_it,
    a_person_is_not_pre_authorised_until_the_admin_says_so,
    a_node_owners_tool_rules_are_kept_apart_from_the_rest_of_their_settings,
    a_token_is_revoked_once,
    a_user_token_is_kept_apart_from_node_tokens_and_revoked_once,
    pins_are_dropped_with_their_node,
);

#[tokio::test]
async fn one_gateway_leads_and_a_release_hands_over_at_once() {
    let Some(store) = open().await else {
        return;
    };
    let ttl = Duration::from_secs(30);
    let (first, second) = (store.leader(ttl), store.leader(ttl));
    let lease = first.acquire("gateway-a").await.unwrap().unwrap();
    assert_eq!(second.acquire("gateway-b").await.unwrap(), None);
    let renewed = first.renew(&lease).await.unwrap().unwrap();
    assert_eq!(second.acquire("gateway-b").await.unwrap(), None);
    first.release(renewed).await.unwrap();
    assert!(second.acquire("gateway-b").await.unwrap().is_some());
}

#[tokio::test]
async fn an_expired_lease_is_taken_over_and_its_old_holder_learns_on_renewal() {
    let Some(store) = open().await else {
        return;
    };
    let ttl = Duration::from_secs(1);
    let (first, second) = (store.leader(ttl), store.leader(ttl));
    let stale = first.acquire("gateway-a").await.unwrap().unwrap();
    tokio::time::sleep(Duration::from_millis(1_500)).await;
    let taken = second.acquire("gateway-b").await.unwrap();
    assert!(taken.is_some(), "the expired lease was not taken over");
    assert_eq!(first.renew(&stale).await.unwrap(), None);
}

#[tokio::test]
async fn two_gateways_racing_for_a_free_lease_never_both_win() {
    let Some(store) = open().await else {
        return;
    };
    let ttl = Duration::from_secs(30);
    let leaders: Vec<_> = (0..6).map(|_| store.leader(ttl)).collect();
    let attempts = leaders
        .iter()
        .enumerate()
        .map(|(index, leader)| async move { leader.acquire(&format!("gateway-{index}")).await });
    let won = futures_util::future::join_all(attempts)
        .await
        .into_iter()
        .filter(|result| matches!(result, Ok(Some(_))))
        .count();
    assert_eq!(won, 1);
}

/// Where every deployed gateway already keeps its pins; moving them strands the threads.
#[tokio::test]
async fn a_pin_keeps_its_document_and_fields() {
    let Some(options) = options() else {
        return;
    };
    let store = FirestoreStore::open(options.clone()).await.unwrap();
    store
        .set_pin(&Pin {
            conversation: Conversation {
                channel: "C0123ABCD".into(),
                thread_ts: "1712345678.000100".into(),
            },
            node: "n1".into(),
            session_id: "s1".into(),
            user: slack_user("U0PERSON1").unwrap(),
            pinned_at: OffsetDateTime::from_unix_timestamp(1_789_000_000).unwrap(),
        })
        .await
        .unwrap();
    let raw = LooseStore::open(options).await.unwrap();
    let doc = raw
        .get("pins/C0123ABCD-1712345678.000100")
        .await
        .unwrap()
        .unwrap();
    assert_eq!(
        serde_json::Value::Object(doc.fields().clone()),
        json!({
            "channel": {"stringValue": "C0123ABCD"},
            "thread_ts": {"stringValue": "1712345678.000100"},
            "node": {"stringValue": "n1"},
            "session_id": {"stringValue": "s1"},
            "user_id": {"stringValue": "U0PERSON1"},
            "pinned_at": {"timestampValue": "2026-09-10T00:26:40Z"},
        })
    );
}

/// As SQLite does: a stored id that is not a Slack user's is corrupt, not a person.
#[tokio::test]
async fn a_stored_id_that_is_not_a_slack_users_is_corrupt() {
    let Some(options) = options() else {
        return;
    };
    let loose = LooseStore::open(options.clone()).await.unwrap();
    rax_gateway::store::Store::set_admin(
        &loose,
        &rax_gateway::store::UserId::parse("C0123ABCD").unwrap(),
    )
    .await
    .unwrap();
    let store = FirestoreStore::open(options).await.unwrap();
    match store.admin().await.unwrap_err() {
        StoreError::Corrupt(message) => {
            assert_eq!(message, slack_user("C0123ABCD").unwrap_err().to_string())
        }
        other => panic!("not corrupt: {other}"),
    }
}
