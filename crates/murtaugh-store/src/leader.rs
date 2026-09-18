use std::path::PathBuf;
use std::sync::Mutex;
use std::time::Duration;

use async_trait::async_trait;

use crate::sqlite::{LeaderLock, SqliteStore};
use crate::{Result, StoreError};

/// Proof of leadership until the next renewal. `version` is whatever the backend needs to tell a
/// renewal of this lease apart from someone else's takeover.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Lease {
    pub holder: String,
    pub version: String,
}

/// One gateway per Slack app: only the holder connects to Slack and serves nodes.
#[async_trait]
pub trait Leader: Send + Sync + 'static {
    /// How long a lease outlives its last renewal; renew well within it.
    fn ttl(&self) -> Duration;
    /// `None` means someone else leads right now.
    async fn acquire(&self, holder: &str) -> Result<Option<Lease>>;
    /// `None` means the lease was lost, and the caller must stop serving at once.
    async fn renew(&self, lease: &Lease) -> Result<Option<Lease>>;
    async fn release(&self, lease: Lease) -> Result<()>;
}

/// The OS file lock: one gateway per machine, released by the kernel if the process dies.
pub struct SqliteLeader {
    path: PathBuf,
    held: Mutex<Option<LeaderLock>>,
}

impl SqliteLeader {
    pub fn new(path: PathBuf) -> Self {
        Self {
            path,
            held: Mutex::new(None),
        }
    }

    fn held(&self) -> std::sync::MutexGuard<'_, Option<LeaderLock>> {
        self.held
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
}

#[async_trait]
impl Leader for SqliteLeader {
    fn ttl(&self) -> Duration {
        Duration::from_secs(30)
    }

    async fn acquire(&self, holder: &str) -> Result<Option<Lease>> {
        let lease = Lease {
            holder: holder.to_owned(),
            version: "flock".to_owned(),
        };
        let mut held = self.held();
        if held.is_some() {
            return Ok(Some(lease));
        }
        match SqliteStore::lock_leader(&self.path) {
            Ok(lock) => {
                *held = Some(lock);
                Ok(Some(lease))
            }
            Err(StoreError::Locked(_)) => Ok(None),
            Err(err) => Err(err),
        }
    }

    async fn renew(&self, lease: &Lease) -> Result<Option<Lease>> {
        Ok(self.held().is_some().then(|| lease.clone()))
    }

    async fn release(&self, _lease: Lease) -> Result<()> {
        self.held().take();
        Ok(())
    }
}
