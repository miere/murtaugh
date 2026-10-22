//! Who may do what, and what each change carries with it, as `rax-gateway` rules it. Murtaugh only
//! names its tokens.

use std::collections::BTreeSet;

use murtaugh_store::{Result, Scope, Store, UserId};

pub use rax_gateway::roles::{
    Minted, Reconciled, allow, disallow, grant, reconcile, revoke_grant, set_admin,
};

pub async fn mint(store: &dyn Store, owner: &UserId, name: &str, by: &UserId) -> Result<Minted> {
    rax_gateway::roles::mint(store, crate::settings(), owner, name, by).await
}

pub async fn mint_client(
    store: &dyn Store,
    owner: &UserId,
    name: &str,
    scopes: BTreeSet<Scope>,
) -> Result<Minted> {
    rax_gateway::roles::mint_client(store, crate::settings(), owner, name, scopes).await
}
