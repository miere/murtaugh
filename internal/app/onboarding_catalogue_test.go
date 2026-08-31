package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/onboarding"
	"github.com/miere/murtaugh/internal/toolset"
)

// TestToolFamilyCatalogueCoversTheRegistry is the drift guard for the setup
// form's tool picker.
//
// The catalogue in internal/onboarding is curated by hand — a picker needs a
// label, a sentence of consequence and a considered default, none of which a
// registry can supply. The cost of that is a list that can silently fall behind
// a newly registered namespace, and the symptom is the worst kind: a tool that
// exists, works, and is simply never offered to anybody.
//
// This test lives in internal/app because this is the only package that can
// build the real registry (the composition root), and it is deliberately
// one-directional: the catalogue may carry entries the registry does not
// (toolset's synthesized native groups, and `manage`, which registers no tool
// at all), but never the reverse.
func TestToolFamilyCatalogueCoversTheRegistry(t *testing.T) {
	registry := buildRegistry(config.Config{}, nil, "", "test", nil, nil, nil, nil)
	// Without this the whole test passes vacuously if buildRegistry ever starts
	// returning nothing for a zero config — which is exactly the state a drift
	// guard must not be able to reach quietly.
	if len(registry.All()) == 0 {
		t.Fatal("the registry built no tools; this guard would pass without checking anything")
	}

	catalogued := make(map[string]bool, len(onboarding.ToolFamilies))
	for _, family := range onboarding.ToolFamilies {
		catalogued[family.Name] = true
	}

	var missing []string
	for _, tool := range registry.All() {
		// The allowlist selects by namespace (the part before the first dot),
		// or by the whole name when a tool has none.
		family := tool.Name()
		if i := strings.IndexByte(family, '.'); i >= 0 {
			family = family[:i]
		}
		if !catalogued[family] && !slices.Contains(missing, family) {
			missing = append(missing, family)
		}
	}
	if len(missing) > 0 {
		t.Errorf("tool families %v are registered but absent from onboarding.ToolFamilies, "+
			"so the setup form can never offer them; add them to the catalogue", missing)
	}
}

// TestCataloguedNativeGroupsExist keeps the other end honest: a native group
// named in the catalogue but not in toolset.NativeGroups would resolve to
// nothing, offering the operator a family that quietly grants no tool.
func TestCataloguedNativeGroupsExist(t *testing.T) {
	registry := buildRegistry(config.Config{}, nil, "", "test", nil, nil, nil, nil)

	registered := make(map[string]bool)
	for _, tool := range registry.All() {
		name := tool.Name()
		if i := strings.IndexByte(name, '.'); i >= 0 {
			name = name[:i]
		}
		registered[name] = true
	}

	for _, family := range onboarding.ToolFamilies {
		switch {
		case registered[family.Name], toolset.IsNativeGroup(family.Name):
			continue
		case family.Name == toolset.GroupManage:
			// Contributes no tool by design: it is a skills-visibility token.
			continue
		default:
			t.Errorf("catalogued family %q matches no registry namespace and no native group; "+
				"selecting it would grant nothing", family.Name)
		}
	}
}
