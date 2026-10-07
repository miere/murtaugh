use std::io::{self, Write};

use tracing_subscriber::fmt::MakeWriter;

use crate::config::{LogConfig, LogFormat};

const REDACTED: &str = "[redacted]";
const SECRET_PREFIXES: [&str; 5] = ["mrtg_node_", "mrtg_user_", "xoxb-", "xapp-", "xoxp-"];

pub fn init(config: &LogConfig) {
    let builder = tracing_subscriber::fmt()
        .with_max_level(config.level)
        .with_ansi(false)
        .with_writer(Redacting);
    let _ = match config.format {
        LogFormat::Text => builder.try_init(),
        LogFormat::Json => builder.json().try_init(),
    };
}

struct Redacting;

impl<'a> MakeWriter<'a> for Redacting {
    type Writer = RedactingWriter;

    fn make_writer(&'a self) -> Self::Writer {
        RedactingWriter
    }
}

struct RedactingWriter;

impl Write for RedactingWriter {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        let text = String::from_utf8_lossy(buf);
        io::stderr().lock().write_all(redact(&text).as_bytes())?;
        Ok(buf.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        io::stderr().flush()
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
