// Package oneshot is separate from agentdelegate so the gateway, which must not
// link agentbuild, can still drive the same headless turn.
package oneshot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

type Request struct {
	Agent  string
	Prompt string
	// Counts inactivity, not wall clock, so a long turn that keeps working is never killed.
	IdleTimeout time.Duration
}

type sessionCloser interface {
	CloseSession(sessionID string)
}

// The session is ephemeral so claude_code never resumes a previous run, and it is
// closed here because no session manager ever sees it to evict it.
func Drive(ctx context.Context, client agent.Client, req Request) (string, error) {
	session, err := client.NewSession(ctx, agent.SessionMetadata{
		Source:    "delegate",
		Ephemeral: true,
		Headless:  true,
	})
	if err != nil {
		return "", fmt.Errorf("delegate-to-agent: create session for agent %q: %w", req.Agent, err)
	}
	defer func() {
		if closer, ok := client.(sessionCloser); ok {
			closer.CloseSession(session.ID)
		}
	}()

	promptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := client.Prompt(promptCtx, session.ID, agent.PromptRequest{Text: req.Prompt})
	if err != nil {
		return "", fmt.Errorf("delegate-to-agent: prompt agent %q: %w", req.Agent, err)
	}

	var buf strings.Builder
	var idle <-chan time.Time
	var timer *time.Timer
	if req.IdleTimeout > 0 {
		timer = time.NewTimer(req.IdleTimeout)
		defer timer.Stop()
		idle = timer.C
	}
	for {
		select {
		case <-idle:
			cancel()
			for range events {
			}
			return buf.String(), fmt.Errorf("delegate-to-agent: agent %q went idle for %s", req.Agent, req.IdleTimeout)
		case event, ok := <-events:
			if !ok {
				return buf.String(), nil
			}
			if timer != nil {
				resetIdleTimer(timer, req.IdleTimeout)
			}
			if done, err := accumulate(&buf, event, req.Agent); done {
				return buf.String(), err
			}
		}
	}
}

func accumulate(buf *strings.Builder, event agent.Event, agentName string) (bool, error) {
	switch event.Type {
	case agent.EventText:
		buf.WriteString(event.Text)
	case agent.EventError:
		return true, fmt.Errorf("delegate-to-agent: agent %q failed: %w", agentName, event.Error)
	case agent.EventComplete:
		return true, nil
	}
	return false, nil
}

func ExpectJSON(out, agentName string, logger *slog.Logger) ([]byte, error) {
	trimmed := strings.TrimSpace(out)
	if !json.Valid([]byte(trimmed)) {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("delegate-to-agent expected a JSON response but the agent produced something else; skipping render",
			"agent", agentName, "output", trimmed)
		return nil, agent.ErrNonJSONOutput
	}
	return []byte(trimmed), nil
}

func resetIdleTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
