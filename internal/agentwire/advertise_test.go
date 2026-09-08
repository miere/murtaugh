package agentwire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// This file is the advertisement's half of the round-trip proof: what a node
// claims must mean the same thing on the far side, on both of the two paths it
// travels — the handshake answer and the later push.

func fullAdvertisement() Advertisement {
	return Advertisement{
		Profiles: []string{"reviewer", "shipper"},
		Claims: []AssignmentClaim{
			{Match: "C0123456789", Profile: "shipper"},
			{Match: "nc-releases", Profile: "shipper"},
			{Match: "review-*", Profile: "reviewer"},
		},
	}
}

// The connect-time path: the claim rides the answer the gateway already reads,
// so it is in hand at the moment the registry entry is built.
func TestAdvertisementRidesInitializeResult(t *testing.T) {
	yes := true
	result, err := Result("1", InitializeResult{Interruptible: &yes, Advertisement: fullAdvertisement()})
	if err != nil {
		t.Fatalf("build result: %v", err)
	}
	var got InitializeResult
	if err := tripMessage(t, result).Into(&got); err != nil {
		t.Fatalf("read result: %v", err)
	}
	if got.Interruptible == nil || !*got.Interruptible {
		t.Fatal("the advertisement displaced the interruptible answer sharing the frame")
	}
	if !reflect.DeepEqual(got.Advertisement, fullAdvertisement()) {
		t.Fatalf("advertisement came back as %+v", got.Advertisement)
	}
}

// The change path: the same value as a node-initiated request, because a
// configuration edit must reach the gateway without a reconnect and the
// handshake has already happened.
func TestAdvertisementRoundTripsAsARequest(t *testing.T) {
	request, err := Request("7", MethodAdvertise, fullAdvertisement())
	if err != nil {
		t.Fatalf("build advertise: %v", err)
	}
	back := tripMessage(t, request)
	if back.Kind != MessageRequest || back.Method != MethodAdvertise || back.ID != "7" {
		t.Fatalf("advertise request came back as %+v", back)
	}
	var got Advertisement
	if err := back.Into(&got); err != nil {
		t.Fatalf("read advertisement: %v", err)
	}
	if !reflect.DeepEqual(got, fullAdvertisement()) {
		t.Fatalf("advertisement came back as %+v", got)
	}
}

// Claim ORDER is the node's own first-match-wins order. The gateway never
// merges rule lists across nodes, so this list's order is the only precedence
// there is — a serialisation that reordered it would change which profile a
// channel lands on with nothing failing.
func TestClaimOrderSurvives(t *testing.T) {
	request, err := Request("1", MethodAdvertise, fullAdvertisement())
	if err != nil {
		t.Fatalf("build advertise: %v", err)
	}
	var got Advertisement
	if err := tripMessage(t, request).Into(&got); err != nil {
		t.Fatalf("read advertisement: %v", err)
	}
	want := []string{"C0123456789", "nc-releases", "review-*"}
	for i, match := range want {
		if got.Claims[i].Match != match {
			t.Fatalf("claim %d is %q, want %q — the order the node wrote is the only precedence there is", i, got.Claims[i].Match, match)
		}
	}
}

// A node that claims nothing is a node that has never been configured, which
// #170 makes an onboarding trigger rather than an error. It has to survive the
// hop as itself and not as an absence.
func TestEmptyAdvertisementIsAnAnswer(t *testing.T) {
	result, err := Result("1", InitializeResult{})
	if err != nil {
		t.Fatalf("build result: %v", err)
	}
	var got InitializeResult
	if err := tripMessage(t, result).Into(&got); err != nil {
		t.Fatalf("read result: %v", err)
	}
	if !got.Advertisement.Empty() {
		t.Fatalf("a silent node came back claiming %+v", got.Advertisement)
	}
}

// The rule that keeps a node from asserting anything the gateway would act on
// as a permission. A reviewer checks this by reading the struct; this checks it
// by reading the JSON, which is what actually crosses.
func TestAdvertisementCarriesNoIdentityAndNothingThatGrants(t *testing.T) {
	raw, err := json.Marshal(fullAdvertisement())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// node_id: a node that announces its own identity can announce somebody
	// else's. allow_anyone: it waives the gateway's access list, and the node
	// admin who writes it is not the gateway admin.
	for _, forbidden := range []string{"node_id", "user_id", "allow_anyone", "reply_on_thread"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("the advertisement carries %q; identity comes from the credential and access decisions are the gateway's", forbidden)
		}
	}
}

// The gateway holds one of these under a mutex and hands it to delegation while
// the node rebuilds its own on every configuration edit. A value copy shares
// both slices, which is the aliasing that looks safe and is not.
func TestCloneSharesNothing(t *testing.T) {
	original := fullAdvertisement()
	clone := original.Clone()
	clone.Profiles[0] = "mutated"
	clone.Claims[0].Match = "mutated"
	if original.Profiles[0] == "mutated" || original.Claims[0].Match == "mutated" {
		t.Fatal("Clone shares its backing arrays with the original")
	}
	var nothing Advertisement
	if !nothing.Clone().Empty() {
		t.Fatal("cloning nothing produced something")
	}
}

// TestAClaimMeansTheSameOnBothSidesOfTheLink covers the three shapes a claim's
// match can take.
//
// They are chat.channels' three, and the reason the matcher lives on the type
// rather than in the gateway is that a claim written on a node must select the
// same channels on the gateway that reads it. A divergence would be silent: a
// channel that matches on one side and not the other looks exactly like a
// channel nobody claimed, which round robins and never errors.
func TestAClaimMeansTheSameOnBothSidesOfTheLink(t *testing.T) {
	for _, tc := range []struct {
		name        string
		match       string
		channelID   string
		channelName string
		want        bool
	}{
		{"an exact channel id", "C123", "C123", "nc-releases", true},
		{"an exact channel name", "nc-releases", "C123", "nc-releases", true},
		{"a leading glob", "nc-*", "C123", "nc-releases", true},
		{"a trailing glob", "*-prod", "C123", "payments-prod", true},
		{"a glob that does not match", "review-*", "C123", "nc-releases", false},
		{"a name claim with no name resolved", "nc-*", "C123", "", false},
		{"an id claim with no name resolved", "C123", "C123", "", true},
		{"an empty match claims nothing", "", "C123", "nc-releases", false},
		{"a malformed glob claims nothing rather than everything", "[", "C123", "nc-releases", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := AssignmentClaim{Match: tc.match}
			if got := claim.Matches(tc.channelID, tc.channelName); got != tc.want {
				t.Fatalf("Matches(%q, %q) = %v, want %v", tc.channelID, tc.channelName, got, tc.want)
			}
		})
	}
}

// TestTheFirstMatchingClaimWins is the node's own rule list, evaluated in the
// order its admin wrote it — the same positional precedence chat.channels has.
func TestTheFirstMatchingClaimWins(t *testing.T) {
	ad := Advertisement{Claims: []AssignmentClaim{
		{Match: "nc-releases", Profile: "releases"},
		{Match: "nc-*", Profile: "general"},
	}}
	claim, ok := ad.ClaimFor("C1", "nc-releases")
	if !ok || claim.Profile != "releases" {
		t.Fatalf("the narrower rule listed first did not win: %+v ok=%v", claim, ok)
	}
	claim, ok = ad.ClaimFor("C2", "nc-anything-else")
	if !ok || claim.Profile != "general" {
		t.Fatalf("the broader rule did not catch the rest: %+v ok=%v", claim, ok)
	}
	if _, ok := ad.ClaimFor("C3", "review-pr-1"); ok {
		t.Fatal("a node claimed a channel none of its rules names")
	}
	// A node that claims nothing answers no, rather than answering for
	// everything. That is what makes an unconfigured node round-robin fodder
	// instead of a channel's owner.
	if _, ok := (Advertisement{}).ClaimFor("C1", "nc-releases"); ok {
		t.Fatal("an empty advertisement claimed a channel")
	}
}
