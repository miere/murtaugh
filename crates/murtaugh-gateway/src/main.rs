use std::path::PathBuf;
use std::process::ExitCode;

use clap::Parser;
use murtaugh_gateway::cli::{Cli, Command};
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
            eprintln!("murtaugh-gateway: {message}");
            ExitCode::FAILURE
        }
    }
}

fn config_path(flag: Option<PathBuf>) -> Result<PathBuf, String> {
    flag.or_else(config::default_path)
        .ok_or_else(|| "HOME is not set; pass --config".to_owned())
}

fn dispatch(cli: Cli) -> Result<String, String> {
    let path = config_path(cli.config)?;
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(|err| format!("cannot start the async runtime: {err}"))?;
    match cli.command {
        Command::Run => runtime.block_on(murtaugh_gateway::run::run(&path)),
        Command::Validate => {
            let loaded = config::load(&path).map_err(|err| err.to_string())?;
            Ok(format!("{} is valid", loaded.path.display()))
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
                    Command::Run | Command::Validate => Ok(String::new()),
                }
            })
        }
    }
}
