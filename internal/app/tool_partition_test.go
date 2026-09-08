package app

import (
	"slices"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/toolset"
)

// TestEveryRegisteredToolFamilyHasAPartitionVerdict is the drift guard for the
// tool channel's trust boundary.
//
// toolset.Families is authored by hand, because nothing about a tools.Tool says
// what it holds — the interface is Name/Description/InputSchema/Invoke, and the
// credentials are closed over at construction right here in buildRegistry.
// slack.send-msg and ping are the same type. So the classification cannot be
// derived, which means it can fall behind, and this is the test that stops it.
//
// It lives in internal/app for the same reason the onboarding catalogue guard
// does: this is the only package that can build the real registry.
//
// It is deliberately one-directional. toolset.Families may carry entries the
// registry does not — the synthesized native groups, `manage` — but never the
// reverse. An unclassified family is already REFUSED at runtime (Reach's zero
// value denies), so what this test catches is not a security hole; it is the
// quieter failure where somebody adds a tool a node ought to reach and nobody
// notices it never crosses.
func TestEveryRegisteredToolFamilyHasAPartitionVerdict(t *testing.T) {
	registry := buildRegistry(config.Config{}, nil, "", "test", nil, nil, nil, nil, Agents{})
	if len(registry.All()) == 0 {
		t.Fatal("the registry built no tools; this guard would pass without checking anything")
	}

	classified := make(map[string]bool, len(toolset.Families))
	for _, family := range toolset.Families {
		classified[family.Name] = true
	}

	var missing []string
	for _, tool := range registry.All() {
		family := toolset.FamilyOf(tool.Name())
		if !classified[family] && !slices.Contains(missing, family) {
			missing = append(missing, family)
		}
	}
	if len(missing) > 0 {
		t.Errorf("tool families %v are registered but carry no verdict in toolset.Families. "+
			"A node is refused them by default, which may be right — but say so explicitly, "+
			"because the alternative is a tool nobody can reach and nobody can explain", missing)
	}
}

// And the other end: the set a node actually gets, spelled out against the real
// registry. This is the assertion that would have failed the day somebody
// registered a tool under an existing node-reachable namespace without noticing
// it now crosses the network.
func TestWhatANodeActuallyReachesFromTheRealRegistry(t *testing.T) {
	registry := buildRegistry(config.Config{}, nil, "", "test", nil, nil, nil, nil, Agents{})

	var reachable []string
	for _, tool := range registry.All() {
		if toolset.NodeMayReach(tool.Name()) {
			reachable = append(reachable, tool.Name())
		}
	}
	slices.Sort(reachable)

	want := []string{"ask", "ping", "present_plan", "version"}
	if !slices.Equal(reachable, want) {
		t.Errorf("a runtime node's agent reaches %v; the reviewed set is %v. "+
			"Every name here executes on the gateway with the gateway's credentials", reachable, want)
	}
}
