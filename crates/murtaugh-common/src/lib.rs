//! What every Murtaugh binary shares: credentials, logging that never leaks them, the version and
//! its check, TLS setup, configuration paths and the launchd job.

pub mod launchd;
pub mod logging;
pub mod paths;
pub mod tls;
pub mod token;
pub mod version;
