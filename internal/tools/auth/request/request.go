// Package request implements the `auth.request` tool: the agent's way to ask
// for credentials it does not have, and WAIT until an admin has granted them.
//
// The tool never authenticates anything by itself. It posts a card to the
// configured admin — the only person who can approve — and blocks until the
// admin completes the sign-in, denies it, or the request expires. The requester
// sees a partial notice in their own thread and nothing more.
//
// It fails closed: anything other than a completed authentication returns an
// error, so a model cannot read a denial or a timeout as permission to carry on.
package request

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/auth"
	"github.com/miere/murtaugh/internal/slack/authcard"
)

// Tool is the `auth.request` capability.
type Tool struct {
	flow *authcard.Flow
}

// New constructs the tool against the shared auth flow. A nil flow leaves the
// tool registered but inert — the right behaviour where there is no gateway to
// route the admin's click back from.
func New(flow *authcard.Flow) *Tool { return &Tool{flow: flow} }

// Name returns the registry key.
func (t *Tool) Name() string { return "auth.request" }

// Description is the model-facing summary.
//
// The `tool` guidance is the important part and is deliberately laboured: the
// model must name the capability the user recognises, not the binary that
// happens to sit underneath it. A card reading "the tool 'gcloud' requires
// authentication" tells an admin nothing about which request they are
// approving; "the tool 'gcp-mcp' requires authentication" does.
func (t *Tool) Description() string {
	return "Request credentials you do not have and WAIT until an admin grants them. Use this " +
		"when a tool call has failed because of missing or expired authentication — never " +
		"guess, retry blindly, or ask the user to run auth commands themselves. " +
		"Pass `tool` as the capability that is DIRECTLY affected — the MCP server or tool the " +
		"user recognises (e.g. `gcp-mcp`, `postgres-mcp`), NOT the helper binary it shells out " +
		"to underneath (e.g. `gcloud`). Name the helper only when you are invoking it yourself. " +
		"The request goes to the configured admin, not to whoever you are talking to. Returns " +
		"an error if the admin denies it, it times out, or authentication fails — treat any " +
		"error as a hard stop and do not retry the original call."
}

// InputSchema declares the profile selector plus the two custom-only arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"tool": {
				Type: "string",
				Description: "The capability that needs authentication, as the user knows it " +
					"(e.g. `gcp-mcp`). Use the directly affected tool, not the binary it shells out to.",
			},
			"profile": {
				Type: "string",
				Description: "Which authentication workflow to run: " + strings.Join(auth.Names(), ", ") +
					". `gcloud` signs in the user credential; `gcloud-adc` writes application-default " +
					"credentials, which is what client libraries and MCP servers usually read; " +
					"`claude-code` re-authenticates the Claude Code CLI itself — the credential every " +
					"claude_code agent runs on — when it has been revoked or has lapsed.",
				Enum: profileEnum(),
			},
			"command": {
				Type: "string",
				Description: "Only with the `custom` profile: the command line to run in the " +
					"background to authenticate. Ignored — and rejected — for built-in profiles.",
			},
			"needs_code": {
				Type: "boolean",
				Description: "Only with the `custom` profile: true when the flow finishes by the " +
					"user pasting a verification code back, false when it completes entirely in " +
					"the browser. Defaults to false.",
			},
		},
		Required: []string{"tool", "profile"},
	}
}

func profileEnum() []any {
	names := auth.Names()
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, n)
	}
	return out
}

// Result is the success shape. Failures are returned as errors instead, so the
// fail-closed outcome cannot be mistaken for a partial success.
type Result struct {
	Authenticated bool   `json:"authenticated"`
	Tool          string `json:"tool"`
	Profile       string `json:"profile"`
}

// String renders the line fed back to the model / shown in the CLI.
func (r Result) String() string {
	return fmt.Sprintf("Authentication for %s completed by the admin (profile: %s). Retry the call that failed.", r.Tool, r.Profile)
}

// Invoke posts the request and blocks until it reaches a terminal state.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	if t.flow == nil {
		return nil, fmt.Errorf("Error: authentication requests are not available in this context")
	}
	toolName := strings.TrimSpace(stringArg(args, "tool"))
	if toolName == "" {
		return nil, fmt.Errorf("Error: `tool` is required — name the capability that needs authentication")
	}

	profile, err := auth.Resolve(
		stringArg(args, "profile"),
		stringArg(args, "command"),
		boolArg(args, "needs_code"),
	)
	if err != nil {
		return nil, fmt.Errorf("Error: %s", err.Error())
	}

	req := authcard.Request{
		ToolName: toolName,
		Profile:  profile,
	}
	// Outside a Slack turn (CLI/MCP) there is no requester to notify, and the
	// flow collapses to the admin card alone.
	if loc, ok := agent.TurnLocationFromContext(ctx); ok {
		req.Requester = authcard.Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS}
		req.RequesterUserID = loc.UserID
	}

	outcome, err := t.flow.Run(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("Error: %s", err.Error())
	}

	switch {
	case outcome.Authenticated:
		return Result{Authenticated: true, Tool: toolName, Profile: profile.Name}, nil
	case outcome.Denied:
		return nil, fmt.Errorf("Error: the admin denied the authentication request for %s. Stop and tell the user; do not retry", toolName)
	case outcome.TimedOut:
		return nil, fmt.Errorf("Error: the authentication request for %s expired before the admin completed it. Stop and tell the user; do not retry", toolName)
	case outcome.Cancelled:
		return nil, fmt.Errorf("Error: the authentication request for %s was cancelled", toolName)
	default:
		return nil, fmt.Errorf("Error: authentication for %s failed: %s", toolName, fallback(outcome.Reason, "no further detail was reported"))
	}
}

func fallback(s, alt string) string {
	if strings.TrimSpace(s) == "" {
		return alt
	}
	return s
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func boolArg(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}
