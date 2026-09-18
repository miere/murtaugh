#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::process::{Command, Output};

fn gateway(config: &Path, args: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_murtaugh-gateway"))
        .arg("--config")
        .arg(config)
        .args(args)
        .env_remove("SLACK_APP_TOKEN")
        .env_remove("SLACK_BOT_TOKEN")
        .output()
        .unwrap()
}

fn ok(output: Output) -> String {
    assert!(
        output.status.success(),
        "stdout: {}\nstderr: {}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout).unwrap()
}

fn failed(output: Output) -> String {
    assert!(!output.status.success());
    String::from_utf8(output.stderr).unwrap()
}

fn setup() -> (tempfile::TempDir, std::path::PathBuf) {
    let dir = tempfile::tempdir().unwrap();
    let config = dir.path().join("murtaugh.toml");
    std::fs::write(
        &config,
        "[slack]\napp_token = \"${SLACK_APP_TOKEN}\"\nbot_token = \"${SLACK_BOT_TOKEN}\"\n[database]\nbackend = \"sqlite\"\n",
    )
    .unwrap();
    (dir, config)
}

#[test]
fn validate_names_every_missing_credential_without_connecting() {
    let (_dir, config) = setup();
    let stderr = failed(gateway(&config, &["validate"]));
    assert!(stderr.contains("slack.app_token: is not set"), "{stderr}");
    assert!(stderr.contains("slack.bot_token: is not set"), "{stderr}");
}

#[test]
fn credentials_come_from_a_dotenv_beside_the_config() {
    let (dir, config) = setup();
    std::fs::write(
        dir.path().join(".env"),
        "SLACK_APP_TOKEN=xapp-1\nSLACK_BOT_TOKEN=xoxb-1\n",
    )
    .unwrap();
    assert!(ok(gateway(&config, &["validate"])).contains("is valid"));
}

#[test]
fn the_admin_approves_a_person_mints_their_node_and_revokes_it() {
    let (dir, config) = setup();
    let stderr = failed(gateway(&config, &["grant", "approve", "U0PERSON1"]));
    assert!(stderr.contains("admin set"), "{stderr}");

    ok(gateway(&config, &["admin", "set", "U0ADMIN01"]));
    assert_eq!(ok(gateway(&config, &["admin", "show"])).trim(), "U0ADMIN01");

    let stderr = failed(gateway(
        &config,
        &["node", "mint", "--owner", "U0PERSON1", "--name", "laptop"],
    ));
    assert!(stderr.contains("grant approve U0PERSON1"), "{stderr}");

    ok(gateway(&config, &["grant", "approve", "U0PERSON1"]));
    let token_file = dir.path().join("node-token");
    let minted = ok(gateway(
        &config,
        &[
            "node",
            "mint",
            "--owner",
            "U0PERSON1",
            "--name",
            "laptop",
            "--token-file",
            token_file.to_str().unwrap(),
        ],
    ));
    let token = std::fs::read_to_string(&token_file).unwrap();
    assert!(token.starts_with("mrtg_node_"));
    assert!(!minted.contains(token.trim()), "the token was printed too");
    let mode = std::fs::metadata(&token_file).unwrap().permissions().mode();
    assert_eq!(mode & 0o777, 0o600);

    let listed = ok(gateway(&config, &["node", "list"]));
    let selector = listed.split('\t').next().unwrap().to_owned();
    assert!(listed.contains("U0PERSON1\tlaptop\tlive"), "{listed}");
    assert!(ok(gateway(&config, &["node", "revoke", &selector])).contains("Revoked"));
    assert!(ok(gateway(&config, &["node", "list"])).contains("revoked"));
    assert!(ok(gateway(&config, &["grant", "revoke", "U0PERSON1"])).contains("no longer"));
}

#[test]
fn a_channel_id_is_never_accepted_as_a_person() {
    let (_dir, config) = setup();
    let stderr = failed(gateway(&config, &["admin", "set", "C0123ABCD"]));
    assert!(stderr.contains("not a Slack user id"), "{stderr}");
}

#[test]
fn a_node_owners_tool_mode_and_whitelist_are_set_from_the_cli() {
    let (_dir, config) = setup();
    assert_eq!(
        ok(gateway(&config, &["tools", "show", "U0PERSON1"])).trim(),
        "mode\talways-allowed"
    );
    let stderr = failed(gateway(
        &config,
        &["tools", "mode", "U0PERSON1", "sometimes"],
    ));
    assert!(stderr.contains("allowed-whitelist"), "{stderr}");

    ok(gateway(
        &config,
        &["tools", "mode", "U0PERSON1", "allowed-whitelist"],
    ));
    assert!(ok(gateway(&config, &["tools", "allow", "U0PERSON1", "Bash"])).contains("is on"));
    assert!(ok(gateway(&config, &["tools", "allow", "U0PERSON1", "Bash"])).contains("already"));
    ok(gateway(&config, &["tools", "allow", "U0PERSON1", "Read"]));
    assert!(
        ok(gateway(
            &config,
            &["tools", "disallow", "U0PERSON1", "Read"]
        ))
        .contains("is off")
    );
    assert_eq!(
        ok(gateway(&config, &["tools", "show", "U0PERSON1"])).trim(),
        "mode\tallowed-whitelist\nallowed\tBash"
    );
}
