package gateway

import (
	"context"
	"testing"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/agent"
)

// TestBufferedCardBlock_PostsThenUpdates: the first task event posts a PlanBlock;
// a later status change edits it in place (one post, then updates).
func TestBufferedCardBlock_PostsThenUpdates(t *testing.T) {
	msgr := &fakeStatusMessenger{}
	b := newBufferedCardBlock(msgr, "C1", "100.0", StreamWriterOptions{Logger: discardLogger()}, discardLogger())
	ctx := context.Background()

	if err := b.UpdateFromEvent(ctx, &agent.TaskEvent{ID: "t1", Title: "read", Status: agent.TaskStatusInProgress}); err != nil {
		t.Fatalf("update 1: %v", err)
	}
	if msgr.posts != 1 || msgr.updates != 0 {
		t.Fatalf("after first event: posts=%d updates=%d, want 1/0", msgr.posts, msgr.updates)
	}
	if msgr.postThreadTS != "100.0" {
		t.Fatalf("post thread_ts = %q, want 100.0", msgr.postThreadTS)
	}
	// A terminal status always paints (never throttled): it edits the message.
	if err := b.UpdateFromEvent(ctx, &agent.TaskEvent{ID: "t1", Status: agent.TaskStatusComplete}); err != nil {
		t.Fatalf("update 2: %v", err)
	}
	if msgr.posts != 1 || msgr.updates != 1 {
		t.Fatalf("after terminal event: posts=%d updates=%d, want 1/1", msgr.posts, msgr.updates)
	}

	pb := b.planBlock(false)
	if pb.Title != defaultPlanTitle {
		t.Fatalf("plan title = %q, want %q", pb.Title, defaultPlanTitle)
	}
	if len(pb.Tasks) != 1 || pb.Tasks[0].TaskID != "t1" || pb.Tasks[0].Title != "read" {
		t.Fatalf("tasks = %+v, want one 'read' card", pb.Tasks)
	}
	if pb.Tasks[0].Status != slack.TaskCardStatusComplete {
		t.Fatalf("card status = %q, want complete", pb.Tasks[0].Status)
	}
}

// TestBufferedCardBlock_FinalizesRunningCards: a card still in progress at the end
// is shown complete, so nothing is stranded mid-spinner.
func TestBufferedCardBlock_FinalizesRunningCards(t *testing.T) {
	msgr := &fakeStatusMessenger{}
	b := newBufferedCardBlock(msgr, "C1", "", StreamWriterOptions{Logger: discardLogger()}, discardLogger())
	ctx := context.Background()

	if err := b.UpdateFromEvent(ctx, &agent.TaskEvent{ID: "t1", Title: "work", Status: agent.TaskStatusInProgress}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := b.FinishWith(ctx, "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if msgr.posts != 1 || msgr.updates != 1 {
		t.Fatalf("posts=%d updates=%d, want 1 post + 1 finalizing update", msgr.posts, msgr.updates)
	}
	if pb := b.planBlock(true); pb.Tasks[0].Status != slack.TaskCardStatusComplete {
		t.Fatalf("finalized card status = %q, want complete", pb.Tasks[0].Status)
	}
}

// TestBufferedCardBlock_EmptyBlockPostsNothing: a tool block that never saw an
// event posts nothing on FinishWith.
func TestBufferedCardBlock_EmptyBlockPostsNothing(t *testing.T) {
	msgr := &fakeStatusMessenger{}
	b := newBufferedCardBlock(msgr, "C1", "", StreamWriterOptions{Logger: discardLogger()}, discardLogger())
	if err := b.FinishWith(context.Background(), ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if msgr.posts != 0 || msgr.updates != 0 {
		t.Fatalf("empty block should post nothing, got posts=%d updates=%d", msgr.posts, msgr.updates)
	}
}

// TestDefaultCardBlock_StreamsWhenSupported: on an ordinary surface the cards
// stream and no buffered post happens.
func TestDefaultCardBlock_StreamsWhenSupported(t *testing.T) {
	api := &fakeStreamAPI{}
	msgr := &fakeStatusMessenger{}
	c := newDefaultCardBlock(api, msgr, "C1", "100.0", StreamWriterOptions{MinChars: 1, Logger: discardLogger()}, discardLogger())
	ctx := context.Background()

	if err := c.UpdateFromEvent(ctx, &agent.TaskEvent{ID: "t1", Title: "read", Status: agent.TaskStatusInProgress}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if api.starts != 1 {
		t.Fatalf("expected the cards to stream (starts=1), got %d", api.starts)
	}
	if msgr.posts != 0 {
		t.Fatalf("expected no buffered post on a streamable surface, got %d", msgr.posts)
	}
}

// TestDefaultCardBlock_DowngradesOnCanvas: a canvas rejects the card stream, so the
// block downgrades to a buffered PlanBlock post — tasks-mode cards still render.
func TestDefaultCardBlock_DowngradesOnCanvas(t *testing.T) {
	api := canvasAPI()
	msgr := &fakeStatusMessenger{}
	c := newDefaultCardBlock(api, msgr, "C1", "100.0", StreamWriterOptions{MinChars: 1, Logger: discardLogger()}, discardLogger())
	ctx := context.Background()

	if err := c.UpdateFromEvent(ctx, &agent.TaskEvent{ID: "t1", Title: "read", Status: agent.TaskStatusInProgress}); err != nil {
		t.Fatalf("update returned error (downgrade should swallow it): %v", err)
	}
	if api.starts != 1 || api.appends != 0 {
		t.Fatalf("expected exactly one doomed stream-open, no appends: starts=%d appends=%d", api.starts, api.appends)
	}
	if msgr.posts != 1 {
		t.Fatalf("expected the cards posted as a buffered PlanBlock, got %d posts", msgr.posts)
	}
	// Subsequent events and finalisation ride the buffered block.
	if err := c.UpdateFromEvent(ctx, &agent.TaskEvent{ID: "t1", Status: agent.TaskStatusComplete}); err != nil {
		t.Fatalf("update 2: %v", err)
	}
	if err := c.FinishWith(ctx, "done"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if msgr.posts != 1 || msgr.updates == 0 {
		t.Fatalf("expected the single post edited in place, got posts=%d updates=%d", msgr.posts, msgr.updates)
	}
}
