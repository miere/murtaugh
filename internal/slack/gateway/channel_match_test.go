package gateway

import (
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

func agentChannels(m map[string]string) config.ChannelRules {
	out := make(config.ChannelRules, 0, len(m))
	for k, v := range m {
		out = append(out, config.ChannelConfig{Match: k, Agent: v})
	}
	return out
}

func TestMatchChannel(t *testing.T) {
	// Ordered: the narrow rules are listed above the broad ones they must beat.
	channels := config.ChannelRules{
		{Match: "C123", Agent: "id-agent"},
		{Match: "general", Agent: "general-agent"},
		{Match: "feature-prod-*", Agent: "feature-prod-agent"},
		{Match: "feature-*", Agent: "feature-agent"},
		{Match: "release-?", Agent: "release-agent"}, // ? is a valid path.Match glob too
		{Match: "design-channel*", Agent: "design-agent"},
		{Match: "*-prod", Agent: "prod-agent"},
	}

	tests := []struct {
		name        string
		channelID   string
		channelName string
		wantAgent   string
		wantOK      bool
	}{
		{
			name:      "exact channel-ID rule matches",
			channelID: "C123", channelName: "feature-anything",
			wantAgent: "id-agent", wantOK: true,
		},
		{
			name:      "exact channel-name rule",
			channelID: "C999", channelName: "general",
			wantAgent: "general-agent", wantOK: true,
		},
		{
			name:      "single-star glob on name",
			channelID: "C999", channelName: "feature-login",
			wantAgent: "feature-agent", wantOK: true,
		},
		{
			name:      "narrower glob listed first wins",
			channelID: "C999", channelName: "feature-prod-deploy",
			wantAgent: "feature-prod-agent", wantOK: true,
		},
		{
			name:      "suffix glob on name",
			channelID: "C999", channelName: "anything-prod",
			wantAgent: "prod-agent", wantOK: true,
		},
		{
			name:      "no match falls through to default (ok=false)",
			channelID: "C999", channelName: "random",
			wantAgent: "", wantOK: false,
		},
		{
			name:      "empty name with no id match cannot glob-match",
			channelID: "C999", channelName: "",
			wantAgent: "", wantOK: false,
		},
		{
			name:      "empty name still matches an exact channel-ID rule",
			channelID: "C123", channelName: "",
			wantAgent: "id-agent", wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotOK := matchChannel(tt.channelID, tt.channelName, channels)
			if got.Agent != tt.wantAgent || gotOK != tt.wantOK {
				t.Fatalf("matchChannel(%q, %q) = (%q, %v), want (%q, %v)",
					tt.channelID, tt.channelName, got.Agent, gotOK, tt.wantAgent, tt.wantOK)
			}
		})
	}
}

func TestMatchChannelEmptyChannels(t *testing.T) {
	if cc, ok := matchChannel("C123", "general", nil); ok || cc.Agent != "" {
		t.Fatalf("nil channels: got (%q, %v), want (\"\", false)", cc.Agent, ok)
	}
}

// TestMatchChannelReplyOnlyEntry covers an entry that overrides only the reply
// strategy: it matches (ok=true) with an empty Agent, so the caller falls back to
// the default agent while still honouring reply_on_thread.
func TestMatchChannelReplyOnlyEntry(t *testing.T) {
	off := false
	channels := config.ChannelRules{
		{Match: "support-*", ReplyOnThread: &off},
	}
	cc, ok := matchChannel("C1", "support-eu", channels)
	if !ok {
		t.Fatalf("expected a match for support-eu")
	}
	if cc.Agent != "" {
		t.Fatalf("expected empty agent (fall back to default), got %q", cc.Agent)
	}
	if cc.ReplyOnThread == nil || *cc.ReplyOnThread {
		t.Fatalf("expected reply_on_thread=false on the matched entry, got %v", cc.ReplyOnThread)
	}
}

// TestMatchChannelPrecedenceIsPositional pins the defining property of the
// ordered rule list: the FIRST matching rule wins even when a LATER one is more
// specific. This is the deliberate inversion of the old map-based scoring, where
// an exact name always beat a glob no matter where it appeared.
func TestMatchChannelPrecedenceIsPositional(t *testing.T) {
	broadFirst := config.ChannelRules{
		{Match: "feature-*", Agent: "glob-agent"},
		{Match: "feature-x", Agent: "exact-name-agent"},
	}
	if cc, ok := matchChannel("C1", "feature-x", broadFirst); !ok || cc.Agent != "glob-agent" {
		t.Fatalf("broad-first: got (%q, %v), want the first (glob) rule to win", cc.Agent, ok)
	}
	// Reordering is the operator's lever: put the narrow rule on top and it wins.
	narrowFirst := config.ChannelRules{
		{Match: "feature-x", Agent: "exact-name-agent"},
		{Match: "feature-*", Agent: "glob-agent"},
	}
	if cc, ok := matchChannel("C1", "feature-x", narrowFirst); !ok || cc.Agent != "exact-name-agent" {
		t.Fatalf("narrow-first: got (%q, %v), want the exact-name rule to win", cc.Agent, ok)
	}
}

func TestChannelAllowsAnyone(t *testing.T) {
	tests := []struct {
		name        string
		channelName string
		rules       config.ChannelRules
		want        bool
	}{
		{
			name:        "matched rule opts in",
			channelName: "nc-platform",
			rules:       config.ChannelRules{{Match: "nc-*", Agent: "coder", AllowAnyone: true}},
			want:        true,
		},
		{
			name:        "matched rule does not opt in",
			channelName: "mt-ops",
			rules:       config.ChannelRules{{Match: "mt-*", Agent: "admin"}},
			want:        false,
		},
		{
			name:        "no rule matches",
			channelName: "random",
			rules:       config.ChannelRules{{Match: "nc-*", AllowAnyone: true}},
			want:        false,
		},
		{
			name:        "unresolved channel name cannot match a name glob",
			channelName: "",
			rules:       config.ChannelRules{{Match: "nc-*", AllowAnyone: true}},
			want:        false,
		},
		{
			// The safety property: first-match-wins means a narrow CLOSED rule
			// above a broad open one keeps its channel closed. Union semantics
			// would have let the broad rule silently open it.
			name:        "narrow closed rule above a broad open one stays closed",
			channelName: "nc-secrets",
			rules: config.ChannelRules{
				{Match: "nc-secrets", Agent: "admin"},
				{Match: "nc-*", Agent: "coder", AllowAnyone: true},
			},
			want: false,
		},
		{
			name:        "sibling channel under the same broad rule is still open",
			channelName: "nc-platform",
			rules: config.ChannelRules{
				{Match: "nc-secrets", Agent: "admin"},
				{Match: "nc-*", Agent: "coder", AllowAnyone: true},
			},
			want: true,
		},
		{
			name:        "no rules at all",
			channelName: "nc-platform",
			rules:       nil,
			want:        false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := channelAllowsAnyone("C1", tt.channelName, tt.rules); got != tt.want {
				t.Fatalf("channelAllowsAnyone(%q) = %v, want %v", tt.channelName, got, tt.want)
			}
		})
	}
}

func TestValidChannelAgentGlob(t *testing.T) {
	valid := []string{"C123", "general", "feature-*", "*-prod", "release-?", "[a-z]*"}
	for _, k := range valid {
		if !validChannelAgentGlob(k) {
			t.Errorf("validChannelAgentGlob(%q) = false, want true", k)
		}
	}
	// An unterminated character class is a malformed path.Match pattern.
	if validChannelAgentGlob("feature-[a-*") {
		t.Errorf("validChannelAgentGlob(%q) = true, want false (malformed glob)", "feature-[a-*")
	}
}
