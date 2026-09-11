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

func TestAdvertiseDerivesClaimsFromChannelRules(t *testing.T) {
	cfg := config.Config{
		Agents: map[string]config.AgentProfile{"reviewer": {}, "shipper": {}},
		Chat: config.ChatConfig{
			Enabled:  true,
			Defaults: config.ChatDefaults{Agent: "shipper"},
			Channels: config.ChannelRules{
				{Match: "review-*", Agent: "reviewer"},
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

// A node admin, possibly a guest, writes the node's config; if allow_anyone crossed, they could
// open the gateway to the whole workspace from their laptop.
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

func TestAnUnconfiguredNodeClaimsNothing(t *testing.T) {
	if !nodeclaim.Advertise(config.Config{}, nil).Empty() {
		t.Fatal("a node with no configuration claimed something")
	}
}

// A blank match means every channel or none depending on who reads it, so it never leaves the node.
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

// Otherwise the watcher would see a change, and push it, on every poll.
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
