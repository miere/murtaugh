package nodehost_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
	"github.com/miere/murtaugh/internal/nodehost"
)

// HEADLESS DISPATCH (#199). Everything below is about work with no user behind
// it: a scheduled job, a workflow trigger, a link unfurl. #170 puts all three on
// the MAIN node, and the reason the tests are written against the real Runtime
// builder rather than against the delegator directly is that "the gateway has no
// delegator at all" was the state before this item and it failed by reporting
// "agent delegation is unavailable" — a silence the wiring, not the algorithm,
// is responsible for.

// delegatorFor builds the runtime a broker gateway would, over an attached
// loopback node, and returns the headless delegator off it.
func delegatorFor(t *testing.T, rig *loopback, access config.AccessConfig, chat bool) agentruntime.Delegator {
	t.Helper()
	cfg := config.Config{
		Agents: map[string]config.AgentProfile{"default": {}},
		Chat:   config.ChatConfig{Enabled: chat, Defaults: config.ChatDefaults{Agent: "default"}},
		Access: access,
	}
	// The gateway's approval gate is wired exactly as the rig wired it, so a
	// card raised by a headless turn lands somewhere a test can see it. Building
	// the runtime with no approver would have made "no card arrived" true for the
	// wrong reason.
	rt := nodehost.Runtime(rig.host)(cfg, testLogger())(agentruntime.Hooks{
		Chat: chat,
		Approvers: map[string]agentruntime.Approver{
			"default": approverFunc(func(_ context.Context, tool, summary string) (bool, string) {
				ask := approval{tool: tool, summary: summary, answer: make(chan approvalAnswer, 1)}
				rig.approved <- ask
				answer := <-ask.answer
				return answer.allowed, answer.note
			}),
		},
	})
	if rt.Delegator == nil {
		t.Fatal("the runtime built no delegator, so every job, workflow trigger and unfurl on this gateway reports that delegation is unavailable")
	}
	return rt.Delegator
}

// answering is a scripted agent that replies with one body and completes.
func answering(body string) func(*scriptedTurn) {
	return func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventText, Text: body})
		turn.emit(agent.Event{Type: agent.EventComplete})
	}
}

// #199's own acceptance: a job, a workflow trigger and an unfurl each executing
// through the broker. The three are one test because they are one mechanism —
// two verbs over one selection path — and splitting them would let a change
// break the pairing without breaking either half.
func TestAJobAWorkflowTriggerAndAnUnfurlAllRunOnTheMainNode(t *testing.T) {
	script := newScriptedAgent(answering(`{"text":"done"}`))
	rig := dialLoopback(t, script)
	delegator := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-1"}, true)

	// A scheduled job. RunAndForget: the text is discarded and the agent is
	// expected to have acted through its own tools.
	if err := delegator.RunAndForget(context.Background(), "default", "run the nightly backup"); err != nil {
		t.Fatalf("a scheduled job did not execute through the broker: %v", err)
	}
	if got := script.lastPrompt().Text; got != "run the nightly backup" {
		t.Fatalf("the job's prompt reached the node as %q", got)
	}

	// A workflow trigger's reply-to-slack arm. RunForJSON: the gateway renders
	// what comes back, so it must be JSON and it must arrive.
	out, err := delegator.RunForJSON(context.Background(), "default", "summarise this form submission")
	if err != nil {
		t.Fatalf("a workflow trigger did not execute through the broker: %v", err)
	}
	if string(out) != `{"text":"done"}` {
		t.Fatalf("the workflow trigger got back %q", out)
	}

	// A link unfurl. Same verb, different consumer — and the one #170 puts on
	// the main node precisely because the sharer usually owns no node.
	if _, err := delegator.RunForJSON(context.Background(), "default", "unfurl https://example.test/x"); err != nil {
		t.Fatalf("an unfurl did not execute through the broker: %v", err)
	}

	if n := script.prompts(); n != 3 {
		t.Fatalf("the main node served %d of the three headless turns", n)
	}
}

// The whole reason headless dispatch needed building: none of these callers has
// a user, and item 10's fleet is built from one. This asserts the two paths stay
// apart — the delegation path still refuses an empty user id, and the headless
// path does not care.
func TestHeadlessWorkNeedsNoUserAndDoesNotTouchTheFleet(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(answering("ok")))
	delegator := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-1"}, true)

	// A chat turn with no user still has no fleet, and says so. If this ever
	// starts passing, fleetFor has been relaxed and headless work is silently
	// borrowing whichever node is attached.
	manager := rig.sessions["default"]
	key := agent.ConversationKey{TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1"}
	_, err := manager.Prompt(context.Background(), key, agent.SessionMetadata{ChannelID: "C1"}, agent.PromptRequest{Text: "hello"})
	if err == nil || !errors.Is(err, nodehost.ErrNoFleet) {
		t.Fatalf("a chat turn with no user was served anyway: %v", err)
	}

	// The same absent user is fine here, because the node was chosen by the
	// gateway's designation and not by whose it is.
	if err := delegator.RunAndForget(context.Background(), "default", "03:00"); err != nil {
		t.Fatalf("headless work was refused for having no user: %v", err)
	}
}

// The main node is a DESIGNATION, and it selects. A second attached node is not
// a fallback and must not be reached.
func TestOnlyTheDesignatedNodeServesHeadlessWork(t *testing.T) {
	main := newScriptedAgent(answering("main"))
	rig := dialLoopback(t, main)
	other := attachScripted(t, rig, "node-2", newScriptedAgent(answering("not main")))

	delegator := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-2"}, true)
	if err := delegator.RunAndForget(context.Background(), "default", "the job"); err != nil {
		t.Fatalf("the designated node did not run the job: %v", err)
	}
	if n := other.agent.prompts(); n != 1 {
		t.Fatalf("node-2 was designated main and was prompted %d times", n)
	}
	if n := main.prompts(); n != 0 {
		t.Fatalf("an undesignated node served headless work %d times; there is no fallback and there must not be one", n)
	}
}

// The failure this item exists to prevent is the SILENT one. Both refusals name
// themselves, and both are journalled at ERROR — the log alone is what "jobs
// stopped firing and nobody noticed for weeks" already looked like.
func TestHeadlessWorkThatCannotBeDispatchedIsLoud(t *testing.T) {
	t.Run("no main node is designated", func(t *testing.T) {
		rec := &recordingJournal{}
		rig := dialLoopback(t, newScriptedAgent(answering("ok")), journalling(rec))
		delegator := delegatorFor(t, rig, config.AccessConfig{}, true)

		err := delegator.RunAndForget(context.Background(), "default", "the job")
		if !errors.Is(err, nodehost.ErrNoMainNode) {
			t.Fatalf("a gateway with no main node reported %v", err)
		}
		// Named, not generic: an operator reading this has to be sent to the
		// configuration and not to the machine.
		if !strings.Contains(err.Error(), "access.main_node") {
			t.Fatalf("the refusal did not say how to fix it: %v", err)
		}
		assertHeadlessRefusal(t, rec, "no-main-node")
	})

	t.Run("the main node is asleep", func(t *testing.T) {
		rec := &recordingJournal{}
		rig := dialLoopback(t, newScriptedAgent(answering("ok")), journalling(rec))
		// Designated, never attached — a laptop that is shut. Distinct from the
		// case above because the remedy is a different person's.
		delegator := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-asleep"}, true)

		err := delegator.RunAndForget(context.Background(), "default", "the job")
		if !errors.Is(err, nodehost.ErrMainNodeOffline) {
			t.Fatalf("a gateway whose main node is gone reported %v", err)
		}
		if errors.Is(err, nodehost.ErrNoMainNode) {
			t.Fatal("the two refusals collapsed into one; they send the reader to different places")
		}
		assertHeadlessRefusal(t, rec, "main-node-offline")
	})
}

func assertHeadlessRefusal(t *testing.T, rec *recordingJournal, state string) {
	t.Helper()
	entry := rec.find(state)
	if entry.Kind != "headless" {
		t.Fatalf("nothing was journalled for %q; the only evidence would have been a log line at 03:00", state)
	}
	if entry.Level != journal.LevelError {
		t.Fatalf("a headless dispatch that could not happen was journalled at %v", entry.Level)
	}
	if entry.Stream != journal.StreamGateway {
		t.Fatalf("the refusal was journalled on stream %q", entry.Stream)
	}
}

// A gateway with chat disabled is exactly the deployment that exists to run
// scheduled jobs, so the delegator has to be built ABOVE the chat check. This is
// the one-line regression that would take jobs out again.
func TestHeadlessWorkRunsOnAGatewayWithChatDisabled(t *testing.T) {
	script := newScriptedAgent(answering("ok"))
	rig := dialLoopback(t, script)
	delegator := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-1"}, false)

	if err := delegator.RunAndForget(context.Background(), "default", "the job"); err != nil {
		t.Fatalf("a chat-disabled gateway could not run a scheduled job: %v", err)
	}
	if n := script.prompts(); n != 1 {
		t.Fatalf("the node was prompted %d times", n)
	}
}

// The wedged-03:00-job failure. In process a delegated agent is built with no
// approver at all, so a job never asks; on a node every agent carries the gate,
// and a job that raised a card would block on an answer nobody can give until
// its timeout burned. A headless session is served with no stream, so the gate
// takes its no-turn branch.
func TestAHeadlessTurnNeverWaitsForAnApprovalNobodyCanGive(t *testing.T) {
	answers := make(chan approvalAnswer, 1)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		allowed, note := turn.gate.Approve(turn.ctx, "terminal", "rm -rf /tmp/x")
		answers <- approvalAnswer{allowed: allowed, note: note}
		turn.emit(agent.Event{Type: agent.EventText, Text: "done"})
		turn.emit(agent.Event{Type: agent.EventComplete})
	})
	rig := dialLoopback(t, script)
	delegator := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-1"}, true)

	done := make(chan error, 1)
	go func() { done <- delegator.RunAndForget(context.Background(), "default", "the 03:00 job") }()

	select {
	case answer := <-answers:
		if !answer.allowed || answer.note != "" {
			t.Fatalf("the gate did not run ungated for a headless turn: %+v", answer)
		}
	case ask := <-rig.approved:
		t.Fatalf("a headless turn raised an approval card for %q; there is no thread to answer it in and the job would have hung", ask.tool)
	case <-time.After(10 * time.Second):
		t.Fatal("the headless turn wedged, which is the 03:00 job burning its whole timeout")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the headless run failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the headless run never returned")
	}

	// And nothing was asked of the gateway after the fact either.
	select {
	case ask := <-rig.approved:
		t.Fatalf("a card arrived late for %q", ask.tool)
	default:
	}
}

// headlessRuns is the probe size, and it is a hundred rather than two because
// this failure is invisible at two. A leak of one session per run is a working
// system for an afternoon.
const headlessRuns = 100

// TestEveryHeadlessRunClosesItsSessionOnTheNode is the leak guard.
//
// oneshot.Drive opens an EPHEMERAL session — a fresh id every call, by design,
// so a claude_code backend does not resume the previous job's transcript — and
// for the in-process runner that was the end of it: agentdelegate closes the
// client and the process dies with it. Over the link the client IS the node's
// long-lived connection, so nothing else will ever end that session. Left open
// it is a retained session on the node, an aggregator registration whose release
// only runs from CloseSession, and on the ACP backend a real subprocess — one
// per scheduled job, per workflow trigger and per PASTED LINK, unbounded until
// the node restarts.
//
// The count is taken on the NODE's agent, past the gateway, past the wire and
// past nodeserve, because every one of those layers is somewhere the close can
// be dropped.
func TestEveryHeadlessRunClosesItsSessionOnTheNode(t *testing.T) {
	script := newScriptedAgent(answering(`{"text":"ok"}`))
	rig := dialLoopback(t, script)
	delegator := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-1"}, true)

	for i := range headlessRuns {
		if err := delegator.RunAndForget(context.Background(), "default", "the 03:00 job"); err != nil {
			t.Fatalf("headless run %d failed: %v", i, err)
		}
	}

	// CloseSession is fire-and-forget by design — the gateway's session manager
	// calls it under its own mutex, so it queues the frame rather than waiting
	// for the node — which is why this waits for the count rather than reading
	// it once.
	deadline := time.Now().Add(10 * time.Second)
	var opened, closed int
	for {
		opened, closed = script.sessionCounts()
		if closed >= opened || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if opened != headlessRuns {
		t.Fatalf("the node opened %d sessions for %d headless runs", opened, headlessRuns)
	}
	if closed != opened {
		t.Fatalf("sessions opened on the node: %d, closed: %d. Each one left behind is a retained session — "+
			"and on an acp backend a live subprocess — per job, per workflow trigger and per pasted link, for as long as the node stays up", opened, closed)
	}
}

// And the same for a delegation that ENDS BADLY, because the ways a turn can end
// outnumber the happy one and each was its own return statement.
func TestAHeadlessRunThatFailsStillClosesItsSession(t *testing.T) {
	script := newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventError, Error: errors.New("the agent fell over")})
	})
	rig := dialLoopback(t, script)
	delegator := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-1"}, true)

	if err := delegator.RunAndForget(context.Background(), "default", "the 03:00 job"); err == nil {
		t.Fatal("a failing agent reported success")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		opened, closed := script.sessionCounts()
		if closed >= opened && opened == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sessions opened on the node: %d, closed: %d, after a turn that ended in an error", opened, closed)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
