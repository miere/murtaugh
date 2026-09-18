use std::path::{Path, PathBuf};
use std::process::ExitCode;

use clap::Parser;
use murtaugh_gateway::cli::{Cli, Command, LaunchdArgs, VersionArgs};
use murtaugh_gateway::launchd::{self, Plan};
use murtaugh_gateway::logging::redact;
use murtaugh_gateway::version::{self, GitHub, VERSION};
use murtaugh_gateway::{admin, config, open_store};

fn main() -> ExitCode {
    let cli = Cli::parse();
    match dispatch(cli) {
        Ok(output) => {
            if !output.is_empty() {
                println!("{output}");
            }
            ExitCode::SUCCESS
        }
        Err(message) => {
            eprintln!("murtaugh-gateway: {}", redact(&message));
            ExitCode::FAILURE
        }
    }
}

fn config_path(flag: Option<PathBuf>, profile: &str) -> Result<PathBuf, String> {
    match flag {
        Some(path) => Ok(config::absolute(&path)),
        None => config::default_path(profile).map_err(|err| err.to_string()),
    }
}

fn dispatch(cli: Cli) -> Result<String, String> {
    match cli.command {
        Command::Launchd(args) => return install_launchd(cli.config, args),
        Command::Version(args) => return print_version(args),
        _ => {}
    }
    let path = config_path(cli.config, config::DEFAULT_PROFILE)?;
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(|err| format!("cannot start the async runtime: {err}"))?;
    match cli.command {
        Command::Run => runtime.block_on(murtaugh_gateway::run::run(&path)),
        Command::Validate => {
            let loaded = config::load(&path).map_err(|err| err.to_string())?;
            Ok(format!(
                "{} is valid\n{}",
                loaded.path.display(),
                murtaugh_gateway::run::banner(&loaded)
            ))
        }
        store_command => {
            let database = config::database(&path).map_err(|err| err.to_string())?;
            runtime.block_on(async move {
                let store = open_store(&database).await?;
                match store_command {
                    Command::Admin(command) => admin::admin(&*store, command).await,
                    Command::Grant(command) => admin::grant(&*store, command).await,
                    Command::User(command) => admin::user_settings(&*store, command).await,
                    Command::Node(command) => admin::node(&*store, command).await,
                    _ => Ok(String::new()),
                }
            })
        }
    }
}

fn install_launchd(flag: Option<PathBuf>, args: LaunchdArgs) -> Result<String, String> {
    launchd::ensure_macos().map_err(|err| err.to_string())?;
    let config = config_path(flag, &args.alias)?;
    let home =
        config::home().ok_or("HOME is not set, so there is no LaunchAgents folder to write to")?;
    let binary = match args.binary_path {
        Some(path) => config::absolute(&path),
        None => std::env::current_exe().map_err(|err| {
            format!("cannot find this murtaugh-gateway binary; pass --binary-path: {err}")
        })?,
    };
    let plan = Plan {
        alias: args.alias,
        binary,
        config,
        home,
    };
    let installed = launchd::install(&plan, args.update_existing).map_err(|err| err.to_string())?;
    let verb = if installed.replaced {
        "Replaced"
    } else {
        "Wrote"
    };
    let mut out = format!(
        "{verb} {} (label {}).",
        installed.path.display(),
        installed.label
    );
    if !plan.config.exists() {
        out.push_str(&format!(
            "\nNote: {} does not exist yet; create it before loading the job.",
            plan.config.display()
        ));
    }
    out.push_str(&format!(
        "\nLoad it with: launchctl bootstrap gui/$(id -u) {}",
        quote(&installed.path)
    ));
    Ok(out)
}

fn quote(path: &Path) -> String {
    format!("'{}'", path.display().to_string().replace('\'', r"'\''"))
}

fn print_version(args: VersionArgs) -> Result<String, String> {
    if !args.check {
        return Ok(format!("murtaugh-gateway {VERSION}"));
    }
    let checked = version::check(VERSION, &GitHub).map_err(|err| err.to_string())?;
    Ok(checked.to_string())
}
