package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The directory does not exist yet, which is the shape that used to kill the
// first start: migrate.Run stamped `.schema_version` into a directory that was
// not there, so the operator was told about a schema file instead of about the
// credential they had not set.
func TestAGatewayWithNoSlackCredentialsFailsClosedNamingTheField(t *testing.T) {
	t.Setenv("SLACK_APP_TOKEN", "")
	t.Setenv("SLACK_BOT_TOKEN", "")
	path := filepath.Join(t.TempDir(), "fresh", "config.yaml")

	err := run([]string{"--config", path})

	if err == nil {
		t.Fatal("a gateway with no Slack credentials started anyway")
	}
	if strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("the gateway died on the schema stamp rather than on the missing credential: %v", err)
	}
	for _, field := range []string{"oauth.app_token", "oauth.bot_token"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("the refusal does not name %s: %v", field, err)
		}
	}
}

// Setting the variables to empty hides the bug this covers: on a real first run
// nothing sets them, the seeded .env supplies its placeholder, and the gateway
// used to sail past the check and die at Slack with `auth.test: invalid_auth`.
func TestAFirstStartWithNothingInTheEnvironmentNamesTheField(t *testing.T) {
	t.Setenv("SLACK_APP_TOKEN", "")
	t.Setenv("SLACK_BOT_TOKEN", "")
	if err := os.Unsetenv("SLACK_APP_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("SLACK_BOT_TOKEN"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fresh", "config.yaml")

	err := run([]string{"--config", path})

	if err == nil {
		t.Fatal("a gateway with no Slack credentials started anyway")
	}
	if strings.Contains(err.Error(), "invalid_auth") {
		t.Fatalf("the gateway reached Slack with a placeholder credential instead of refusing: %v", err)
	}
	if !strings.Contains(err.Error(), "oauth.app_token") {
		t.Errorf("the refusal does not name oauth.app_token: %v", err)
	}
}

// `help` is resolved before the configuration is touched, so an operator who has
// not configured anything can still find out how to.
func TestHelpAnswersOnAMachineThatWasNeverConfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh", "config.yaml")

	out := captureStdout(t, func() {
		if err := run([]string{"--config", path, "help", "cfg", "launchd"}); err != nil {
			t.Fatalf("help: %v", err)
		}
	})

	if !strings.Contains(out, "cfg launchd") {
		t.Errorf("help did not render the section it was asked for:\n%s", out)
	}
}
