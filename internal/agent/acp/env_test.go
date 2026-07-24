package acp

import (
	"os"
	"testing"
)

func TestAgentEnvStripsNestedClaudeMarker(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	env := agentEnv([]string{"FOO=bar"})

	for _, kv := range env {
		if kv == "CLAUDECODE=1" {
			t.Fatal("agentEnv must strip the nested-Claude-Code marker")
		}
	}
	// The profile override is preserved, and a real inherited var passes through.
	var sawOverride, sawInherited bool
	os.Setenv("ACP_ENV_TEST_MARKER", "keep") // an ordinary inherited var
	t.Cleanup(func() { os.Unsetenv("ACP_ENV_TEST_MARKER") })
	env = agentEnv([]string{"FOO=bar"})
	for _, kv := range env {
		switch kv {
		case "FOO=bar":
			sawOverride = true
		case "ACP_ENV_TEST_MARKER=keep":
			sawInherited = true
		}
	}
	if !sawOverride {
		t.Fatal("agentEnv dropped the profile override")
	}
	if !sawInherited {
		t.Fatal("agentEnv dropped an ordinary inherited var")
	}
}
