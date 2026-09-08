package toolset

import "testing"

// The partition is a trust decision, so the pin is on the VERDICTS, not on the
// table's shape. A test that only checked "every family has a row" would pass
// after somebody flipped slack to ReachNode.

func TestTheNodeReachableSetIsExactlyThese(t *testing.T) {
	// Deliberately spelled out rather than derived. Widening a node's surface
	// hands a laptop the gateway's Slack token, its config store or its job
	// runner, and it must not be possible to do that without editing a list that
	// says so.
	want := map[string]bool{"ping": true, "version": true, "help": true, "ask": true, "present_plan": true}

	got := make(map[string]bool)
	for _, family := range Families {
		if family.Reach == ReachNode {
			got[family.Name] = true
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%q is no longer node-reachable; a node's agent silently loses it", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("%q became node-reachable. That is a trust decision: it runs on the gateway with the gateway's credentials. "+
				"If it is right, say so here; do not let the table drift alone", name)
		}
	}
}

func TestTheCredentialBearingFamiliesStayGatewayOnly(t *testing.T) {
	// Each of these was classified from a specific capability — a bot token,
	// exec on the gateway host, the config store, the node credential store, the
	// journal, binary replacement. The reasons live in Families.Why.
	for _, name := range []string{"slack", "jobs", "cfg", "setup", "node", "journal", "troubleshoot", "restart"} {
		reach, why := ReachOf(name)
		if reach != ReachGatewayOnly {
			t.Errorf("%q is no longer gateway-only (%s)", name, why)
		}
		if NodeMayReach(name + ".anything") {
			t.Errorf("a node may reach %q by naming a tool inside it", name)
		}
	}
}

// The zero value denies, so the failure mode of forgetting to classify a new
// family is a tool that does not cross — never a credential that does.
func TestAnUnclassifiedFamilyIsRefused(t *testing.T) {
	reach, why := ReachOf("something.nobody.classified")
	if reach != ReachGatewayOnly {
		t.Fatalf("an unclassified family read as %v; the safe default is the only acceptable one", reach)
	}
	if why == "" {
		t.Fatal("an unclassified family gave no reason, so the refusal is unexplainable to whoever hits it")
	}
}

// The partition is expressed at the granularity the `tools:` allowlist selects
// at — the namespace before the first dot — because any finer and it could not
// be reconciled with what an operator actually writes.
func TestFamilyIsTheNamespaceBeforeTheFirstDot(t *testing.T) {
	for name, want := range map[string]string{
		"ping":            "ping",
		"slack.send_msg":  "slack",
		"node.token.mint": "node",
		"":                "",
	} {
		if got := FamilyOf(name); got != want {
			t.Errorf("FamilyOf(%q) = %q, want %q", name, got, want)
		}
	}
}

// auth.request is the one family classified as wrong-remotely rather than
// unsafe, and the distinction matters: served gateway-side it would SUCCEED and
// write the credential into the gateway's environment, where the node's agent
// never looks. Classifying it node-reachable to be generous would ship a tool
// that reports success and does nothing useful.
func TestAuthRequestIsWrongRemotelyRatherThanForbidden(t *testing.T) {
	reach, _ := ReachOf("auth.request")
	if reach != ReachLocal {
		t.Fatalf("auth.request is classified %v; it belongs in the meaningless-remotely column", reach)
	}
	if NodeMayReach("auth.request") {
		t.Fatal("auth.request crossed to a node")
	}
}
