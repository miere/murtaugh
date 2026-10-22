//! Murtaugh's credentials: `mrtg_node_<selector>_<secret>` for a node, `mrtg_user_<selector>_<secret>`
//! for a person's own client. The gateway mints and checks them with `rax-gateway`; a client only
//! needs to know a token when it sees one.

pub const NODE_PREFIX: &str = "mrtg_node_";
pub const USER_PREFIX: &str = "mrtg_user_";
const SELECTOR_BYTES: usize = 8;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Credential {
    pub selector: String,
    pub secret: String,
}

/// Only a credential of the kind `prefix` names: a node's token never opens a client's link, nor
/// the reverse. The same shape `rax-gateway` mints.
pub fn parse(prefix: &str, presented: &str) -> Option<Credential> {
    let rest = presented.trim().strip_prefix(prefix)?;
    let (selector, secret) = rest.split_once('_')?;
    let valid_selector = selector.len() == SELECTOR_BYTES * 2
        && selector.bytes().all(|byte| byte.is_ascii_hexdigit());
    (valid_selector && !secret.is_empty()).then(|| Credential {
        selector: selector.to_owned(),
        secret: secret.to_owned(),
    })
}

#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod tests {
    use super::*;

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
            assert!(parse(NODE_PREFIX, bad).is_none(), "{bad:?} parsed");
        }
    }

    #[test]
    fn a_token_parses_only_as_its_own_kind() {
        let user = "mrtg_user_0123456789abcdef_secret";
        assert!(parse(USER_PREFIX, user).is_some());
        assert!(parse(NODE_PREFIX, user).is_none());
    }
}
