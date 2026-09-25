//! Who may do what, read from the store and refreshed on a timer, so a CLI change reaches the
//! live gateway without a restart. Lookups are synchronous because RAX authenticates mid-handshake.

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant};

use murtaugh_store::{NodeToken, Store, StoreError, UserId};
use rax_tokio::gateway::{Authenticator, NodeIdentity};
use tokio::runtime::{Handle, RuntimeFlavor};

use crate::token;

#[derive(Debug, Default)]
pub struct Snapshot {
    admin: Option<UserId>,
    grants: HashSet<UserId>,
    allowed: HashSet<UserId>,
    tokens: HashMap<String, NodeToken>,
}

impl Snapshot {
    pub async fn load(store: &dyn Store) -> Result<Self, StoreError> {
        Ok(Self {
            admin: store.admin().await?,
            grants: store.grants().await?.into_iter().map(|g| g.user).collect(),
            allowed: store
                .users()
                .await?
                .into_iter()
                .filter(|user| user.allowed)
                .map(|user| user.user)
                .collect(),
            tokens: store
                .node_tokens()
                .await?
                .into_iter()
                .map(|token| (token.selector.clone(), token))
                .collect(),
        })
    }

    /// Every credential not revoked, whether or not its node is attached.
    pub fn live_nodes(&self) -> impl Iterator<Item = &NodeToken> {
        self.tokens
            .values()
            .filter(|token| token.revoked_at.is_none())
    }

    pub fn admin(&self) -> Option<&UserId> {
        self.admin.as_ref()
    }

    pub fn may_run_nodes(&self, user: &UserId) -> bool {
        self.admin.as_ref() == Some(user) || self.grants.contains(user)
    }

    /// A node's selector if its token is live and its owner may still run nodes.
    pub fn authenticate(&self, presented: &str) -> Option<String> {
        let credential = token::parse(presented)?;
        let record = self.tokens.get(&credential.selector)?;
        let live = record.revoked_at.is_none() && self.may_run_nodes(&record.owner);
        (live && token::matches(&credential.secret, &record.secret_hash))
            .then_some(credential.selector)
    }

    pub fn is_live(&self, selector: &str) -> bool {
        self.tokens
            .get(selector)
            .is_some_and(|record| record.revoked_at.is_none() && self.may_run_nodes(&record.owner))
    }

    pub fn owner(&self, selector: &str) -> Option<&UserId> {
        self.tokens.get(selector).map(|record| &record.owner)
    }

    /// Whether a node may be routed to. Unlike [`Self::is_live`] this has nothing to do with the
    /// credential: a disabled node keeps its link and can be re-enabled without reconnecting.
    pub fn is_enabled(&self, selector: &str) -> bool {
        self.tokens
            .get(selector)
            .is_some_and(|record| record.disabled_at.is_none())
    }

    pub fn node_name(&self, selector: &str) -> Option<&str> {
        self.tokens.get(selector).map(|record| record.name.as_str())
    }

    /// Everyone allowed on the gateway, node admins included: granting someone allows them too.
    /// Which nodes they may use is up to each node's owner, not the gateway.
    pub fn may_chat(&self, user: &UserId) -> bool {
        self.allowed.contains(user) || self.may_run_nodes(user)
    }

    /// People granted the right to connect nodes, the admin aside, who needs no grant.
    pub fn node_admins(&self) -> impl Iterator<Item = &UserId> {
        self.grants.iter()
    }

    /// People allowed to talk to the gateway, as the store lists them.
    pub fn allowed_users(&self) -> impl Iterator<Item = &UserId> {
        self.allowed.iter()
    }
}

/// Bounds how often a stranger's token can make the gateway read the store.
pub const RELOAD_AT_MOST_EVERY: Duration = Duration::from_secs(1);
/// Time for a booked read to land before a waiting token looks again.
const RECHECK_MARGIN: Duration = Duration::from_millis(250);

enum Plan {
    Read(Duration),
    Recheck(Duration),
}

struct Reload {
    store: Arc<dyn Store>,
    handle: Handle,
    last: Mutex<Option<Instant>>,
}

/// Cheap to clone; every clone sees the latest snapshot.
#[derive(Clone, Default)]
pub struct Access {
    current: Arc<RwLock<Arc<Snapshot>>>,
    reload: Option<Arc<Reload>>,
}

impl Access {
    pub fn new(snapshot: Snapshot) -> Self {
        Self {
            current: Arc::new(RwLock::new(Arc::new(snapshot))),
            reload: None,
        }
    }

    /// Also reads the store when a node presents a token minted since the last refresh, so a node
    /// started straight after `node mint` is admitted instead of refused for good.
    pub fn reloading(snapshot: Snapshot, store: Arc<dyn Store>) -> Self {
        Self {
            reload: Some(Arc::new(Reload {
                store,
                handle: Handle::current(),
                last: Mutex::new(None),
            })),
            ..Self::new(snapshot)
        }
    }

    fn admit_new(&self, presented: &str) -> Option<String> {
        let reload = self.reload.as_ref()?;
        let selector = token::parse(presented)?.selector;
        if self.snapshot().tokens.contains_key(&selector) {
            return None;
        }
        if reload.handle.runtime_flavor() != RuntimeFlavor::MultiThread {
            return None;
        }
        // Reads stay at most one a second: a token that arrives inside the window waits for the
        // next slot, or, if a read is already booked, for that read and checks again.
        let plan = {
            let mut last = reload
                .last
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            let now = Instant::now();
            match *last {
                Some(booked) if booked > now => Plan::Recheck(booked - now + RECHECK_MARGIN),
                Some(done) if now < done + RELOAD_AT_MOST_EVERY => {
                    let slot = done + RELOAD_AT_MOST_EVERY;
                    *last = Some(slot);
                    Plan::Read(slot - now)
                }
                _ => {
                    *last = Some(now);
                    Plan::Read(Duration::ZERO)
                }
            }
        };
        let wait = match plan {
            Plan::Recheck(wait) => {
                tokio::task::block_in_place(|| reload.handle.block_on(tokio::time::sleep(wait)));
                return self.snapshot().authenticate(presented);
            }
            Plan::Read(wait) => wait,
        };
        let fresh = tokio::task::block_in_place(|| {
            reload.handle.block_on(async {
                tokio::time::sleep(wait).await;
                Snapshot::load(&*reload.store).await
            })
        })
        .map_err(|err| tracing::warn!(error = %err, "could not read the store to admit a new node"))
        .ok()?;
        let admitted = fresh.authenticate(presented)?;
        let record = fresh.tokens.get(&admitted)?.clone();
        let mut current = self
            .current
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let mut next = Snapshot {
            admin: current.admin.clone(),
            grants: current.grants.clone(),
            allowed: current.allowed.clone(),
            tokens: current.tokens.clone(),
        };
        if fresh.grants.contains(&record.owner) {
            next.grants.insert(record.owner.clone());
        }
        next.tokens.insert(admitted.clone(), record);
        *current = Arc::new(next);
        Some(admitted)
    }

    pub fn snapshot(&self) -> Arc<Snapshot> {
        self.current
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clone()
    }

    /// Returns the selectors that were live before and are not now, so their links can be closed.
    pub fn replace(&self, next: Snapshot) -> Vec<String> {
        let mut current = self
            .current
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let lost = current
            .tokens
            .keys()
            .filter(|selector| current.is_live(selector) && !next.is_live(selector))
            .cloned()
            .collect();
        *current = Arc::new(next);
        lost
    }
}

impl Authenticator for Access {
    fn authenticate(&self, token: &str) -> Option<NodeIdentity> {
        self.snapshot()
            .authenticate(token)
            .or_else(|| self.admit_new(token))
            .map(NodeIdentity)
    }
}

#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod tests {
    use time::OffsetDateTime;

    use super::*;

    fn user(raw: &str) -> UserId {
        UserId::parse(raw).unwrap()
    }

    fn with_token(owner: &str) -> (Snapshot, String) {
        let minted = token::mint();
        let mut snapshot = Snapshot {
            admin: Some(user("U0ADMIN01")),
            ..Default::default()
        };
        snapshot.tokens.insert(
            minted.selector.clone(),
            NodeToken {
                selector: minted.selector.clone(),
                secret_hash: minted.secret_hash,
                owner: user(owner),
                name: "laptop".into(),
                created_at: OffsetDateTime::UNIX_EPOCH,
                revoked_at: None,
                disabled_at: None,
            },
        );
        (snapshot, minted.token)
    }

    #[test]
    fn a_token_authenticates_only_while_its_owner_holds_a_grant() {
        let (mut snapshot, presented) = with_token("U0PERSON1");
        assert_eq!(snapshot.authenticate(&presented), None);
        snapshot.grants.insert(user("U0PERSON1"));
        assert!(snapshot.authenticate(&presented).is_some());
        assert!(snapshot.may_chat(&user("U0PERSON1")));
        let access = Access::new(snapshot);
        let lost = access.replace(Snapshot {
            tokens: access.snapshot().tokens.clone(),
            ..Default::default()
        });
        assert_eq!(lost.len(), 1);
        assert_eq!(access.authenticate(&presented), None);
    }

    #[test]
    fn the_admins_nodes_need_no_grant_and_a_wrong_secret_never_passes() {
        let (snapshot, presented) = with_token("U0ADMIN01");
        assert!(snapshot.authenticate(&presented).is_some());
        let forged = format!("{}x", presented);
        assert_eq!(snapshot.authenticate(&forged), None);
    }

    #[test]
    fn the_allowed_node_admins_and_the_admin_may_chat_and_nobody_else() {
        let (mut snapshot, _) = with_token("U0ADMIN01");
        let guest = user("U0GUEST01");
        assert!(!snapshot.may_chat(&guest));
        snapshot.allowed.insert(guest.clone());
        assert!(snapshot.may_chat(&guest));
        let node_admin = user("U0PERSON1");
        snapshot.grants.insert(node_admin.clone());
        assert!(snapshot.may_chat(&node_admin));
        assert!(snapshot.may_chat(&user("U0ADMIN01")));
    }
}
