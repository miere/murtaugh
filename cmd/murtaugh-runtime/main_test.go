package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// The report is this binary's whole output today, and its job is to answer one
// question before the node link exists: would this machine serve anything, and
// as what? A node enrolled against a gateway that then serves nothing is the
// failure it is here to make visible, so a configured agent must appear by name
// AND by resolved kind — the kind is what decides which backend the node would
// start, and it is defaulted rather than stored, so printing the raw field would
// show a blank for every agent that never set one.
func TestReportNamesEachAgentAndItsResolvedKind(t *testing.T) {
	cfg := config.Config{
		BaseDir: "/tmp/murtaugh-runtime-test",
		Agents: map[string]config.AgentProfile{
			// The kind is not stored: it is derived from which backend
			// sub-block is present, and absent means native.
			"zeta":    {ClaudeCode: &config.ClaudeCodeProfile{}},
			"default": {},
		},
	}

	var out strings.Builder
	reportAgents(&out, cfg)
	got := out.String()

	for _, want := range []string{
		"agents: 2",
		"default (native)",
		"zeta (claude_code)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q; got:\n%s", want, got)
		}
	}
	// Sorted, because this is read by a human comparing two machines and map
	// order would make that a diff of nothing.
	if strings.Index(got, "default (") > strings.Index(got, "zeta (") {
		t.Errorf("agents are not listed in name order; got:\n%s", got)
	}
}

// An unconfigured machine must say so rather than printing an empty list that
// reads like a successful enrolment.
func TestReportSaysSoWhenNoAgentIsConfigured(t *testing.T) {
	var out strings.Builder
	reportAgents(&out, config.Config{BaseDir: "/tmp/murtaugh-runtime-test"})

	if got := out.String(); !strings.Contains(got, "agents: none configured") {
		t.Errorf("report does not say the machine has no agents; got:\n%s", got)
	}
}

// The node link is #170 Changes C and D and does not exist. Until it does, a
// full startup must FAIL rather than idle: a supervisor pointed at this binary
// too early has to see a non-zero exit, not a process that starts cleanly and
// serves nothing.
//
// It drives run() rather than reading the sentinel, because the sentinel is
// worth nothing if some later edit forgets to return it. Everything before that
// point is real — migration, config bootstrap, opening the store — so this also
// pins that the startup itself works against a fresh config directory.
func TestRunRefusesToStartWithoutTheNodeLink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	err := run([]string{"--config", path})

	if !errors.Is(err, errNoLinkYet) {
		t.Fatalf("run() = %v, want errNoLinkYet — the binary must not exit 0 having served nothing", err)
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("run() = %q, want it to say the link is not implemented", err)
	}
}
