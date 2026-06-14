package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
)

func TestStatusLineWriterEmitsSingleCardNoPlan(t *testing.T) {
	api := &fakeStreamAPI{}
	streamer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 5, TaskDisplayMode: slack.TaskDisplayModeTimeline})
	writer := NewStatusLineWriter(api, streamer, time.Hour, nil)

	if err := writer.UpdateFromEvent(context.Background(), &acp.TaskEvent{ID: "abc", Title: "Reading file…"}); err != nil {
		t.Fatalf("UpdateFromEvent returned error: %v", err)
	}
	if api.appends != 0 || len(api.startOptions) != 1 {
		t.Fatalf("expected one stream start, no appends, got appends=%d starts=%d", api.appends, len(api.startOptions))
	}
	chunks, err := extractChunksFromOptions(api.startOptions[0]...)
	if err != nil {
		t.Fatalf("extract chunks: %v", err)
	}
	// Simplified mode never opens a Plan block — a single bare task card only.
	if got := planChunks(chunks); len(got) != 0 {
		t.Fatalf("expected no plan_update chunk, got %+v", got)
	}
	tasks := taskChunks(chunks)
	if len(tasks) != 1 {
		t.Fatalf("expected one task_update chunk, got %d", len(tasks))
	}
	if tasks[0].ID != statusLineTaskID || tasks[0].Title != "Reading file…" || tasks[0].Status != slack.TaskCardStatusInProgress {
		t.Fatalf("unexpected chunk: %+v", tasks[0])
	}
}

func TestStatusLineWriterReusesOneIDLastWriteWins(t *testing.T) {
	api := &fakeStreamAPI{}
	streamer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 5})
	// interval 0 → default; use a tiny interval so the second update is not throttled.
	writer := NewStatusLineWriter(api, streamer, time.Nanosecond, nil)
	ctx := context.Background()

	// Two different ACP task ids must collapse onto the one status-line id, so the
	// second write overwrites the first card rather than stacking a new one.
	if err := writer.UpdateFromEvent(ctx, &acp.TaskEvent{ID: "task-1", Title: "Reading"}); err != nil {
		t.Fatalf("first update: %v", err)
	}
	time.Sleep(time.Millisecond)
	if err := writer.UpdateFromEvent(ctx, &acp.TaskEvent{ID: "task-2", Title: "Writing"}); err != nil {
		t.Fatalf("second update: %v", err)
	}
	startChunks, _ := extractChunksFromOptions(api.startOptions[0]...)
	if id := taskChunks(startChunks)[0].ID; id != statusLineTaskID {
		t.Fatalf("first card id = %q, want %q", id, statusLineTaskID)
	}
	appendChunks, _ := extractChunksFromOptions(api.appendOptions[len(api.appendOptions)-1]...)
	second := taskChunks(appendChunks)
	if len(second) != 1 || second[0].ID != statusLineTaskID || second[0].Title != "Writing" {
		t.Fatalf("expected the same id with the latest title, got %+v", second)
	}
}

func TestStatusLineWriterThrottlesInProgress(t *testing.T) {
	api := &fakeStreamAPI{}
	streamer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 5})
	writer := NewStatusLineWriter(api, streamer, time.Hour, nil)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := writer.UpdateFromEvent(ctx, &acp.TaskEvent{ID: "t", Title: "tick"}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	// First write starts the stream; the rest fall inside the one-hour window.
	if len(api.startOptions) != 1 || api.appends != 0 {
		t.Fatalf("expected a single throttled write, got starts=%d appends=%d", len(api.startOptions), api.appends)
	}
}

func TestStatusLineWriterCompleteResolvesOnceWithLatestTitle(t *testing.T) {
	api := &fakeStreamAPI{}
	streamer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 5})
	writer := NewStatusLineWriter(api, streamer, time.Hour, nil)
	ctx := context.Background()

	if err := writer.UpdateFromEvent(ctx, &acp.TaskEvent{ID: "t", Title: "Reading file…"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	// finalizeTasks may call Complete once per still-running task; only the first
	// must reach Slack, and it reuses the last title since Complete carries none.
	if err := writer.Complete(ctx, "t", ""); err != nil {
		t.Fatalf("first complete: %v", err)
	}
	if err := writer.Complete(ctx, "other", ""); err != nil {
		t.Fatalf("second complete: %v", err)
	}
	if api.appends != 1 {
		t.Fatalf("expected exactly one terminal write, got appends=%d", api.appends)
	}
	chunks, _ := extractChunksFromOptions(api.appendOptions[0]...)
	tasks := taskChunks(chunks)
	if len(tasks) != 1 || tasks[0].Status != slack.TaskCardStatusComplete || tasks[0].Title != "Reading file…" {
		t.Fatalf("expected single complete card with the last title, got %+v", tasks)
	}
}

func TestStatusLineWriterDefaultsBlankTitle(t *testing.T) {
	api := &fakeStreamAPI{}
	streamer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 5})
	writer := NewStatusLineWriter(api, streamer, time.Hour, nil)

	if err := writer.UpdateFromEvent(context.Background(), &acp.TaskEvent{ID: "t"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	chunks, _ := extractChunksFromOptions(api.startOptions[0]...)
	if title := taskChunks(chunks)[0].Title; title != defaultStatusLineTitle {
		t.Fatalf("expected default title %q, got %q", defaultStatusLineTitle, title)
	}
}
