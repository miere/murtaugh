package auth

import (
	"context"
	"strings"
	"testing"
)

// A profile with no probe says nothing. Most flows have none, and a diagnostic
// that invented a sentence for them would be noise on every failure card.
func TestVersionDriftIsSilentWithoutAProbe(t *testing.T) {
	p, _ := Lookup("gcloud")
	if got := p.VersionDrift(context.Background()); got != "" {
		t.Fatalf("VersionDrift = %q, want empty for a profile with no probe", got)
	}
}

// A probe that cannot run says nothing either. This is a diagnostic on a path
// that is already failing; it must never add a failure of its own.
func TestVersionDriftIsSilentWhenTheProbeFails(t *testing.T) {
	p := Profile{
		Name:            "broken",
		Command:         "murtaugh-no-such-binary",
		VerifiedVersion: "1.0.0",
		versionProbe:    []string{"--version"},
	}
	if got := p.VersionDrift(context.Background()); got != "" {
		t.Fatalf("VersionDrift = %q, want empty when the probe cannot run", got)
	}
}

// The matching case is the common one, and it must stay quiet.
func TestVersionDriftIsSilentWhenTheVersionsAgree(t *testing.T) {
	p := Profile{
		Name:            "agreeing",
		Command:         "echo",
		VerifiedVersion: "2.1.238",
		versionProbe:    []string{"2.1.238"},
	}
	if got := p.VersionDrift(context.Background()); got != "" {
		t.Fatalf("VersionDrift = %q, want empty when the versions agree", got)
	}
}

// Drift is reported with both numbers, because the useful question on a failure
// card is "verified against what?" — and only the first field of the version
// output is the number, the rest being a product name or build stamp.
func TestVersionDriftNamesBothVersions(t *testing.T) {
	p := Profile{
		Name:            "drifted",
		Command:         "echo",
		VerifiedVersion: "2.1.238",
		versionProbe:    []string{"2.1.260 (Claude Code)"},
	}
	got := p.VersionDrift(context.Background())
	if !strings.Contains(got, "2.1.260") || !strings.Contains(got, "2.1.238") {
		t.Fatalf("VersionDrift = %q, want both the installed and verified versions", got)
	}
	if strings.Contains(got, "Claude Code") {
		t.Errorf("VersionDrift = %q, want only the version field, not the trailing product name", got)
	}
}

// The claude-code profile is the one this exists for, so it must actually carry
// the wiring rather than only the comment that used to stand in for it.
func TestClaudeCodeCarriesAVersionProbe(t *testing.T) {
	p, _ := Lookup("claude-code")
	if p.VerifiedVersion == "" {
		t.Error("claude-code must record the CLI version its output shape was verified against")
	}
	if len(p.versionProbe) == 0 {
		t.Error("claude-code must be able to ask the CLI what version it is now")
	}
}
