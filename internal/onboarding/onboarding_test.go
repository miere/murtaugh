package onboarding

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// completeDraft is a form filled in for an OpenAI-compatible backend.
func completeDraft() Draft {
	return Draft{
		Step:    StepModel,
		Name:    "default",
		Kind:    KindOpenAI,
		BaseURL: "https://api.example.com/v1",
		KeyEnv:  "OPENAI_API_KEY",
		Key:     "sk-secret",
		Model:   "gpt-5",
		WorkDir: "/srv/work",
	}
}

// TestClaudeCodeSkipsCredentials pins the conditional flow. Claude Code
// authenticates itself, so a credentials page would be an empty form — which is
// exactly the clutter the multi-step design exists to avoid.
func TestClaudeCodeSkipsCredentials(t *testing.T) {
	d := NewDraft()
	d.Kind = KindClaudeCode

	next := d.Next()
	if next.Step != StepModel {
		t.Fatalf("step = %q after choosing Claude Code, want %q", next.Step, StepModel)
	}
	if len(next.Models) == 0 {
		t.Error("Claude Code went to the model step with no models to choose from")
	}
}

// TestCredentialBackendsAskForAKey covers the other branch.
func TestCredentialBackendsAskForAKey(t *testing.T) {
	for _, kind := range []Kind{KindGemini, KindAnthropic, KindOpenAI} {
		d := NewDraft()
		d.Kind = kind
		if next := d.Next(); next.Step != StepCredentials {
			t.Errorf("%s went to %q, want %q", kind, next.Step, StepCredentials)
		}
	}
}

// TestBaseURLIsOfferedOnlyWhereItMeans pins the field-visibility rule. Gemini
// and Claude Code have exactly one endpoint each, so the box would do nothing.
func TestBaseURLIsOfferedOnlyWhereItMeans(t *testing.T) {
	for kind, want := range map[Kind]bool{
		KindClaudeCode: false,
		KindGemini:     false,
		KindAnthropic:  true,
		KindOpenAI:     true,
	} {
		p, ok := ProviderFor(kind)
		if !ok {
			t.Fatalf("no provider entry for %s", kind)
		}
		if p.NeedsBaseURL != want {
			t.Errorf("%s NeedsBaseURL = %v, want %v", kind, p.NeedsBaseURL, want)
		}
	}
}

// TestBuildProducesTwoProfilesWithDifferentTrust is the security heart of the
// feature. The two profiles differ only in how far they are trusted, and
// collapsing that difference is what the whole split exists to prevent.
func TestBuildProducesTwoProfilesWithDifferentTrust(t *testing.T) {
	out, err := Build(completeDraft(), "/home/op/.config/murtaugh", "U01ADMIN")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The general profile is confined to the workspace and asks before acting.
	if out.DefaultWorkDir != "/srv/work" {
		t.Errorf("default workdir = %q, want the operator's workspace", out.DefaultWorkDir)
	}
	if out.Default.Approval.Terminal != "prompt" {
		t.Errorf("default terminal approval = %q, want prompt", out.Default.Approval.Terminal)
	}
	if out.Default.Approval.Requests != "ask" {
		t.Errorf("default request approval = %q, want ask", out.Default.Approval.Requests)
	}

	// The tweaker is rooted where the configuration lives and is ungated,
	// because it exists to change Murtaugh itself.
	if out.TweakerWorkDir != "/home/op/.config/murtaugh" {
		t.Errorf("tweaker workdir = %q, want the config directory", out.TweakerWorkDir)
	}
	if out.Tweaker.Approval.Terminal != "off" {
		t.Errorf("tweaker terminal approval = %q, want off", out.Tweaker.Approval.Terminal)
	}
	if len(out.Tweaker.ExportSkillsToFS) == 0 {
		t.Error("the tweaker got no skills on disk; it cannot learn how Murtaugh is configured")
	}

	// Both report progress as a task list.
	for name, profile := range map[string]config.AgentProfile{"default": out.Default, "tweaker": out.Tweaker} {
		if profile.ProgressDisplay != string(config.ProgressDisplayTasks) {
			t.Errorf("%s progress display = %q, want tasks", name, profile.ProgressDisplay)
		}
	}
}

// TestTweakerIsBoundToTheAdminAlone is the privilege-escalation guard. Routed
// through dm_agent the tweaker would answer EVERY allowlisted user — an
// unsandboxed, ungated agent rooted in the directory holding the Slack tokens
// and every provider key.
func TestTweakerIsBoundToTheAdminAlone(t *testing.T) {
	out, err := Build(completeDraft(), "/cfg", "U01ADMIN")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if out.Chat.Defaults.DMAgent != "" {
		t.Fatalf("dm_agent = %q; that routes every direct message, not just the admin's", out.Chat.Defaults.DMAgent)
	}
	if got := out.Chat.Defaults.DMAgents["U01ADMIN"]; got != TweakerName {
		t.Errorf("admin DM routes to %q, want %q", got, TweakerName)
	}
	// Everybody else falls through to the general profile.
	if got := out.Chat.Defaults.AgentForDM("U01GUEST"); got != out.Name {
		t.Errorf("a guest's DM routes to %q, want the general profile %q", got, out.Name)
	}
	if got := out.Chat.Defaults.AgentForDM("U01ADMIN"); got != TweakerName {
		t.Errorf("the admin's DM routes to %q, want %q", got, TweakerName)
	}
}

// TestBuildEnablesChat closes the onboarding loop: chat stays off until a
// default agent exists, so the form has to turn it on or the install never
// finishes.
func TestBuildEnablesChat(t *testing.T) {
	out, err := Build(completeDraft(), "/cfg", "U01ADMIN")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !out.Chat.Enabled {
		t.Error("chat was left disabled; the daemon would still answer nobody")
	}
	if out.Chat.Defaults.Agent != "default" {
		t.Errorf("chat default agent = %q, want default", out.Chat.Defaults.Agent)
	}
}

// TestBuiltConfigValidates is the check that matters operationally: the
// profiles are written straight into the store, and one that fails validation
// would leave the daemon unloadable.
func TestBuiltConfigValidates(t *testing.T) {
	for _, kind := range []Kind{KindClaudeCode, KindGemini, KindAnthropic, KindOpenAI} {
		t.Run(string(kind), func(t *testing.T) {
			d := completeDraft()
			d.Kind = kind
			if kind == KindClaudeCode {
				d.Command = "claude"
				d.Model = "opus"
			}
			out, err := Build(d, "/cfg", "U01ADMIN")
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			cfg := config.Config{
				OAuth: config.OAuthConfig{AppToken: "xapp-x", BotToken: "xoxb-x"},
				Agents: map[string]config.AgentProfile{
					out.Name:    out.Default,
					TweakerName: out.Tweaker,
				},
				Chat:   out.Chat,
				Access: config.AccessConfig{AdminUser: "U01ADMIN"},
			}
			if err := cfg.Validate(); err != nil {
				t.Errorf("the generated configuration does not validate: %v", err)
			}
		})
	}
}

// TestCredentialGoesToEnvNotYAML pins where the secret lands. The profile must
// reference the variable; only the .env write carries the value.
func TestCredentialGoesToEnvNotYAML(t *testing.T) {
	out, err := Build(completeDraft(), "/cfg", "U01ADMIN")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out.EnvKey != "OPENAI_API_KEY" || out.EnvValue != "sk-secret" {
		t.Errorf("credential not routed to .env: key=%q value set=%v", out.EnvKey, out.EnvValue != "")
	}
	if out.Default.Native == nil || out.Default.Native.APIKeyEnv != "OPENAI_API_KEY" {
		t.Error("the profile does not reference the credential by variable name")
	}
	// The key itself must not be anywhere in the stored profile.
	if strings.Contains(out.Default.Native.BaseURL, "sk-secret") {
		t.Error("the credential leaked into the profile")
	}
}

// TestReservedNameIsRefused stops the form overwriting the administrator's own
// profile with a general-purpose one.
func TestReservedNameIsRefused(t *testing.T) {
	d := completeDraft()
	d.Name = TweakerName
	if _, err := Build(d, "/cfg", "U01ADMIN"); err == nil {
		t.Fatal("Build accepted the reserved tweaker name")
	}
}

// TestBuildRequiresAnAdmin guards the binding: without an admin there is nobody
// to route the tweaker to, and a tweaker reachable by everyone is the exact
// escalation this design avoids.
func TestBuildRequiresAnAdmin(t *testing.T) {
	if _, err := Build(completeDraft(), "/cfg", "  "); err == nil {
		t.Fatal("Build produced a tweaker with no administrator to bind it to")
	}
}

// TestDraftRoundTripsThroughMetadata covers the state carried between modal
// steps.
func TestDraftRoundTripsThroughMetadata(t *testing.T) {
	original := completeDraft()
	encoded, err := original.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	back, err := DecodeDraft(encoded)
	if err != nil {
		t.Fatalf("DecodeDraft: %v", err)
	}
	if !reflect.DeepEqual(back, original) {
		t.Errorf("draft did not round-trip:\n got %+v\nwant %+v", back, original)
	}
	if fresh, err := DecodeDraft(""); err != nil || fresh.Step != StepProvider {
		t.Errorf("empty metadata did not start a fresh form: %+v err=%v", fresh, err)
	}
}

// TestEncodeStaysUnderSlacksLimit guards the one field that can grow without
// bound. Slack rejects private_metadata over 3000 characters, and an
// OpenAI-compatible endpoint can return hundreds of models.
func TestEncodeStaysUnderSlacksLimit(t *testing.T) {
	d := completeDraft()
	for i := 0; i < 400; i++ {
		d.Models = append(d.Models, strings.Repeat("model-name-that-is-quite-long", 2))
	}
	encoded, err := d.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(encoded) > 3000 {
		t.Errorf("encoded draft is %d characters; Slack rejects anything over 3000", len(encoded))
	}
}

// stubDoer answers a discovery probe from canned JSON.
type stubDoer struct {
	status int
	body   string
	seen   *http.Request
}

func (s *stubDoer) Do(req *http.Request) (*http.Response, error) {
	s.seen = req
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Request:    req,
	}, nil
}

// TestDiscoverModelsPerProvider covers each provider's envelope and auth shape.
// Getting either wrong means the form offers nothing and the install stalls.
func TestDiscoverModelsPerProvider(t *testing.T) {
	t.Run("openai", func(t *testing.T) {
		doer := &stubDoer{body: `{"data":[{"id":"gpt-5"},{"id":"gpt-4o"}]}`}
		d := completeDraft()
		models, err := DiscoverModels(context.Background(), doer, d)
		if err != nil {
			t.Fatalf("DiscoverModels: %v", err)
		}
		if len(models) != 2 || models[0] != "gpt-4o" {
			t.Errorf("models = %v, want a sorted pair", models)
		}
		if got := doer.seen.Header.Get("Authorization"); got != "Bearer sk-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if !strings.HasSuffix(doer.seen.URL.String(), "/v1/models") {
			t.Errorf("URL = %q, want the base URL plus /models", doer.seen.URL)
		}
	})

	t.Run("anthropic sends the version header", func(t *testing.T) {
		doer := &stubDoer{body: `{"data":[{"id":"claude-opus-4"}]}`}
		d := completeDraft()
		d.Kind = KindAnthropic
		d.BaseURL = ""
		if _, err := DiscoverModels(context.Background(), doer, d); err != nil {
			t.Fatalf("DiscoverModels: %v", err)
		}
		if got := doer.seen.Header.Get("anthropic-version"); got == "" {
			t.Error("no anthropic-version header; the API answers 400 without it")
		}
		if got := doer.seen.Header.Get("x-api-key"); got != "sk-secret" {
			t.Errorf("x-api-key = %q", got)
		}
	})

	t.Run("gemini filters non-chat models", func(t *testing.T) {
		doer := &stubDoer{body: `{"models":[
			{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent"]},
			{"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent"]}
		]}`}
		d := completeDraft()
		d.Kind = KindGemini
		models, err := DiscoverModels(context.Background(), doer, d)
		if err != nil {
			t.Fatalf("DiscoverModels: %v", err)
		}
		if len(models) != 1 || models[0] != "gemini-2.5-pro" {
			t.Errorf("models = %v; an embedding model would produce a broken agent", models)
		}
		if !strings.Contains(doer.seen.URL.String(), "key=sk-secret") {
			t.Errorf("Gemini authenticates by query parameter; URL = %q", doer.seen.URL)
		}
	})

	t.Run("claude code needs no probe", func(t *testing.T) {
		d := NewDraft()
		d.Kind = KindClaudeCode
		models, err := DiscoverModels(context.Background(), &stubDoer{status: 500}, d)
		if err != nil {
			t.Fatalf("DiscoverModels: %v", err)
		}
		if len(models) != len(ClaudeCodeModels) {
			t.Errorf("models = %v, want the built-in aliases", models)
		}
	})
}

// TestDiscoverModelsReportsProviderErrors keeps the operator able to tell a
// wrong key from a wrong URL, which is the whole diagnostic value of the step.
func TestDiscoverModelsReportsProviderErrors(t *testing.T) {
	doer := &stubDoer{status: http.StatusUnauthorized, body: `{"error":{"message":"invalid api key"}}`}
	if _, err := DiscoverModels(context.Background(), doer, completeDraft()); err == nil {
		t.Fatal("a 401 was reported as success")
	} else if !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error does not carry the provider's explanation: %v", err)
	}

	empty := &stubDoer{body: `{"data":[]}`}
	if _, err := DiscoverModels(context.Background(), empty, completeDraft()); err == nil {
		t.Fatal("an empty model list was reported as success; the form would offer nothing")
	}
}

// TestDiscoverModelsRequiresAKey stops a pointless unauthenticated probe.
func TestDiscoverModelsRequiresAKey(t *testing.T) {
	d := completeDraft()
	d.Key = ""
	if _, err := DiscoverModels(context.Background(), &stubDoer{}, d); err == nil {
		t.Fatal("discovery ran without a credential")
	}
}
