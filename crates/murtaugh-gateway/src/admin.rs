//! The CLI's store commands. They write to the same store the gateway reads, and the gateway
//! picks the change up on its next refresh.

use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;

use murtaugh_store::{NodeToken, Store, UserId};
use time::OffsetDateTime;

use crate::cli::{AdminCommand, GrantCommand, MintArgs, NodeCommand, UserCommand};
use crate::token;

fn user(raw: &str) -> Result<UserId, String> {
    UserId::parse(raw).map_err(|err| err.to_string())
}

async fn admin_of(store: &dyn Store) -> Result<UserId, String> {
    store
        .admin()
        .await
        .map_err(|err| err.to_string())?
        .ok_or_else(|| {
            "there is no admin yet; run `murtaugh-gateway admin set <slack user id>` first"
                .to_owned()
        })
}

pub async fn admin(store: &dyn Store, command: AdminCommand) -> Result<String, String> {
    match command {
        AdminCommand::Set { user: raw } => {
            let admin = user(&raw)?;
            store
                .set_admin(&admin)
                .await
                .map_err(|err| err.to_string())?;
            Ok(format!("{admin} is the admin."))
        }
        AdminCommand::Show => Ok(match store.admin().await.map_err(|err| err.to_string())? {
            Some(admin) => admin.to_string(),
            None => "no admin yet".to_owned(),
        }),
    }
}

pub async fn grant(store: &dyn Store, command: GrantCommand) -> Result<String, String> {
    match command {
        GrantCommand::Approve { user: raw } => {
            let person = user(&raw)?;
            let admin = admin_of(store).await?;
            let grant = store
                .approve(&person, &admin)
                .await
                .map_err(|err| err.to_string())?;
            Ok(format!(
                "{} may run nodes (approved by {} at {}).",
                grant.user, grant.approved_by, grant.approved_at
            ))
        }
        GrantCommand::Revoke { user: raw } => {
            let person = user(&raw)?;
            Ok(
                if store.revoke(&person).await.map_err(|err| err.to_string())? {
                    format!("{person} may no longer run nodes; theirs will be disconnected.")
                } else {
                    format!("{person} had no grant.")
                },
            )
        }
        GrantCommand::List => {
            let grants = store.grants().await.map_err(|err| err.to_string())?;
            Ok(grants
                .iter()
                .map(|grant| {
                    format!(
                        "{}\tapproved by {} at {}",
                        grant.user, grant.approved_by, grant.approved_at
                    )
                })
                .collect::<Vec<_>>()
                .join("\n"))
        }
    }
}

pub async fn user_settings(store: &dyn Store, command: UserCommand) -> Result<String, String> {
    match command {
        UserCommand::Allow { user: raw } => {
            let person = user(&raw)?;
            admin_of(store).await?;
            store
                .set_allowed(&person, true)
                .await
                .map_err(|err| err.to_string())?;
            Ok(format!("{person} may use the admin's nodes."))
        }
        UserCommand::Disallow { user: raw } => {
            let person = user(&raw)?;
            store
                .set_allowed(&person, false)
                .await
                .map_err(|err| err.to_string())?;
            Ok(format!("{person} may no longer use the admin's nodes."))
        }
        UserCommand::List => {
            let users = store.users().await.map_err(|err| err.to_string())?;
            Ok(users
                .iter()
                .filter(|config| config.allowed)
                .map(|config| format!("{}\tallowed", config.user))
                .collect::<Vec<_>>()
                .join("\n"))
        }
    }
}

pub async fn node(store: &dyn Store, command: NodeCommand) -> Result<String, String> {
    match command {
        NodeCommand::Mint(args) => mint(store, args).await,
        NodeCommand::Revoke { selector } => Ok(
            if store
                .revoke_node_token(&selector)
                .await
                .map_err(|err| err.to_string())?
            {
                format!("Revoked {selector}; its node will be disconnected.")
            } else {
                format!("No live credential {selector}.")
            },
        ),
        NodeCommand::List => {
            let tokens = store.node_tokens().await.map_err(|err| err.to_string())?;
            Ok(tokens
                .iter()
                .map(|token| {
                    let state = if token.revoked_at.is_some() {
                        "revoked"
                    } else {
                        "live"
                    };
                    format!(
                        "{}\t{}\t{}\t{state}",
                        token.selector, token.owner, token.name
                    )
                })
                .collect::<Vec<_>>()
                .join("\n"))
        }
    }
}

async fn mint(store: &dyn Store, args: MintArgs) -> Result<String, String> {
    let owner = user(&args.owner)?;
    let admin = admin_of(store).await?;
    let granted = store
        .grants()
        .await
        .map_err(|err| err.to_string())?
        .iter()
        .any(|grant| grant.user == owner);
    if owner != admin && !granted {
        return Err(format!(
            "{owner} has no grant; run `murtaugh-gateway grant approve {owner}` first"
        ));
    }
    let name = args.name.trim().to_owned();
    if name.is_empty() {
        return Err("--name must not be empty".to_owned());
    }
    let minted = token::mint();
    store
        .add_node_token(&NodeToken {
            selector: minted.selector.clone(),
            secret_hash: minted.secret_hash,
            owner: owner.clone(),
            name: name.clone(),
            created_at: OffsetDateTime::now_utc(),
            revoked_at: None,
        })
        .await
        .map_err(|err| err.to_string())?;
    match args.token_file {
        Some(path) => {
            write_secret(&path, &minted.token)?;
            Ok(format!(
                "Minted {} for {owner}'s node {name:?}; the token is in {}.",
                minted.selector,
                path.display()
            ))
        }
        None => Ok(format!(
            "Minted {} for {owner}'s node {name:?}. Hand this token over in person; it is not shown again:\n{}",
            minted.selector, minted.token
        )),
    }
}

fn write_secret(path: &Path, token: &str) -> Result<(), String> {
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)
        .map_err(|err| format!("{}: {err}", path.display()))?;
    writeln!(file, "{token}").map_err(|err| format!("{}: {err}", path.display()))
}
