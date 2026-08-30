package gateway

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/credwarden"
)

func TestIsAuthSlashCommand(t *testing.T) {
	for text, want := range map[string]bool{
		"auth":                true,
		"auth status":         true,
		"AUTH":                true,
		"  auth  ":            true,
		"authenticate":        false,
		"restart":             false,
		"":                    false,
		"chat auth something": false,
	} {
		if got := isAuthSlashCommand(text); got != want {
			t.Errorf("isAuthSlashCommand(%q) = %v, want %v", text, got, want)
		}
	}
}

// Anything that is not an explicit `status` is the re-authentication request, so
// a typo cannot silently do nothing.
func TestAuthSlashWantsStatus(t *testing.T) {
	for text, want := range map[string]bool{
		"auth status": true,
		"auth STATUS": true,
		"auth":        false,
		"auth stauts": false,
		"auth login":  false,
	} {
		if got := authSlashWantsStatus(text); got != want {
			t.Errorf("authSlashWantsStatus(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestCredentialStatusWithoutClaudeCodeAgent(t *testing.T) {
	g := &Gateway{}
	got := g.credentialStatusText()
	if !strings.Contains(got, "No `claude_code` agent") {
		t.Fatalf("expected a clear no-agent message, got %q", got)
	}
}

func TestCredentialStatusReportsExpiryAndRefresh(t *testing.T) {
	w := credwarden.New(credwarden.Options{
		Identities: []credwarden.Identity{{Command: "/usr/local/bin/claude"}},
	})
	g := &Gateway{credWarden: w}

	got := g.credentialStatusText()
	if !strings.Contains(got, "/usr/local/bin/claude") {
		t.Fatalf("expected the credential to be named, got %q", got)
	}
	// Nothing observed yet: must say so rather than render a zero time as 1970.
	if !strings.Contains(got, "not yet read") {
		t.Fatalf("expected an unread expiry to be reported as such, got %q", got)
	}
	if !strings.Contains(got, "none this run") {
		t.Fatalf("expected the no-refresh case to be reported, got %q", got)
	}
}

// The status surface is rendered into Slack and the diagnostics bundle, so it
// must never carry credential material — only timings and errors.
func TestCredentialStatusCarriesNoSecretMaterial(t *testing.T) {
	w := credwarden.New(credwarden.Options{
		Identities: []credwarden.Identity{{Command: "/usr/local/bin/claude", Home: "/srv/x"}},
	})
	got := (&Gateway{credWarden: w}).credentialStatusText()
	for _, forbidden := range []string{"sk-ant", "accessToken", "refreshToken"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("status text leaked %q: %s", forbidden, got)
		}
	}
}

// A never-observed credential must not render its zero time as an epoch date.
func TestCredentialStatusNeverRendersEpochForUnreadExpiry(t *testing.T) {
	w := credwarden.New(credwarden.Options{
		Identities: []credwarden.Identity{{Command: "/bin/claude"}},
	})
	if got := (&Gateway{credWarden: w}).credentialStatusText(); strings.Contains(got, "1970") {
		t.Fatalf("zero expiry rendered as an epoch date: %s", got)
	}
}

func TestAuthVerbIsAdvertisedInHelp(t *testing.T) {
	resp := NewDefaultSlashCommandHandler().help("/murtaugh")

	var body strings.Builder
	body.WriteString(resp.Text)
	for _, block := range resp.Blocks {
		if section, ok := block.(*slack.SectionBlock); ok && section.Text != nil {
			body.WriteString(" " + section.Text.Text)
		}
	}
	if !strings.Contains(body.String(), "/murtaugh auth") {
		t.Fatalf("help text does not advertise the auth verb: %q", body.String())
	}
}
