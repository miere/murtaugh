package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/slack-go/slack"
)

// recordingTransport records the Slack methods that actually reached the wire.
type recordingTransport struct {
	mu      atomic.Int64
	methods chan string
}

func newRecordingTransport() *recordingTransport {
	return &recordingTransport{methods: make(chan string, 32)}
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Add(1)
	select {
	case t.methods <- slackMethod(req):
	default:
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

func (t *recordingTransport) calls() int64 { return t.mu.Load() }

func testGate(t *testing.T, base http.RoundTripper) *outboundGate {
	t.Helper()
	return newOutboundGate(base, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func slackRequest(t *testing.T, method string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://slack.com/api/"+method, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	return req
}

// TestGateBlocksWritesFromANonLeader is the property the whole gate exists for:
// a demoted node must not be able to post, however it tries.
func TestGateBlocksWritesFromANonLeader(t *testing.T) {
	base := newRecordingTransport()
	gate := testGate(t, base)
	gate.setAllow(func(context.Context) bool { return false })

	for _, method := range []string{
		"chat.postMessage", "chat.update", "chat.delete",
		"files.upload", "files.completeUploadExternal",
		"views.publish", "reactions.add", "conversations.create",
		"canvases.edit", "chat.postEphemeral",
	} {
		t.Run(method, func(t *testing.T) {
			_, err := gate.RoundTrip(slackRequest(t, method))
			if !errors.Is(err, errNotLeader) {
				t.Errorf("RoundTrip(%s) error = %v, want errNotLeader", method, err)
			}
		})
	}
	if base.calls() != 0 {
		t.Errorf("%d requests reached the wire from a non-leader", base.calls())
	}
}

// TestGateExemptsIdentityAndConnection checks the two methods that must work
// regardless: they are how a node establishes leadership, not how it exercises
// it. Blocking auth.test in particular would deadlock the feature — the lock
// key is derived from it.
func TestGateExemptsIdentityAndConnection(t *testing.T) {
	base := newRecordingTransport()
	gate := testGate(t, base)
	gate.setAllow(func(context.Context) bool { return false })

	for _, method := range []string{"auth.test", "apps.connections.open"} {
		if _, err := gate.RoundTrip(slackRequest(t, method)); err != nil {
			t.Errorf("RoundTrip(%s) was blocked: %v", method, err)
		}
	}
	if got := base.calls(); got != 2 {
		t.Errorf("%d exempt requests reached the wire, want 2", got)
	}
}

// TestGateIsOpenWithoutAnElector pins that a single-node deployment pays nothing
// for a feature it did not enable.
func TestGateIsOpenWithoutAnElector(t *testing.T) {
	base := newRecordingTransport()
	gate := testGate(t, base) // allow never set

	if _, err := gate.RoundTrip(slackRequest(t, "chat.postMessage")); err != nil {
		t.Fatalf("an ungated node was blocked: %v", err)
	}
	if base.calls() != 1 {
		t.Error("the request did not reach the wire")
	}
}

// TestGateReopensOnPromotion covers the re-promotion path: a node demoted and
// later promoted again must be able to post, so the gate must consult the
// predicate per request rather than latching.
func TestGateReopensOnPromotion(t *testing.T) {
	base := newRecordingTransport()
	gate := testGate(t, base)

	var leading atomic.Bool
	gate.setAllow(func(context.Context) bool { return leading.Load() })

	if _, err := gate.RoundTrip(slackRequest(t, "chat.postMessage")); !errors.Is(err, errNotLeader) {
		t.Fatalf("standby was allowed to post: %v", err)
	}
	leading.Store(true)
	if _, err := gate.RoundTrip(slackRequest(t, "chat.postMessage")); err != nil {
		t.Fatalf("a re-promoted node was still blocked: %v", err)
	}
	leading.Store(false)
	if _, err := gate.RoundTrip(slackRequest(t, "chat.postMessage")); !errors.Is(err, errNotLeader) {
		t.Fatalf("a re-demoted node was allowed to post: %v", err)
	}
}

// TestGateStopsARealSlackClient is the end-to-end check that the gate is wired
// where it claims to be: a genuine slack-go client built over the gated
// transport must fail to post, not merely a hand-made request.
//
// This is the difference between the gate working and the gate existing. The
// interfaces above it (messaging, alert poster, stream writers) all funnel here,
// so proving one real client is stopped proves the layer is the right one.
func TestGateStopsARealSlackClient(t *testing.T) {
	base := newRecordingTransport()
	gate := testGate(t, base)
	gate.setAllow(func(context.Context) bool { return false })

	api := slack.New("xoxb-test", slack.OptionHTTPClient(gate.httpClient()))
	if _, _, err := api.PostMessageContext(context.Background(), "C123", slack.MsgOptionText("hi", false)); err == nil {
		t.Fatal("a real Slack client posted from a non-leader node")
	}
	if base.calls() != 0 {
		t.Errorf("%d requests reached the wire", base.calls())
	}

	// And the same client works once leadership is granted, so the block is
	// leadership and not a broken transport.
	gate.setAllow(func(context.Context) bool { return true })
	if _, _, err := api.PostMessageContext(context.Background(), "C123", slack.MsgOptionText("hi", false)); err != nil {
		t.Fatalf("a leader could not post through the gate: %v", err)
	}
}

// TestGatePassesThroughToTheRealTransport guards against the gate swallowing a
// response or mangling the request when it does allow one.
func TestGatePassesThroughToTheRealTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"channel":"C1","ts":"1.0"}`)
	}))
	defer server.Close()

	gate := testGate(t, nil) // real http.DefaultTransport
	gate.setAllow(func(context.Context) bool { return true })

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/chat.postMessage", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := gate.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("response body not passed through: %s", body)
	}
}

// TestSlackMethodExtraction pins the parsing the exemption list depends on. A
// mis-parsed method silently turns an exemption into a block (breaking
// promotion) or a block into an exemption (letting a demoted node post).
func TestSlackMethodExtraction(t *testing.T) {
	for url, want := range map[string]string{
		"https://slack.com/api/chat.postMessage":      "chat.postMessage",
		"https://slack.com/api/auth.test":             "auth.test",
		"https://slack.com/api/apps.connections.open": "apps.connections.open",
		"https://wss-primary.slack.com/link":          "link",
	} {
		req, err := http.NewRequest(http.MethodPost, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := slackMethod(req); got != want {
			t.Errorf("slackMethod(%s) = %q, want %q", url, got, want)
		}
	}
	if got := slackMethod(nil); got != "" {
		t.Errorf("slackMethod(nil) = %q, want empty", got)
	}
}
