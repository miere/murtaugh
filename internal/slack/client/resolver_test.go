package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestResolveTarget_ChannelByName(t *testing.T) {
	api := &fakeAPI{channels: []Channel{{ID: "C123", Name: "general"}, {ID: "C456", Name: "engineering"}}}
	got, err := ResolveTarget(context.Background(), api, "#engineering")
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if got != "C456" {
		t.Fatalf("ResolveTarget = %q, want %q", got, "C456")
	}
}

func TestResolveTarget_ChannelNotFound(t *testing.T) {
	api := &fakeAPI{channels: []Channel{{ID: "C123", Name: "general"}}}
	_, err := ResolveTarget(context.Background(), api, "#missing")
	if err == nil || !strings.Contains(err.Error(), "Channel 'missing' not found") {
		t.Fatalf("ResolveTarget err = %v, want channel-not-found", err)
	}
}

func TestResolveTarget_UserOpensDM(t *testing.T) {
	api := &fakeAPI{
		users: []User{{ID: "U987", Name: "miere", DisplayName: "Miere", RealName: "Miere de Oliveira"}},
		dmFor: map[string]string{"U987": "D111"},
	}
	got, err := ResolveTarget(context.Background(), api, "@miere")
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if got != "D111" {
		t.Fatalf("ResolveTarget = %q, want %q", got, "D111")
	}
}

func TestResolveTarget_PassesThroughChannelIDs(t *testing.T) {
	api := &fakeAPI{}
	for _, id := range []string{"C123", "G123", "D123"} {
		got, err := ResolveTarget(context.Background(), api, id)
		if err != nil {
			t.Fatalf("ResolveTarget(%q): %v", id, err)
		}
		if got != id {
			t.Fatalf("ResolveTarget(%q) = %q, want %q", id, got, id)
		}
	}
}

// Built from inputs that failed in real agent sessions; listing errors so an ID that
// triggers a lookup fails, since DMs never appear in conversations.list.
func TestResolveTarget_EveryFormAnAgentHolds(t *testing.T) {
	noLookup := &fakeAPI{
		dmFor:           map[string]string{"U0B20G0ET9T": "D0B69D0JVUK", "W0ENTERPR1": "D0ENTDM01"},
		listChannelsErr: errors.New("ListChannels must not be called for an id"),
		listUsersErr:    errors.New("ListUsers must not be called for an id"),
	}
	cases := []struct{ in, want string }{
		{"U0B20G0ET9T", "D0B69D0JVUK"},
		{"@U0B20G0ET9T", "D0B69D0JVUK"},
		{"<@U0B20G0ET9T>", "D0B69D0JVUK"},
		{"<@U0B20G0ET9T|miere>", "D0B69D0JVUK"},
		{"W0ENTERPR1", "D0ENTDM01"},
		{"D0B69D0JVUK", "D0B69D0JVUK"},
		{"C08FH7W48CC", "C08FH7W48CC"},
		{"<#C08FH7W48CC>", "C08FH7W48CC"},
		{"<#C08FH7W48CC|nc-alerts>", "C08FH7W48CC"},
		{"  G0PRIVATE1  ", "G0PRIVATE1"},
	}
	for _, tc := range cases {
		got, err := ResolveTarget(context.Background(), noLookup, tc.in)
		if err != nil {
			t.Errorf("ResolveTarget(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ResolveTarget(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveTarget_NamesStillResolve(t *testing.T) {
	api := &fakeAPI{
		channels: []Channel{{ID: "C0BFN6TN9JP", Name: "nc-reports"}},
		users:    []User{{ID: "U0B20G0ET9T", Name: "miere", DisplayName: "Miere"}},
		dmFor:    map[string]string{"U0B20G0ET9T": "D0B69D0JVUK"},
	}
	cases := []struct{ in, want string }{
		{"@Miere", "D0B69D0JVUK"},
		{"@miere", "D0B69D0JVUK"},
		{"#nc-reports", "C0BFN6TN9JP"},
		{"nc-reports", "C0BFN6TN9JP"},
	}
	for _, tc := range cases {
		got, err := ResolveTarget(context.Background(), api, tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ResolveTarget(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

// A digit is what separates an ID from an all-caps display name.
func TestResolveTarget_AllCapsHandleIsNotAnID(t *testing.T) {
	api := &fakeAPI{
		users: []User{{ID: "U0QA00001", DisplayName: "QA"}},
		dmFor: map[string]string{"U0QA00001": "D0QADM001"},
	}
	got, err := ResolveTarget(context.Background(), api, "@QA")
	if err != nil || got != "D0QADM001" {
		t.Fatalf("ResolveTarget(@QA) = %q, %v; want D0QADM001", got, err)
	}
}

func TestResolveChannel_NotFoundTeachesTheForms(t *testing.T) {
	_, err := ResolveTarget(context.Background(), &fakeAPI{}, "#alerts")
	if err == nil {
		t.Fatal("ResolveTarget(#alerts) succeeded against an empty workspace")
	}
	for _, want := range []string{"Channel 'alerts' not found", "private channel", "D…", "<@U…>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestResolveChannel_RejectsAPerson(t *testing.T) {
	for _, in := range []string{"@miere", "U0B20G0ET9T", "<@U0B20G0ET9T>"} {
		_, err := ResolveChannel(context.Background(), &fakeAPI{}, in)
		if err == nil || !strings.Contains(err.Error(), "names a person") {
			t.Errorf("ResolveChannel(%q) err = %v, want a names-a-person error", in, err)
		}
	}
}

func TestResolveUser_AcceptsEveryIDForm(t *testing.T) {
	api := &fakeAPI{listUsersErr: errors.New("ListUsers must not be called for an id")}
	for _, in := range []string{"U0B20G0ET9T", "@U0B20G0ET9T", "<@U0B20G0ET9T>", "<@U0B20G0ET9T|miere>"} {
		got, err := ResolveUser(context.Background(), api, in)
		if err != nil || got != "U0B20G0ET9T" {
			t.Errorf("ResolveUser(%q) = %q, %v; want U0B20G0ET9T", in, got, err)
		}
	}
}

// Accepting bare IDs would otherwise rewrite the @U… inside <@U…> into <<@U…>>.
func TestResolveMentions_LeavesEscapedMentionsAlone(t *testing.T) {
	in := "ping <@U0B20G0ET9T> about it"
	if got := ResolveMentions(context.Background(), &fakeAPI{}, in, io.Discard); got != in {
		t.Fatalf("ResolveMentions(%q) = %q, want it unchanged", in, got)
	}
}

func TestResolveMentions_ExpandsBareUserID(t *testing.T) {
	got := ResolveMentions(context.Background(), &fakeAPI{}, "cc @U0B20G0ET9T", io.Discard)
	if got != "cc <@U0B20G0ET9T>" {
		t.Fatalf("ResolveMentions = %q, want %q", got, "cc <@U0B20G0ET9T>")
	}
}

func TestResolveTarget_RejectsEmpty(t *testing.T) {
	_, err := ResolveTarget(context.Background(), &fakeAPI{}, "  ")
	if err == nil || !strings.Contains(err.Error(), "--to is required") {
		t.Fatalf("ResolveTarget(empty) err = %v, want required error", err)
	}
}

func TestResolveChannel_MatchesByNameOrID(t *testing.T) {
	api := &fakeAPI{channels: []Channel{{ID: "C123", Name: "general"}}}
	for _, in := range []string{"general", "#general", "C123"} {
		got, err := ResolveChannel(context.Background(), api, in)
		if err != nil {
			t.Fatalf("ResolveChannel(%q): %v", in, err)
		}
		if got != "C123" {
			t.Fatalf("ResolveChannel(%q) = %q, want C123", in, got)
		}
	}
}

func TestResolveUser_PriorityNameDisplayReal(t *testing.T) {
	api := &fakeAPI{users: []User{
		{ID: "U1", Name: "ada", DisplayName: "miere", RealName: "Some Other"},
		{ID: "U2", Name: "bob", DisplayName: "Bob", RealName: "Miere de Oliveira"},
		{ID: "U3", Name: "miere", DisplayName: "Miere", RealName: "Miere"},
	}}
	got, err := ResolveUser(context.Background(), api, "@miere")
	if err != nil {
		t.Fatalf("ResolveUser: %v", err)
	}
	if got != "U3" {
		t.Fatalf("ResolveUser by handle = %q, want U3 (name match first)", got)
	}
	got, err = ResolveUser(context.Background(), api, "miere de oliveira")
	if err != nil {
		t.Fatalf("ResolveUser: %v", err)
	}
	if got != "U2" {
		t.Fatalf("ResolveUser by real name = %q, want U2", got)
	}
}

func TestResolveUser_CaseInsensitive(t *testing.T) {
	api := &fakeAPI{users: []User{{ID: "U1", Name: "Ada"}}}
	got, err := ResolveUser(context.Background(), api, "@ADA")
	if err != nil {
		t.Fatalf("ResolveUser: %v", err)
	}
	if got != "U1" {
		t.Fatalf("ResolveUser case-insensitive = %q, want U1", got)
	}
}

func TestResolveMentions_ReplacesKnownHandle(t *testing.T) {
	api := &fakeAPI{users: []User{{ID: "U1", Name: "ada"}}}
	var warn bytes.Buffer
	got := ResolveMentions(context.Background(), api, "hi @ada and bye", &warn)
	if got != "hi <@U1> and bye" {
		t.Fatalf("ResolveMentions = %q, want hi <@U1> and bye", got)
	}
	if warn.Len() != 0 {
		t.Fatalf("ResolveMentions emitted warnings: %q", warn.String())
	}
}

func TestResolveMentions_UnknownHandleLeftAsIsWithWarning(t *testing.T) {
	api := &fakeAPI{users: []User{{ID: "U1", Name: "ada"}}}
	var warn bytes.Buffer
	got := ResolveMentions(context.Background(), api, "hi @ghost", &warn)
	if got != "hi @ghost" {
		t.Fatalf("ResolveMentions = %q, want hi @ghost", got)
	}
	if !strings.Contains(warn.String(), "Warning: user '@ghost' not found") {
		t.Fatalf("expected warning, got %q", warn.String())
	}
}

func TestResolveMentions_SkipsAtAfterWordChar(t *testing.T) {
	api := &fakeAPI{users: []User{{ID: "U1", Name: "ada"}}}
	got := ResolveMentions(context.Background(), api, "send to foo@ada.dev not @ada", io.Discard)
	if got != "send to foo@ada.dev not <@U1>" {
		t.Fatalf("ResolveMentions = %q, want lookbehind to skip email-like @", got)
	}
}
