//! Logs that never carry a credential: anything shaped like a node, client or Slack token is
//! redacted on its way out, whether to stderr or to a file.

use std::fs::{File, OpenOptions};
use std::io::{self, Write};
use std::path::Path;
use std::sync::{Arc, Mutex};

use tracing_subscriber::fmt::MakeWriter;

const REDACTED: &str = "[redacted]";
const SECRET_PREFIXES: [&str; 5] = ["mrtg_node_", "mrtg_user_", "xoxb-", "xapp-", "xoxp-"];

/// To stderr, as a daemon under launchd or a container logs.
pub fn init_stderr(level: tracing::Level, json: bool) {
    let builder = tracing_subscriber::fmt()
        .with_max_level(level)
        .with_ansi(false)
        .with_writer(Redacting { file: None });
    let _ = match json {
        false => builder.try_init(),
        true => builder.json().try_init(),
    };
}

/// To a file, appended to, for a process whose stdout and stderr belong to someone else, such as
/// an editor that speaks a protocol over them.
pub fn init_file(path: &Path, level: tracing::Level) -> io::Result<()> {
    if let Some(dir) = path.parent() {
        std::fs::create_dir_all(dir)?;
    }
    let file = OpenOptions::new().create(true).append(true).open(path)?;
    let _ = tracing_subscriber::fmt()
        .with_max_level(level)
        .with_ansi(false)
        .with_writer(Redacting {
            file: Some(Arc::new(Mutex::new(file))),
        })
        .try_init();
    Ok(())
}

struct Redacting {
    file: Option<Arc<Mutex<File>>>,
}

impl<'a> MakeWriter<'a> for Redacting {
    type Writer = RedactingWriter;

    fn make_writer(&'a self) -> Self::Writer {
        RedactingWriter {
            file: self.file.clone(),
        }
    }
}

struct RedactingWriter {
    file: Option<Arc<Mutex<File>>>,
}

impl Write for RedactingWriter {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        let text = String::from_utf8_lossy(buf);
        let redacted = redact(&text);
        match &self.file {
            Some(file) => file
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .write_all(redacted.as_bytes())?,
            None => io::stderr().lock().write_all(redacted.as_bytes())?,
        }
        Ok(buf.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        match &self.file {
            Some(file) => file
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .flush(),
            None => io::stderr().flush(),
        }
    }
}

/// Anything shaped like a node or Slack token, so a log line quoting one never leaks it.
pub fn redact(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut rest = text;
    loop {
        let next = SECRET_PREFIXES
            .iter()
            .filter_map(|prefix| rest.find(prefix).map(|at| (at, *prefix)))
            .min_by_key(|(at, _)| *at);
        let Some((at, prefix)) = next else {
            break;
        };
        let (before, from) = rest.split_at(at);
        out.push_str(before);
        let body = &from[prefix.len()..];
        let end = body
            .find(|c: char| !(c.is_ascii_alphanumeric() || c == '_' || c == '-'))
            .unwrap_or(body.len());
        out.push_str(prefix);
        if end > 0 {
            out.push_str(REDACTED);
        }
        rest = &body[end..];
    }
    out.push_str(rest);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn node_and_slack_tokens_are_redacted() {
        assert_eq!(
            redact("node mrtg_node_0123456789abcdef_se-cret, bot xoxb-1-2-abc and xapp-9"),
            "node mrtg_node_[redacted], bot xoxb-[redacted] and xapp-[redacted]"
        );
        assert_eq!(
            redact("client mrtg_user_0123456789abcdef_se-cret"),
            "client mrtg_user_[redacted]"
        );
        assert_eq!(redact("nothing secret"), "nothing secret");
    }
}
