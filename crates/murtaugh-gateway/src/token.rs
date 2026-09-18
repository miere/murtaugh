//! Node credentials: `mrtg_node_<selector>_<secret>`. The selector finds the record; only the
//! secret's SHA-256 is stored, so the database never holds anything a node could present.

use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use sha2::{Digest, Sha256};
use subtle::ConstantTimeEq;

pub const PREFIX: &str = "mrtg_node_";
const SELECTOR_BYTES: usize = 8;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Credential {
    pub selector: String,
    pub secret: String,
}

/// The token is shown once, at minting, and never stored.
pub struct Minted {
    pub token: String,
    pub selector: String,
    pub secret_hash: String,
}

pub fn mint() -> Minted {
    let selector = hex::encode(rand::random::<[u8; SELECTOR_BYTES]>());
    let secret = URL_SAFE_NO_PAD.encode(rand::random::<[u8; 32]>());
    Minted {
        token: format!("{PREFIX}{selector}_{secret}"),
        secret_hash: hash(&secret),
        selector,
    }
}

pub fn parse(presented: &str) -> Option<Credential> {
    let rest = presented.trim().strip_prefix(PREFIX)?;
    let (selector, secret) = rest.split_once('_')?;
    let valid_selector = selector.len() == SELECTOR_BYTES * 2
        && selector.bytes().all(|byte| byte.is_ascii_hexdigit());
    (valid_selector && !secret.is_empty()).then(|| Credential {
        selector: selector.to_owned(),
        secret: secret.to_owned(),
    })
}

pub fn hash(secret: &str) -> String {
    hex::encode(Sha256::digest(secret.as_bytes()))
}

pub fn matches(secret: &str, stored_hash: &str) -> bool {
    hash(secret).as_bytes().ct_eq(stored_hash.as_bytes()).into()
}

#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod tests {
    use super::*;

    #[test]
    fn a_minted_token_parses_and_matches_only_its_own_hash() {
        let minted = mint();
        let credential = parse(&minted.token).unwrap();
        assert_eq!(credential.selector, minted.selector);
        assert!(matches(&credential.secret, &minted.secret_hash));
        assert!(!matches("another-secret", &minted.secret_hash));
        assert!(!minted.secret_hash.contains(&credential.secret));
    }

    #[test]
    fn malformed_tokens_do_not_parse() {
        for bad in [
            "",
            "mrtg_node_",
            "mrtg_node_0123456789abcdef",
            "mrtg_node_0123456789abcdef_",
            "mrtg_node_0123_secret",
            "mrtg_node_0123456789abcdeg_secret",
            "xoxb-0123456789abcdef_secret",
        ] {
            assert!(parse(bad).is_none(), "{bad:?} parsed");
        }
    }
}
