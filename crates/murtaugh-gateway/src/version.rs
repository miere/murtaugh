/// Release builds set `MURTAUGH_VERSION` from the tag; anything else reports the crate version.
pub const VERSION: &str = match option_env!("MURTAUGH_VERSION") {
    Some(version) => version,
    None => env!("CARGO_PKG_VERSION"),
};
