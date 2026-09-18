//! The Murtaugh gateway: Slack on one side, RAX nodes on the other.

pub mod access;
pub mod admin;
pub mod chat;
pub mod cli;
pub mod config;
pub mod fleet;
pub mod hub;
pub mod launchd;
pub mod logging;
pub mod manifest;
pub mod render;
pub mod run;
pub mod tls;
pub mod token;
pub mod version;

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
