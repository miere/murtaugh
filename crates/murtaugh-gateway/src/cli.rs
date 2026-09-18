use std::path::PathBuf;

use clap::{Args, Parser, Subcommand};

use crate::config::DEFAULT_PROFILE;

#[derive(Debug, Parser)]
#[command(
    name = "murtaugh-gateway",
    about = "Connects Slack to the AI agents running on your nodes",
    arg_required_else_help = true,
    disable_version_flag = true
)]
pub struct Cli {
    /// Configuration file [default: ~/.config/murtaugh/default/murtaugh.toml]
    #[arg(long, global = true, value_name = "PATH")]
    pub config: Option<PathBuf>,
    #[command(subcommand)]
    pub command: Command,
}

#[derive(Debug, Subcommand)]
pub enum Command {
    /// Connect to Slack and serve nodes until stopped
    Run,
    /// Check the bootstrap file without connecting to anything
    Validate,
    /// Write a macOS LaunchAgent that keeps `murtaugh-gateway run` alive
    Launchd(LaunchdArgs),
    /// Print the version, or check GitHub for a newer release
    Version(VersionArgs),
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
}

#[derive(Debug, Args)]
pub struct LaunchdArgs {
    /// Names the job murtaugh.<alias> and picks the profile's default config path
    #[arg(long, default_value = DEFAULT_PROFILE)]
    pub alias: String,
    /// The murtaugh-gateway binary launchd runs [default: this binary]
    #[arg(long, value_name = "PATH")]
    pub binary_path: Option<PathBuf>,
    /// Replace an existing plist
    #[arg(long)]
    pub update_existing: bool,
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
    /// Let a person run their own nodes; approve only someone you have spoken to
    Approve {
        user: String,
    },
    /// Take it back; their nodes are disconnected within seconds
    Revoke {
        user: String,
    },
    List,
}

#[derive(Debug, Subcommand)]
pub enum UserCommand {
    /// Pre-authorise a person on the admin's nodes
    Allow {
        user: String,
    },
    Disallow {
        user: String,
    },
    List,
}

#[derive(Debug, Subcommand)]
pub enum NodeCommand {
    /// Mint a credential for one of a person's nodes
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
