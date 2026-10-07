use std::path::PathBuf;
use std::process::ExitCode;

use clap::Parser;
use murtaugh_gateway::cli::{Cli, Command, InstallArgs, LaunchdCommand, VersionArgs};
use murtaugh_gateway::launchd::{self, Job, Launchctl, Plan};
use murtaugh_gateway::logging::redact;
use murtaugh_gateway::version::{self, GitHub, VERSION};

const BINARY: &str = "murtaugh-gateway";
use murtaugh_gateway::{admin, config, open_store};

fn main() -> ExitCode {
    murtaugh_gateway::tls::init();
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

fn config_path(flag: Option<PathBuf>, alias: &str) -> Result<PathBuf, String> {
    match flag {
        Some(path) => Ok(config::absolute(&path)),
        None => config::default_path(alias).map_err(|err| err.to_string()),
    }
}

fn dispatch(cli: Cli) -> Result<String, String> {
    match cli.command {
        Command::Launchd(command) => return launchd_command(cli.config, &cli.alias, command),
        Command::Version(args) => return print_version(args),
        Command::SlackManifest(args) => return murtaugh_gateway::manifest::render(&args.name),
        _ => {}
    }
    let path = config_path(cli.config, &cli.alias)?;
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
                    Command::Tools(command) => admin::tools(&*store, command).await,
                    _ => Ok(String::new()),
                }
            })
        }
    }
}

fn launchd_command(
    flag: Option<PathBuf>,
    alias: &str,
    command: LaunchdCommand,
) -> Result<String, String> {
    launchd::ensure_macos(BINARY).map_err(|err| err.to_string())?;
    let home =
        config::home().ok_or("HOME is not set, so there is no LaunchAgents folder to write to")?;
    let job = Job::new(BINARY, alias, home).map_err(|err| err.to_string())?;
    if let LaunchdCommand::Install(args) = command {
        return install_launchd(flag, job, args);
    }
    // Only install reads the configuration; the rest address the job --alias names.
    if flag.is_some() {
        return Err(format!(
            "--config only applies to `murtaugh-gateway launchd install`; {} acts on the job --alias names",
            job.label()
        ));
    }
    if let LaunchdCommand::Status = command {
        let status = launchd::status(&job, &Launchctl).map_err(|err| err.to_string())?;
        return Ok(status.to_string());
    }
    let outcome = match command {
        LaunchdCommand::Install(_) | LaunchdCommand::Status => unreachable!("handled above"),
        LaunchdCommand::Uninstall => launchd::uninstall(&job, &Launchctl),
        LaunchdCommand::Start => launchd::start(&job, &Launchctl),
        LaunchdCommand::Stop => launchd::stop(&job, &Launchctl),
        LaunchdCommand::Restart(args) => launchd::restart(&job, &Launchctl, args.force),
    }
    .map_err(|err| err.to_string())?;
    Ok(outcome.message(&job.label()))
}

fn install_launchd(flag: Option<PathBuf>, job: Job, args: InstallArgs) -> Result<String, String> {
    let config = config_path(flag, job.alias())?;
    let binary = match args.binary_path {
        Some(path) => config::absolute(&path),
        None => std::env::current_exe().map_err(|err| {
            format!("cannot find this murtaugh-gateway binary; pass --binary-path: {err}")
        })?,
    };
    let plan = Plan {
        job,
        binary,
        arguments: vec![
            "--config".to_owned(),
            config.display().to_string(),
            "run".to_owned(),
        ],
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
    if !config.exists() {
        out.push_str(&format!(
            "\nNote: {} does not exist yet; create it before starting the job.",
            config.display()
        ));
    }
    out.push_str(&format!(
        "\nStart it with: murtaugh-gateway launchd start{}",
        alias_flag(&plan.job)
    ));
    Ok(out)
}

fn alias_flag(job: &Job) -> String {
    if job.alias() == config::DEFAULT_ALIAS {
        String::new()
    } else {
        format!(" --alias {}", job.alias())
    }
}

fn print_version(args: VersionArgs) -> Result<String, String> {
    if !args.check {
        return Ok(format!("murtaugh-gateway {VERSION}"));
    }
    let checked =
        version::check(VERSION, &GitHub { binary: BINARY }).map_err(|err| err.to_string())?;
    Ok(checked.message(BINARY))
}
