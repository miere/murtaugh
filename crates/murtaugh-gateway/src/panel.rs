//! What the Home tab's controls do. Every change goes through `roles`, then the store is read back
//! into the live access rules, links that lost their credential are closed, and the Home tab is
//! published again for everyone the change touched. A click carries no channel to answer an error
//! into, so a denied or failed change is silent beyond the log: the tab keeps showing what the
//! store holds.

use std::collections::BTreeSet;
use std::sync::Arc;

use murtaugh_slack::{HomeClick, PostMessage, SlackClient, Upload, ViewSubmission};
use murtaugh_store::{Store, ToolMode, UserId};

use crate::access::{Access, Snapshot};
use crate::fleet::Fleet;
use crate::home::{self, MenuAction};
use crate::hub;
use crate::render;
use crate::roles::{self, Minted};

pub struct Panel {
    pub slack: SlackClient,
    pub store: Arc<dyn Store>,
    pub access: Access,
    pub fleet: Fleet,
    pub bot_user: String,
}

impl Panel {
    pub async fn show(&self, viewer: &UserId) {
        let tool_mode = match self.store.user(viewer).await {
            Ok(config) => config.tool_mode,
            Err(err) => {
                tracing::warn!(error = %err, "could not read the viewer's tool mode for the Home tab");
                ToolMode::default()
            }
        };
        let blocks = home::view(
            viewer,
            tool_mode,
            &self.access.snapshot(),
            &self.fleet.summaries(),
        );
        if let Err(err) = self.slack.publish_home(viewer.as_str(), &blocks).await {
            tracing::warn!(error = %err, "could not publish the Home tab");
        }
    }

    /// Reads the store back into the live rules, closing the links it no longer admits, then
    /// shows each touched person their Home tab as it now stands.
    async fn settle(&self, touched: impl IntoIterator<Item = UserId>) {
        match Snapshot::load(&*self.store).await {
            Ok(fresh) => hub::apply(&self.access, &self.fleet, fresh).await,
            Err(err) => {
                tracing::warn!(error = %err, "could not reload access after a Home tab change")
            }
        }
        let touched: BTreeSet<UserId> = touched.into_iter().collect();
        for person in &touched {
            self.show(person).await;
        }
    }

    /// People a user picker handed back, less the bot and anyone who is not a user at all.
    fn people(&self, raw: &[String]) -> BTreeSet<UserId> {
        raw.iter()
            .filter(|id| **id != self.bot_user)
            .filter_map(|id| UserId::parse(id).ok())
            .collect()
    }

    pub async fn click(&self, click: HomeClick) {
        let Ok(user) = UserId::parse(&click.user) else {
            return;
        };
        let snapshot = self.access.snapshot();
        let admin = snapshot.admin() == Some(&user);
        let node_admin = snapshot.may_run_nodes(&user);
        let denied = |what: &str| {
            tracing::info!(user = %click.user, action = what, "denied a Home tab change");
        };
        match click.action_id.as_str() {
            home::TOOL_MODE if node_admin => {
                let Ok(mode) = click.value.parse::<ToolMode>() else {
                    return;
                };
                if let Err(err) = self.store.set_tool_mode(&user, mode).await {
                    tracing::warn!(error = %err, "could not set a tool mode from the Home tab");
                }
                self.settle([user]).await;
            }
            home::ADMIN_SET if admin => {
                let Some(next) = self.people(std::slice::from_ref(&click.value)).pop_first() else {
                    return;
                };
                if next == user {
                    return;
                }
                if let Err(err) = roles::set_admin(&*self.store, &next).await {
                    tracing::warn!(error = %err, "could not hand the gateway over");
                }
                tracing::info!(from = %user, to = %next, "the gateway changed hands from the Home tab");
                self.settle([user, next]).await;
            }
            home::NODE_ADMINS_SET if admin => {
                let wanted = self.without(self.people(&click.users), &user);
                let current = self.without(snapshot.node_admins().cloned().collect(), &user);
                for person in wanted.difference(&current) {
                    if let Err(err) = roles::grant(&*self.store, person, &user).await {
                        tracing::warn!(%person, error = %err, "could not grant a node admin");
                    }
                }
                for person in current.difference(&wanted) {
                    if let Err(err) = roles::revoke_grant(&*self.store, person).await {
                        tracing::warn!(%person, error = %err, "could not revoke a node admin");
                    }
                }
                let changed = wanted.symmetric_difference(&current).cloned();
                self.settle(changed.chain([user.clone()]).collect::<Vec<_>>())
                    .await;
            }
            home::ALLOWED_USERS_SET if admin => {
                let wanted = self.without(self.people(&click.users), &user);
                let current = self.without(snapshot.allowed_users().cloned().collect(), &user);
                for person in wanted.difference(&current) {
                    if let Err(err) = roles::allow(&*self.store, person).await {
                        tracing::warn!(%person, error = %err, "could not allow someone");
                    }
                }
                for person in current.difference(&wanted) {
                    if let Err(err) = roles::disallow(&*self.store, person).await {
                        tracing::warn!(%person, error = %err, "could not disallow someone");
                    }
                }
                let changed = wanted.symmetric_difference(&current).cloned();
                self.settle(changed.chain([user.clone()]).collect::<Vec<_>>())
                    .await;
            }
            home::NODE_NEW if node_admin => {
                let modal = home::new_node_modal(&user, admin);
                if let Err(err) = self.slack.open_view(&click.trigger_id, &modal).await {
                    tracing::warn!(error = %err, "could not open the new node modal");
                }
            }
            home::NODE_MENU => {
                let Some((selector, action)) = home::parse_menu(&click.value) else {
                    return;
                };
                let Some(owner) = snapshot.owner(&selector).cloned() else {
                    return;
                };
                if !admin && owner != user {
                    denied(home::NODE_MENU);
                    return;
                }
                let disabled = match action {
                    MenuAction::Revoke => {
                        let name = snapshot.node_name(&selector).unwrap_or(&selector);
                        let modal = home::revoke_modal(&selector, name);
                        if let Err(err) = self.slack.open_view(&click.trigger_id, &modal).await {
                            tracing::warn!(error = %err, "could not open the revoke modal");
                        }
                        return;
                    }
                    MenuAction::Disable => true,
                    MenuAction::Enable => false,
                };
                if let Err(err) = self.store.set_node_disabled(&selector, disabled).await {
                    tracing::warn!(%selector, error = %err, "could not toggle a node's routing");
                    return;
                }
                self.settle([user, owner]).await;
            }
            other => denied(other),
        }
    }

    /// The admin is never picked into or out of a list: they may do all of it regardless, and
    /// removing themselves by accident must not strip their own machines.
    fn without(&self, mut people: BTreeSet<UserId>, admin: &UserId) -> BTreeSet<UserId> {
        people.remove(admin);
        people
    }

    pub async fn submit(&self, submission: ViewSubmission) {
        let Ok(user) = UserId::parse(&submission.user) else {
            return;
        };
        match submission.callback_id.as_str() {
            home::NODE_NEW_SUBMIT => self.mint(&user, &submission).await,
            home::NODE_REVOKE_SUBMIT => self.revoke(&user, &submission.private_metadata).await,
            _ => {}
        }
    }

    async fn mint(&self, user: &UserId, submission: &ViewSubmission) {
        let snapshot = self.access.snapshot();
        let admin = snapshot.admin() == Some(user);
        if !snapshot.may_run_nodes(user) {
            tracing::info!(%user, "denied minting a node to someone who may not run nodes");
            return;
        }
        let name = submission
            .value(home::NODE_NAME.0, home::NODE_NAME.1)
            .map(str::trim)
            .unwrap_or_default();
        if name.is_empty() {
            return;
        }
        // Only the admin mints for someone else; anyone else's owner field is ignored.
        let owner = match submission.value(home::NODE_OWNER.0, home::NODE_OWNER.1) {
            Some(picked) if admin => match self.people(&[picked.to_owned()]).pop_first() {
                Some(owner) => owner,
                None => return,
            },
            _ => user.clone(),
        };
        let minted = match roles::mint(&*self.store, &owner, name, user).await {
            Ok(minted) => minted,
            Err(err) => {
                tracing::warn!(%owner, error = %err, "could not mint a node from the Home tab");
                return;
            }
        };
        tracing::info!(%owner, by = %user, selector = %minted.selector, "minted a node from the Home tab");
        if let Err(reason) = self.deliver(&minted).await {
            tracing::warn!(%owner, selector = %minted.selector, %reason, "could not DM a new token; revoking it");
            if let Err(err) = self.store.revoke_node_token(&minted.selector).await {
                tracing::warn!(selector = %minted.selector, error = %err, "could not revoke an undelivered token");
            }
            let note = format!(
                "I couldn't send <@{owner}> the token for *{}*, so I revoked it. Try again from the Home tab.",
                render::escape(name)
            );
            if let Err(err) = self.slack.post_message(&dm(user, note)).await {
                tracing::warn!(error = %err, "could not say a token went undelivered");
            }
        }
        self.settle([user.clone(), owner]).await;
    }

    /// Sends the token to its owner alone, as a file under a note on what it is for.
    async fn deliver(&self, minted: &Minted) -> Result<(), String> {
        let note = home::token_message(&self.bot_user, &minted.name);
        let posted = self
            .slack
            .post_message(&dm(&minted.owner, note))
            .await
            .map_err(|err| err.to_string())?;
        self.slack
            .upload_file(&Upload {
                channel: posted.channel,
                thread_ts: None,
                filename: format!("{}.token", file_stem(&minted.name)),
                title: Some(format!("Node token for {}", minted.name)),
                initial_comment: None,
                bytes: format!("{}\n", minted.token).into_bytes(),
            })
            .await
            .map(drop)
            .map_err(|err| err.to_string())
    }

    async fn revoke(&self, user: &UserId, selector: &str) {
        let snapshot = self.access.snapshot();
        let Some(owner) = snapshot.owner(selector).cloned() else {
            return;
        };
        if snapshot.admin() != Some(user) && &owner != user {
            tracing::info!(%user, %selector, "denied revoking a node from someone who does not own it");
            return;
        }
        match self.store.revoke_node_token(selector).await {
            Ok(_) => tracing::info!(%selector, by = %user, "revoked a node from the Home tab"),
            Err(err) => tracing::warn!(%selector, error = %err, "could not revoke a node"),
        }
        self.settle([user.clone(), owner]).await;
    }
}

/// Posting to a person's id lands in the bot's DM with them, and names that DM's channel.
fn dm(user: &UserId, text: String) -> PostMessage {
    PostMessage {
        channel: user.to_string(),
        thread_ts: None,
        text,
        blocks: Vec::new(),
    }
}

/// A node's name as a file name: letters, digits, dashes and underscores, or `node`.
fn file_stem(name: &str) -> String {
    let stem: String = name
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '-' || c == '_' {
                c
            } else {
                '-'
            }
        })
        .collect();
    let stem = stem.trim_matches('-');
    if stem.is_empty() {
        "node".to_owned()
    } else {
        stem.to_owned()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_token_file_is_named_after_its_node_as_far_as_is_safe() {
        assert_eq!(file_stem("laptop"), "laptop");
        assert_eq!(file_stem("Miere's Mac"), "Miere-s-Mac");
        assert_eq!(file_stem("../../etc"), "etc");
        assert_eq!(file_stem("🙂"), "node");
    }
}
