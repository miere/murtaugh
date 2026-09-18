//! The bootstrap file: Slack credentials, where the configuration store lives, and where nodes
//! dial. Everything else is in the store and changes through the CLI while the gateway runs.

use std::collections::HashMap;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};

use serde::Deserialize;

pub const DEFAULT_LISTEN: &str = "127.0.0.1:7443";

#[derive(Debug, Clone)]
pub struct Config {
    pub path: PathBuf,
    pub slack: SlackTokens,
    pub database: Database,
    pub listen: SocketAddr,
}

#[derive(Clone)]
pub struct SlackTokens {
    pub app_token: String,
    pub bot_token: String,
}

impl std::fmt::Debug for SlackTokens {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("SlackTokens { .. }")
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Database {
    Sqlite { path: PathBuf },
    Firestore(FirestoreConfig),
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct FirestoreConfig {
    pub project_id: Option<String>,
    pub database_id: Option<String>,
    pub collection: Option<String>,
    pub credentials_file: Option<PathBuf>,
}

#[derive(Debug, thiserror::Error)]
pub enum ConfigError {
    #[error("{path}: cannot read it: {source}")]
    Unreadable {
        path: PathBuf,
        #[source]
        source: std::io::Error,
    },
    #[error("{path}: {message}")]
    Syntax { path: PathBuf, message: String },
    #[error("{path} has problems:\n{}", .problems.iter().map(|p| format!("  - {p}")).collect::<Vec<_>>().join("\n"))]
    Invalid {
        path: PathBuf,
        problems: Vec<String>,
    },
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct File {
    #[serde(default)]
    oauth: OAuth,
    #[serde(default)]
    database: DatabaseFile,
    #[serde(default)]
    nodes: Nodes,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct OAuth {
    app_token: Option<String>,
    bot_token: Option<String>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct DatabaseFile {
    backend: Option<String>,
    sqlite: Option<SqliteFile>,
    firestore: Option<FirestoreConfig>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct SqliteFile {
    path: Option<PathBuf>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct Nodes {
    listen: Option<String>,
}

pub fn default_path() -> Option<PathBuf> {
    let home = std::env::var_os("HOME").filter(|home| !home.is_empty())?;
    Some(PathBuf::from(home).join(".config/murtaugh/config.yaml"))
}

fn read(path: &Path) -> Result<File, ConfigError> {
    let text = std::fs::read_to_string(path).map_err(|source| ConfigError::Unreadable {
        path: path.to_path_buf(),
        source,
    })?;
    serde_yaml::from_str(&text).map_err(|err| ConfigError::Syntax {
        path: path.to_path_buf(),
        message: err.to_string(),
    })
}

fn invalid(path: &Path, problems: Vec<String>) -> ConfigError {
    ConfigError::Invalid {
        path: path.to_path_buf(),
        problems,
    }
}

/// `${VAR}` reads the process environment first, then a `.env` beside the file, so secrets never
/// have to sit in the YAML itself.
pub fn load(path: &Path) -> Result<Config, ConfigError> {
    let file = read(path)?;
    let dotenv = read_dotenv(&dir_of(path).join(".env"));
    let mut problems = Vec::new();
    let mut secret = |field: &str, raw: Option<String>, prefix: &str| {
        let value = raw
            .map(|raw| expand(&raw, &dotenv))
            .unwrap_or_default()
            .trim()
            .to_owned();
        if value.is_empty() || value.ends_with("-replace-me") {
            problems.push(format!(
                "{field} is not set; give a Slack token starting {prefix}"
            ));
        } else if !value.starts_with(prefix) {
            problems.push(format!("{field} must start with {prefix}"));
        }
        value
    };
    let slack = SlackTokens {
        app_token: secret("oauth.app_token", file.oauth.app_token, "xapp-"),
        bot_token: secret("oauth.bot_token", file.oauth.bot_token, "xoxb-"),
    };
    let database = resolve_database(path, file.database).map_err(|problem| problems.push(problem));
    let listen_raw = file
        .nodes
        .listen
        .unwrap_or_else(|| DEFAULT_LISTEN.to_owned());
    let listen = listen_raw.parse::<SocketAddr>().map_err(|err| {
        problems.push(format!(
            "nodes.listen {listen_raw:?} is not an address like {DEFAULT_LISTEN}: {err}"
        ));
    });
    match (database, listen) {
        (Ok(database), Ok(listen)) if problems.is_empty() => Ok(Config {
            path: path.to_path_buf(),
            slack,
            database,
            listen,
        }),
        _ => Err(invalid(path, problems)),
    }
}

/// Only the database section, so admin commands work before the Slack credentials exist.
pub fn database(path: &Path) -> Result<Database, ConfigError> {
    resolve_database(path, read(path)?.database).map_err(|problem| invalid(path, vec![problem]))
}

fn dir_of(path: &Path) -> &Path {
    path.parent().unwrap_or_else(|| Path::new("."))
}

fn resolve_database(path: &Path, database: DatabaseFile) -> Result<Database, String> {
    match database.backend.as_deref().unwrap_or("sqlite") {
        "sqlite" => {
            let dir = dir_of(path);
            let stem = path.file_stem().unwrap_or_default().to_string_lossy();
            let path = match database.sqlite.and_then(|sqlite| sqlite.path) {
                Some(raw) if raw.is_absolute() => raw,
                Some(raw) => dir.join(raw),
                None => dir.join(format!("{stem}.db")),
            };
            Ok(Database::Sqlite { path })
        }
        "firestore" => Ok(Database::Firestore(database.firestore.unwrap_or_default())),
        other => Err(format!(
            "database.backend {other:?} is not one of sqlite or firestore"
        )),
    }
}

fn read_dotenv(path: &Path) -> HashMap<String, String> {
    let Ok(text) = std::fs::read_to_string(path) else {
        return HashMap::new();
    };
    text.lines()
        .map(str::trim)
        .filter(|line| !line.is_empty() && !line.starts_with('#'))
        .filter_map(|line| line.split_once('='))
        .map(|(key, value)| (key.trim().to_owned(), value.trim().to_owned()))
        .collect()
}

fn expand(raw: &str, dotenv: &HashMap<String, String>) -> String {
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
            .or_else(|| dotenv.get(name).cloned())
            .unwrap_or_default();
        out.push_str(&value);
        rest = &after[end + 1..];
    }
    out.push_str(rest);
    out
}
