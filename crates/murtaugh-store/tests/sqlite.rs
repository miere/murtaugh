#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod contract;

use contract::*;
use murtaugh_store::{Leader, SqliteLeader, SqliteStore, Store, StoreError, UserId};

fn open() -> (tempfile::TempDir, SqliteStore) {
    let dir = tempfile::tempdir().unwrap();
    let store = SqliteStore::open(&dir.path().join("config.db")).unwrap();
    (dir, store)
}

macro_rules! contract {
    ($($name:ident),+ $(,)?) => {$(
        #[tokio::test]
        async fn $name() {
            let (_dir, store) = open();
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
    pins_are_dropped_with_their_node,
);

#[test]
fn only_slack_user_ids_parse() {
    assert!(UserId::parse("U0123ABCD").is_ok());
    assert!(UserId::parse("W0123ABCD").is_ok());
    for bad in [
        "C0123ABCD",
        "B0123ABCD",
        "u0123abcd",
        "U01",
        "",
        "U0123 ABCD",
    ] {
        assert!(UserId::parse(bad).is_err(), "{bad} parsed");
    }
}

#[tokio::test]
async fn a_second_process_sees_every_write() {
    let (dir, store) = open();
    let cli = SqliteStore::open(&dir.path().join("config.db")).unwrap();
    cli.approve(&user("U0PERSON1"), &user("U0ADMIN01"))
        .await
        .unwrap();
    assert_eq!(store.grants().await.unwrap().len(), 1);
}

#[test]
fn only_one_gateway_holds_the_leader_lock_and_it_frees_on_drop() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("config.db");
    let held = SqliteStore::lock_leader(&path).unwrap();
    assert!(matches!(
        SqliteStore::lock_leader(&path),
        Err(StoreError::Locked(_))
    ));
    drop(held);
    SqliteStore::lock_leader(&path).unwrap();
}

#[tokio::test]
async fn one_sqlite_leader_per_machine_until_it_releases() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("config.db");
    let (first, second) = (SqliteLeader::new(path.clone()), SqliteLeader::new(path));
    let lease = first.acquire("a").await.unwrap().unwrap();
    assert_eq!(second.acquire("b").await.unwrap(), None);
    assert!(first.renew(&lease).await.unwrap().is_some());
    first.release(lease).await.unwrap();
    assert!(second.acquire("b").await.unwrap().is_some());
}
