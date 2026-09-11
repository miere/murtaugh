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
	"github.com/miere/murtaugh/internal/tools/jobs/run"
)

func delegatorFor(t *testing.T, rig *loopback, access config.AccessConfig, chat bool) agentruntime.Delegator {
	t.Helper()
	cfg := config.Config{
		Agents: map[string]config.AgentProfile{"default": {}},
		Chat:   config.ChatConfig{Enabled: chat, Defaults: config.ChatDefaults{Agent: "default"}},
		Access: access,
	}
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

func answering(body string) func(*scriptedTurn) {
	return func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventText, Text: body})
		turn.emit(agent.Event{Type: agent.EventComplete})
	}
}

// One test because it is one mechanism: split up, a change could break the
// pairing without failing either half.
func TestAJobAWorkflowTriggerAndAnUnfurlAllRunOnTheMainNode(t *testing.T) {
	script := newScriptedAgent(answering(`{"text":"done"}`))
	rig := dialLoopback(t, script)
	delegator := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-1"}, true)

	if err := delegator.RunAndForget(context.Background(), "default", "run the nightly backup"); err != nil {
		t.Fatalf("a scheduled job did not execute through the broker: %v", err)
	}
	if got := script.lastPrompt().Text; got != "run the nightly backup" {
		t.Fatalf("the job's prompt reached the node as %q", got)
	}

	out, err := delegator.RunForJSON(context.Background(), "default", "summarise this form submission")
	if err != nil {
		t.Fatalf("a workflow trigger did not execute through the broker: %v", err)
	}
	if string(out) != `{"text":"done"}` {
		t.Fatalf("the workflow trigger got back %q", out)
	}

	if _, err := delegator.RunForJSON(context.Background(), "default", "unfurl https://example.test/x"); err != nil {
		t.Fatalf("an unfurl did not execute through the broker: %v", err)
	}

	if n := script.prompts(); n != 3 {
		t.Fatalf("the main node served %d of the three headless turns", n)
	}
}

// A node is never trusted to name itself, so the owner must come from its
// credential.
func TestAHeadlessJobHandsBackItsReplyAndTheNodeThatRanIt(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(answering("backups are green")))
	delegator := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-1"}, true)

	replying, ok := delegator.(run.ReplyingDelegator)
	if !ok {
		t.Fatal("the headless delegator cannot hand a reply back, so no scheduled job on a node can ever be reported")
	}
	reply, err := replying.RunForReply(context.Background(), "default", "check the backups")
	if err != nil {
		t.Fatalf("the job did not run: %v", err)
	}
	want := agentruntime.Reply{Text: "backups are green", NodeID: "node-1", NodeOwner: nodeOwner}
	if reply != want {
		t.Fatalf("reply = %+v, want %+v", reply, want)
	}
}

func TestHeadlessWorkNeedsNoUserAndDoesNotTouchTheFleet(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(answering("ok")))
	delegator := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-1"}, true)

	manager := rig.sessions["default"]
	key := agent.ConversationKey{TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1"}
	_, err := manager.Prompt(context.Background(), key, agent.SessionMetadata{ChannelID: "C1"}, agent.PromptRequest{Text: "hello"})
	if err == nil || !errors.Is(err, agentruntime.ErrNoFleet) {
		t.Fatalf("a chat turn with no user was served anyway: %v", err)
	}

	if err := delegator.RunAndForget(context.Background(), "default", "03:00"); err != nil {
		t.Fatalf("headless work was refused for having no user: %v", err)
	}
}

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

// A silent failure here is "jobs stopped firing and nobody noticed for weeks".
func TestHeadlessWorkThatCannotBeDispatchedIsLoud(t *testing.T) {
	t.Run("no main node is designated", func(t *testing.T) {
		rec := &recordingJournal{}
		rig := dialLoopback(t, newScriptedAgent(answering("ok")), journalling(rec))
		delegator := delegatorFor(t, rig, config.AccessConfig{}, true)

		err := delegator.RunAndForget(context.Background(), "default", "the job")
		if !errors.Is(err, nodehost.ErrNoMainNode) {
			t.Fatalf("a gateway with no main node reported %v", err)
		}
		if !strings.Contains(err.Error(), "access.main_node") {
			t.Fatalf("the refusal did not say how to fix it: %v", err)
		}
		assertHeadlessRefusal(t, rec, "no-main-node")
	})

	t.Run("the main node is asleep", func(t *testing.T) {
		rec := &recordingJournal{}
		rig := dialLoopback(t, newScriptedAgent(answering("ok")), journalling(rec))
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

// Chat-disabled gateways exist to run jobs, so the delegator must be built
// before the chat check.
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

// On a node every agent has an approval gate, and nobody can answer a job's
// card, so it would block until its timeout.
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

	select {
	case ask := <-rig.approved:
		t.Fatalf("a card arrived late for %q", ask.tool)
	default:
	}
}

const headlessRuns = 100

// Each run opens a fresh session on the node's long-lived link; unclosed they
// pile up (an ACP subprocess per job) until the node restarts.
func TestEveryHeadlessRunClosesItsSessionOnTheNode(t *testing.T) {
	script := newScriptedAgent(answering(`{"text":"ok"}`))
	rig := dialLoopback(t, script)
	delegator := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-1"}, true)

	for i := range headlessRuns {
		if err := delegator.RunAndForget(context.Background(), "default", "the 03:00 job"); err != nil {
			t.Fatalf("headless run %d failed: %v", i, err)
		}
	}

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
