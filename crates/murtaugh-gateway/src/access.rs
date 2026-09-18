//! Who may do what, read from the store and refreshed on a timer, so a CLI change reaches the
//! live gateway without a restart. Lookups are synchronous because RAX authenticates mid-handshake.

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, RwLock};

use murtaugh_store::{NodeToken, Store, StoreError, UserId};
use rax_tokio::gateway::{Authenticator, NodeIdentity};

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

    pub fn node_name(&self, selector: &str) -> Option<&str> {
        self.tokens.get(selector).map(|record| record.name.as_str())
    }

    fn owns_a_node(&self, user: &UserId) -> bool {
        self.tokens
            .values()
            .any(|record| &record.owner == user && record.revoked_at.is_none())
    }

    /// People with nodes of their own, or pre-authorised on the admin's. Everyone else is ignored.
    pub fn may_chat(&self, user: &UserId) -> bool {
        self.allowed.contains(user) || (self.may_run_nodes(user) && self.owns_a_node(user))
    }

    /// Whose nodes a person may fall back to when none of their own is online.
    pub fn fallback_owners(&self, user: &UserId) -> Vec<UserId> {
        match &self.admin {
            Some(admin) if admin != user && self.allowed.contains(user) => vec![admin.clone()],
            _ => Vec::new(),
        }
    }
}

/// Cheap to clone; every clone sees the latest snapshot.
#[derive(Clone, Default)]
pub struct Access {
    current: Arc<RwLock<Arc<Snapshot>>>,
}

impl Access {
    pub fn new(snapshot: Snapshot) -> Self {
        Self {
            current: Arc::new(RwLock::new(Arc::new(snapshot))),
        }
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
        self.snapshot().authenticate(token).map(NodeIdentity)
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
    fn people_without_nodes_chat_only_when_pre_authorised_and_fall_back_to_the_admin() {
        let (mut snapshot, _) = with_token("U0ADMIN01");
        let guest = user("U0GUEST01");
        assert!(!snapshot.may_chat(&guest));
        assert!(snapshot.fallback_owners(&guest).is_empty());
        snapshot.allowed.insert(guest.clone());
        assert!(snapshot.may_chat(&guest));
        assert_eq!(snapshot.fallback_owners(&guest), [user("U0ADMIN01")]);
    }
}
