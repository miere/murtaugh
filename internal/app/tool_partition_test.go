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
// slack.send_msg and ping are the same type. So the classification cannot be
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

	// slack.send_msg is the one per-TOOL exception (toolset.Tools), reviewed
	// under #199: a headless job's only way to report its result is to post it,
	// and RunAndForget discards the text on purpose. It executes gateway-side
	// like everything else here, so the bot token does not cross, and its
	// attachment/blocks file-path arguments are denied by name. The rest of the
	// slack family stays gateway-only, which is why this list names the tool and
	// not the namespace.
	want := []string{"ask", "help", "ping", "present_plan", "slack.send_msg", "version"}
	if !slices.Equal(reachable, want) {
		t.Errorf("a runtime node's agent reaches %v; the reviewed set is %v. "+
			"Every name here executes on the gateway with the gateway's credentials", reachable, want)
	}
}

// TestWhatANodeMayARGUEFromTheRealRegistry is the same guard one level down, and
// it exists because the one above was not enough.
//
// The set was reviewed BY NAME. `slack.send_msg` passed that review — the bot
// token executes gateway-side and never crosses — while carrying an `as`
// argument whose own description reads "admin posts as the human admin via their
// Slack user token". A node's agent could therefore post AS THE HUMAN ADMIN, in
// any channel that human can reach, and every assertion in this file stayed
// green because the NAME had not changed.
//
// A tool is its name and its arguments. Widening one at the name level grants
// every argument it happens to carry, including the ones added later by somebody
// who has never read this file, so the reviewed surface has to be spelled out
// argument by argument. What follows is exactly what a node is offered, after
// toolset.DeniedArgs has pruned the schema — the same computation
// nodehost.withoutArgs performs when it publishes the tool list.
func TestWhatANodeMayARGUEFromTheRealRegistry(t *testing.T) {
	registry := buildRegistry(config.Config{}, nil, "", "test", nil, nil, nil, nil, Agents{})

	// Every argument named here executes on the GATEWAY with the gateway's
	// credentials and the gateway's filesystem. Before adding one, ask what it
	// selects, reads or writes on this machine — not what it means on the node.
	want := map[string][]string{
		"ask":          {"questions"},
		"help":         {"command"},
		"ping":         nil,
		"present_plan": {"plan", "title"},
		"version":      nil,
		// No `attachment`/`blocks` (gateway file paths) and no `as` (selects the
		// admin's personal user token). What is left posts text, to a channel,
		// as the app.
		"slack.send_msg": {"attachment_type", "body", "thread", "to"},
	}

	got := map[string][]string{}
	for _, tool := range registry.All() {
		if !toolset.NodeMayReach(tool.Name()) {
			continue
		}
		denied := toolset.DeniedArgs(tool.Name())
		var offered []string
		if schema := tool.InputSchema(); schema != nil {
			for name := range schema.Properties {
				if !slices.Contains(denied, name) {
					offered = append(offered, name)
				}
			}
		}
		slices.Sort(offered)
		got[tool.Name()] = offered
	}

	for name, offered := range got {
		reviewed, ok := want[name]
		if !ok {
			t.Errorf("%s is reachable from a node and its arguments have never been reviewed (%v)", name, offered)
			continue
		}
		if !slices.Equal(offered, reviewed) {
			t.Errorf("a node may pass %s%v; the reviewed arguments are %v. "+
				"An argument that selects a credential, names a gateway path or widens the grant must be added to DenyArgs in toolset.Tools, not to this list",
				name, offered, reviewed)
		}
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("%s is in the reviewed argument table but is no longer reachable from a node; drop the row so the table keeps meaning what it says", name)
		}
	}
}

// TestTheAdminsOwnTokenCannotBeSelectedFromANode names the specific hole, so a
// regression reads as a sentence rather than as a diff of two lists.
func TestTheAdminsOwnTokenCannotBeSelectedFromANode(t *testing.T) {
	if !slices.Contains(toolset.DeniedArgs("slack.send_msg"), "as") {
		t.Fatal("`as` is not denied on slack.send_msg. `as: \"admin\"` selects the admin's personal xoxp- token " +
			"(sendmsg.Tool: case \"admin\": client = t.adminClient) with no identity check and no approval classifier, " +
			"so a node's agent — possibly on somebody else's laptop — could post as the human admin")
	}
}
