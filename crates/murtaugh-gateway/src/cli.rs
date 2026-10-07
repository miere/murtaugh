use std::path::PathBuf;

use clap::{Args, Parser, Subcommand};

use crate::config::DEFAULT_ALIAS;

#[derive(Debug, Parser)]
#[command(
    name = "murtaugh-gateway",
    about = "Connects Slack to the AI agents running on your nodes",
    arg_required_else_help = true,
    disable_version_flag = true
)]
pub struct Cli {
    /// Configuration file [default: ~/.config/murtaugh/<alias>/murtaugh.toml]
    #[arg(long, global = true, value_name = "PATH")]
    pub config: Option<PathBuf>,
    /// Which gateway to act on: the launchd job murtaugh.<alias> and ~/.config/murtaugh/<alias>
    #[arg(long, global = true, default_value = DEFAULT_ALIAS, value_name = "NAME")]
    pub alias: String,
    #[command(subcommand)]
    pub command: Command,
}

#[derive(Debug, Subcommand)]
pub enum Command {
    /// Connect to Slack and serve nodes until stopped
    Run,
    /// Check the bootstrap file without connecting to anything
    Validate,
    /// Manage the macOS LaunchAgent that keeps `murtaugh-gateway run` alive
    #[command(subcommand)]
    Launchd(LaunchdCommand),
    /// Print the version, or check GitHub for a newer release
    Version(VersionArgs),
    /// Print the Slack app manifest to paste into api.slack.com/apps → "Create New App"
    SlackManifest(ManifestArgs),
    /// Who administers this gateway
    #[command(subcommand)]
    Admin(AdminCommand),
    /// People allowed to run their own nodes here
    #[command(subcommand)]
    Grant(GrantCommand),
    /// Per-person settings, such as pre-authorising someone on the admin's nodes
    #[command(subcommand)]
    User(UserCommand),
    /// Node credentials and the nodes attached right now
    #[command(subcommand)]
    Node(NodeCommand),
    /// How the tools agents use on a person's nodes are ruled on
    #[command(subcommand)]
    Tools(ToolsCommand),
}

#[derive(Debug, Subcommand)]
pub enum LaunchdCommand {
    /// Write the LaunchAgent for this alias
    Install(InstallArgs),
    /// Stop the job and delete its LaunchAgent
    Uninstall,
    /// Hand the job to launchd, which starts it and keeps it alive
    Start,
    /// Take the job off launchd, letting the gateway shut down cleanly
    Stop,
    /// Shut the gateway down cleanly and hand the job back to launchd
    Restart(RestartArgs),
    /// Say whether launchd has the job, whether it is running, and where its logs are
    Status,
}

#[derive(Debug, Args)]
pub struct InstallArgs {
    /// The murtaugh-gateway binary launchd runs [default: this binary]
    #[arg(long, value_name = "PATH")]
    pub binary_path: Option<PathBuf>,
    /// Replace an existing plist
    #[arg(long)]
    pub update_existing: bool,
}

#[derive(Debug, Args)]
pub struct RestartArgs {
    /// Kill the gateway instead of waiting for it to shut down cleanly
    #[arg(long)]
    pub force: bool,
}

#[derive(Debug, Args)]
pub struct ManifestArgs {
    /// The app's name, and its bot's display name in Slack
    #[arg(long, default_value = "Murtaugh")]
    pub name: String,
}

#[derive(Debug, Args)]
pub struct VersionArgs {
    /// Ask GitHub whether a newer release exists; set GH_TOKEN, as the repository is private
    #[arg(long)]
    pub check: bool,
}

#[derive(Debug, Subcommand)]
pub enum AdminCommand {
    /// Make a Slack user the admin
    Set {
        user: String,
    },
    Show,
}

#[derive(Debug, Subcommand)]
pub enum GrantCommand {
    /// Let a person run their own nodes, and use the gateway; approve only someone you have
    /// spoken to
    Approve {
        user: String,
    },
    /// Take it back; every token they hold is revoked for good, and their nodes are disconnected
    /// within seconds. They may still use the gateway
    Revoke {
        user: String,
    },
    List,
}

#[derive(Debug, Subcommand)]
pub enum UserCommand {
    /// Let a person use the gateway, on any node whose owner lets them in
    Allow {
        user: String,
    },
    /// Take it back, with their grant and every token they hold
    Disallow {
        user: String,
    },
    List,
    /// Credentials for a person's own clients, such as `murtaugh-client acp`
    #[command(subcommand)]
    Token(UserTokenCommand),
}

#[derive(Debug, Subcommand)]
pub enum UserTokenCommand {
    /// Mint a client credential for a person, allowing them on the gateway if need be
    Mint(UserTokenMintArgs),
    /// Revoke a client credential; its client is disconnected within seconds
    Revoke { selector: String },
    /// Every client credential and whether it is revoked
    List,
}

#[derive(Debug, Args)]
pub struct UserTokenMintArgs {
    /// The person the client belongs to
    #[arg(long)]
    pub owner: String,
    /// A name for the client, such as "editor"
    #[arg(long)]
    pub name: String,
    /// An entry point the token opens; repeat it for several. `rax` is the RAX API
    #[arg(long = "scope", value_name = "SCOPE", default_values_t = vec!["rax".to_owned()])]
    pub scopes: Vec<String>,
    /// Write the token to this file (mode 0600) instead of printing it
    #[arg(long, value_name = "PATH")]
    pub out: Option<PathBuf>,
}

#[derive(Debug, Subcommand)]
pub enum ToolsCommand {
    /// always-allowed (the default), allowed-whitelist (ask the owner for anything off the
    /// whitelist) or denied
    Mode { user: String, mode: String },
    /// Put a tool on the owner's whitelist; the name is the one the approval card shows
    Allow { user: String, tool: String },
    /// Take a tool off the owner's whitelist
    Disallow { user: String, tool: String },
    /// The owner's mode and whitelist
    Show { user: String },
}

#[derive(Debug, Subcommand)]
pub enum NodeCommand {
    /// Mint a credential for one of a person's nodes, granting them first if need be
    Mint(MintArgs),
    /// Revoke a credential; its node is disconnected within seconds
    Revoke { selector: String },
    /// Every credential and whether it is revoked
    List,
}

#[derive(Debug, Args)]
pub struct MintArgs {
    /// The person who runs the node
    #[arg(long)]
    pub owner: String,
    /// A name for the node, such as "laptop"
    #[arg(long)]
    pub name: String,
    /// Write the token to this file (mode 0600) instead of printing it
    #[arg(long, value_name = "PATH")]
    pub token_file: Option<PathBuf>,
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::panic)]

    use clap::CommandFactory;

    use super::*;

    fn parse(args: &[&str]) -> Result<Cli, clap::Error> {
        Cli::try_parse_from(std::iter::once("murtaugh-gateway").chain(args.iter().copied()))
    }

    #[test]
    fn the_definition_is_consistent() {
        Cli::command().debug_assert();
    }

    #[test]
    fn the_alias_defaults_and_is_global() {
        assert_eq!(parse(&["validate"]).unwrap().alias, "default");
        assert_eq!(parse(&["--alias", "work", "run"]).unwrap().alias, "work");
        assert_eq!(
            parse(&["launchd", "restart", "--alias", "work"])
                .unwrap()
                .alias,
            "work"
        );
    }

    #[test]
    fn a_restart_is_graceful_unless_forced() {
        for (args, forced) in [
            (&["launchd", "restart"][..], false),
            (&["launchd", "restart", "--force"][..], true),
        ] {
            let Command::Launchd(LaunchdCommand::Restart(restart)) = parse(args).unwrap().command
            else {
                panic!("expected launchd restart")
            };
            assert_eq!(restart.force, forced);
        }
    }

    #[test]
    fn launchd_on_its_own_asks_for_a_verb() {
        assert_eq!(parse(&["launchd"]).unwrap_err().exit_code(), 2);
    }
}
