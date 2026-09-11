package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/agent"
)

func newTestSectionRenderer(api, cards *fakeStreamAPI) *sectionRenderer {
	opts := StreamWriterOptions{ThreadTS: "100.0", Interval: time.Hour, MinChars: 1, Logger: discardLogger()}
	return newSectionRenderer(
		func() SlackSink { return NewStreamWriter(api, "C1", opts) },
		func() toolBlock { return newCardToolBlock(cards, "C1", opts, discardLogger()) },
		nil, nil, "C1", "100.0",
		discardLogger(),
	)
}

// TestSectionRenderer_AlternatesBlocksAndMessages is the core UX guarantee: tool
// activity and reply text are rendered as a SEPARATE, ordered sequence of Slack
// messages — a tool block per contiguous tool run, a streamed message per
// contiguous text run — never mixed, regardless of model interleaving. Mirrors
// the canonical "run read/skill/write → talk → run a tool → wrap up" flow, which
// must produce exactly: block, message, block, message.
func TestSectionRenderer_AlternatesBlocksAndMessages(t *testing.T) {
	api := &fakeStreamAPI{}
	cards := &fakeStreamAPI{}
	r := newTestSectionRenderer(api, cards)
	ctx := context.Background()

	// Block 1: three contiguous tools coalesce into one block.
	_ = r.Task(ctx, &agent.TaskEvent{ID: "1", Title: "read", Status: agent.TaskStatusInProgress})
	_ = r.Task(ctx, &agent.TaskEvent{ID: "2", Title: "skill", Status: agent.TaskStatusInProgress})
	_ = r.Task(ctx, &agent.TaskEvent{ID: "3", Title: "write", Status: agent.TaskStatusInProgress})
	// Message 1.
	_ = r.Text(ctx, "here is what I found")
	// Block 2.
	_ = r.Task(ctx, &agent.TaskEvent{ID: "4", Title: "fetch", Status: agent.TaskStatusInProgress})
	// Message 2 (the wrap-up).
	_ = r.Text(ctx, "all done")
	if err := r.Finish(ctx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if cards.starts != 2 {
		t.Errorf("expected 2 tool-block messages, got %d", cards.starts)
	}
	if len(api.startOptions) != 2 {
		t.Errorf("expected 2 text messages, got %d", len(api.startOptions))
	}
	if api.stops != 2 {
		t.Errorf("expected both text messages to be stopped, got %d", api.stops)
	}
}

// A turn that ends with tools still in flight must not leave their cards
// spinning, since a spinner that never stops reads as work still going on.
func TestSectionRenderer_BlockCompletesItsCards(t *testing.T) {
	api := &fakeStreamAPI{}
	cards := &fakeStreamAPI{}
	r := newTestSectionRenderer(api, cards)
	ctx := context.Background()

	_ = r.Task(ctx, &agent.TaskEvent{ID: "1", Title: "read", Status: agent.TaskStatusInProgress})
	_ = r.Task(ctx, &agent.TaskEvent{ID: "2", Title: "skill", Status: agent.TaskStatusInProgress})
	_ = r.Task(ctx, &agent.TaskEvent{ID: "3", Title: "write", Status: agent.TaskStatusInProgress})
	if err := r.Finish(ctx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	last := map[string]slack.TaskCardStatus{}
	for _, chunks := range allStreamWrites(t, cards) {
		for _, task := range taskChunks(chunks) {
			last[task.ID] = task.Status
		}
	}
	for _, id := range []string{"1", "2", "3"} {
		if last[id] != slack.TaskCardStatusComplete {
			t.Errorf("card %s ended %q, want complete (all: %v)", id, last[id], last)
		}
	}
	if cards.stops != 1 {
		t.Errorf("the tool block's message was stopped %d times, want once", cards.stops)
	}
}

// TestSectionRenderer_PlanSnapshotsDoNotChopReply reproduces the ACP failure that
// motivated the "only new tool runs seal" rule: an agent re-sends its plan as a
// full snapshot many times while its reply streams. Each snapshot arrives as a
// burst of task events interleaved with the text — and must NOT split the reply
// (the thread's "coder-mono ag | [card] | ent" mid-word shredding). The reply
// stays one streamed message; the plan renders once, as a trailing block.
func TestSectionRenderer_PlanSnapshotsDoNotChopReply(t *testing.T) {
	api := &fakeStreamAPI{}
	cards := &fakeStreamAPI{}
	r := newTestSectionRenderer(api, cards)
	ctx := context.Background()

	_ = r.Text(ctx, "The config diff shows a new coder-mono ")
	_ = r.Task(ctx, &agent.TaskEvent{ID: "plan-0", Title: "check diff", Status: agent.TaskStatusInProgress, Kind: agent.TaskKindPlan})
	_ = r.Text(ctx, "agent plus routing changes. ")
	// A refreshed snapshot: same entries re-sent, one advancing to complete.
	_ = r.Task(ctx, &agent.TaskEvent{ID: "plan-0", Title: "check diff", Status: agent.TaskStatusComplete, Kind: agent.TaskKindPlan})
	_ = r.Task(ctx, &agent.TaskEvent{ID: "plan-1", Title: "scan jobs", Status: agent.TaskStatusInProgress, Kind: agent.TaskKindPlan})
	_ = r.Text(ctx, "Nothing screaming announcements yet.")
	if err := r.Finish(ctx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if len(api.startOptions) != 1 {
		t.Errorf("plan updates must not split the reply; got %d text messages, want 1", len(api.startOptions))
	}
	if api.stops != 1 {
		t.Errorf("expected the single reply committed once, got stops=%d", api.stops)
	}
	if cards.starts != 1 {
		t.Errorf("expected the plan to render as exactly one tool block, got %d", cards.starts)
	}
}

// TestSectionRenderer_ToolUpdateDoesNotReseal confirms a status tick for a tool
// already shown (a tool_call_update after we've moved on to reply text) is dropped
// rather than sealing the live reply into a second message. Only a NEW tool id is
// a text→tools transition.
func TestSectionRenderer_ToolUpdateDoesNotReseal(t *testing.T) {
	api := &fakeStreamAPI{}
	cards := &fakeStreamAPI{}
	r := newTestSectionRenderer(api, cards)
	ctx := context.Background()

	_ = r.Task(ctx, &agent.TaskEvent{ID: "t1", Title: "read", Status: agent.TaskStatusInProgress})
	_ = r.Text(ctx, "reading the file")
	// Late completion of the SAME tool, after the block sealed: must not re-chop.
	_ = r.Task(ctx, &agent.TaskEvent{ID: "t1", Title: "read", Status: agent.TaskStatusComplete})
	_ = r.Text(ctx, " and here is the result")
	if err := r.Finish(ctx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if len(api.startOptions) != 1 {
		t.Errorf("a repeat tool id must not open a second reply message, got %d", len(api.startOptions))
	}
	if cards.starts != 1 {
		t.Errorf("expected exactly one tool block, got %d", cards.starts)
	}
}

// TestSectionRenderer_PlanFoldsIntoToolBlock confirms a buffered plan rides the
// first real tool run's block rather than opening a block of its own — the reply
// stays two messages (before/after the tool), with a single block between them.
func TestSectionRenderer_PlanFoldsIntoToolBlock(t *testing.T) {
	api := &fakeStreamAPI{}
	cards := &fakeStreamAPI{}
	r := newTestSectionRenderer(api, cards)
	ctx := context.Background()

	_ = r.Task(ctx, &agent.TaskEvent{ID: "plan-0", Title: "check diff", Status: agent.TaskStatusInProgress, Kind: agent.TaskKindPlan})
	_ = r.Text(ctx, "let me look")
	_ = r.Task(ctx, &agent.TaskEvent{ID: "read-1", Title: "read", Status: agent.TaskStatusInProgress})
	_ = r.Text(ctx, "done")
	if err := r.Finish(ctx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if cards.starts != 1 {
		t.Errorf("plan should fold into the tool block, not add its own; got %d blocks", cards.starts)
	}
	if len(api.startOptions) != 2 {
		t.Errorf("expected two text messages around the single block, got %d", len(api.startOptions))
	}
}

// TestSectionRenderer_TextOnlyIsASingleMessage confirms a pure reply (no tools)
// stays one streamed message with no tool block — the common chat case.
func TestSectionRenderer_TextOnlyIsASingleMessage(t *testing.T) {
	api := &fakeStreamAPI{}
	cards := &fakeStreamAPI{}
	r := newTestSectionRenderer(api, cards)
	ctx := context.Background()

	_ = r.Text(ctx, "just an answer")
	if err := r.Finish(ctx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if cards.starts != 0 {
		t.Errorf("a tool-less reply must post no tool block, got %d", cards.starts)
	}
	if len(api.startOptions) != 1 || api.stops != 1 {
		t.Errorf("expected one streamed message, got starts=%d stops=%d", len(api.startOptions), api.stops)
	}
}
