//! Who a node's owner lets talk to their machine, read from the `murtaugh_access` key of the
//! metadata the node sends. The owner decides and the gateway enforces, because only the gateway
//! knows who is speaking. Nobody is let past it, the gateway's admin included: it is the owner's
//! machine.

use std::collections::HashSet;

use murtaugh_store::UserId;
use rax::{Metadata, Rejection};
use serde_json::Value;

use crate::tools::NAMESPACE;

/// The name under this gateway's prefix, so the key a node writes is `murtaugh_access`.
pub const ACCESS: &str = "access";

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub enum NodeAccess {
    /// What a node that says nothing gets, so a node from before this key keeps working as it did.
    #[default]
    AlwaysAllow,
    AllowList(HashSet<UserId>),
}

impl NodeAccess {
    /// The owner is let in whether listed or not: an allow list is about other people.
    pub fn admits(&self, owner: &UserId, user: &UserId) -> bool {
        match self {
            Self::AlwaysAllow => true,
            Self::AllowList(people) => user == owner || people.contains(user),
        }
    }
}

/// A node's metadata as this gateway understands it, and the keys it did not.
#[derive(Debug, Default)]
pub struct Read {
    pub access: NodeAccess,
    pub ignored: Vec<String>,
}

/// Strict about the keys this gateway owns and indifferent to every other: a mistyped allow list
/// that fell back to `always_allow` would leave the machine open to everyone, so it is refused.
pub fn read(metadata: &Metadata) -> Result<Read, Rejection> {
    let mut read = Read::default();
    for (key, value) in metadata {
        match rax::metadata::owner(key) {
            Some((NAMESPACE, ACCESS)) => {
                read.access = access(value).map_err(|problem| Rejection {
                    key: Some(key.clone()),
                    message: format!("`{key}` {problem}"),
                })?;
            }
            _ => read.ignored.push(key.clone()),
        }
    }
    Ok(read)
}

fn access(value: &Value) -> Result<NodeAccess, String> {
    let Some(fields) = value.as_object() else {
        return Err("must be a table with a `policy`".to_owned());
    };
    if let Some(unknown) = fields
        .keys()
        .find(|field| !matches!(field.as_str(), "policy" | "people"))
    {
        return Err(format!("has an unknown field `{unknown}`"));
    }
    let people = fields.get("people");
    match fields.get("policy").and_then(Value::as_str) {
        Some("always_allow") if people.is_some() => Err(
            "lists `people`, which `always_allow` would ignore; remove them or use `allow_list`"
                .to_owned(),
        ),
        Some("always_allow") => Ok(NodeAccess::AlwaysAllow),
        Some("allow_list") => {
            let Some(people) = people.and_then(Value::as_array) else {
                return Err("needs `people`, a list of Slack user IDs, for `allow_list`".to_owned());
            };
            people
                .iter()
                .map(|person| {
                    person
                        .as_str()
                        .and_then(|raw| UserId::parse(raw).ok())
                        .ok_or_else(|| format!("lists {person}, which is not a Slack user ID"))
                })
                .collect::<Result<_, _>>()
                .map(NodeAccess::AllowList)
        }
        Some(other) => Err(format!(
            "has an unknown policy `{other}`; use `always_allow` or `allow_list`"
        )),
        None => Err("needs a `policy`: `always_allow` or `allow_list`".to_owned()),
    }
}

#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod tests {
    use serde_json::json;

    use super::*;

    fn user(raw: &str) -> UserId {
        UserId::parse(raw).unwrap()
    }

    fn metadata(value: Value) -> Metadata {
        Metadata::from([("murtaugh_access".to_owned(), value)])
    }

    #[test]
    fn a_node_that_says_nothing_lets_everyone_in() {
        let read = read(&Metadata::new()).unwrap();
        assert_eq!(read.access, NodeAccess::AlwaysAllow);
        assert!(read.ignored.is_empty());
    }

    #[test]
    fn an_allow_list_admits_the_listed_and_the_owner_and_nobody_else() {
        let read = read(&metadata(
            json!({"policy": "allow_list", "people": ["U0BOB0001"]}),
        ))
        .unwrap();
        let owner = user("U0ALICE01");
        assert!(read.access.admits(&owner, &owner));
        assert!(read.access.admits(&owner, &user("U0BOB0001")));
        assert!(!read.access.admits(&owner, &user("U0ADMIN01")));
    }

    #[test]
    fn keys_this_gateway_does_not_own_or_know_are_ignored() {
        let metadata = Metadata::from([
            (
                "murtaugh_access".to_owned(),
                json!({"policy": "always_allow"}),
            ),
            ("murtaugh_colour".to_owned(), json!("teal")),
            ("other_access".to_owned(), json!(1)),
        ]);
        let read = read(&metadata).unwrap();
        assert_eq!(read.ignored, ["murtaugh_colour", "other_access"]);
    }

    #[test]
    fn a_malformed_access_key_is_rejected_by_name() {
        for (value, problem) in [
            (json!("everyone"), "must be a table"),
            (json!({"people": []}), "needs a `policy`"),
            (json!({"policy": "everyone"}), "unknown policy `everyone`"),
            (json!({"policy": "allow_list"}), "needs `people`"),
            (
                json!({"policy": "allow_list", "people": "U0BOB0001"}),
                "needs `people`",
            ),
            (
                json!({"policy": "allow_list", "people": ["bob"]}),
                "\"bob\"",
            ),
            (json!({"policy": "allow_list", "people": [7]}), "lists 7"),
            (
                json!({"policy": "always_allow", "people": []}),
                "would ignore",
            ),
            (
                json!({"policy": "always_allow", "who": 1}),
                "unknown field `who`",
            ),
        ] {
            let rejection = read(&metadata(value)).unwrap_err();
            assert_eq!(rejection.key.as_deref(), Some("murtaugh_access"));
            assert!(
                rejection.message.contains(problem),
                "{} does not mention {problem}",
                rejection.message
            );
        }
    }
}
