package agent

import (
	"os"
	"testing"
)

func TestSpawnEnvStripsNestedClaudeMarker(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	os.Setenv("SPAWN_ENV_TEST_MARKER", "keep")
	t.Cleanup(func() { os.Unsetenv("SPAWN_ENV_TEST_MARKER") })

	env := SpawnEnv([]string{"FOO=bar"})

	var sawMarker, sawOverride, sawInherited bool
	for _, kv := range env {
		switch kv {
		case "CLAUDECODE=1":
			sawMarker = true
		case "FOO=bar":
			sawOverride = true
		case "SPAWN_ENV_TEST_MARKER=keep":
			sawInherited = true
		}
	}
	if sawMarker {
		t.Fatal("SpawnEnv must strip the nested-Claude-Code marker")
	}
	if !sawOverride {
		t.Fatal("SpawnEnv dropped the override")
	}
	if !sawInherited {
		t.Fatal("SpawnEnv dropped an ordinary inherited var")
	}
}
