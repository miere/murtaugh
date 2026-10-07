use std::io::Read;
use std::path::PathBuf;
use std::process::ExitCode;

use agent_client_protocol::Stdio;
use clap::{Args, Parser, Subcommand};
use murtaugh_client::bridge::{self, Options};
use murtaugh_client::profile::{self, DEFAULT_PROFILE};
use murtaugh_common::logging;
use murtaugh_common::version::{self, GitHub, VERSION};

const BINARY: &str = "murtaugh-client";

#[derive(Debug, Parser)]
#[command(name = BINARY, version = VERSION, about = "A person's own client for Murtaugh's RAX API")]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Debug, Subcommand)]
enum Command {
    /// Bridge the editor that launched this process, over ACP on stdio, to Murtaugh
    Acp(AcpArgs),
    /// Save a client token, read from stdin, into a profile
    Login(LoginArgs),
    Version {
        /// Also ask GitHub whether a newer release exists
        #[arg(long)]
        check: bool,
    },
}

#[derive(Debug, Args)]
struct AcpArgs {
    /// Murtaugh's RAX address, such as wss://murtaugh.example.com; overrides the profile
    #[arg(long, value_name = "URL")]
    gateway: Option<String>,
    /// The client token file; overrides the profile
    #[arg(long, value_name = "PATH")]
    token_file: Option<PathBuf>,
    /// The namespace the editor's tools are lent under: [a-z0-9_], at most 27 characters
    #[arg(long, default_value = "acp")]
    name: String,
    /// Which saved profile to read
    #[arg(long, default_value = DEFAULT_PROFILE, value_name = "ALIAS")]
    profile: String,
}

#[derive(Debug, Args)]
struct LoginArgs {
    /// Also save Murtaugh's RAX address in the profile
    #[arg(long, value_name = "URL")]
    gateway: Option<String>,
    #[arg(long, default_value = DEFAULT_PROFILE, value_name = "ALIAS")]
    profile: String,
}

fn main() -> ExitCode {
    let cli = Cli::parse();
    let result = match cli.command {
        Command::Acp(args) => acp(args),
        Command::Login(args) => login(args),
        Command::Version { check } => print_version(check),
    };
    match result {
        Ok(()) => ExitCode::SUCCESS,
        Err(message) => {
            eprintln!("{BINARY}: {}", logging::redact(&message));
            ExitCode::FAILURE
        }
    }
}

/// stdout carries ACP, so logs go to a file and only a failure to start reaches stderr.
fn acp(args: AcpArgs) -> Result<(), String> {
    if !bridge::valid_name(&args.name) {
        return Err(format!(
            "--name {:?} must be [a-z0-9_] and at most 27 characters",
            args.name
        ));
    }
    let profile_path = profile::path(&args.profile).map_err(|err| err.to_string())?;
    let saved = profile::load(&profile_path).map_err(|err| err.to_string())?;
    let resolved = profile::resolve(&saved, &profile_path, args.gateway, args.token_file)
        .map_err(|err| err.to_string())?;
    rax::transport::resolve_endpoint(&resolved.gateway).map_err(|err| err.to_string())?;
    let token = profile::read_token(&resolved.token_file).map_err(|err| err.to_string())?;
    if let Some(home) = murtaugh_common::paths::home() {
        let log = home
            .join("Library/Logs/murtaugh/client")
            .join(format!("{}.log", args.profile));
        logging::init_file(&log, tracing::Level::INFO)
            .map_err(|err| format!("cannot open {}: {err}", log.display()))?;
    }
    murtaugh_common::tls::init();
    let options = Options {
        gateway: resolved.gateway,
        token,
        name: args.name,
    };
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(|err| format!("cannot start the async runtime: {err}"))?;
    tracing::info!(gateway = %options.gateway, name = %options.name, "{BINARY} {VERSION} bridging an editor");
    runtime
        .block_on(bridge::serve(options, Stdio::new()))
        .map_err(|err| err.to_string())
}

fn login(args: LoginArgs) -> Result<(), String> {
    let mut token = String::new();
    std::io::stdin()
        .read_to_string(&mut token)
        .map_err(|err| format!("cannot read the token from stdin: {err}"))?;
    let token = token.trim();
    if murtaugh_common::token::parse(murtaugh_common::token::USER_PREFIX, token).is_none() {
        return Err("that is not a client token; they start with mrtg_user_".to_owned());
    }
    if let Some(gateway) = &args.gateway {
        rax::transport::resolve_endpoint(gateway).map_err(|err| err.to_string())?;
    }
    let saved = profile::save(&args.profile, token, args.gateway).map_err(|err| err.to_string())?;
    eprintln!("Saved the token; profile {}.", saved.display());
    Ok(())
}

fn print_version(check: bool) -> Result<(), String> {
    if !check {
        println!("{BINARY} {VERSION}");
        return Ok(());
    }
    let checked =
        version::check(VERSION, &GitHub { binary: BINARY }).map_err(|err| err.to_string())?;
    println!("{}", checked.message(BINARY));
    Ok(())
}
