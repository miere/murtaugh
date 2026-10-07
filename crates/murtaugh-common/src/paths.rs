//! Where Murtaugh's binaries keep their configuration: `~/.config/murtaugh/…`.

use std::path::{Path, PathBuf};

pub fn home() -> Option<PathBuf> {
    std::env::var_os("HOME")
        .filter(|home| !home.is_empty())
        .map(PathBuf::from)
}

/// `~/.config/murtaugh`, under which each binary keeps its own folder.
pub fn config_root() -> Option<PathBuf> {
    home().map(|home| home.join(".config/murtaugh"))
}

pub fn absolute(path: &Path) -> PathBuf {
    std::path::absolute(path).unwrap_or_else(|_| path.to_path_buf())
}

/// A leading `~/` is the home folder, as a shell would read it.
pub fn expand_home(raw: &str) -> PathBuf {
    match (raw.strip_prefix("~/"), home()) {
        (Some(rest), Some(home)) => home.join(rest),
        _ => PathBuf::from(raw),
    }
}
