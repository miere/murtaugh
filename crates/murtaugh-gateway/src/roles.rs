//! Who may do what, and what each change carries with it, as `rax-gateway` rules it. Murtaugh only
//! names its tokens.

use std::collections::BTreeSet;

use murtaugh_store::{Result, Scope, Store, UserId};

pub use rax_gateway::roles::{
    Minted, Reconciled, allow, disallow, grant, reconcile, revoke_grant, set_admin,
};

/// A node's owner without tool rules of their own starts on the read-only whitelist.
pub async fn mint(store: &dyn Store, owner: &UserId, name: &str, by: &UserId) -> Result<Minted> {
    let minted = rax_gateway::roles::mint(store, crate::settings(), owner, name, by).await?;
    rax_gateway::policy::seed(store, owner).await?;
    Ok(minted)
}

/// Starts every node owner who has no tool rules of their own on the default mode and the
/// read-only whitelist. Returns who it started.
pub async fn seed_owners(store: &dyn Store) -> Result<Vec<UserId>> {
    let owners: BTreeSet<UserId> = store
        .node_tokens()
        .await?
        .into_iter()
        .filter(|token| token.revoked_at.is_none())
        .map(|token| token.owner)
        .collect();
    let mut seeded = Vec::new();
    for owner in owners {
        if rax_gateway::policy::seed(store, &owner).await? {
            seeded.push(owner);
        }
    }
    Ok(seeded)
}

pub async fn mint_client(
    store: &dyn Store,
    owner: &UserId,
    name: &str,
    scopes: BTreeSet<Scope>,
) -> Result<Minted> {
    rax_gateway::roles::mint_client(store, crate::settings(), owner, name, scopes).await
}
