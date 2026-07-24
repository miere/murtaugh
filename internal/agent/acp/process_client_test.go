package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/miere/murtaugh/internal/agent"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestProcessClientUnsubscribeOnlyRetractsOwnSubscription(t *testing.T) {
	c := NewProcessClient(ProcessOptions{Command: "true"})
	first := &subscription{events: make(chan agent.Event, 1)}
	second := &subscription{events: make(chan agent.Event, 1)}

	c.subscribers["s"] = first
	// A second prompt reuses the session and overwrites the subscriber.
	c.subscribers["s"] = second

	// The first prompt tearing down must NOT remove the live (second)
	// subscription.
	c.unsubscribe("s", first)
	if c.subscribers["s"] != second {
		t.Fatalf("stale teardown removed the live subscription: got %v", c.subscribers["s"])
	}

	// The live prompt tearing down removes itself.
	c.unsubscribe("s", second)
	if _, ok := c.subscribers["s"]; ok {
		t.Fatal("live teardown should have removed the subscription")
	}
}

// A trailing session/update can land on the readLoop at the exact moment a turn
// tears down. deliverNotification captures the subscription under the lock and
// then sends on it WITHOUT the lock (the send blocks, deliberately, for
// back-pressure). Teardown must wait for that in-flight send to drain before it
// closes the channel — closing under the send is what panicked the gateway with
// "send on closed channel" (process_client.go readLoop → deliverNotification).
// This models a sender parked mid-send and asserts closeSubscription blocks in
// its drain barrier until the send completes, then closes.
func TestCloseSubscriptionWaitsForInFlightSend(t *testing.T) {
	c := NewProcessClient(ProcessOptions{Command: "true"})
	sub := &subscription{events: make(chan agent.Event)} // unbuffered: the send blocks until drained
	c.subscribers["s"] = sub

	// A readLoop-path send that has already passed the lock (wg.Add under it) and
	// is now parked in the blocking send — exactly the state deliverNotification is
	// in when a late notification arrives during teardown.
	sub.wg.Add(1)
	sent := make(chan struct{})
	go func() {
		sub.events <- agent.Event{Type: agent.EventText, Text: "trailing"}
		sub.wg.Done()
		close(sent)
	}()

	closed := make(chan struct{})
	go func() {
		c.closeSubscription("s", sub)
		close(closed)
	}()

	// Teardown must be parked in the drain barrier while the send is in flight; it
	// must NOT have closed the channel yet.
	select {
	case <-closed:
		t.Fatal("closeSubscription closed the channel under an in-flight send — would panic in prod")
	case <-time.After(100 * time.Millisecond):
	}

	// Drain the send; the sender completes and teardown may now proceed.
	if ev := <-sub.events; ev.Text != "trailing" {
		t.Fatalf("unexpected in-flight event: %+v", ev)
	}
	<-sent
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("closeSubscription did not finish after the in-flight send drained")
	}
	if _, ok := <-sub.events; ok {
		t.Fatal("events channel should be closed after teardown")
	}
	if _, ok := c.subscribers["s"]; ok {
		t.Fatal("subscription should be retracted after teardown")
	}
}

// The real readLoop → deliverNotification path racing teardown, run hot under the
// race detector. On the pre-fix code this panics ("send on closed channel") once
// the close lands inside deliverNotification's send window; with the drain
// barrier no ordering produces a send on a closed channel.
func TestDeliverNotificationDoesNotRaceTeardown(t *testing.T) {
	const chunk = `{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}`
	for i := 0; i < 200; i++ {
		c := NewProcessClient(ProcessOptions{Command: "true"})
		sub := &subscription{events: make(chan agent.Event, 32)}
		c.subscribers["s"] = sub
		c.dests["s"] = promptScope{}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.deliverNotification(rpcNotification{Method: "session/update", Params: json.RawMessage(chunk)})
		}()
		go func() {
			defer wg.Done()
			c.closeSubscription("s", sub)
		}()
		// Teardown closes the channel, so this range terminates; it also drains any
		// event the sender enqueued before the close so nothing blocks.
		for range sub.events {
		}
		wg.Wait()
	}
}

func TestExtractTextFromACPAgentMessageChunkUpdate(t *testing.T) {
	raw := json.RawMessage(`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"pong"}}}`)
	if got := extractNotificationText(raw); got != "pong" {
		t.Fatalf("unexpected extracted text: %q", got)
	}
}

func TestExtractTextFromACPAgentMessageUpdate(t *testing.T) {
	raw := json.RawMessage(`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message","content":[{"type":"text","text":"final "},{"type":"text","text":"answer"}]}}`)
	if got := extractNotificationText(raw); got != "final answer" {
		t.Fatalf("unexpected extracted text: %q", got)
	}
}

func TestExtractTextIgnoresACPToolCallContent(t *testing.T) {
	raw := json.RawMessage(`{"sessionId":"session-1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"in_progress","content":[{"type":"content","content":{"type":"text","text":"raw tool output"}}]}}`)
	if got := extractNotificationText(raw); got != "" {
		t.Fatalf("expected tool output to be hidden from assistant stream, got %q", got)
	}
}

func TestExtractTaskFromACPNotification(t *testing.T) {
	t.Run("valid task with all fields", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","task":{"id":"task-1","title":"Searching codebase","status":"in_progress","description":"looking for references"}}`)
		task := extractTask(raw)
		if task == nil {
			t.Fatal("expected non-nil task")
		}
		if task.ID != "task-1" {
			t.Fatalf("expected id task-1, got %q", task.ID)
		}
		if task.Title != "Searching codebase" {
			t.Fatalf("expected title 'Searching codebase', got %q", task.Title)
		}
		if task.Status != agent.TaskStatusInProgress {
			t.Fatalf("expected status in_progress, got %q", task.Status)
		}
		if task.Description != "looking for references" {
			t.Fatalf("expected description 'looking for references', got %q", task.Description)
		}
	})

	t.Run("missing id", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","task":{"title":"Foo","status":"pending"}}`)
		if task := extractTask(raw); task != nil {
			t.Fatalf("expected nil for missing id, got %+v", task)
		}
	})

	t.Run("no task field", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","content":{"type":"text","text":"hello"}}`)
		if task := extractTask(raw); task != nil {
			t.Fatalf("expected nil for no task field, got %+v", task)
		}
	})

	t.Run("taskId camelCase alias", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","task":{"taskId":"t-2","title":"Build","status":"complete"}}`)
		task := extractTask(raw)
		if task == nil {
			t.Fatal("expected non-nil task")
		}
		if task.ID != "t-2" {
			t.Fatalf("expected id t-2, got %q", task.ID)
		}
		if task.Status != agent.TaskStatusComplete {
			t.Fatalf("expected status complete, got %q", task.Status)
		}
	})

	t.Run("ACP tool_call update", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"List files","kind":"read","status":"pending"}}`)
		task := extractTask(raw)
		if task == nil {
			t.Fatal("expected non-nil task")
		}
		if task.ID != "call-1" || task.Title != "List files" || task.Status != agent.TaskStatusPending {
			t.Fatalf("unexpected ACP tool task: %+v", task)
		}
		if task.Description != "read" {
			t.Fatalf("expected kind as description, got %q", task.Description)
		}
	})

	t.Run("ACP tool_call_update completed alias", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"done"}}]}}`)
		task := extractTask(raw)
		if task == nil {
			t.Fatal("expected non-nil task")
		}
		if task.ID != "call-1" || task.Status != agent.TaskStatusComplete || task.Output != "done" {
			t.Fatalf("unexpected ACP tool update: %+v", task)
		}
	})
}

func TestExtractPlanTasks(t *testing.T) {
	t.Run("plan entries become indexed tasks", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"plan","entries":[{"content":"Scan","status":"completed"},{"content":"Fix","status":"in_progress"}]}}`)
		tasks := extractPlanTasks(raw)
		if len(tasks) != 2 {
			t.Fatalf("expected 2 tasks, got %d: %+v", len(tasks), tasks)
		}
		if tasks[0].ID != "plan-0" || tasks[0].Title != "Scan" || tasks[0].Status != agent.TaskStatusComplete {
			t.Fatalf("task 0: %+v", tasks[0])
		}
		if tasks[1].ID != "plan-1" || tasks[1].Title != "Fix" || tasks[1].Status != agent.TaskStatusInProgress {
			t.Fatalf("task 1: %+v", tasks[1])
		}
	})

	t.Run("non-plan update yields nil", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"c1"}}`)
		if tasks := extractPlanTasks(raw); tasks != nil {
			t.Fatalf("expected nil for non-plan update, got %+v", tasks)
		}
	})

	t.Run("entry with no content is skipped but indexes stay stable", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"plan","entries":[{"content":"","status":"pending"},{"content":"Real","status":"pending"}]}}`)
		tasks := extractPlanTasks(raw)
		if len(tasks) != 1 {
			t.Fatalf("expected the empty entry to be skipped, got %+v", tasks)
		}
		// The kept entry retains its position-based id (the second entry → plan-1).
		if tasks[0].ID != "plan-1" || tasks[0].Title != "Real" {
			t.Fatalf("expected plan-1/Real, got %+v", tasks[0])
		}
	})
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

func TestACPHelperProcess(t *testing.T) {
	mode := ""
	if len(os.Args) > 0 {
		mode = os.Args[len(os.Args)-1]
	}
	if mode != "acp-helper" && mode != "acp-helper-cancellable" {
		return
	}
	supportsCancel := mode == "acp-helper-cancellable"
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			os.Exit(2)
		}
		switch req.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": 1,
				"agentCapabilities": map[string]any{
					"loadSession":     true,
					"mcpCapabilities": map[string]any{"http": true, "sse": false},
				},
			}})
		case "session/new":
			var params struct {
				CWD        string `json:"cwd"`
				MCPServers []any  `json:"mcpServers"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil || params.CWD == "" || params.MCPServers == nil {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "invalid session/new params"}})
				continue
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"sessionId": "session-1"}})
		case "session/prompt":
			var params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			}
			_ = json.Unmarshal(req.Params, &params)
			// The user's text is always the final content block; any leading
			// blocks are the <context>/<conversation-context> stand-ins.
			promptText := ""
			if len(params.Prompt) > 0 {
				promptText = params.Prompt[len(params.Prompt)-1].Text
			}
			if name, ok := strings.CutPrefix(promptText, "echoenv:"); ok {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "agent_message", "content": []map[string]string{{"type": "text", "text": os.Getenv(name)}}}}})
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
				continue
			}
			if reply, ok := strings.CutPrefix(promptText, "dupe:"); ok {
				// Stream the reply as a chunk, then echo the SAME text in the prompt
				// result — the shape a streaming agent produces (it both streams and
				// returns the final text). The client must surface it once.
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": reply}}}})
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn", "content": []map[string]string{{"type": "text", "text": reply}}}})
				continue
			}
			if entries, ok := strings.CutPrefix(promptText, "plan:"); ok {
				// Emit a `plan` update with one entry per comma-separated "title=status"
				// pair, so the client can be checked for turning the plan into task
				// events rather than dropping it.
				var planEntries []map[string]any
				for _, pair := range strings.Split(entries, ",") {
					title, status, _ := strings.Cut(pair, "=")
					planEntries = append(planEntries, map[string]any{"content": title, "status": status})
				}
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "plan", "entries": planEntries}}})
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
				continue
			}
			if n := parseBurstCount(promptText); n > 0 {
				for i := 0; i < n; i++ {
					_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "x"}}}})
				}
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
				continue
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "task-1", "title": "Thinking", "status": "in_progress"}}})
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "task-1", "status": "completed", "content": []map[string]any{{"type": "content", "content": map[string]string{"type": "text", "text": "tool output should not stream"}}}}}})
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "agent_message", "content": []map[string]string{{"type": "text", "text": "Hello from ACP"}}}}})
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
		case "session/cancel":
			if supportsCancel {
				// An interruptible agent accepts the call (here: reports the
				// probe's synthetic session is unknown — not method-not-found).
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "unknown session"}})
			} else {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "session/cancel not supported"}})
			}
		default:
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": fmt.Sprintf("unknown method %s", req.Method)}})
		}
	}
	os.Exit(0)
}

func parseBurstCount(prompt string) int {
	const prefix = "burst:"
	if !strings.HasPrefix(prompt, prefix) {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(prompt[len(prefix):], "%d", &n); err != nil {
		return 0
	}
	return n
}
