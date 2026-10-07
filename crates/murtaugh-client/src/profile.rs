//! A profile names a gateway and the token file to dial it with, so an editor's entry for the
//! bridge stays one short line. It lives at `~/.config/murtaugh/client/<alias>.toml`, and flags
//! override it.

use std::io;
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};

use murtaugh_common::paths;
use serde::{Deserialize, Serialize};

pub const DEFAULT_PROFILE: &str = "default";

#[derive(Debug, thiserror::Error)]
pub enum ProfileError {
    #[error("HOME is not set, so there is no profile folder; pass --gateway and --token-file")]
    NoHome,
    #[error("profile {alias:?} cannot be a file name; use letters, digits, dashes or underscores")]
    Alias { alias: String },
    #[error("{path}: {source}")]
    Io {
        path: PathBuf,
        #[source]
        source: io::Error,
    },
    #[error("{path} is not a valid profile: {message}")]
    Syntax { path: PathBuf, message: String },
    #[error("no gateway: pass --gateway, or save one with `murtaugh-client login --gateway <url>`")]
    NoGateway,
    #[error(
        "no token file: pass --token-file, or save one with `murtaugh-client login`, which reads the token from stdin"
    )]
    NoToken,
    #[error("the token in {path} is not a client token; mint one from the gateway's Home tab")]
    NotAClientToken { path: PathBuf },
}

#[derive(Debug, Default, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Profile {
    pub gateway: Option<String>,
    pub token_file: Option<String>,
}

/// What a command dials with, once the profile and the flags are merged.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Resolved {
    pub gateway: String,
    pub token_file: PathBuf,
}

fn valid_alias(alias: &str) -> bool {
    !alias.is_empty()
        && alias
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_')
}

/// `~/.config/murtaugh/client`.
pub fn dir() -> Result<PathBuf, ProfileError> {
    paths::config_root()
        .map(|root| root.join("client"))
        .ok_or(ProfileError::NoHome)
}

pub fn path(alias: &str) -> Result<PathBuf, ProfileError> {
    if !valid_alias(alias) {
        return Err(ProfileError::Alias {
            alias: alias.to_owned(),
        });
    }
    Ok(dir()?.join(format!("{alias}.toml")))
}

/// An absent profile is an empty one: flags alone are enough.
pub fn load(path: &Path) -> Result<Profile, ProfileError> {
    let text = match std::fs::read_to_string(path) {
        Ok(text) => text,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(Profile::default()),
        Err(source) => {
            return Err(ProfileError::Io {
                path: path.to_path_buf(),
                source,
            });
        }
    };
    toml::from_str(&text).map_err(|err| ProfileError::Syntax {
        path: path.to_path_buf(),
        message: err.to_string().trim().to_owned(),
    })
}

/// Flags win over the profile. A relative token file in the profile is beside it.
pub fn resolve(
    profile: &Profile,
    profile_path: &Path,
    gateway: Option<String>,
    token_file: Option<PathBuf>,
) -> Result<Resolved, ProfileError> {
    let gateway = gateway
        .or_else(|| profile.gateway.clone())
        .filter(|gateway| !gateway.trim().is_empty())
        .ok_or(ProfileError::NoGateway)?;
    let token_file = match token_file {
        Some(flag) => paths::absolute(&flag),
        None => {
            let raw = profile.token_file.as_deref().ok_or(ProfileError::NoToken)?;
            let expanded = paths::expand_home(raw);
            match profile_path.parent() {
                Some(dir) if expanded.is_relative() => dir.join(expanded),
                _ => expanded,
            }
        }
    };
    Ok(Resolved {
        gateway,
        token_file,
    })
}

pub fn read_token(path: &Path) -> Result<String, ProfileError> {
    let token = std::fs::read_to_string(path).map_err(|source| ProfileError::Io {
        path: path.to_path_buf(),
        source,
    })?;
    let token = token.trim().to_owned();
    if murtaugh_common::token::parse(murtaugh_common::token::USER_PREFIX, &token).is_none() {
        return Err(ProfileError::NotAClientToken {
            path: path.to_path_buf(),
        });
    }
    Ok(token)
}

/// Saves `token` beside the profile, readable by its owner alone, and points the profile at it.
/// The gateway is kept unless a new one is given.
pub fn save(alias: &str, token: &str, gateway: Option<String>) -> Result<PathBuf, ProfileError> {
    let profile_path = path(alias)?;
    let dir = dir()?;
    let io_error = |path: &Path| {
        let path = path.to_path_buf();
        move |source| ProfileError::Io { path, source }
    };
    std::fs::create_dir_all(&dir).map_err(io_error(&dir))?;
    let token_path = dir.join(format!("{alias}.token"));
    let tmp = dir.join(format!(".{alias}.token.tmp"));
    {
        use std::io::Write;
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(&tmp)
            .map_err(io_error(&tmp))?;
        writeln!(file, "{}", token.trim()).map_err(io_error(&tmp))?;
    }
    std::fs::rename(&tmp, &token_path).map_err(io_error(&token_path))?;
    let mut profile = load(&profile_path)?;
    if gateway.is_some() {
        profile.gateway = gateway;
    }
    profile.token_file = Some(format!("{alias}.token"));
    let text = toml::to_string(&profile).map_err(|err| ProfileError::Syntax {
        path: profile_path.clone(),
        message: err.to_string(),
    })?;
    std::fs::write(&profile_path, text).map_err(io_error(&profile_path))?;
    Ok(profile_path)
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]

    use super::*;

    #[test]
    fn flags_win_over_the_profile_and_a_relative_token_is_beside_it() {
        let profile = Profile {
            gateway: Some("wss://murtaugh.example".into()),
            token_file: Some("work.token".into()),
        };
        let at = Path::new("/home/me/.config/murtaugh/client/work.toml");
        let resolved = resolve(&profile, at, None, None).unwrap();
        assert_eq!(resolved.gateway, "wss://murtaugh.example");
        assert_eq!(
            resolved.token_file,
            Path::new("/home/me/.config/murtaugh/client/work.token")
        );
        let resolved = resolve(
            &profile,
            at,
            Some("ws://127.0.0.1:9".into()),
            Some("/tmp/t".into()),
        )
        .unwrap();
        assert_eq!(resolved.gateway, "ws://127.0.0.1:9");
        assert_eq!(resolved.token_file, Path::new("/tmp/t"));
        assert!(matches!(
            resolve(&Profile::default(), at, None, None),
            Err(ProfileError::NoGateway)
        ));
    }

    #[test]
    fn an_alias_must_be_a_plain_file_name() {
        assert!(valid_alias("work_2"));
        assert!(!valid_alias("../etc"));
        assert!(!valid_alias(""));
    }
}
