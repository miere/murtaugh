//! The bootstrap file, `~/.config/murtaugh/<alias>/murtaugh.toml`: Slack credentials, where the
//! configuration store lives, and where nodes dial. Everything else is in the store.

use std::collections::BTreeMap;
use std::fmt;
use std::io;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};

use serde::Deserialize;

pub const FILE_NAME: &str = "murtaugh.toml";
pub const DEFAULT_ALIAS: &str = "default";
pub const DEFAULT_LISTEN: &str = "127.0.0.1:7443";
const DEFAULT_DATABASE: &str = "murtaugh.db";
const DEFAULT_ENV_FILE: &str = ".env";

#[derive(Debug, Clone)]
pub struct Config {
    pub path: PathBuf,
    pub dir: PathBuf,
    pub slack: SlackTokens,
    pub database: Database,
    pub listen: SocketAddr,
    pub log: LogConfig,
}

#[derive(Clone)]
pub struct SlackTokens {
    pub app_token: String,
    pub bot_token: String,
}

impl fmt::Debug for SlackTokens {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("SlackTokens { .. }")
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Database {
    Sqlite { path: PathBuf },
    Firestore(FirestoreConfig),
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct FirestoreConfig {
    pub project_id: Option<String>,
    pub database_id: Option<String>,
    pub collection: Option<String>,
    pub credentials_file: Option<PathBuf>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LogFormat {
    Text,
    Json,
}

#[derive(Debug, Clone)]
pub struct LogConfig {
    pub level: tracing::Level,
    pub format: LogFormat,
    /// Off by default: one info line per turn, timing the node's acceptance and first output.
    pub turn_timings: bool,
}

#[derive(Debug, thiserror::Error)]
pub enum ConfigError {
    #[error("no configuration file at {path}; create it or pass --config PATH")]
    Missing { path: PathBuf },
    #[error("cannot read {path}: {source}")]
    Unreadable {
        path: PathBuf,
        #[source]
        source: io::Error,
    },
    #[error("{path} is not valid Murtaugh configuration: {message}")]
    Syntax { path: PathBuf, message: String },
    #[error("{path} has problems:\n{problems}")]
    Invalid { path: PathBuf, problems: Problems },
    #[error(
        "HOME is not set, so the default configuration path cannot be found; pass --config PATH"
    )]
    NoHome,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Problem {
    pub field: String,
    pub message: String,
}

/// All of them at once, so an operator fixes a config in one pass.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct Problems(pub Vec<Problem>);

impl Problems {
    fn add(&mut self, field: &str, message: impl Into<String>) {
        self.0.push(Problem {
            field: field.to_owned(),
            message: message.into(),
        });
    }
}

impl fmt::Display for Problems {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let lines: Vec<String> = self
            .0
            .iter()
            .map(|problem| format!("  - {}: {}", problem.field, problem.message))
            .collect();
        f.write_str(&lines.join("\n"))
    }
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct File {
    env_file: Option<String>,
    #[serde(default)]
    slack: SlackFile,
    #[serde(default)]
    database: DatabaseFile,
    #[serde(default)]
    nodes: NodesFile,
    #[serde(default)]
    log: LogFile,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct SlackFile {
    app_token: Option<String>,
    bot_token: Option<String>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct DatabaseFile {
    backend: Option<String>,
    #[serde(default)]
    sqlite: SqliteFile,
    #[serde(default)]
    firestore: FirestoreFile,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct SqliteFile {
    path: Option<String>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct FirestoreFile {
    project_id: Option<String>,
    database_id: Option<String>,
    collection: Option<String>,
    credentials_file: Option<String>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct NodesFile {
    listen: Option<String>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct LogFile {
    level: Option<String>,
    format: Option<String>,
    turn_timings: Option<bool>,
}

pub fn home() -> Option<PathBuf> {
    std::env::var_os("HOME")
        .filter(|home| !home.is_empty())
        .map(PathBuf::from)
}

pub fn default_path(alias: &str) -> Result<PathBuf, ConfigError> {
    let home = home().ok_or(ConfigError::NoHome)?;
    Ok(home.join(".config/murtaugh").join(alias).join(FILE_NAME))
}

pub fn absolute(path: &Path) -> PathBuf {
    std::path::absolute(path).unwrap_or_else(|_| path.to_path_buf())
}

fn read(path: &Path) -> Result<File, ConfigError> {
    let text = match std::fs::read_to_string(path) {
        Ok(text) => text,
        Err(err) if err.kind() == io::ErrorKind::NotFound => {
            return Err(ConfigError::Missing {
                path: path.to_path_buf(),
            });
        }
        Err(source) => {
            return Err(ConfigError::Unreadable {
                path: path.to_path_buf(),
                source,
            });
        }
    };
    toml::from_str(&text).map_err(|err| ConfigError::Syntax {
        path: path.to_path_buf(),
        message: err.to_string().trim().to_owned(),
    })
}

fn dir_of(path: &Path) -> PathBuf {
    path.parent()
        .map(Path::to_path_buf)
        .unwrap_or_else(|| PathBuf::from("/"))
}

fn expand_home(raw: &str) -> PathBuf {
    match (raw.strip_prefix("~/"), home()) {
        (Some(rest), Some(home)) => home.join(rest),
        _ => PathBuf::from(raw),
    }
}

/// Relative paths are resolved against the configuration's folder, so an alias is self-contained.
fn resolve(dir: &Path, raw: &str) -> PathBuf {
    let path = expand_home(raw.trim());
    if path.is_absolute() {
        path
    } else {
        dir.join(path)
    }
}

/// `${VAR}` reads the process environment first, then `env_file` (by default a `.env` beside the
/// file), so secrets never have to sit in the TOML itself.
pub fn load(path: &Path) -> Result<Config, ConfigError> {
    let path = absolute(path);
    let file = read(&path)?;
    let dir = dir_of(&path);
    let mut problems = Problems::default();
    let env = env_file(&dir, file.env_file.as_deref(), &mut problems);
    let slack = SlackTokens {
        app_token: token(
            &mut problems,
            "slack.app_token",
            file.slack.app_token,
            "xapp-",
            &env,
        ),
        bot_token: token(
            &mut problems,
            "slack.bot_token",
            file.slack.bot_token,
            "xoxb-",
            &env,
        ),
    };
    let database = database_of(&dir, file.database, &mut problems);
    let listen_raw = file
        .nodes
        .listen
        .unwrap_or_else(|| DEFAULT_LISTEN.to_owned());
    let listen = match listen_raw.trim().parse::<SocketAddr>() {
        Ok(listen) => Some(listen),
        Err(err) => {
            problems.add(
                "nodes.listen",
                format!("{listen_raw:?} is not an address like {DEFAULT_LISTEN}: {err}"),
            );
            None
        }
    };
    let log = log(&file.log, &mut problems);
    match (database, listen, log) {
        (Some(database), Some(listen), Some(log)) if problems.0.is_empty() => Ok(Config {
            path,
            dir,
            slack,
            database,
            listen,
            log,
        }),
        _ => Err(ConfigError::Invalid { path, problems }),
    }
}

/// Only the database section, so admin commands work before the Slack credentials exist.
pub fn database(path: &Path) -> Result<Database, ConfigError> {
    let path = absolute(path);
    let file = read(&path)?;
    let mut problems = Problems::default();
    match database_of(&dir_of(&path), file.database, &mut problems) {
        Some(database) if problems.0.is_empty() => Ok(database),
        _ => Err(ConfigError::Invalid { path, problems }),
    }
}

fn database_of(dir: &Path, section: DatabaseFile, problems: &mut Problems) -> Option<Database> {
    match section
        .backend
        .as_deref()
        .map(str::trim)
        .unwrap_or("sqlite")
    {
        "sqlite" => {
            let raw = section
                .sqlite
                .path
                .unwrap_or_else(|| DEFAULT_DATABASE.to_owned());
            Some(Database::Sqlite {
                path: resolve(dir, &raw),
            })
        }
        "firestore" => {
            let firestore = section.firestore;
            Some(Database::Firestore(FirestoreConfig {
                project_id: configured(firestore.project_id),
                database_id: configured(firestore.database_id),
                collection: configured(firestore.collection),
                credentials_file: configured(firestore.credentials_file)
                    .map(|raw| resolve(dir, &raw)),
            }))
        }
        other => {
            problems.add(
                "database.backend",
                format!("{other:?} is not a backend; use sqlite or firestore"),
            );
            None
        }
    }
}

fn configured(value: Option<String>) -> Option<String> {
    value
        .map(|value| value.trim().to_owned())
        .filter(|value| !value.is_empty())
}

fn token(
    problems: &mut Problems,
    field: &str,
    raw: Option<String>,
    prefix: &str,
    env: &BTreeMap<String, String>,
) -> String {
    let value = raw
        .map(|raw| expand(&raw, env))
        .unwrap_or_default()
        .trim()
        .to_owned();
    if value.is_empty() || value.ends_with("-replace-me") {
        problems.add(
            field,
            format!("is not set; give a Slack token starting {prefix}"),
        );
    } else if !value.starts_with(prefix) {
        problems.add(field, format!("must start with {prefix}"));
    }
    value
}

fn env_file(dir: &Path, raw: Option<&str>, problems: &mut Problems) -> BTreeMap<String, String> {
    let (path, required) = match raw.map(str::trim).filter(|raw| !raw.is_empty()) {
        Some(raw) => (resolve(dir, raw), true),
        None => (dir.join(DEFAULT_ENV_FILE), false),
    };
    if !required && !path.exists() {
        return BTreeMap::new();
    }
    match dotenvy::from_path_iter(&path) {
        Ok(entries) => {
            let mut values = BTreeMap::new();
            for entry in entries {
                match entry {
                    Ok((key, value)) => {
                        values.insert(key, value);
                    }
                    Err(err) => {
                        problems.add("env_file", format!("{}: {err}", path.display()));
                        break;
                    }
                }
            }
            values
        }
        Err(err) => {
            problems.add("env_file", format!("cannot read {}: {err}", path.display()));
            BTreeMap::new()
        }
    }
}

fn expand(raw: &str, env: &BTreeMap<String, String>) -> String {
    let mut out = String::new();
    let mut rest = raw;
    while let Some(start) = rest.find("${") {
        out.push_str(&rest[..start]);
        let after = &rest[start + 2..];
        let Some(end) = after.find('}') else {
            out.push_str(&rest[start..]);
            return out;
        };
        let name = &after[..end];
        let value = std::env::var(name)
            .ok()
            .filter(|value| !value.is_empty())
            .or_else(|| env.get(name).cloned())
            .unwrap_or_default();
        out.push_str(&value);
        rest = &after[end + 1..];
    }
    out.push_str(rest);
    out
}

fn log(section: &LogFile, problems: &mut Problems) -> Option<LogConfig> {
    let level = match section.level.as_deref().map(str::trim) {
        None | Some("info") => Some(tracing::Level::INFO),
        Some("trace") => Some(tracing::Level::TRACE),
        Some("debug") => Some(tracing::Level::DEBUG),
        Some("warn") => Some(tracing::Level::WARN),
        Some("error") => Some(tracing::Level::ERROR),
        Some(other) => {
            problems.add(
                "log.level",
                format!("{other:?} is not a level; use trace, debug, info, warn or error"),
            );
            None
        }
    };
    let format = match section.format.as_deref().map(str::trim) {
        None | Some("text") => Some(LogFormat::Text),
        Some("json") => Some(LogFormat::Json),
        Some(other) => {
            problems.add(
                "log.format",
                format!("{other:?} is not a format; use text or json"),
            );
            None
        }
    };
    Some(LogConfig {
        level: level?,
        format: format?,
        turn_timings: section.turn_timings.unwrap_or(false),
    })
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
mod tests {
    use super::*;

    const GOOD: &str = r#"
[slack]
app_token = "xapp-1"
bot_token = "xoxb-1"
"#;

    fn write(text: &str) -> (tempfile::TempDir, PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(FILE_NAME);
        std::fs::write(&path, text).unwrap();
        (dir, path)
    }

    fn fields(text: &str) -> Vec<String> {
        let (_dir, path) = write(text);
        match load(&path) {
            Err(ConfigError::Invalid { problems, .. }) => problems
                .0
                .into_iter()
                .map(|problem| problem.field)
                .collect(),
            other => panic!("expected problems, got {other:?}"),
        }
    }

    #[test]
    fn a_minimal_alias_resolves_every_default_against_its_folder() {
        let (dir, path) = write(GOOD);
        let config = load(&path).unwrap();
        assert_eq!(config.dir, dir.path());
        assert_eq!(
            config.database,
            Database::Sqlite {
                path: dir.path().join("murtaugh.db")
            }
        );
        assert_eq!(config.listen.to_string(), DEFAULT_LISTEN);
        assert_eq!(config.log.level, tracing::Level::INFO);
        assert_eq!(config.log.format, LogFormat::Text);
        assert!(!config.log.turn_timings, "turn timings must be opt-in");
    }

    #[test]
    fn turn_timings_can_be_switched_on() {
        let (_dir, path) = write(&format!("{GOOD}\n[log]\nturn_timings = true\n"));
        assert!(load(&path).unwrap().log.turn_timings);
    }

    #[test]
    fn the_default_path_is_per_alias() {
        let path = default_path("work").unwrap();
        assert!(
            path.ends_with(".config/murtaugh/work/murtaugh.toml"),
            "{}",
            path.display()
        );
    }

    #[test]
    fn an_empty_file_names_every_missing_credential_together() {
        assert_eq!(fields(""), ["slack.app_token", "slack.bot_token"]);
    }

    #[test]
    fn every_problem_is_reported_in_one_pass() {
        let found = fields(
            r#"
[slack]
app_token = "xoxb-wrong-kind"
bot_token = "xoxb-1"
[database]
backend = "postgres"
[nodes]
listen = "localhost"
[log]
level = "loud"
"#,
        );
        assert_eq!(
            found,
            [
                "slack.app_token",
                "database.backend",
                "nodes.listen",
                "log.level"
            ]
        );
    }

    #[test]
    fn unknown_keys_are_an_error_naming_the_key() {
        let (_dir, path) = write(&format!("{GOOD}\n[nodes]\nlisten_on = \"x\"\n"));
        let message = load(&path).unwrap_err().to_string();
        assert!(message.contains("listen_on"), "{message}");
    }

    #[test]
    fn secrets_come_from_the_env_file_beside_the_alias() {
        let (dir, path) = write(
            "[slack]\napp_token = \"${MURTAUGH_TEST_APP}\"\nbot_token = \"${MURTAUGH_TEST_BOT}\"\n",
        );
        std::fs::write(
            dir.path().join(".env"),
            "MURTAUGH_TEST_APP=xapp-from-env\nMURTAUGH_TEST_BOT=xoxb-from-env\n",
        )
        .unwrap();
        let config = load(&path).unwrap();
        assert_eq!(config.slack.app_token, "xapp-from-env");
        assert_eq!(config.slack.bot_token, "xoxb-from-env");
    }

    #[test]
    fn a_named_env_file_that_is_missing_is_a_problem() {
        assert_eq!(
            fields(&format!("env_file = \"secrets.env\"\n{GOOD}")),
            ["env_file"]
        );
    }

    #[test]
    fn firestore_paths_resolve_against_the_alias_and_admin_commands_need_no_slack() {
        let (dir, path) = write(
            "[database]\nbackend = \"firestore\"\n[database.firestore]\ncollection = \"team\"\ncredentials_file = \"sa.json\"\n",
        );
        assert_eq!(
            database(&path).unwrap(),
            Database::Firestore(FirestoreConfig {
                collection: Some("team".into()),
                credentials_file: Some(dir.path().join("sa.json")),
                ..Default::default()
            })
        );
    }
}
