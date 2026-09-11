package run

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
)

type replyingDelegator struct {
	fakeDelegator
	reply agentruntime.Reply
}

func (r *replyingDelegator) RunForReply(_ context.Context, agent, prompt string) (agentruntime.Reply, error) {
	r.calls++
	r.agent, r.prompt = agent, prompt
	return r.reply, r.err
}

func (r *replyingDelegator) RunAndForget(context.Context, string, string) error {
	panic("a delegator that can hand the reply back was asked to throw it away")
}

func digestJobs() map[string]config.JobProfile {
	return map[string]config.JobProfile{"digest": {Agent: "default", Prompt: "summarise yesterday"}}
}

func TestInvoke_AgentJob_CarriesTheReplyBack(t *testing.T) {
	want := agentruntime.Reply{Text: "all green", NodeID: "node-1", NodeOwner: "U1"}
	tl := New(lookupFrom(digestJobs())).WithDelegator(&replyingDelegator{reply: want})

	res, err := tl.Invoke(context.Background(), map[string]any{"name": "digest"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	got := res.(Result).Reply
	if got == nil || *got != want {
		t.Fatalf("Result.Reply = %+v, want %+v", got, want)
	}
}

// A delegator that cannot hand a reply back still runs the job; the result
// simply has nothing to report, rather than an empty reply that reads as one.
func TestInvoke_AgentJob_WithoutAReplyingDelegatorHasNoReply(t *testing.T) {
	tl := New(lookupFrom(digestJobs())).WithDelegator(&fakeDelegator{})

	res, err := tl.Invoke(context.Background(), map[string]any{"name": "digest"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := res.(Result).Reply; got != nil {
		t.Fatalf("Result.Reply = %+v, want nil", got)
	}
}

// Only the gateway knows whether the node that wrote a reply is the admin's, so
// jobs.run keeps none of it: no text in the row and no blob beside it.
func TestInvoke_AgentJob_JournalsNoReplyContent(t *testing.T) {
	rec := &fakeRecorder{}
	secret := "the payroll figures for the node owner's eyes only"
	tl := New(lookupFrom(digestJobs())).
		WithDelegator(&replyingDelegator{reply: agentruntime.Reply{Text: secret, NodeID: "node-bob", NodeOwner: "UBOB0000"}}).
		WithRecorder(rec)

	if _, err := tl.Invoke(context.Background(), map[string]any{"name": "digest"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	e := rec.only(t)
	encoded, err := json.Marshal(e.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if strings.Contains(string(encoded), "payroll") || strings.Contains(e.Summary, "payroll") || e.BlobRef != "" {
		t.Fatalf("jobs.run kept the reply before the gateway decided who may see it: %s (blob %q)", encoded, e.BlobRef)
	}
}
