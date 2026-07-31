package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// matches flattens rules to their match strings so order can be asserted.
func matches(rules ChannelRules) []string {
	out := make([]string, 0, len(rules))
	for _, cc := range rules {
		out = append(out, cc.Match)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestChannelRulesDecodesOrderedListFromYAML(t *testing.T) {
	var chat ChatConfig
	if err := yaml.Unmarshal([]byte(`
channels:
  - match: "nc-secrets"
    agent: admin
  - match: "nc-*"
    agent: coder
    allow_anyone: true
  - match: "mt-*"
    agent: admin
`), &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"nc-secrets", "nc-*", "mt-*"}
	if got := matches(chat.Channels); !equalStrings(got, want) {
		t.Fatalf("rule order = %v, want %v (as written)", got, want)
	}
	if !chat.Channels[1].AllowAnyone {
		t.Fatal("expected allow_anyone=true on the nc-* rule")
	}
	if chat.Channels[0].AllowAnyone || chat.Channels[2].AllowAnyone {
		t.Fatal("expected allow_anyone to default to false where omitted")
	}
}

// TestChannelRulesDecodesLegacyMapFromYAML: the map shape predates ordering, so
// it is converted to the list that reproduces the OLD matcher's precedence —
// exact IDs, then exact names, then globs by descending literal-prefix length.
// An existing config must keep routing exactly as it did before the upgrade.
func TestChannelRulesDecodesLegacyMapFromYAML(t *testing.T) {
	var chat ChatConfig
	if err := yaml.Unmarshal([]byte(`
channels:
  "feature-*":
    agent: broad
  "*-prod":
    agent: suffix
  "general":
    agent: exact-name
  "feature-prod-*":
    agent: narrow
  "C12345":
    agent: by-id
`), &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"C12345", "general", "feature-prod-*", "feature-*", "*-prod"}
	if got := matches(chat.Channels); !equalStrings(got, want) {
		t.Fatalf("legacy map order = %v, want %v (old precedence preserved)", got, want)
	}
}

// TestChannelRulesLegacyMapOrderIsDeterministic: Go map iteration is randomised,
// so the conversion must not inherit it. Repeat to make any leak surface.
func TestChannelRulesLegacyMapOrderIsDeterministic(t *testing.T) {
	src := []byte(`
channels:
  "alpha-*": {agent: a}
  "beta-*": {agent: b}
  "gamma-*": {agent: c}
  "delta": {agent: d}
`)
	var first []string
	for i := 0; i < 50; i++ {
		var chat ChatConfig
		if err := yaml.Unmarshal(src, &chat); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got := matches(chat.Channels)
		if first == nil {
			first = got
			continue
		}
		if !equalStrings(got, first) {
			t.Fatalf("iteration %d: order %v differs from %v", i, got, first)
		}
	}
}

// TestChannelRulesDecodesLegacyMapFromJSON covers the shape that actually sits in
// the config store today: the chat singleton is persisted as JSON, so an
// already-stored map must keep loading after the upgrade.
func TestChannelRulesDecodesLegacyMapFromJSON(t *testing.T) {
	var chat ChatConfig
	if err := json.Unmarshal([]byte(`{"channels":{"nc-*":{"agent":"coder"},"C999":{"agent":"ops"}}}`), &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"C999", "nc-*"}
	if got := matches(chat.Channels); !equalStrings(got, want) {
		t.Fatalf("legacy JSON map order = %v, want %v", got, want)
	}
}

func TestChannelRulesDecodesOrderedListFromJSON(t *testing.T) {
	var chat ChatConfig
	if err := json.Unmarshal([]byte(`{"channels":[{"match":"nc-*","agent":"coder","allow_anyone":true},{"match":"mt-*","agent":"admin"}]}`), &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, want := matches(chat.Channels), []string{"nc-*", "mt-*"}; !equalStrings(got, want) {
		t.Fatalf("rule order = %v, want %v", got, want)
	}
	if !chat.Channels[0].AllowAnyone {
		t.Fatal("expected allow_anyone=true to survive the JSON round-trip")
	}
}

// TestChannelRulesMarshalsAsList: a legacy map is rewritten to the canonical
// ordered shape the first time the chat singleton is saved, so the map form
// fades out on its own rather than lingering forever.
func TestChannelRulesMarshalsAsList(t *testing.T) {
	var chat ChatConfig
	if err := json.Unmarshal([]byte(`{"channels":{"nc-*":{"agent":"coder"}}}`), &chat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data, err := json.Marshal(chat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round ChatConfig
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if got, want := matches(round.Channels), []string{"nc-*"}; !equalStrings(got, want) {
		t.Fatalf("round-trip = %v, want %v", got, want)
	}
	if round.Channels[0].Agent != "coder" {
		t.Fatalf("round-trip lost the agent: %#v", round.Channels[0])
	}
	// The persisted form must be a JSON array, not an object.
	var probe struct {
		Channels json.RawMessage `json:"channels"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(probe.Channels) == 0 || probe.Channels[0] != '[' {
		t.Fatalf("channels marshalled as %s, want a JSON array", probe.Channels)
	}
}

func TestChannelRulesRejectsScalar(t *testing.T) {
	var chat ChatConfig
	if err := yaml.Unmarshal([]byte("channels: nonsense\n"), &chat); err == nil {
		t.Fatal("expected a scalar channels value to be rejected")
	}
}

// allowAnyoneValidationConfig is a minimal valid config carrying the given rules.
func allowAnyoneValidationConfig(rules ChannelRules) Config {
	return Config{
		OAuth:  OAuthConfig{AppToken: "xapp-test", BotToken: "xoxb-test"},
		Agents: map[string]AgentProfile{"coding": {ACP: &ACPProfile{Command: "/bin/agent"}}},
		Chat:   ChatConfig{Enabled: true, Defaults: ChatDefaults{Agent: "coding"}, Channels: rules},
	}
}

func TestValidateRejectsRuleWithoutMatch(t *testing.T) {
	cfg := allowAnyoneValidationConfig(ChannelRules{{Agent: "coding"}})
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected a rule with no match to be rejected")
	}
}

// TestValidateRejectsDuplicateMatch: with first-match-wins, a duplicate rule is
// unreachable. Surfacing it is friendlier than silently ignoring the second one.
func TestValidateRejectsDuplicateMatch(t *testing.T) {
	cfg := allowAnyoneValidationConfig(ChannelRules{
		{Match: "nc-*", Agent: "coding"},
		{Match: "nc-*", Agent: "coding", AllowAnyone: true},
	})
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected a duplicate match to be rejected")
	}
	if !strings.Contains(err.Error(), "can never match") {
		t.Fatalf("expected the error to explain unreachability, got: %v", err)
	}
}

func TestValidateAcceptsAllowAnyoneRule(t *testing.T) {
	cfg := allowAnyoneValidationConfig(ChannelRules{
		{Match: "nc-*", Agent: "coding", AllowAnyone: true},
		{Match: "mt-*", Agent: "coding"},
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected allow_anyone rules to validate, got: %v", err)
	}
}
