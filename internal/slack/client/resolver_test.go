package client

import (
	"bytes"
	"context"
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

func TestResolveTarget_RejectsRawUserID(t *testing.T) {
	api := &fakeAPI{}
	_, err := ResolveTarget(context.Background(), api, "U987")
	if err == nil || !strings.Contains(err.Error(), "--to must start with #") {
		t.Fatalf("ResolveTarget(U987) err = %v, want rejection", err)
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
