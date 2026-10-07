//! The CLI's store commands. They write to the same store the gateway reads, and the gateway
//! picks the change up on its next refresh.

use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;

use murtaugh_store::{Store, ToolMode, ToolModeError, UserId};

use crate::cli::{
    AdminCommand, GrantCommand, MintArgs, NodeCommand, ToolsCommand, UserCommand, UserTokenCommand,
    UserTokenMintArgs,
};
use crate::roles;

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
            let previous = store.admin().await.map_err(|err| err.to_string())?;
            roles::set_admin(store, &admin)
                .await
                .map_err(|err| err.to_string())?;
            Ok(match previous.filter(|previous| previous != &admin) {
                Some(previous) => {
                    format!("{admin} is the admin; {previous} keeps their nodes as a node admin.")
                }
                None => format!("{admin} is the admin."),
            })
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
            let grant = roles::grant(store, &person, &admin)
                .await
                .map_err(|err| err.to_string())?;
            Ok(format!(
                "{} may run nodes and use the gateway (approved by {} at {}).",
                grant.user, grant.approved_by, grant.approved_at
            ))
        }
        GrantCommand::Revoke { user: raw } => {
            let person = user(&raw)?;
            let (had, revoked) = roles::revoke_grant(store, &person)
                .await
                .map_err(|err| err.to_string())?;
            Ok(if had {
                format!(
                    "{person} may no longer run nodes; {} revoked, and their nodes will be disconnected. They may still use the gateway.",
                    tokens(revoked.len())
                )
            } else {
                format!("{person} had no grant.")
            })
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
            roles::allow(store, &person)
                .await
                .map_err(|err| err.to_string())?;
            Ok(format!(
                "{person} may use the gateway, on any node whose owner lets them in."
            ))
        }
        UserCommand::Disallow { user: raw } => {
            let person = user(&raw)?;
            let revoked = roles::disallow(store, &person)
                .await
                .map_err(|err| err.to_string())?;
            Ok(if revoked.is_empty() {
                format!("{person} may no longer use the gateway.")
            } else {
                format!(
                    "{person} may no longer use the gateway or run nodes; {} revoked.",
                    tokens(revoked.len())
                )
            })
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
        UserCommand::Token(command) => user_token(store, command).await,
    }
}

async fn user_token(store: &dyn Store, command: UserTokenCommand) -> Result<String, String> {
    match command {
        UserTokenCommand::Mint(args) => mint_client(store, args).await,
        UserTokenCommand::Revoke { selector } => Ok(
            if store
                .revoke_user_token(&selector)
                .await
                .map_err(|err| err.to_string())?
            {
                format!("Revoked {selector}; its client will be disconnected.")
            } else {
                format!("No live client credential {selector}.")
            },
        ),
        UserTokenCommand::List => {
            let tokens = store.user_tokens().await.map_err(|err| err.to_string())?;
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

async fn mint_client(store: &dyn Store, args: UserTokenMintArgs) -> Result<String, String> {
    let owner = user(&args.owner)?;
    admin_of(store).await?;
    let name = args.name.trim().to_owned();
    if name.is_empty() {
        return Err("--name must not be empty".to_owned());
    }
    let minted = roles::mint_client(store, &owner, &name)
        .await
        .map_err(|err| err.to_string())?;
    match args.out {
        Some(path) => {
            write_secret(&path, &minted.token)?;
            Ok(format!(
                "Minted {} for {owner}'s client {name:?}; the token is in {}.",
                minted.selector,
                path.display()
            ))
        }
        None => Ok(format!(
            "Minted {} for {owner}'s client {name:?}. Hand this token over in person; it is not shown again:\n{}",
            minted.selector, minted.token
        )),
    }
}

pub async fn tools(store: &dyn Store, command: ToolsCommand) -> Result<String, String> {
    match command {
        ToolsCommand::Mode { user: raw, mode } => {
            let owner = user(&raw)?;
            let mode: ToolMode = mode.parse().map_err(|err: ToolModeError| err.to_string())?;
            store
                .set_tool_mode(&owner, mode)
                .await
                .map_err(|err| err.to_string())?;
            Ok(format!("Tools on {owner}'s nodes are now {mode}."))
        }
        ToolsCommand::Allow { user: raw, tool } => {
            let owner = user(&raw)?;
            let added = store
                .whitelist_tool(&owner, tool.trim())
                .await
                .map_err(|err| err.to_string())?;
            Ok(if added {
                format!("{} is on {owner}'s whitelist.", tool.trim())
            } else {
                format!("{} was already on {owner}'s whitelist.", tool.trim())
            })
        }
        ToolsCommand::Disallow { user: raw, tool } => {
            let owner = user(&raw)?;
            let removed = store
                .unwhitelist_tool(&owner, tool.trim())
                .await
                .map_err(|err| err.to_string())?;
            Ok(if removed {
                format!("{} is off {owner}'s whitelist.", tool.trim())
            } else {
                format!("{} was not on {owner}'s whitelist.", tool.trim())
            })
        }
        ToolsCommand::Show { user: raw } => {
            let config = store
                .user(&user(&raw)?)
                .await
                .map_err(|err| err.to_string())?;
            let mut lines = vec![format!("mode\t{}", config.tool_mode)];
            lines.extend(
                config
                    .whitelist
                    .iter()
                    .map(|tool| format!("allowed\t{tool}")),
            );
            Ok(lines.join("\n"))
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
    let name = args.name.trim().to_owned();
    if name.is_empty() {
        return Err("--name must not be empty".to_owned());
    }
    let minted = roles::mint(store, &owner, &name, &admin)
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

fn tokens(count: usize) -> String {
    match count {
        1 => "1 token".to_owned(),
        count => format!("{count} tokens"),
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
