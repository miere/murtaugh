package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type fakeAsker struct {
	called bool
	loc    TurnLocation
	req    PermissionRequest
	ret    string
}

func (f *fakeAsker) AskPermission(_ context.Context, loc TurnLocation, req PermissionRequest) (string, error) {
	f.called = true
	f.loc = loc
	f.req = req
	return f.ret, nil
}

// runAgentRequest feeds one agent→client request line through readLoop on a client
// configured with opts (and an optional seeded per-session scope map), then returns
// the single JSON-RPC response the client writes back to the agent.
func runAgentRequest(t *testing.T, opts ProcessOptions, dests map[string]promptScope, line string) map[string]any {
	t.Helper()
	pr, pw := io.Pipe()
	c := &ProcessClient{
		opts:        opts,
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		pending:     make(map[int64]chan rpcResponse),
		subscribers: make(map[string]chan Event),
		dests:       make(map[string]promptScope),
		stdin:       pw,
		started:     true,
	}
	for k, v := range dests {
		c.dests[k] = v
	}
	go c.readLoop(strings.NewReader(line + "\n"))

	type result struct {
		m   map[string]any
		err error
	}
	done := make(chan result, 1)
	go func() {
		var m map[string]any
		err := json.NewDecoder(pr).Decode(&m)
		done <- result{m, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("decode ACP response: %v", r.err)
		}
		return r.m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ACP response")
		return nil
	}
}

func outcomeOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result object: %v", resp)
	}
	out, ok := res["outcome"].(map[string]any)
	if !ok {
		t.Fatalf("result has no outcome object: %v", res)
	}
	return out
}

const permReqAllowDeny = `{"jsonrpc":"2.0","id":1,"method":"session/request_permission",` +
	`"params":{"sessionId":"S1","toolCall":{"title":"Edit agents.yaml"},` +
	`"options":[{"optionId":"a","name":"Allow","kind":"allow_once"},` +
	`{"optionId":"d","name":"Reject","kind":"reject_once"}]}}`

func TestACPPermissionAutoAllow(t *testing.T) {
	resp := runAgentRequest(t, ProcessOptions{PermissionPolicy: "auto-allow"}, nil, permReqAllowDeny)
	out := outcomeOf(t, resp)
	if out["outcome"] != "selected" || out["optionId"] != "a" {
		t.Fatalf("auto-allow: expected selected a, got %v", out)
	}
}

func TestACPPermissionAutoDenyCancelsWhenNoRejectOption(t *testing.T) {
	line := `{"jsonrpc":"2.0","id":2,"method":"session/request_permission",` +
		`"params":{"sessionId":"S1","options":[{"optionId":"a","name":"Allow","kind":"allow_once"}]}}`
	resp := runAgentRequest(t, ProcessOptions{PermissionPolicy: "auto-deny"}, nil, line)
	out := outcomeOf(t, resp)
	if out["outcome"] != "cancelled" {
		t.Fatalf("auto-deny without a reject option should cancel, got %v", out)
	}
}

func TestACPPermissionAskRoutesToAsker(t *testing.T) {
	asker := &fakeAsker{ret: "a"}
	dests := map[string]promptScope{"S1": {loc: TurnLocation{ChannelID: "C1", ThreadTS: "T1"}, ctx: context.Background()}}
	resp := runAgentRequest(t, ProcessOptions{PermissionPolicy: "ask", PermissionAsker: asker}, dests, permReqAllowDeny)
	if !asker.called {
		t.Fatal("ask policy did not call the asker")
	}
	if asker.loc.ChannelID != "C1" || asker.loc.ThreadTS != "T1" {
		t.Fatalf("asker got wrong location: %+v", asker.loc)
	}
	if asker.req.ToolName != "Edit agents.yaml" || len(asker.req.Options) != 2 {
		t.Fatalf("asker got wrong request: %+v", asker.req)
	}
	out := outcomeOf(t, resp)
	if out["outcome"] != "selected" || out["optionId"] != "a" {
		t.Fatalf("ask: expected selected a, got %v", out)
	}
}

func TestACPPermissionAskWithoutAskerDenies(t *testing.T) {
	// ask policy on a headless path (no asker) must deny (cancelled), not hang.
	resp := runAgentRequest(t, ProcessOptions{PermissionPolicy: "ask"}, nil, permReqAllowDeny)
	out := outcomeOf(t, resp)
	if out["outcome"] != "cancelled" {
		t.Fatalf("ask without an asker should cancel, got %v", out)
	}
}

func TestACPPermissionEmptyPolicyDefaultsToAsk(t *testing.T) {
	asker := &fakeAsker{ret: "a"}
	dests := map[string]promptScope{"S1": {loc: TurnLocation{ChannelID: "C1"}, ctx: context.Background()}}
	resp := runAgentRequest(t, ProcessOptions{PermissionAsker: asker}, dests, permReqAllowDeny)
	if !asker.called {
		t.Fatal("empty policy should default to ask and call the asker")
	}
	if out := outcomeOf(t, resp); out["optionId"] != "a" {
		t.Fatalf("expected selected a, got %v", out)
	}
}

func TestACPUnhandledAgentRequestRepliesMethodNotFound(t *testing.T) {
	line := `{"jsonrpc":"2.0","id":5,"method":"fs/read_text_file","params":{}}`
	resp := runAgentRequest(t, ProcessOptions{}, nil, line)
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error response, got %v", resp)
	}
	if code, _ := errObj["code"].(float64); int(code) != jsonRPCMethodNotFound {
		t.Fatalf("expected method-not-found (%d), got %v", jsonRPCMethodNotFound, errObj["code"])
	}
}

func TestPickOptionByKind(t *testing.T) {
	opts := []PermissionOption{
		{ID: "ao", Kind: "allow_once"},
		{ID: "aa", Kind: "allow_always"},
		{ID: "ro", Kind: "reject_once"},
	}
	if got := pickOptionByKind(opts, "allow"); got != "ao" {
		t.Fatalf("allow should prefer allow_once, got %q", got)
	}
	if got := pickOptionByKind(opts, "reject"); got != "ro" {
		t.Fatalf("reject should pick reject_once, got %q", got)
	}
	if got := pickOptionByKind([]PermissionOption{{ID: "x", Kind: "allow_once"}}, "reject"); got != "" {
		t.Fatalf("no reject option should yield \"\", got %q", got)
	}
}

func TestParsePermissionRequest(t *testing.T) {
	raw := json.RawMessage(`{"sessionId":"S9","toolCall":{"kind":"execute"},"options":[{"optionId":"o1","name":"Yes","kind":"allow_once"}]}`)
	sid, tool, opts := parsePermissionRequest(raw)
	if sid != "S9" {
		t.Fatalf("sessionID: got %q", sid)
	}
	if tool != "execute" { // falls back to kind when title is absent
		t.Fatalf("toolName: got %q", tool)
	}
	if len(opts) != 1 || opts[0].ID != "o1" || opts[0].Kind != "allow_once" {
		t.Fatalf("options: got %+v", opts)
	}
}
