package gateway

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// leaderTestGateway builds the minimum Gateway the demotion path touches.
func leaderTestGateway(drain time.Duration) *Gateway {
	return &Gateway{
		logger:    quietLogger(),
		inFlight:  NewInFlightRegistry(),
		drainWait: drain,
	}
}

func convKey(thread string) agent.ConversationKey {
	return agent.ConversationKey{TeamID: "T1", ChannelID: "C1", ThreadTS: thread}
}

// TestCancelAllStopsEveryTurn covers the registry primitive demotion relies on.
func TestCancelAllStopsEveryTurn(t *testing.T) {
	registry := NewInFlightRegistry()
	var cancelled atomic.Int64
	for _, thread := range []string{"1.0", "2.0", "3.0"} {
		registry.Register(convKey(thread), func() { cancelled.Add(1) }, "code")
	}

	if got := registry.CancelAll(); got != 3 {
		t.Errorf("CancelAll reported %d, want 3", got)
	}
	if got := cancelled.Load(); got != 3 {
		t.Errorf("%d cancel closures ran, want 3", got)
	}
	if registry.Len() != 0 {
		t.Errorf("registry still holds %d entries after CancelAll", registry.Len())
	}
	// A second call must be a no-op rather than double-cancelling.
	if got := registry.CancelAll(); got != 0 {
		t.Errorf("second CancelAll reported %d, want 0", got)
	}
}

// TestDrainWaitsForTurnsToFinish is the good path: a turn that finishes inside
// the window is left alone, so a demoted node does not kill work mid-write and
// strand a half-edited file.
func TestDrainWaitsForTurnsToFinish(t *testing.T) {
	gw := leaderTestGateway(2 * time.Second)
	var cancelled atomic.Bool
	token, _ := gw.inFlight.Register(convKey("1.0"), func() { cancelled.Store(true) }, "code")

	// The turn completes shortly after the drain begins.
	go func() {
		time.Sleep(200 * time.Millisecond)
		gw.inFlight.Unregister(convKey("1.0"), token)
	}()

	start := time.Now()
	gw.drainInFlight(context.Background())
	elapsed := time.Since(start)

	if cancelled.Load() {
		t.Error("a turn that finished inside the drain window was cancelled anyway")
	}
	if elapsed >= 2*time.Second {
		t.Errorf("drain waited the full timeout (%v) despite the turn finishing", elapsed)
	}
}

// TestDrainCancelsStragglers is the bound: a standby is waiting to take over, so
// a turn that will not finish must not hold the handover open indefinitely.
func TestDrainCancelsStragglers(t *testing.T) {
	gw := leaderTestGateway(300 * time.Millisecond)
	var cancelled atomic.Bool
	gw.inFlight.Register(convKey("1.0"), func() { cancelled.Store(true) }, "code")

	start := time.Now()
	gw.drainInFlight(context.Background())

	if !cancelled.Load() {
		t.Fatal("a turn outlasting the drain window was not cancelled")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("drain took %v; it must be bounded by the timeout", elapsed)
	}
	if gw.inFlight.Len() != 0 {
		t.Errorf("registry still holds %d entries after the drain", gw.inFlight.Len())
	}
}

// TestDrainStopsEarlyOnShutdown checks the drain does not make shutdown wait:
// when the daemon is going away there is no successor to hand over to and
// nobody to read the work.
func TestDrainStopsEarlyOnShutdown(t *testing.T) {
	gw := leaderTestGateway(30 * time.Second)
	var cancelled atomic.Bool
	gw.inFlight.Register(convKey("1.0"), func() { cancelled.Store(true) }, "code")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	gw.drainInFlight(ctx)

	if !cancelled.Load() {
		t.Error("shutdown left an in-flight turn running")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("drain waited %v during shutdown; it should abandon immediately", elapsed)
	}
}

// TestStopServingWithoutPromotionIsANoop covers the standby that never led:
// demotion may be called on it (shutdown runs the same path) and must not block
// or panic.
func TestStopServingWithoutPromotionIsANoop(t *testing.T) {
	gw := leaderTestGateway(time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		gw.StopServing(context.Background(), "shutting down")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StopServing blocked on a node that never served")
	}
}

// TestStopServingIsIdempotent guards the shutdown-racing-a-demotion case: the
// runner's stand-down and the daemon's exit can both reach here.
func TestStopServingIsIdempotent(t *testing.T) {
	gw := leaderTestGateway(200 * time.Millisecond)

	closed := make(chan struct{})
	close(closed)
	_, stop := context.WithCancel(context.Background())
	gw.serveCancel = stop
	gw.serveDone = closed

	for i := 0; i < 3; i++ {
		gw.StopServing(context.Background(), "repeat")
	}
	if gw.serveCancel != nil || gw.serveDone != nil {
		t.Error("StopServing left serve state behind")
	}
}

// TestPromotionAlertNamesTheNode checks the announcement answers the question an
// operator actually has during a failover — which machine is serving now — and
// carries the epoch that orders one takeover against the next.
func TestPromotionAlertNamesTheNode(t *testing.T) {
	report := NodeReport{
		Host:     "murtaugh-2.internal",
		LocalIP:  "10.1.2.3",
		PublicIP: "203.0.113.9",
		Version:  "v1.4.0",
		PID:      4242,
	}
	spec := promotionAlert(report, 7, "took over from the previous leader")

	if !strings.Contains(spec.Subtitle, "murtaugh-2.internal") {
		t.Errorf("subtitle does not name the node: %q", spec.Subtitle)
	}
	if !strings.Contains(spec.Subtitle, "took over from the previous leader") {
		t.Errorf("subtitle does not carry the reason: %q", spec.Subtitle)
	}
	for _, want := range []string{"10.1.2.3", "203.0.113.9", "v1.4.0", "4242", "7"} {
		if !strings.Contains(spec.Detail, want) {
			t.Errorf("detail is missing %q:\n%s", want, spec.Detail)
		}
	}
}

// TestPromotionAlertDegradesWithoutAddresses checks a node that could not learn
// its addresses still produces a legible notice. The public-IP lookup goes to a
// third party and will sometimes fail; that must not turn into a blank card.
func TestPromotionAlertDegradesWithoutAddresses(t *testing.T) {
	spec := promotionAlert(NodeReport{PID: 1}, 1, "first leader for this Slack app")

	if strings.TrimSpace(spec.Subtitle) == "" {
		t.Error("subtitle is empty for a node with no identity")
	}
	if !strings.Contains(spec.Detail, "unavailable") {
		t.Errorf("detail does not mark the missing fields:\n%s", spec.Detail)
	}
	if strings.Contains(spec.Subtitle, "()") {
		t.Errorf("subtitle has an empty parenthetical: %q", spec.Subtitle)
	}
}

// TestResolveNodeReportToleratesAFailedLookup pins that the public-IP call is
// best-effort: a promotion must not fail, or stall, because a third-party
// service is down.
func TestResolveNodeReportToleratesAFailedLookup(t *testing.T) {
	report := resolveNodeReport(context.Background(), "v1", func(context.Context, string) (string, error) {
		return "", io.ErrUnexpectedEOF
	})
	if report.PublicIP != "" {
		t.Errorf("PublicIP = %q after a failed lookup, want empty", report.PublicIP)
	}
	if report.PID == 0 {
		t.Error("PID was not resolved")
	}
	if report.Version != "v1" {
		t.Errorf("Version = %q, want v1", report.Version)
	}
}

// TestAnnouncePromotionWithoutAdminIsSilent covers a deployment with no admin
// configured: there is nobody to tell, and the notice must not become a nil
// dereference on the promotion path.
func TestAnnouncePromotionWithoutAdminIsSilent(t *testing.T) {
	gw := leaderTestGateway(time.Second)
	gw.AnnouncePromotion(context.Background(), 1, "first leader")
}

// TestLeaderAllowsWithoutElector pins the single-node default: with no election
// wired, every Slack action is permitted.
func TestLeaderAllowsWithoutElector(t *testing.T) {
	gw := leaderTestGateway(time.Second)
	if !gw.leaderAllows(context.Background()) {
		t.Error("a gateway with no elector refused its own Slack calls")
	}
}

// stubElector is a LeaderElector whose verdict the test controls.
type stubElector struct {
	allow atomic.Bool
	ran   atomic.Bool
}

func (e *stubElector) Run(ctx context.Context) error {
	e.ran.Store(true)
	<-ctx.Done()
	return nil
}

func (e *stubElector) Allow(context.Context) bool { return e.allow.Load() }

// TestLeaderAllowsFollowsTheElector checks the gateway defers to the elector
// once one is wired, in both directions.
func TestLeaderAllowsFollowsTheElector(t *testing.T) {
	gw := leaderTestGateway(time.Second)
	elector := &stubElector{}
	gw.WithLeaderElection(elector)

	if gw.leaderAllows(context.Background()) {
		t.Error("a standby was allowed to act")
	}
	elector.allow.Store(true)
	if !gw.leaderAllows(context.Background()) {
		t.Error("a leader was refused")
	}
}

// TestRunDelegatesToTheElector checks Run hands control to the election rather
// than serving unconditionally — the difference between contending for
// leadership and being a second gateway.
func TestRunDelegatesToTheElector(t *testing.T) {
	gw := leaderTestGateway(time.Second)
	gw.chatSessions = map[string]ChatSessionManager{}
	elector := &stubElector{}
	gw.WithLeaderElection(elector)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for !elector.ran.Load() {
		select {
		case <-deadline:
			t.Fatal("Run did not delegate to the elector")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
