//! The Murtaugh gateway: Slack on one side, RAX nodes on the other.

pub mod access;
pub mod admin;
pub mod alerts;
pub mod approval;
pub mod chat;
pub mod cli;
pub mod config;
pub mod faults;
pub mod files;
pub mod fleet;
pub mod home;
pub mod hub;
pub mod logging;
pub mod manifest;
pub mod node_access;
pub mod panel;
pub mod picker;
pub mod policy;
pub mod port;
pub mod prompts;
pub mod relay;
pub mod render;
pub mod reply;
pub mod roles;
pub mod run;
pub mod signin;
pub mod thread_commands;
pub mod tools;
pub mod workloads;

pub use murtaugh_common::{launchd, tls, token, version};

use std::sync::Arc;
use std::time::Duration;

use murtaugh_store::{
    FirestoreLeader, FirestoreOptions, FirestoreStore, Leader, SqliteLeader, SqliteStore, Store,
};

use crate::config::{Database, FirestoreConfig};

pub const LEASE_TTL: Duration = Duration::from_secs(30);

fn firestore_options(config: &FirestoreConfig) -> FirestoreOptions {
    FirestoreOptions {
        project_id: config.project_id.clone(),
        database_id: config.database_id.clone(),
        collection: config.collection.clone(),
        credentials_file: config.credentials_file.clone(),
        emulator_host: None,
    }
}

pub async fn open_store(database: &Database) -> Result<Arc<dyn Store>, String> {
    Ok(open_leader(database).await?.0)
}

pub async fn open_leader(database: &Database) -> Result<(Arc<dyn Store>, Arc<dyn Leader>), String> {
    match database {
        Database::Sqlite { path } => {
            let store = SqliteStore::open(path).map_err(|err| err.to_string())?;
            Ok((Arc::new(store), Arc::new(SqliteLeader::new(path.clone()))))
        }
        Database::Firestore(config) => {
            let store = FirestoreStore::open(firestore_options(config))
                .await
                .map_err(|err| err.to_string())?;
            let leader = FirestoreLeader::new(store.clone(), LEASE_TTL);
            Ok((Arc::new(store), Arc::new(leader)))
        }
    }
}
