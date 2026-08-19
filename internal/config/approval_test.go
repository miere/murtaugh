package config

import "testing"

func boolPtr(b bool) *bool { return &b }

// keep_resolved is a *bool precisely so these four cases are distinguishable.
// With a plain bool, "the agent said false" and "the agent said nothing" are the
// same value, and an operator who turned the flag on globally could never exempt
// a single agent — EffectiveApproval bakes the defaults into every profile, so
// the agent's own answer has to be able to win.
func TestEffectiveApprovalKeepResolved(t *testing.T) {
	for _, tc := range []struct {
		name   string
		global *bool
		agent  *bool
		want   bool
	}{
		{"unset everywhere sweeps", nil, nil, false},
		{"agent opts in", nil, boolPtr(true), true},
		{"global opts in", boolPtr(true), nil, true},
		{"agent overrides a global yes", boolPtr(true), boolPtr(false), false},
		{"agent overrides a global no", boolPtr(false), boolPtr(true), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Defaults: RuntimeDefaults{Approval: ApprovalConfig{KeepResolved: tc.global}}}
			got := cfg.EffectiveApproval(AgentProfile{Approval: ApprovalConfig{KeepResolved: tc.agent}})
			if got.KeepsResolved() != tc.want {
				t.Fatalf("KeepsResolved() = %v, want %v", got.KeepsResolved(), tc.want)
			}
		})
	}
}

// The other approval fields must survive the merge unchanged; the new flag is
// additive, not a rewrite of the precedence rules.
func TestEffectiveApprovalKeepsOtherFields(t *testing.T) {
	cfg := Config{Defaults: RuntimeDefaults{Approval: ApprovalConfig{Terminal: "allowlist", Requests: "ask"}}}
	got := cfg.EffectiveApproval(AgentProfile{Approval: ApprovalConfig{
		Terminal:     "prompt",
		KeepResolved: boolPtr(true),
	}})
	if got.Terminal != "prompt" {
		t.Errorf("Terminal = %q, want the agent's override", got.Terminal)
	}
	if got.Requests != "ask" {
		t.Errorf("Requests = %q, want the inherited default", got.Requests)
	}
	if !got.KeepsResolved() {
		t.Error("KeepResolved was dropped by the merge")
	}
}
