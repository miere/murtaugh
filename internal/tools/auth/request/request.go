// Package request implements `auth.request`, which runs a sign-in where the
// agent runs, because that machine's credentials are the ones missing.
package request

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/auth"
)

// Display is how the tool reaches whoever draws the turn; it names no Slack
// destination, so a node can never aim a sign-in card anywhere.
type Display interface {
	SignIn(ctx context.Context, req agent.SignInRequest) (*agent.SignInPrompt, bool)
	SettleSignIn(ctx context.Context, update agent.SignInSettled)
}

// Tool is the `auth.request` capability.
type Tool struct {
	display  Display
	headless Display
	timeout  time.Duration
	urlWait  time.Duration
}

const confirmTimeout = 30 * time.Second

// New leaves the tool registered but inert with a nil display, which is right
// wherever nothing can draw a card.
func New(display Display) *Tool {
	return &Tool{display: display, timeout: auth.DefaultTimeout, urlWait: auth.DefaultURLWait}
}

// WithoutConversation exists because a machine's owner can be reached by DM
// even from work nobody is chatting in, such as a scheduled job.
func (t *Tool) WithoutConversation(display Display) *Tool {
	t.headless = display
	return t
}

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
	return "Request credentials you do not have and WAIT until they are granted. Use this " +
		"when a tool call has failed because of missing or expired authentication — never " +
		"guess, retry blindly, or ask the user to run auth commands themselves. " +
		"Pass `tool` as the capability that is DIRECTLY affected — the MCP server or tool the " +
		"user recognises (e.g. `gcp-mcp`, `postgres-mcp`), NOT the helper binary it shells out " +
		"to underneath (e.g. `gcloud`). Name the helper only when you are invoking it yourself. " +
		"The sign-in runs on the machine you run on, and that machine's owner is sent a direct " +
		"message to complete it — not whoever you are talking to. Returns an error if the owner " +
		"declines it, it times out, or authentication fails — treat any error as a hard stop and " +
		"do not retry the original call. Outside a Slack conversation it only works on a runtime " +
		"node, where the owner is always reached by direct message."
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
	return fmt.Sprintf("Signed in for %s (profile: %s). Retry the call that failed.", r.Tool, r.Profile)
}

// Invoke refuses before starting anything when there is no conversation and no
// owner to message, because a sign-in nobody can be shown would run until it timed out.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	if t.display == nil {
		return nil, fmt.Errorf("Error: authentication requests are not available in this context")
	}
	toolName := strings.TrimSpace(stringArg(args, "tool"))
	if toolName == "" {
		return nil, fmt.Errorf("Error: `tool` is required — name the capability that needs authentication")
	}
	profile, err := auth.Resolve(stringArg(args, "profile"), stringArg(args, "command"), boolArg(args, "needs_code"))
	if err != nil {
		return nil, fmt.Errorf("Error: %s", err.Error())
	}
	display := t.display
	if _, ok := agent.TurnLocationFromContext(ctx); !ok {
		if t.headless == nil {
			return nil, errNoConversation()
		}
		display = t.headless
	}

	timer := time.NewTimer(t.timeout)
	defer timer.Stop()
	req := agent.SignInRequest{Tool: toolName, Profile: profile.Name, NeedsCode: profile.NeedsCode, Command: profile.ApprovalCommand()}
	var prompt *agent.SignInPrompt
	settle := func(state agent.SignInState, reason string) {
		display.SettleSignIn(context.WithoutCancel(ctx), agent.SignInSettled{Prompt: prompt, State: state, Reason: reason})
	}
	start := func() (*auth.Login, string, error) {
		login, url, err := auth.StartLogin(ctx, profile, agent.TurnEnvFromContext(ctx), t.urlWait)
		if err != nil {
			return nil, "", fmt.Errorf("Error: authentication for %s failed: %s", toolName, err.Error())
		}
		return login, url, nil
	}

	var login *auth.Login
	if req.Command != "" {
		var ok bool
		if prompt, ok = display.SignIn(ctx, req); !ok {
			return nil, fmt.Errorf("Error: the sign-in for %s could not be shown to anyone, so its command was not run", toolName)
		}
		if err := t.awaitApproval(ctx, prompt, timer, settle, toolName); err != nil {
			return nil, err
		}
		l, url, err := start()
		if err != nil {
			settle(agent.SignInFailed, err.Error())
			return nil, err
		}
		login = l
		display.SettleSignIn(context.WithoutCancel(ctx), agent.SignInSettled{Prompt: prompt, State: agent.SignInReady, URL: url})
	} else {
		l, url, err := start()
		if err != nil {
			return nil, err
		}
		login = l
		req.URL = url
		var ok bool
		if prompt, ok = display.SignIn(ctx, req); !ok {
			login.Stop()
			return nil, fmt.Errorf("Error: the sign-in for %s could not be shown to anyone, so it was stopped", toolName)
		}
	}
	defer login.Stop()

	for {
		select {
		case answer := <-prompt.Answer:
			if answer.Outcome == agent.DisplayAnswered {
				if answer.Code == "" {
					continue
				}
				if err := login.SendCode(answer.Code); err != nil {
					reason := "could not hand the verification code to the authentication command: " + err.Error()
					settle(agent.SignInFailed, reason)
					return nil, fmt.Errorf("Error: authentication for %s failed: %s", toolName, reason)
				}
				settle(agent.SignInWorking, "")
				continue
			}
			if answer.Outcome == agent.DisplayApproved {
				continue
			}
			settle(agent.SignInCancelled, "")
			return nil, stopped(toolName, answer)

		case <-login.Exited():
			ok, detail := login.Result()
			if ok {
				return t.confirm(ctx, prompt, settle, toolName, profile.Name)
			}
			settle(agent.SignInFailed, detail)
			return nil, fmt.Errorf("Error: authentication for %s failed: %s", toolName, detail)

		case <-timer.C:
			settle(agent.SignInTimedOut, "the sign-in expired before it was completed")
			return nil, fmt.Errorf("Error: the sign-in for %s expired before it was completed. Stop and tell the user; do not retry", toolName)

		case <-ctx.Done():
			settle(agent.SignInCancelled, "")
			return nil, fmt.Errorf("Error: the sign-in for %s was cancelled", toolName)
		}
	}
}

func (t *Tool) confirm(ctx context.Context, prompt *agent.SignInPrompt, settle func(agent.SignInState, string), toolName, profileName string) (any, error) {
	settle(agent.SignInConfirming, "")
	timer := time.NewTimer(confirmTimeout)
	defer timer.Stop()
	for {
		select {
		case answer := <-prompt.Answer:
			switch answer.Outcome {
			case agent.DisplayApproved:
				settle(agent.SignInSuccess, "")
				return Result{Authenticated: true, Tool: toolName, Profile: profileName}, nil
			case agent.DisplayAnswered:
				continue
			}
			settle(agent.SignInFailed, "the owner of this machine lost access before the sign-in finished")
			return nil, fmt.Errorf("Error: the owner of this machine lost access before the sign-in for %s finished, so it counts as declined. Stop and tell the user; do not retry", toolName)
		case <-timer.C:
			settle(agent.SignInFailed, "nobody confirmed the owner could still use the gateway")
			return nil, fmt.Errorf("Error: the sign-in for %s finished, but nobody could confirm its owner may still use the gateway, so it counts as declined. Stop and tell the user; do not retry", toolName)
		case <-ctx.Done():
			settle(agent.SignInCancelled, "")
			return nil, fmt.Errorf("Error: the sign-in for %s was cancelled", toolName)
		}
	}
}

func (t *Tool) awaitApproval(ctx context.Context, prompt *agent.SignInPrompt, timer *time.Timer, settle func(agent.SignInState, string), toolName string) error {
	for {
		select {
		case answer := <-prompt.Answer:
			switch answer.Outcome {
			case agent.DisplayApproved:
				return nil
			case agent.DisplayAnswered:
				continue
			}
			settle(agent.SignInCancelled, "")
			return stopped(toolName, answer)
		case <-timer.C:
			settle(agent.SignInTimedOut, "nobody approved the command before the sign-in expired")
			return fmt.Errorf("Error: nobody approved the sign-in for %s before it expired, so its command was not run. Stop and tell the user; do not retry", toolName)
		case <-ctx.Done():
			settle(agent.SignInCancelled, "")
			return fmt.Errorf("Error: the sign-in for %s was cancelled before its command was approved", toolName)
		}
	}
}

func stopped(toolName string, answer agent.DisplayAnswer) error {
	switch answer.Outcome {
	case agent.DisplayDenied:
		return fmt.Errorf("Error: the owner of this machine declined the sign-in for %s. Stop and tell the user; do not retry", toolName)
	case agent.DisplayDismissed:
		return fmt.Errorf("Error: the sign-in for %s was cancelled", toolName)
	case agent.DisplayNoConversation:
		return errNoConversation()
	}
	if answer.Note != "" {
		return fmt.Errorf("Error: the sign-in for %s was stopped: %s", toolName, answer.Note)
	}
	return fmt.Errorf("Error: sign-ins are not available in this context")
}

func errNoConversation() error {
	return fmt.Errorf("Error: the auth.request tool only works inside a Slack conversation")
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func boolArg(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}
