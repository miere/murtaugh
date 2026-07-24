package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

func TestProcessClientStreamsPromptUpdates(t *testing.T) {
	client := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"}})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	session, err := client.NewSession(ctx, agent.SessionMetadata{TeamID: "T1"})
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	events, err := client.Prompt(ctx, session.ID, agent.PromptRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("Prompt returned error: %v", err)
	}
	var text strings.Builder
	var taskEvents []*agent.TaskEvent
	for event := range events {
		switch event.Type {
		case agent.EventText:
			text.WriteString(event.Text)
		case agent.EventTask:
			taskEvents = append(taskEvents, event.Task)
		}
	}
	if got := text.String(); got != "Hello from ACP" {
		t.Fatalf("unexpected streamed text %q", got)
	}
	if len(taskEvents) != 2 {
		t.Fatalf("expected two task events, got %d", len(taskEvents))
	}
	if taskEvents[0].ID != "task-1" || taskEvents[1].ID != "task-1" {
		t.Fatalf("unexpected task ids: %+v", taskEvents)
	}
	if taskEvents[0].Status != agent.TaskStatusInProgress || taskEvents[1].Status != agent.TaskStatusComplete {
		t.Fatalf("unexpected task statuses: %+v", taskEvents)
	}
}

func TestProcessClientDoesNotDuplicateStreamedReplyInResult(t *testing.T) {
	client := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"}})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	session, err := client.NewSession(ctx, agent.SessionMetadata{TeamID: "T1"})
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	events, err := client.Prompt(ctx, session.ID, agent.PromptRequest{Text: "dupe:hello world"})
	if err != nil {
		t.Fatalf("Prompt returned error: %v", err)
	}
	var text strings.Builder
	for event := range events {
		if event.Type == agent.EventText {
			text.WriteString(event.Text)
		}
	}
	// The reply was both streamed and echoed in the result; it must appear once,
	// not concatenated with itself.
	if got := text.String(); got != "hello world" {
		t.Fatalf("streamed reply was duplicated or dropped: got %q", got)
	}
}

func TestProcessClientRendersPlanAsTaskEvents(t *testing.T) {
	client := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"}})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	session, err := client.NewSession(ctx, agent.SessionMetadata{TeamID: "T1"})
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	events, err := client.Prompt(ctx, session.ID, agent.PromptRequest{Text: "plan:Scan=completed,Fix=in_progress,Verify=pending"})
	if err != nil {
		t.Fatalf("Prompt returned error: %v", err)
	}
	var tasks []*agent.TaskEvent
	for event := range events {
		if event.Type == agent.EventTask {
			tasks = append(tasks, event.Task)
		}
	}
	if len(tasks) != 3 {
		t.Fatalf("expected three task events from the plan, got %d: %+v", len(tasks), tasks)
	}
	want := []struct {
		id, title string
		status    agent.TaskStatus
	}{
		{"plan-0", "Scan", agent.TaskStatusComplete},
		{"plan-1", "Fix", agent.TaskStatusInProgress},
		{"plan-2", "Verify", agent.TaskStatusPending},
	}
	for i, w := range want {
		if tasks[i].ID != w.id || tasks[i].Title != w.title || tasks[i].Status != w.status {
			t.Fatalf("task %d: got %+v, want id=%s title=%s status=%s", i, tasks[i], w.id, w.title, w.status)
		}
	}
}

func TestProcessClientProcessOutlivesInitializeContext(t *testing.T) {
	client := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"}})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	cancel()
	session, err := client.NewSession(context.Background(), agent.SessionMetadata{TeamID: "T1"})
	if err != nil {
		t.Fatalf("NewSession returned error after initialize context cancellation: %v", err)
	}
	if session.ID == "" {
		t.Fatal("expected session ID")
	}
}

func TestProcessClientSupportsCancelFalseWhenMethodNotFound(t *testing.T) {
	client := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"}})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if client.SupportsCancel(ctx) {
		t.Fatal("expected SupportsCancel=false for an agent that returns -32601")
	}
}

func TestProcessClientSupportsCancelTrueWhenMethodExists(t *testing.T) {
	client := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper-cancellable"}})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if !client.SupportsCancel(ctx) {
		t.Fatal("expected SupportsCancel=true when the agent implements session/cancel")
	}
}

func TestIsMethodNotFound(t *testing.T) {
	if !IsMethodNotFound(&RPCError{Method: "session/cancel", Code: -32601, Message: "nope"}) {
		t.Fatal("expected -32601 RPCError to be method-not-found")
	}
	if IsMethodNotFound(&RPCError{Method: "session/cancel", Code: -32602, Message: "bad params"}) {
		t.Fatal("expected -32602 RPCError not to be method-not-found")
	}
	if IsMethodNotFound(errors.New("plain error")) {
		t.Fatal("expected a non-RPC error not to be method-not-found")
	}
	if IsMethodNotFound(nil) {
		t.Fatal("expected nil not to be method-not-found")
	}
}

func TestProcessClientDoesNotDropEventsForSlowConsumer(t *testing.T) {
	const totalChunks = 200
	client := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"}})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	session, err := client.NewSession(ctx, agent.SessionMetadata{TeamID: "T1"})
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	events, err := client.Prompt(ctx, session.ID, agent.PromptRequest{Text: fmt.Sprintf("burst:%d", totalChunks)})
	if err != nil {
		t.Fatalf("Prompt returned error: %v", err)
	}
	// Consume each event with a small delay so the events channel buffer (32)
	// fills up well before the agent finishes emitting. Without blocking sends
	// in deliverNotification, the chunks beyond the buffer would be silently
	// dropped and the assembled text would be truncated.
	var text strings.Builder
	for event := range events {
		if event.Type == agent.EventText {
			text.WriteString(event.Text)
			time.Sleep(2 * time.Millisecond)
		}
	}
	got := text.String()
	if want := strings.Repeat("x", totalChunks); got != want {
		t.Fatalf("text was truncated by slow consumer: got %d bytes, want %d", len(got), len(want))
	}
}

func TestProcessClientPassesEnvToAgent(t *testing.T) {
	client := NewProcessClient(ProcessOptions{
		Command: os.Args[0],
		Args:    []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"},
		Env:     []string{"MURTAUGH_TEST_ENV=injected-value"},
	})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	session, err := client.NewSession(ctx, agent.SessionMetadata{TeamID: "T1"})
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	events, err := client.Prompt(ctx, session.ID, agent.PromptRequest{Text: "echoenv:MURTAUGH_TEST_ENV"})
	if err != nil {
		t.Fatalf("Prompt returned error: %v", err)
	}
	var text strings.Builder
	for event := range events {
		if event.Type == agent.EventText {
			text.WriteString(event.Text)
		}
	}
	if got := text.String(); got != "injected-value" {
		t.Fatalf("agent did not see injected env var: got %q, want %q", got, "injected-value")
	}
}
