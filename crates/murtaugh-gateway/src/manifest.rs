//! The Slack app this gateway expects, as a manifest to paste into "Create an app".

use serde_json::Value;

const MANIFEST: &str = include_str!("../assets/slack-manifest.json");

/// The events the gateway handles, each with the bot scope Slack requires before delivering it.
pub const EVENT_SCOPES: [(&str, &str); 2] = [
    ("app_mention", "app_mentions:read"),
    ("message.im", "im:history"),
];

pub fn render(name: &str) -> Result<String, String> {
    let mut manifest: Value =
        serde_json::from_str(MANIFEST).map_err(|err| format!("the bundled manifest: {err}"))?;
    let name = name.trim();
    if name.is_empty() || name.chars().count() > 35 {
        return Err("--name must be 1 to 35 characters, Slack's limit for an app name".into());
    }
    manifest["display_information"]["name"] = Value::from(name);
    manifest["features"]["bot_user"]["display_name"] = Value::from(name);
    serde_json::to_string_pretty(&manifest).map_err(|err| err.to_string())
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
mod tests {
    use super::*;

    fn strings(value: &Value) -> Vec<&str> {
        value
            .as_array()
            .unwrap()
            .iter()
            .map(|item| item.as_str().unwrap())
            .collect()
    }

    #[test]
    fn every_event_the_gateway_handles_is_subscribed_with_its_scope() {
        let manifest: Value = serde_json::from_str(&render("Murtaugh").unwrap()).unwrap();
        let events = strings(&manifest["settings"]["event_subscriptions"]["bot_events"]);
        let scopes = strings(&manifest["oauth_config"]["scopes"]["bot"]);
        for (event, scope) in EVENT_SCOPES {
            assert!(events.contains(&event), "{event} is not subscribed");
            assert!(scopes.contains(&scope), "{event} needs {scope}");
        }
        assert_eq!(manifest["settings"]["socket_mode_enabled"], true);
    }

    #[test]
    fn the_methods_the_gateway_calls_have_their_scopes() {
        let manifest: Value = serde_json::from_str(&render("Murtaugh").unwrap()).unwrap();
        let scopes = strings(&manifest["oauth_config"]["scopes"]["bot"]);
        for scope in [
            "assistant:write",
            "chat:write",
            "reactions:write",
            "channels:history",
            "groups:history",
            "im:history",
            "mpim:history",
            // read_canvas and edit_canvas; reading also downloads, which is files:read.
            "canvases:read",
            "canvases:write",
            "files:read",
            // the names mentions are labelled with.
            "users:read",
        ] {
            assert!(scopes.contains(&scope), "missing {scope}");
        }
    }

    #[test]
    fn the_name_is_applied_and_bounded() {
        let manifest: Value = serde_json::from_str(&render("Carmen").unwrap()).unwrap();
        assert_eq!(manifest["display_information"]["name"], "Carmen");
        assert_eq!(manifest["features"]["bot_user"]["display_name"], "Carmen");
        assert!(render("").is_err());
        assert!(render(&"x".repeat(36)).is_err());
    }
}
