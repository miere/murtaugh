package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

// newTestSectionRenderer wires a sectionRenderer to the test fakes: every text
// section is a StreamWriter over api, every tool block a StatusLineWriter over
// msgr. A huge throttle interval keeps the status line's in-place updates from
// firing, so post/start counts reflect sections, not refreshes.
func newTestSectionRenderer(api *fakeStreamAPI, msgr *fakeStatusMessenger) *sectionRenderer {
	return newSectionRenderer(
		func() *StreamWriter {
			return NewStreamWriter(api, "C1", StreamWriterOptions{ThreadTS: "100.0", Interval: time.Hour, MinChars: 1, Logger: discardLogger()})
		},
		func() toolBlock {
			return NewStatusLineWriter(msgr, "C1", "100.0", time.Hour, discardLogger())
		},
		nil, "C1", "100.0",
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
	msgr := &fakeStatusMessenger{}
	r := newTestSectionRenderer(api, msgr)
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
	if err := r.Finish(ctx, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if msgr.posts != 2 {
		t.Errorf("expected 2 tool-block messages, got %d", msgr.posts)
	}
	if len(api.startOptions) != 2 {
		t.Errorf("expected 2 text messages, got %d", len(api.startOptions))
	}
	if api.stops != 2 {
		t.Errorf("expected both text messages to be stopped, got %d", api.stops)
	}
}

// TestSectionRenderer_BlockSummarizesItsTools verifies a finalized tool block
// resolves to a compact summary of the tools it ran, not a single "Done
// thinking".
func TestSectionRenderer_BlockSummarizesItsTools(t *testing.T) {
	api := &fakeStreamAPI{}
	msgr := &fakeStatusMessenger{}
	r := newTestSectionRenderer(api, msgr)
	ctx := context.Background()

	_ = r.Task(ctx, &agent.TaskEvent{ID: "1", Title: "read", Status: agent.TaskStatusInProgress})
	_ = r.Task(ctx, &agent.TaskEvent{ID: "2", Title: "skill", Status: agent.TaskStatusInProgress})
	_ = r.Task(ctx, &agent.TaskEvent{ID: "3", Title: "write", Status: agent.TaskStatusInProgress})
	if err := r.Finish(ctx, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	summary := optionValue(msgr.updateOptions, "text")
	if !strings.Contains(summary, "read · skill · write") {
		t.Errorf("block summary = %q, want it to list the tools that ran", summary)
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
	msgr := &fakeStatusMessenger{}
	r := newTestSectionRenderer(api, msgr)
	ctx := context.Background()

	_ = r.Text(ctx, "The config diff shows a new coder-mono ")
	_ = r.Task(ctx, &agent.TaskEvent{ID: "plan-0", Title: "check diff", Status: agent.TaskStatusInProgress, Kind: agent.TaskKindPlan})
	_ = r.Text(ctx, "agent plus routing changes. ")
	// A refreshed snapshot: same entries re-sent, one advancing to complete.
	_ = r.Task(ctx, &agent.TaskEvent{ID: "plan-0", Title: "check diff", Status: agent.TaskStatusComplete, Kind: agent.TaskKindPlan})
	_ = r.Task(ctx, &agent.TaskEvent{ID: "plan-1", Title: "scan jobs", Status: agent.TaskStatusInProgress, Kind: agent.TaskKindPlan})
	_ = r.Text(ctx, "Nothing screaming announcements yet.")
	if err := r.Finish(ctx, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if len(api.startOptions) != 1 {
		t.Errorf("plan updates must not split the reply; got %d text messages, want 1", len(api.startOptions))
	}
	if api.stops != 1 {
		t.Errorf("expected the single reply committed once, got stops=%d", api.stops)
	}
	if msgr.posts != 1 {
		t.Errorf("expected the plan to render as exactly one tool block, got %d", msgr.posts)
	}
}

// TestSectionRenderer_ToolUpdateDoesNotReseal confirms a status tick for a tool
// already shown (a tool_call_update after we've moved on to reply text) is dropped
// rather than sealing the live reply into a second message. Only a NEW tool id is
// a text→tools transition.
func TestSectionRenderer_ToolUpdateDoesNotReseal(t *testing.T) {
	api := &fakeStreamAPI{}
	msgr := &fakeStatusMessenger{}
	r := newTestSectionRenderer(api, msgr)
	ctx := context.Background()

	_ = r.Task(ctx, &agent.TaskEvent{ID: "t1", Title: "read", Status: agent.TaskStatusInProgress})
	_ = r.Text(ctx, "reading the file")
	// Late completion of the SAME tool, after the block sealed: must not re-chop.
	_ = r.Task(ctx, &agent.TaskEvent{ID: "t1", Title: "read", Status: agent.TaskStatusComplete})
	_ = r.Text(ctx, " and here is the result")
	if err := r.Finish(ctx, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if len(api.startOptions) != 1 {
		t.Errorf("a repeat tool id must not open a second reply message, got %d", len(api.startOptions))
	}
	if msgr.posts != 1 {
		t.Errorf("expected exactly one tool block, got %d", msgr.posts)
	}
}

// TestSectionRenderer_PlanFoldsIntoToolBlock confirms a buffered plan rides the
// first real tool run's block rather than opening a block of its own — the reply
// stays two messages (before/after the tool), with a single block between them.
func TestSectionRenderer_PlanFoldsIntoToolBlock(t *testing.T) {
	api := &fakeStreamAPI{}
	msgr := &fakeStatusMessenger{}
	r := newTestSectionRenderer(api, msgr)
	ctx := context.Background()

	_ = r.Task(ctx, &agent.TaskEvent{ID: "plan-0", Title: "check diff", Status: agent.TaskStatusInProgress, Kind: agent.TaskKindPlan})
	_ = r.Text(ctx, "let me look")
	_ = r.Task(ctx, &agent.TaskEvent{ID: "read-1", Title: "read", Status: agent.TaskStatusInProgress})
	_ = r.Text(ctx, "done")
	if err := r.Finish(ctx, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if msgr.posts != 1 {
		t.Errorf("plan should fold into the tool block, not add its own; got %d blocks", msgr.posts)
	}
	if len(api.startOptions) != 2 {
		t.Errorf("expected two text messages around the single block, got %d", len(api.startOptions))
	}
}

// TestSectionRenderer_TextOnlyIsASingleMessage confirms a pure reply (no tools)
// stays one streamed message with no tool block — the common chat case.
func TestSectionRenderer_TextOnlyIsASingleMessage(t *testing.T) {
	api := &fakeStreamAPI{}
	msgr := &fakeStatusMessenger{}
	r := newTestSectionRenderer(api, msgr)
	ctx := context.Background()

	_ = r.Text(ctx, "just an answer")
	if err := r.Finish(ctx, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if msgr.posts != 0 {
		t.Errorf("a tool-less reply must post no tool block, got %d", msgr.posts)
	}
	if len(api.startOptions) != 1 || api.stops != 1 {
		t.Errorf("expected one streamed message, got starts=%d stops=%d", len(api.startOptions), api.stops)
	}
}
