//! The person's own files, served from this machine's disk under the folders `--root` names. The
//! agent runs on a node that cannot see them, and an editor need not offer its file system: an
//! ACP plugin may leave reads and writes to an agent it expects to run beside it.
//!
//! Every path is resolved before it is touched, symbolic links included, and refused unless it
//! lands inside a root.

use std::io;
use std::path::{Component, Path, PathBuf};

/// A read past this is refused rather than sent whole to the agent.
const MAX_READ: u64 = 4 * 1024 * 1024;
/// A listing stops here and says so.
const MAX_ENTRIES: usize = 1000;

/// The folders the agent may reach, each resolved once at start-up.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Roots(Vec<PathBuf>);

impl Roots {
    /// Resolves each folder; one that does not exist, or is not a folder, is an error.
    pub fn new(dirs: &[PathBuf]) -> Result<Self, String> {
        let mut roots = Vec::with_capacity(dirs.len());
        for dir in dirs {
            let real = std::fs::canonicalize(dir)
                .map_err(|err| format!("--root {}: {err}", dir.display()))?;
            if !real.is_dir() {
                return Err(format!("--root {} is not a folder", dir.display()));
            }
            if !roots.contains(&real) {
                roots.push(real);
            }
        }
        Ok(Self(roots))
    }

    pub fn is_empty(&self) -> bool {
        self.0.is_empty()
    }

    pub fn dirs(&self) -> &[PathBuf] {
        &self.0
    }

    /// Where `path` really is, if that is inside a root. A relative path is taken from the first
    /// root. A file that does not exist yet is resolved through the nearest folder that does, and
    /// what is left of the path may not climb out with `..` or be a dangling link.
    pub fn resolve(&self, path: &str) -> Result<PathBuf, String> {
        let first = self
            .0
            .first()
            .ok_or("no folder was shared with the agent")?;
        let asked = Path::new(path);
        let asked = if asked.is_absolute() {
            asked.to_path_buf()
        } else {
            first.join(asked)
        };
        let real = real(&asked).map_err(|err| format!("{path}: {err}"))?;
        if self.0.iter().any(|root| real.starts_with(root)) {
            Ok(real)
        } else {
            Err(format!(
                "{path} is outside the folders shared with the agent: {}",
                self.listed()
            ))
        }
    }

    fn listed(&self) -> String {
        self.0
            .iter()
            .map(|root| root.display().to_string())
            .collect::<Vec<_>>()
            .join(", ")
    }
}

fn real(path: &Path) -> io::Result<PathBuf> {
    match std::fs::canonicalize(path) {
        Ok(real) => return Ok(real),
        Err(err) if err.kind() != io::ErrorKind::NotFound => return Err(err),
        Err(_) => {}
    }
    // A link whose target is missing would be followed by a write, wherever it points.
    if std::fs::symlink_metadata(path).is_ok() {
        return Err(io::Error::other("a link to a file that does not exist"));
    }
    let mut missing = Vec::new();
    let mut existing = path;
    loop {
        match existing.parent() {
            Some(parent) => {
                let name = existing
                    .components()
                    .next_back()
                    .ok_or_else(|| io::Error::other("an empty path"))?;
                missing.push(name);
                existing = parent;
                match std::fs::canonicalize(existing) {
                    Ok(_) => break,
                    Err(err) if err.kind() == io::ErrorKind::NotFound => continue,
                    Err(err) => return Err(err),
                }
            }
            None => return Err(io::Error::other("no part of the path exists")),
        }
    }
    let mut real = std::fs::canonicalize(existing)?;
    for component in missing.into_iter().rev() {
        match component {
            Component::Normal(name) => real.push(name),
            _ => return Err(io::Error::other("a path may not climb with `..`")),
        }
    }
    Ok(real)
}

/// A text file, or `limit` of its lines from `line` (from 1).
pub async fn read(
    roots: &Roots,
    path: &str,
    line: Option<u64>,
    limit: Option<u64>,
) -> Result<String, String> {
    let real = roots.resolve(path)?;
    let size = tokio::fs::metadata(&real)
        .await
        .map_err(|err| format!("{path}: {err}"))?
        .len();
    if size > MAX_READ {
        return Err(format!(
            "{path} is {size} bytes, more than the {MAX_READ} this reads"
        ));
    }
    let bytes = tokio::fs::read(&real)
        .await
        .map_err(|err| format!("{path}: {err}"))?;
    let text = String::from_utf8(bytes).map_err(|_| format!("{path} is not UTF-8 text"))?;
    Ok(lines(&text, line, limit))
}

fn lines(text: &str, line: Option<u64>, limit: Option<u64>) -> String {
    if line.is_none() && limit.is_none() {
        return text.to_owned();
    }
    let skip = line.unwrap_or(1).saturating_sub(1);
    let take = limit.unwrap_or(u64::MAX);
    text.split_inclusive('\n')
        .skip(usize::try_from(skip).unwrap_or(usize::MAX))
        .take(usize::try_from(take).unwrap_or(usize::MAX))
        .collect()
}

/// Writes `content` whole, making the folders it needs.
pub async fn write(roots: &Roots, path: &str, content: &str) -> Result<(), String> {
    let real = roots.resolve(path)?;
    if let Some(parent) = real.parent() {
        tokio::fs::create_dir_all(parent)
            .await
            .map_err(|err| format!("{path}: {err}"))?;
    }
    tokio::fs::write(&real, content)
        .await
        .map_err(|err| format!("{path}: {err}"))
}

/// One folder's entries, folders ending in `/`, sorted.
pub async fn list(roots: &Roots, path: &str) -> Result<String, String> {
    let real = roots.resolve(path)?;
    let mut dir = tokio::fs::read_dir(&real)
        .await
        .map_err(|err| format!("{path}: {err}"))?;
    let mut entries = Vec::new();
    while let Some(entry) = dir
        .next_entry()
        .await
        .map_err(|err| format!("{path}: {err}"))?
    {
        let mut name = entry.file_name().to_string_lossy().into_owned();
        if entry.file_type().await.is_ok_and(|kind| kind.is_dir()) {
            name.push('/');
        }
        entries.push(name);
    }
    entries.sort();
    let total = entries.len();
    entries.truncate(MAX_ENTRIES);
    let mut listed = entries.join("\n");
    if total > MAX_ENTRIES {
        listed.push_str(&format!("\n… and {} more", total - MAX_ENTRIES));
    }
    Ok(listed)
}

/// `text` with `old` replaced by `new`: the one place it appears, or every place with `all`.
pub fn replace(text: &str, old: &str, new: &str, all: bool) -> Result<String, String> {
    if old.is_empty() {
        return Err("old_string is empty".to_owned());
    }
    match text.matches(old).count() {
        0 => Err("old_string was not found in the file".to_owned()),
        1 => Ok(text.replacen(old, new, 1)),
        _ if all => Ok(text.replace(old, new)),
        n => Err(format!(
            "old_string appears {n} times; give more context to pick one, or set replace_all"
        )),
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]

    use super::*;

    fn roots(dir: &Path) -> Roots {
        Roots::new(&[dir.to_path_buf()]).unwrap()
    }

    #[test]
    fn a_path_inside_a_root_resolves_and_one_outside_is_refused() {
        let dir = tempfile::tempdir().unwrap();
        let outside = tempfile::tempdir().unwrap();
        let roots = roots(dir.path());
        let real = std::fs::canonicalize(dir.path()).unwrap();
        assert_eq!(
            roots.resolve(&dir.path().join("a.md").display().to_string()),
            Ok(real.join("a.md"))
        );
        assert_eq!(roots.resolve("notes/b.md"), Ok(real.join("notes/b.md")));
        let refused = roots.resolve(&outside.path().join("c.md").display().to_string());
        assert!(matches!(refused, Err(reason) if reason.contains("outside the folders")));
    }

    #[test]
    fn dot_dot_cannot_climb_out() {
        let dir = tempfile::tempdir().unwrap();
        let roots = roots(dir.path());
        assert!(roots.resolve("../escape.md").is_err());
        assert!(roots.resolve("missing/../../escape.md").is_err());
    }

    #[test]
    fn a_link_out_of_the_root_is_refused() {
        let dir = tempfile::tempdir().unwrap();
        let outside = tempfile::tempdir().unwrap();
        std::os::unix::fs::symlink(outside.path(), dir.path().join("out")).unwrap();
        std::os::unix::fs::symlink(
            outside.path().join("nowhere.md"),
            dir.path().join("dangling.md"),
        )
        .unwrap();
        let roots = roots(dir.path());
        assert!(roots.resolve("out/new.md").is_err());
        assert!(roots.resolve("dangling.md").is_err());
    }

    #[test]
    fn a_missing_root_is_an_error() {
        let dir = tempfile::tempdir().unwrap();
        assert!(Roots::new(&[dir.path().join("missing")]).is_err());
    }

    #[test]
    fn lines_are_cut_from_one() {
        let text = "a\nb\nc\n";
        assert_eq!(lines(text, None, None), text);
        assert_eq!(lines(text, Some(2), None), "b\nc\n");
        assert_eq!(lines(text, Some(1), Some(2)), "a\nb\n");
    }

    #[test]
    fn replace_wants_one_match_unless_told_all() {
        assert_eq!(replace("a b a", "b", "c", false), Ok("a c a".to_owned()));
        assert!(replace("a b a", "a", "c", false).is_err());
        assert_eq!(replace("a b a", "a", "c", true), Ok("c b c".to_owned()));
        assert!(replace("a", "z", "c", false).is_err());
        assert!(replace("a", "", "c", false).is_err());
    }

    #[tokio::test]
    async fn a_write_makes_its_folders_and_reads_back() {
        let dir = tempfile::tempdir().unwrap();
        let roots = roots(dir.path());
        write(&roots, "daily/today.md", "# Today\n").await.unwrap();
        assert_eq!(
            read(&roots, "daily/today.md", None, None).await,
            Ok("# Today\n".to_owned())
        );
        assert_eq!(list(&roots, ".").await, Ok("daily/".to_owned()));
    }
}
