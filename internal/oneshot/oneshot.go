// Package oneshot drives a single headless agent turn over an agent.Client
// that somebody else built: open an ephemeral session, send one prompt,
// accumulate the reply text, stop when the turn finishes or the agent goes
// silent.
//
// It exists as its own package for one reason, and the reason is a build
// constraint rather than tidiness. The same loop is wanted on both sides of
// #170's split — in process by internal/agentdelegate, and on the gateway by
// internal/nodehost driving whichever node is designated main — and
// agentdelegate cannot be linked into cmd/murtaugh-gateway at all: it imports
// internal/agentbuild in order to CONSTRUCT clients, which is one of the five
// packages the reachability guard forbids that binary. Extracting the loop
// inside agentdelegate would have changed nothing, because reachability is a
// property of the package and not of the function.
//
// So the seam is drawn at construction. This package takes a client and never
// makes one, imports only internal/agent, and holds no opinion about whether
// the agent it is talking to is a process on this machine or a laptop across a
// websocket.
//
// What it deliberately does NOT do is Initialize or Close the CLIENT. Both are
// lifecycle, and the two callers have opposite lifecycles: agentdelegate builds
// a fresh process per delegation and must tear it down, while nodehost is handed
// a long-lived connection that was initialized at handshake and that closing
// would drop the whole node.
//
// The SESSION is the other way round, and the distinction is the whole of #199's
// worst bug. This package opens the session, so this package closes it. In
// process that never mattered — agentdelegate's `defer client.Close()` took the
// whole process down with it — but over the link the client IS the long-lived
// connection, so a session nobody closes is a session that lives until the node
// restarts: a procSession retained in the remote client's map, an aggregator
// registration whose release only runs from CloseSession, and on the ACP backend
// a real subprocess, one per scheduled job, per workflow trigger and per pasted
// link. Link unfurling made it a leak per message.
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

// Request is one delegation.
type Request struct {
	// Agent is the profile name, used only in errors and logs — resolving it to
	// something runnable is the caller's job and happens before this package is
	// reached.
	Agent string
	// Prompt is the single user message the turn carries.
	Prompt string
	// IdleTimeout bounds the turn by INACTIVITY rather than by wall clock, so a
	// long but productive delegation is never killed mid-flight. Zero means the
	// turn is bounded only by ctx.
	IdleTimeout time.Duration
}

// sessionCloser is the optional surface a Client implements when a session owns
// a resource that has to be released — a per-conversation process for the ACP
// and claude_code backends, and the node's whole session record for
// agent/remote. It is declared structurally, exactly as internal/agent's session
// manager declares it, so this package keeps taking a plain agent.Client: a
// client that multiplexes everything over one loop implements nothing and the
// close below is a no-op.
type sessionCloser interface {
	CloseSession(sessionID string)
}

// Drive runs one turn and returns the agent's accumulated text.
//
// The session is opened Ephemeral and Headless. Ephemeral because a delegation
// belongs to no conversation, so there is nothing to resume — without it every
// delegation derives the same session id from the same empty conversation
// triple and a claude_code backend resumes the previous run's transcript.
// Headless because there is nobody to ask: in process that is expressed by
// building the client with no approver, but over a link the node builds every
// agent with its gate and needs telling.
//
// Ephemeral is also precisely why the session must be closed here. Nothing else
// can: it belongs to no conversation, so the session manager never sees it and
// its idle eviction never reaches it, and the caller is handed only the text.
// One turn is its entire life.
func Drive(ctx context.Context, client agent.Client, req Request) (string, error) {
	session, err := client.NewSession(ctx, agent.SessionMetadata{
		Source:    "delegate",
		Ephemeral: true,
		Headless:  true,
	})
	if err != nil {
		return "", fmt.Errorf("delegate-to-agent: create session for agent %q: %w", req.Agent, err)
	}
	// Deferred rather than closed on the way out of the loop, because every
	// return below is a way this turn can end — completion, error, idle
	// timeout, a channel that closed without saying so — and the leak is the
	// same one on all of them.
	defer func() {
		if closer, ok := client.(sessionCloser); ok {
			closer.CloseSession(session.ID)
		}
	}()

	// A child context we can cancel ourselves, so the idle watchdog can unblock
	// the in-flight request without disturbing ctx.
	promptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := client.Prompt(promptCtx, session.ID, agent.PromptRequest{Text: req.Prompt})
	if err != nil {
		return "", fmt.Errorf("delegate-to-agent: prompt agent %q: %w", req.Agent, err)
	}

	// A zero IdleTimeout leaves `idle` nil, which never fires — the turn is then
	// bounded only by ctx. That is a caller's choice to make and not a default
	// this package invents.
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
			// Silent for the whole idle window. Unblock the in-flight request
			// and drain so the client tears down cleanly.
			cancel()
			for range events {
			}
			return buf.String(), fmt.Errorf("delegate-to-agent: agent %q went idle for %s", req.Agent, req.IdleTimeout)
		case event, ok := <-events:
			if !ok {
				// The channel closed with no explicit completion event: the
				// accumulated output is the result.
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

// accumulate folds one event into the reply and says whether the turn is over.
func accumulate(buf *strings.Builder, event agent.Event, agentName string) (bool, error) {
	switch event.Type {
	case agent.EventText:
		// Only the agent's reply text is captured. EventStatus is progress and
		// meta (compaction, for instance) and must not pollute the output, which
		// a caller may parse as JSON.
		buf.WriteString(event.Text)
	case agent.EventError:
		return true, fmt.Errorf("delegate-to-agent: agent %q failed: %w", agentName, event.Error)
	case agent.EventComplete:
		return true, nil
	}
	return false, nil
}

// ExpectJSON requires a delegation's output to be a single valid JSON document
// — a Slack message or an unfurl attachment — and returns
// agent.ErrNonJSONOutput when it is not, so the caller can skip rendering
// without treating it as a hard failure.
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

// resetIdleTimer restarts t for another idle window, draining an already-fired
// timer first so the next select does not observe a stale tick.
func resetIdleTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
