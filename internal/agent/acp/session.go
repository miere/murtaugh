package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/miere/murtaugh/internal/agent"
)

// promptScope is the in-flight context for a session's current prompt: where it
// is talking (loc), the context that is cancelled when the turn ends, and whether
// any reply text was already streamed this turn (sawText) so the final result
// payload is not re-emitted on top of the streamed chunks.
type promptScope struct {
	loc     agent.TurnLocation
	ctx     context.Context
	sawText *atomic.Bool
}

func (c *ProcessClient) NewSession(ctx context.Context, meta agent.SessionMetadata) (agent.Session, error) {
	mcpServers, release := c.aggregatorServers(meta)
	result, err := c.call(ctx, "session/new", map[string]any{
		"cwd":        c.sessionCWD(),
		"mcpServers": mcpServers,
	})
	if err != nil {
		if release != nil {
			release()
		}
		return agent.Session{}, err
	}
	var decoded struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
	}
	if len(result) > 0 {
		if err := json.Unmarshal(result, &decoded); err != nil {
			if release != nil {
				release()
			}
			return agent.Session{}, fmt.Errorf("decode session/new response: %w", err)
		}
	}
	id := decoded.SessionID
	if id == "" {
		id = decoded.ID
	}
	if id == "" {
		if release != nil {
			release()
		}
		return agent.Session{}, errors.New("session/new response did not include sessionId")
	}
	if release != nil {
		c.mu.Lock()
		c.releases = append(c.releases, release)
		c.mu.Unlock()
	}
	return agent.Session{ID: id}, nil
}

// aggregatorServers asks the aggregator (if any) to register this session and
// returns the mcpServers value for session/new plus a release to run if the
// session fails to open. An empty list (and nil release) when no aggregator is
// configured or registration fails — the agent then simply gets no Murtaugh
// tools, which is logged loudly rather than failing the session.
func (c *ProcessClient) aggregatorServers(meta agent.SessionMetadata) ([]any, func()) {
	if c.opts.Aggregator == nil {
		return []any{}, nil
	}
	spec, release, err := c.opts.Aggregator.RegisterSession(meta)
	if err != nil {
		c.log.Warn("aggregator registration failed; ACP agent will have no Murtaugh tools", "error", err)
		return []any{}, nil
	}
	// ACP's env shape is an array of {name,value}; emit in stable key order.
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, map[string]string{"name": k, "value": spec.Env[k]})
	}
	server := map[string]any{
		"name":    spec.Name,
		"command": spec.Command,
		"args":    spec.Args,
		"env":     env,
	}
	return []any{server}, release
}

func (c *ProcessClient) sessionCWD() string {
	if strings.TrimSpace(c.opts.WorkDir) != "" {
		return c.opts.WorkDir
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func (c *ProcessClient) Prompt(ctx context.Context, sessionID string, request agent.PromptRequest) (<-chan agent.Event, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session id is required")
	}
	sub := &subscription{events: make(chan agent.Event, 32)}
	events := sub.events
	sawText := &atomic.Bool{}
	watcher := newToolWatcher(c.now)
	c.mu.Lock()
	c.subscribers[sessionID] = sub
	// Stash where this turn is talking and its context so a permission request
	// raised mid-turn can be asked in the same thread and cancelled with the turn.
	c.dests[sessionID] = promptScope{loc: agent.TurnLocation{ChannelID: request.Channel, ThreadTS: request.Thread}, ctx: ctx, sawText: sawText}
	c.toolWatch[sessionID] = watcher
	c.mu.Unlock()

	go func() {
		// The tool heartbeat keeps this turn alive while a tool legitimately runs and
		// fails it if one runs past its ceiling. It rides an inner cancellable context
		// so the ceiling can unblock the in-flight session/prompt (the ACP agent may
		// still be executing the tool) without disturbing the caller's ctx.
		promptCtx, cancelPrompt := context.WithCancelCause(ctx)
		defer cancelPrompt(nil)
		stopHB := make(chan struct{})
		hbDone := make(chan struct{})
		go c.heartbeat(promptCtx, watcher, events, cancelPrompt, stopHB, hbDone)
		defer func() {
			// Stop the heartbeat and wait for it to exit before closing events, so a
			// keep-alive send can never race the close.
			close(stopHB)
			<-hbDone
			c.clearToolWatch(sessionID, watcher)
			c.closeSubscription(sessionID, sub)
		}()
		result, err := c.call(promptCtx, "session/prompt", map[string]any{
			"sessionId": sessionID,
			"prompt":    c.promptBlocks(request),
		})
		if err != nil {
			// A tool that blew past its ceiling cancels promptCtx with ErrToolCeiling;
			// surface that specific cause rather than the bare context cancellation, so
			// the consumer can render why the turn stopped and drop the session.
			if cause := context.Cause(promptCtx); errors.Is(cause, ErrToolCeiling) {
				events <- agent.Event{Type: agent.EventError, Error: cause}
				return
			}
			events <- agent.Event{Type: agent.EventError, Error: err}
			return
		}
		text := extractText(result)
		stopReason := extractStopReason(result)
		// stop_reason is logged at INFO because it explains why a turn ended,
		// including the cases that produce no reply (max_tokens, refusal): the
		// single most useful signal when a chat comes back empty.
		c.log.Info("ACP prompt completed", "session_id", sessionID, "stop_reason", stopReason, "response_text", text != "")
		// Emit the final result text ONLY when nothing was streamed this turn.
		// Streaming agents (e.g. Claude Code) deliver the reply incrementally via
		// agent_message_chunk notifications and echo the same text in the prompt
		// result; re-emitting it here would duplicate the whole reply as one block
		// at the end. A non-streaming agent that returns its reply only in the
		// result still has it surfaced, since nothing set sawText.
		if text != "" && !sawText.Load() {
			events <- agent.Event{Type: agent.EventText, Text: text}
		}
		events <- agent.Event{Type: agent.EventComplete, StopReason: stopReason}
	}()
	return events, nil
}
