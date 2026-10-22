//! Who a node's owner lets talk to their machine, read from the `murtaugh_access` key of the
//! metadata the node sends.

use rax::{Metadata, Rejection};

pub use rax_gateway::node_access::{ACCESS, NodeAccess, Read};

/// This gateway's prefix for the metadata keys it owns. It names the gateway, not a tool group.
pub const GATEWAY: &str = "murtaugh";

pub fn read(metadata: &Metadata) -> Result<Read, Rejection> {
    rax_gateway::node_access::read(metadata, crate::settings())
}

#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod tests {
    use serde_json::json;

    use super::*;

    fn metadata(value: serde_json::Value) -> Metadata {
        Metadata::from([("murtaugh_access".to_owned(), value)])
    }

    #[test]
    fn the_allow_list_is_murtaughs_and_lists_slack_users() {
        let ours = read(&metadata(
            json!({"policy": "allow_list", "people": ["U0BOB0001"]}),
        ))
        .unwrap();
        let owner = murtaugh_store::slack_user("U0ALICE01").unwrap();
        let bob = murtaugh_store::slack_user("U0BOB0001").unwrap();
        assert!(ours.access.admits(&owner, &bob));
        for (people, problem) in [
            (json!("U0BOB0001"), "a list of Slack user IDs"),
            (json!(["bob"]), "\"bob\", which is not a Slack user ID"),
        ] {
            let rejection =
                read(&metadata(json!({"policy": "allow_list", "people": people}))).unwrap_err();
            assert_eq!(rejection.key.as_deref(), Some("murtaugh_access"));
            assert!(rejection.message.contains(problem), "{}", rejection.message);
        }
        let theirs = Metadata::from([("gateway_access".to_owned(), json!("everyone"))]);
        assert_eq!(read(&theirs).unwrap().ignored, ["gateway_access"]);
    }
}
