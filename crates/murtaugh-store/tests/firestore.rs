#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

//! Runs only with `FIRESTORE_EMULATOR_HOST` set, like the Go gateway's CI did; every test gets a
//! collection of its own so they never see each other's documents.

mod contract;

use std::time::Duration;

use murtaugh_store::{FirestoreLeader, FirestoreOptions, FirestoreStore, Leader};

async fn open() -> Option<FirestoreStore> {
    let host = std::env::var("FIRESTORE_EMULATOR_HOST").ok()?;
    let options = FirestoreOptions {
        project_id: Some("demo-murtaugh".into()),
        collection: Some(format!("test-{:016x}", rand::random::<u64>())),
        emulator_host: Some(host),
        ..Default::default()
    };
    Some(FirestoreStore::open(options).await.unwrap())
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
    a_token_is_revoked_once,
    pins_are_dropped_with_their_node,
);

#[tokio::test]
async fn one_gateway_leads_and_a_release_hands_over_at_once() {
    let Some(store) = open().await else {
        return;
    };
    let ttl = Duration::from_secs(30);
    let (first, second) = (
        FirestoreLeader::new(store.clone(), ttl),
        FirestoreLeader::new(store, ttl),
    );
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
    let (first, second) = (
        FirestoreLeader::new(store.clone(), ttl),
        FirestoreLeader::new(store, ttl),
    );
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
    let leaders: Vec<_> = (0..6)
        .map(|_| FirestoreLeader::new(store.clone(), ttl))
        .collect();
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
