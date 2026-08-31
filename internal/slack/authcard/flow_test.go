package authcard

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/auth"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

const (
	adminID     = "UADMIN"
	requesterID = "UREQ"
)

// syncAPI is a concurrency-safe SlackAPI fake. Run posts and updates from the
// goroutine under test while the test itself clicks from another, so the shared
// slacktest.FakeAPI (which has no locking) would race.
type syncAPI struct {
	slacklib.SlackAPI // unused methods panic if ever called

	mu      sync.Mutex
	posts   []slacklib.PostMessageParams
	updates []slacklib.UpdateMessageParams
	views   []slackgo.ModalViewRequest
	postCh  chan struct{}

	openDMErr error
	postErr   error

	// onPost, when set, runs inside PostMessage after the message has been
	// recorded — standing in for the instant Slack has delivered the card and a
	// human could already be clicking it.
	onPost func(index int, p slacklib.PostMessageParams)
}

func newSyncAPI() *syncAPI {
	return &syncAPI{postCh: make(chan struct{}, 16)}
}

func (a *syncAPI) PostMessage(_ context.Context, p slacklib.PostMessageParams) (slacklib.PostMessageResult, error) {
	a.mu.Lock()
	if a.postErr != nil {
		a.mu.Unlock()
		return slacklib.PostMessageResult{}, a.postErr
	}
	a.posts = append(a.posts, p)
	ts := fmt.Sprintf("ts-%d", len(a.posts))
	index := len(a.posts)
	hook := a.onPost
	a.mu.Unlock()

	if hook != nil {
		hook(index, p)
	}

	select {
	case a.postCh <- struct{}{}:
	default:
	}
	return slacklib.PostMessageResult{Channel: p.ChannelID, TS: ts}, nil
}

func (a *syncAPI) UpdateMessage(_ context.Context, p slacklib.UpdateMessageParams) (slacklib.PostMessageResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updates = append(a.updates, p)
	return slacklib.PostMessageResult{Channel: p.ChannelID, TS: p.TS}, nil
}

func (a *syncAPI) OpenDM(_ context.Context, userID string) (string, error) {
	if a.openDMErr != nil {
		return "", a.openDMErr
	}
	return "D-" + userID, nil
}

func (a *syncAPI) OpenView(_ context.Context, _ string, view slackgo.ModalViewRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.views = append(a.views, view)
	return nil
}

func (a *syncAPI) snapshot() ([]slacklib.PostMessageParams, []slacklib.UpdateMessageParams, []slackgo.ModalViewRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]slacklib.PostMessageParams(nil), a.posts...),
		append([]slacklib.UpdateMessageParams(nil), a.updates...),
		append([]slackgo.ModalViewRequest(nil), a.views...)
}

// awaitPosts blocks until at least n messages have been posted.
func (a *syncAPI) awaitPosts(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		a.mu.Lock()
		got := len(a.posts)
		a.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-a.postCh:
		case <-deadline:
			t.Fatalf("timed out waiting for %d posts; saw %d", n, got)
		}
	}
}

func newTestFlow(api *syncAPI) *Flow {
	f := New(
		slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return api, nil }),
		NewRenderer("", assets.FS),
		adminID,
		func(id string) bool { return id == adminID },
	)
	f.now = func() time.Time { return time.Date(2026, 5, 14, 15, 42, 0, 0, time.UTC) }
	f.urlWait = 10 * time.Second
	return f
}

// script builds a custom profile from a shell snippet emulating an auth CLI.
func script(t *testing.T, needsCode bool, body string) auth.Profile {
	t.Helper()
	p, err := auth.Custom(body, needsCode)
	if err != nil {
		t.Fatalf("auth.Custom: %v", err)
	}
	return p
}

func request(p auth.Profile) Request {
	return Request{
		ToolName:        "gcp-mcp",
		Profile:         p,
		Requester:       Destination{ChannelID: "C1", ThreadTS: "100.1"},
		RequesterUserID: requesterID,
		Timeout:         20 * time.Second,
	}
}

// corrOf reads the correlation id back out of a posted admin card.
func corrOf(t *testing.T, blocks []byte) string {
	t.Helper()
	btns := buttons(t, blocks)
	if len(btns) == 0 {
		t.Fatalf("admin card carried no buttons:\n%s", blocks)
	}
	corr, _, ok := ParseActionID(btns[0].ActionID)
	if !ok {
		t.Fatalf("could not parse action_id %q", btns[0].ActionID)
	}
	return corr
}

// The full code flow: URL appears, admin spends the single attempt, the modal
// opens, the code is delivered to the process, and it exits clean.
func TestCodeFlowAuthenticates(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, true, `echo "Go to https://example.com/auth?x=1"; read code; [ "$code" = "GOOD" ]`)

	result := make(chan Outcome, 1)
	go func() {
		o, err := f.Run(context.Background(), request(p))
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		result <- o
	}()

	// Requester notice, then the admin card.
	api.awaitPosts(t, 2)
	posts, _, _ := api.snapshot()
	corr := corrOf(t, posts[1].Blocks)

	if err := f.HandleClick(context.Background(), corr, ActionPrimary, adminID, "trigger-1"); err != nil {
		t.Fatalf("HandleClick: %v", err)
	}
	if err := f.HandleCodeSubmission(corr, "GOOD", adminID); err != nil {
		t.Fatalf("HandleCodeSubmission: %v", err)
	}

	select {
	case o := <-result:
		if !o.Authenticated {
			t.Fatalf("expected success, got %+v", o)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not finish")
	}

	_, _, views := api.snapshot()
	if len(views) != 1 || views[0].CallbackID != ModalCallbackID {
		t.Fatalf("expected the code modal to open, got %+v", views)
	}
}

// A wrong code makes the process exit non-zero; that must fail closed.
func TestWrongCodeFailsClosed(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, true, `echo "Go to https://example.com/auth?x=1"; read code; [ "$code" = "GOOD" ]`)

	result := make(chan Outcome, 1)
	go func() {
		o, _ := f.Run(context.Background(), request(p))
		result <- o
	}()

	api.awaitPosts(t, 2)
	posts, _, _ := api.snapshot()
	corr := corrOf(t, posts[1].Blocks)

	_ = f.HandleClick(context.Background(), corr, ActionPrimary, adminID, "t")
	_ = f.HandleCodeSubmission(corr, "WRONG", adminID)

	o := <-result
	if o.Authenticated {
		t.Fatal("a rejected code must not authenticate")
	}
	if o.Reason == "" {
		t.Fatal("a failure should carry a reason")
	}
}

// Browser-only: nothing is typed back, the process finishes on its own.
func TestBrowserOnlyCompletesWithoutAClick(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, false, `echo "Open https://example.com/auth?x=1"; sleep 0.3`)

	o, err := f.Run(context.Background(), request(p))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !o.Authenticated {
		t.Fatalf("expected success, got %+v", o)
	}
}

// Spending the single attempt retires the whole actions bar and reveals the
// footer, while the flow keeps waiting.
func TestPrimaryClickRetiresTheBarAndShowsTheFooter(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, false, `echo "Open https://example.com/auth?x=1"; sleep 300`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = f.Run(ctx, request(p)) }()

	api.awaitPosts(t, 2)
	posts, _, _ := api.snapshot()
	corr := corrOf(t, posts[1].Blocks)

	if err := f.HandleClick(ctx, corr, ActionPrimary, adminID, "t"); err != nil {
		t.Fatalf("HandleClick: %v", err)
	}

	// The re-render is asynchronous; wait for it.
	deadline := time.After(10 * time.Second)
	for {
		_, updates, _ := api.snapshot()
		if len(updates) > 0 {
			last := updates[len(updates)-1]
			if btns := buttons(t, last.Blocks); len(btns) != 0 {
				t.Fatalf("actions bar survived the single attempt: %+v", btns)
			}
			if !strings.Contains(string(last.Blocks), "murtaugh_auth_admin_context") {
				t.Fatalf("footer missing after the primary click:\n%s", last.Blocks)
			}
			return
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("the admin card was never re-rendered after the primary click")
		}
	}
}

// The secondary "Open In Browser" on a code flow must NOT spend the attempt —
// the admin still needs the bar to come back and enter the code.
func TestSecondaryOpenDoesNotRetireTheBar(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, true, `echo "Go to https://example.com/auth?x=1"; read code; [ "$code" = "GOOD" ]`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan Outcome, 1)
	go func() {
		o, _ := f.Run(ctx, request(p))
		result <- o
	}()

	api.awaitPosts(t, 2)
	posts, _, _ := api.snapshot()
	corr := corrOf(t, posts[1].Blocks)

	if err := f.HandleClick(ctx, corr, ActionOpen, adminID, "t"); err != nil {
		t.Fatalf("HandleClick(open): %v", err)
	}
	// Give any (incorrect) re-render a chance to land.
	time.Sleep(200 * time.Millisecond)
	if _, updates, _ := api.snapshot(); len(updates) != 0 {
		t.Fatalf("the secondary link should not have changed the card: %+v", updates)
	}

	// The attempt is still available, so the flow can still complete.
	_ = f.HandleClick(ctx, corr, ActionPrimary, adminID, "t")
	_ = f.HandleCodeSubmission(corr, "GOOD", adminID)
	if o := <-result; !o.Authenticated {
		t.Fatalf("expected the flow to still be completable, got %+v", o)
	}
}

func TestDenyFailsClosedAndKillsTheProcess(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, true, `echo "Go to https://example.com/auth?x=1"; read code; echo ok`)

	result := make(chan Outcome, 1)
	go func() {
		o, _ := f.Run(context.Background(), request(p))
		result <- o
	}()

	api.awaitPosts(t, 2)
	posts, _, _ := api.snapshot()
	corr := corrOf(t, posts[1].Blocks)

	if err := f.HandleClick(context.Background(), corr, ActionDeny, adminID, "t"); err != nil {
		t.Fatalf("HandleClick(deny): %v", err)
	}

	select {
	case o := <-result:
		if o.Authenticated || !o.Denied {
			t.Fatalf("expected a denial, got %+v", o)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("deny did not end the flow — the process was probably not killed")
	}
}

func TestTimeoutFailsClosed(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, true, `echo "Go to https://example.com/auth?x=1"; sleep 300`)

	req := request(p)
	req.Timeout = 500 * time.Millisecond

	o, err := f.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if o.Authenticated || !o.TimedOut {
		t.Fatalf("expected a timeout, got %+v", o)
	}
}

func TestCancelledTurnFailsClosed(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, true, `echo "Go to https://example.com/auth?x=1"; sleep 300`)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan Outcome, 1)
	go func() {
		o, _ := f.Run(ctx, request(p))
		result <- o
	}()

	api.awaitPosts(t, 2)
	cancel()

	select {
	case o := <-result:
		if o.Authenticated {
			t.Fatal("a cancelled turn must not authenticate")
		}
		if !o.Cancelled {
			t.Fatalf("expected Cancelled, got %+v", o)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancellation did not end the flow")
	}
}

// No admin configured means nobody can approve, so the request cannot even be
// made. This is the fail-closed case with no card at all.
func TestNoAdminConfiguredFailsClosed(t *testing.T) {
	api := newSyncAPI()
	f := New(
		slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return api, nil }),
		NewRenderer("", assets.FS),
		"   ",
		nil,
	)
	p := script(t, false, `echo https://example.com/auth`)

	if _, err := f.Run(context.Background(), request(p)); err == nil {
		t.Fatal("expected an error when no admin is configured")
	}
	if posts, _, _ := api.snapshot(); len(posts) != 0 {
		t.Fatalf("nothing should have been posted, got %+v", posts)
	}
}

// When the requester IS the admin, the two cards collapse into one.
func TestCollapsesWhenRequesterIsTheAdmin(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, false, `echo "Open https://example.com/auth?x=1"; sleep 0.2`)

	req := request(p)
	req.RequesterUserID = adminID

	if _, err := f.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	posts, _, _ := api.snapshot()
	if len(posts) != 1 {
		t.Fatalf("expected a single collapsed card, got %d", len(posts))
	}
	if posts[0].ChannelID != "D-"+adminID {
		t.Fatalf("the collapsed card should go to the admin DM, got %q", posts[0].ChannelID)
	}
}

// The CLI/MCP path has no requesting thread, so it collapses too.
func TestCollapsesWithNoRequesterThread(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, false, `echo "Open https://example.com/auth?x=1"; sleep 0.2`)

	req := request(p)
	req.Requester = Destination{}

	if _, err := f.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if posts, _, _ := api.snapshot(); len(posts) != 1 {
		t.Fatalf("expected a single card, got %d", len(posts))
	}
}

// Only the admin may answer. A click from anyone else is refused even though
// the action_id is valid.
func TestNonAdminCannotResolve(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, true, `echo "Go to https://example.com/auth?x=1"; read code; echo ok`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan Outcome, 1)
	go func() {
		o, _ := f.Run(ctx, request(p))
		result <- o
	}()

	api.awaitPosts(t, 2)
	posts, _, _ := api.snapshot()
	corr := corrOf(t, posts[1].Blocks)

	for _, action := range []Action{ActionPrimary, ActionDeny, ActionOpen} {
		if err := f.HandleClick(ctx, corr, action, requesterID, "t"); err == nil {
			t.Fatalf("a non-admin click on %s was accepted", action)
		}
	}
	if err := f.HandleCodeSubmission(corr, "GOOD", requesterID); err == nil {
		t.Fatal("a non-admin code submission was accepted")
	}

	// The flow is untouched and still resolvable by the real admin.
	if err := f.HandleClick(ctx, corr, ActionDeny, adminID, "t"); err != nil {
		t.Fatalf("admin deny: %v", err)
	}
	if o := <-result; !o.Denied {
		t.Fatalf("expected the admin's denial to land, got %+v", o)
	}
}

// A click can arrive the moment Slack delivers the card — the corr id is
// already in the buttons, so there is no grace period. This pins the ordering
// that makes that safe: the session must be registered BEFORE the admin card is
// posted. With the two reversed, a fast admin's decision was rejected with "no
// authentication request is waiting" and silently lost, which showed up as an
// occasional CI failure and would have been an unreproducible bug in the field.
func TestClickDuringCardDeliveryIsNotLost(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, true, `echo "Go to https://example.com/auth?x=1"; read code; echo ok`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var clickErr error
	var once sync.Once
	api.onPost = func(index int, posted slacklib.PostMessageParams) {
		if index != 2 { // 1 is the requester notice; 2 is the admin card
			return
		}
		once.Do(func() {
			clickErr = f.HandleClick(ctx, corrOf(t, posted.Blocks), ActionDeny, adminID, "t")
		})
	}

	o, err := f.Run(ctx, request(p))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if clickErr != nil {
		t.Fatalf("a click delivered with the card was rejected: %v", clickErr)
	}
	if !o.Denied {
		t.Fatalf("the denial did not take effect: %+v", o)
	}
}

// A command that never prints a link has nothing to show the admin.
func TestNoVerificationURLFailsClosed(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	f.urlWait = 300 * time.Millisecond
	p := script(t, true, `echo "no link here"; sleep 300`)

	o, err := f.Run(context.Background(), request(p))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if o.Authenticated {
		t.Fatal("must not authenticate without a verification URL")
	}
	// The requester's card was posted first, so it must be settled to a
	// terminal state rather than left saying "notified".
	_, updates, _ := api.snapshot()
	if len(updates) == 0 {
		t.Fatal("the requester card was left in its pending state")
	}
}

// An auth command that dies immediately fails closed.
func TestProcessFailureFailsClosed(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, false, `echo "Open https://example.com/auth?x=1"; echo "boom" 1>&2; exit 3`)

	o, err := f.Run(context.Background(), request(p))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if o.Authenticated {
		t.Fatal("a non-zero exit must not authenticate")
	}
	if !strings.Contains(o.Reason, "boom") {
		t.Fatalf("the failure reason should quote the command output, got %q", o.Reason)
	}
}

// A missing binary is reported, not swallowed.
func TestUnstartableCommandFailsClosed(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := auth.Profile{Name: "broken", Command: "murtaugh-no-such-binary-xyz"}

	o, err := f.Run(context.Background(), request(p))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if o.Authenticated {
		t.Fatal("an unstartable command must not authenticate")
	}
}

// Both cards must reach a terminal state — the requester should never be left
// looking at "your admin has been notified" after the request is over.
func TestBothCardsSettleOnSuccess(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, false, `echo "Open https://example.com/auth?x=1"; sleep 0.2`)

	if _, err := f.Run(context.Background(), request(p)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	posts, updates, _ := api.snapshot()
	if len(posts) != 2 {
		t.Fatalf("expected two cards, got %d", len(posts))
	}

	var sawRequester, sawAdmin bool
	for _, u := range updates {
		switch u.ChannelID {
		case "C1":
			sawRequester = true
			if !strings.Contains(string(u.Blocks), "completed the authentication") {
				t.Fatalf("requester card not settled to success:\n%s", u.Blocks)
			}
		case "D-" + adminID:
			sawAdmin = true
		}
	}
	if !sawRequester || !sawAdmin {
		t.Fatalf("both cards must settle (requester=%v admin=%v)", sawRequester, sawAdmin)
	}
}

// The requester is not shown command output; it may quote things that are not
// theirs to read.
func TestRequesterCardOmitsFailureDetail(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	p := script(t, false, `echo "Open https://example.com/auth?x=1"; echo "secret-detail" 1>&2; exit 3`)

	if _, err := f.Run(context.Background(), request(p)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, updates, _ := api.snapshot()
	for _, u := range updates {
		if u.ChannelID == "C1" && strings.Contains(string(u.Blocks), "secret-detail") {
			t.Fatalf("command output leaked to the requester:\n%s", u.Blocks)
		}
	}
}

func TestClickOnUnknownRequestIsRejected(t *testing.T) {
	f := newTestFlow(newSyncAPI())
	if err := f.HandleClick(context.Background(), "nope", ActionPrimary, adminID, "t"); err == nil {
		t.Fatal("expected an error for an unknown correlation id")
	}
	if err := f.HandleCodeSubmission("nope", "code", adminID); err == nil {
		t.Fatal("expected an error for an unknown correlation id")
	}
}

func TestEmptyCodeIsRejected(t *testing.T) {
	f := newTestFlow(newSyncAPI())
	if err := f.HandleCodeSubmission("any", "   ", adminID); err == nil {
		t.Fatal("an empty verification code should be rejected")
	}
}
