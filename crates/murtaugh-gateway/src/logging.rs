//! The gateway logs to stderr, through the redaction every Murtaugh binary shares.

pub use murtaugh_common::logging::redact;

use crate::config::{LogConfig, LogFormat};

pub fn init(config: &LogConfig) {
    murtaugh_common::logging::init_stderr(config.level, config.format == LogFormat::Json);
}
