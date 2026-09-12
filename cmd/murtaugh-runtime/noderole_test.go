package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// This drives run() end to end because the failure was in the composition root:
// the tool itself never ran. An Invoke-level test passed throughout.
func TestCfgNodeSetRunsAgainstANodeRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.BootstrapNode(path); err != nil {
		t.Fatalf("seed a node root: %v", err)
	}

	if err := run([]string{"--config", path, "cfg", "node", "set",
		"--gateway", "wss://gateway.example.com:9443"}); err != nil {
		t.Fatalf("cfg node set on a node root: %v\n"+
			"a node's configuration has no oauth block by design, so an operator asked for one has nothing to do", err)
	}
	if err := run([]string{"--config", path, "cfg", "node", "show"}); err != nil {
		t.Fatalf("cfg node show on a node root: %v", err)
	}

	if err := run([]string{"--config", path, "cfg", "node", "set",
		"--gateway", "https://gateway.example.com"}); err == nil {
		t.Error("an https:// seed address was accepted")
	}
}

// The node root does not exist yet, so this also covers the bug where
// migrate.Run stamped a schema version into a directory that was not there and
// the first start died on `.schema_version` instead of on the missing address.
func TestANodeConfiguresItselfIntoADirectoryThatDoesNotExistYet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh", "config.yaml")

	if err := run([]string{"--config", path, "cfg", "node", "set",
		"--gateway", "wss://gateway.example.com:9443"}); err != nil {
		t.Fatalf("cfg node set against a fresh root: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the node root was not created: %v", err)
	}
}

// A node that cannot say where it dials must fail naming that field, not die on
// a missing schema stamp on the way there.
func TestANodeWithNoSeedAddressFailsClosedNamingTheField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh", "config.yaml")

	err := run([]string{"--config", path})

	if err == nil {
		t.Fatal("a node with no gateway address started anyway")
	}
	if strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("the node died on the schema stamp rather than on the missing address: %v", err)
	}
	if !strings.Contains(err.Error(), "node.gateway") {
		t.Errorf("the refusal does not name the missing field: %v", err)
	}
}
