package nodeclaim_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/nodeclaim"
)

// These are the rules that decide what a node's configuration MEANS as a claim.
// They are worth pinning individually because every one of them fails silently
// if it is got wrong: a conversation lands on the wrong machine, or the gateway
// is handed a decision a node admin had no business making.

func TestAdvertiseDerivesClaimsFromChannelRules(t *testing.T) {
	cfg := config.Config{
		Agents: map[string]config.AgentProfile{"reviewer": {}, "shipper": {}},
		Chat: config.ChatConfig{
			Enabled:  true,
			Defaults: config.ChatDefaults{Agent: "shipper"},
			Channels: config.ChannelRules{
				{Match: "review-*", Agent: "reviewer"},
				// No agent: it falls back to the node's default, exactly as the
				// matcher would resolve it. Resolving it here means the gateway
				// is never handed a claim whose profile it has to guess at.
				{Match: "nc-releases"},
			},
		},
	}
	got := nodeclaim.Advertise(cfg, []string{"shipper", "reviewer"})
	want := agentwire.Advertisement{
		Profiles: []string{"reviewer", "shipper"},
		Claims: []agentwire.AssignmentClaim{
			{Match: "review-*", Profile: "reviewer"},
			{Match: "nc-releases", Profile: "shipper"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("advertised %+v, want %+v", got, want)
	}
}

// A node advertises what its PROCESS serves, not what its file configures. The
// two differ whenever a node's configuration names several agents, which is
// ordinary — cmd/murtaugh-runtime picks one, because a link is an agent.
func TestAdvertiseNamesOnlyTheProfilesTheProcessServes(t *testing.T) {
	cfg := config.Config{
		Agents: map[string]config.AgentProfile{"served": {}, "configured-but-not-served": {}},
		Chat: config.ChatConfig{
			Enabled:  true,
			Defaults: config.ChatDefaults{Agent: "served"},
			Channels: config.ChannelRules{
				{Match: "mine-*", Agent: "served"},
				{Match: "theirs-*", Agent: "configured-but-not-served"},
			},
		},
	}
	got := nodeclaim.Advertise(cfg, []string{"served"})
	if !reflect.DeepEqual(got.Profiles, []string{"served"}) {
		t.Fatalf("advertised profiles %v; a node that names a profile it does not serve makes a claim it cannot honour", got.Profiles)
	}
	if len(got.Claims) != 1 || got.Claims[0].Match != "mine-*" {
		t.Fatalf("advertised claims %+v; a channel routed to an unserved profile must not be claimed", got.Claims)
	}
}

// The gateway's own access decision must not be importable from a node's
// configuration file. A node admin — possibly a guest holding a grant — writes
// that file; letting allow_anyone cross would let them open the gateway to the
// whole workspace from their laptop.
func TestAllowAnyoneDoesNotCross(t *testing.T) {
	yes := true
	cfg := config.Config{
		Agents: map[string]config.AgentProfile{"a": {}},
		Chat: config.ChatConfig{
			Enabled:  true,
			Defaults: config.ChatDefaults{Agent: "a"},
			Channels: config.ChannelRules{
				{Match: "open-*", Agent: "a", AllowAnyone: true, ReplyOnThread: &yes},
			},
		},
	}
	raw, err := json.Marshal(nodeclaim.Advertise(cfg, []string{"a"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"allow_anyone", "reply_on_thread"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("the claim carries %q: %s", forbidden, raw)
		}
	}
}

// A node with nothing configured claims nothing, and that is an answer: #170
// makes a zero-profile node the trigger to onboard its owner.
func TestAnUnconfiguredNodeClaimsNothing(t *testing.T) {
	if !nodeclaim.Advertise(config.Config{}, nil).Empty() {
		t.Fatal("a node with no configuration claimed something")
	}
}

// A blank match selects every channel or none depending on who is asked, which
// is exactly the kind of claim that would be debugged for a day. It never
// leaves the node.
func TestABlankMatchIsNotAClaim(t *testing.T) {
	cfg := config.Config{
		Agents: map[string]config.AgentProfile{"a": {}},
		Chat: config.ChatConfig{
			Enabled:  true,
			Defaults: config.ChatDefaults{Agent: "a"},
			Channels: config.ChannelRules{{Match: "   ", Agent: "a"}, {Match: "real", Agent: "a"}},
		},
	}
	got := nodeclaim.Advertise(cfg, []string{"a"})
	if len(got.Claims) != 1 || got.Claims[0].Match != "real" {
		t.Fatalf("advertised %+v", got.Claims)
	}
}

// Deterministic: two runs over one configuration must produce the identical
// value, or the watcher would journal a change every poll.
func TestAdvertiseIsDeterministic(t *testing.T) {
	cfg := config.Config{
		Agents: map[string]config.AgentProfile{"a": {}, "b": {}},
		Chat: config.ChatConfig{
			Enabled:  true,
			Defaults: config.ChatDefaults{Agent: "a"},
			Channels: config.ChannelRules{{Match: "x-*"}, {Match: "y-*", Agent: "b"}},
		},
	}
	first := nodeclaim.Advertise(cfg, []string{"b", "a", "a"})
	for range 20 {
		if !reflect.DeepEqual(nodeclaim.Advertise(cfg, []string{"a", "b"}), first) {
			t.Fatal("the same configuration advertised differently twice")
		}
	}
}
