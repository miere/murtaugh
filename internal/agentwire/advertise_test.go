package agentwire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

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

func TestAdvertisementCarriesNoIdentityAndNothingThatGrants(t *testing.T) {
	raw, err := json.Marshal(fullAdvertisement())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"node_id", "user_id", "allow_anyone", "reply_on_thread"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("the advertisement carries %q; identity comes from the credential and access decisions are the gateway's", forbidden)
		}
	}
}

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
	if _, ok := (Advertisement{}).ClaimFor("C1", "nc-releases"); ok {
		t.Fatal("an empty advertisement claimed a channel")
	}
}
