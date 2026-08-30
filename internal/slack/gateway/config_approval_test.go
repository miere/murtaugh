package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/config"
	slackclient "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/configcard"
)

// recordingCardAPI captures posts and updates so a test can assert what the
// admin would have seen.
type recordingCardAPI struct {
	mu      sync.Mutex
	posts   []slackclient.PostMessageParams
	updates []slackclient.UpdateMessageParams
}

func (r *recordingCardAPI) PostMessage(_ context.Context, p slackclient.PostMessageParams) (slackclient.PostMessageResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts = append(r.posts, p)
	return slackclient.PostMessageResult{Channel: "D01ADMIN", TS: "111.222"}, nil
}

func (r *recordingCardAPI) UpdateMessage(_ context.Context, p slackclient.UpdateMessageParams) (slackclient.PostMessageResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, p)
	return slackclient.PostMessageResult{Channel: p.ChannelID, TS: p.TS}, nil
}

func (r *recordingCardAPI) snapshot() ([]slackclient.PostMessageParams, []slackclient.UpdateMessageParams) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]slackclient.PostMessageParams(nil), r.posts...),
		append([]slackclient.UpdateMessageParams(nil), r.updates...)
}

// approvalGateway builds a gateway wired for the approval conversation, with
// the admin DM already resolved so no Slack round trip is needed.
func configApprovalGateway(t *testing.T, api *recordingCardAPI) *Gateway {
	t.Helper()
	return &Gateway{
		logger:      quietLogger(),
		configCards: configcard.NewRenderer(t.TempDir(), assets.FS),
		alertAPI:    api,
		alertEditor: api,
		messaging:   stubMessaging{},
		cfg:         config.AccessConfig{AdminUser: "U01ADMIN"},
	}
}

// stubMessaging resolves the admin DM without a Slack round trip.
type stubMessaging struct{}

func (stubMessaging) PostMessageContext(_ context.Context, channelID string, _ ...slack.MsgOption) (string, string, error) {
	return channelID, "111.222", nil
}

func (stubMessaging) UpdateMessageContext(_ context.Context, channelID, ts string, _ ...slack.MsgOption) (string, string, string, error) {
	return channelID, ts, "", nil
}

func (stubMessaging) OpenConversationContext(_ context.Context, _ *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error) {
	channel := &slack.Channel{}
	channel.ID = "D01ADMIN"
	return channel, false, false, nil
}

// clickOn builds the interaction Slack would deliver for a button press.
func clickOn(actionID, user string) slack.InteractionCallback {
	return slack.InteractionCallback{
		User: slack.User{ID: user},
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{{ActionID: actionID}},
		},
	}
}

// awaitDecision runs RequestConfigApproval on its own goroutine and hands back
// a channel carrying the outcome.
func awaitDecision(t *testing.T, gw *Gateway, ctx context.Context, diff string) <-chan ConfigDecision {
	t.Helper()
	out := make(chan ConfigDecision, 1)
	go func() {
		decision, err := gw.RequestConfigApproval(ctx, diff)
		if err != nil && ctx.Err() == nil {
			t.Errorf("RequestConfigApproval: %v", err)
		}
		out <- decision
	}()
	return out
}

// pendingCorr waits for the card to be posted and digs the correlation id out
// of its action_id — the same round trip Slack makes.
func pendingCorr(t *testing.T, api *recordingCardAPI) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		posts, _ := api.snapshot()
		if len(posts) > 0 {
			body := string(posts[0].Blocks)
			idx := strings.Index(body, configcard.ActionPrefix+string(configcard.ActionApply)+"_")
			if idx >= 0 {
				rest := body[idx:]
				end := strings.IndexByte(rest, '"')
				corr, _, ok := configcard.ParseActionID(rest[:end])
				if ok {
					return corr
				}
			}
			t.Fatalf("posted card carries no parseable action_id:\n%s", body)
		}
		select {
		case <-deadline:
			t.Fatal("the approval card was never posted")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

const approvalDiff = " chat:\n-  enabled: false\n+  enabled: true\n"

// TestApproveAppliesAndSettlesTheCard is the happy path: the admin clicks
// Apply, the caller learns it may reload, and the DM stops looking like an open
// question.
func TestApproveAppliesAndSettlesTheCard(t *testing.T) {
	api := &recordingCardAPI{}
	gw := configApprovalGateway(t, api)
	ctx := context.Background()

	decisions := awaitDecision(t, gw, ctx, approvalDiff)
	corr := pendingCorr(t, api)

	gw.handleConfigApprovalClick(ctx, clickOn(configcard.ActionID(corr, configcard.ActionApply), "U01ADMIN"))

	select {
	case got := <-decisions:
		if got != ConfigApply {
			t.Fatalf("decision = %v, want ConfigApply", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the approval never reached the waiting caller")
	}

	_, updates := api.snapshot()
	if len(updates) != 1 {
		t.Fatalf("%d card updates, want 1", len(updates))
	}
	if !strings.Contains(updates[0].Text, "reloading the configuration") {
		t.Errorf("settled card does not say what happens next: %q", updates[0].Text)
	}
	if strings.Contains(string(updates[0].Blocks), configcard.ActionPrefix) {
		t.Error("a settled card still carries live buttons")
	}
}

// TestRollbackIsReportedToTheCaller covers the refusal path.
func TestRollbackIsReportedToTheCaller(t *testing.T) {
	api := &recordingCardAPI{}
	gw := configApprovalGateway(t, api)
	ctx := context.Background()

	decisions := awaitDecision(t, gw, ctx, approvalDiff)
	corr := pendingCorr(t, api)

	gw.handleConfigApprovalClick(ctx, clickOn(configcard.ActionID(corr, configcard.ActionRollback), "U01ADMIN"))

	select {
	case got := <-decisions:
		if got != ConfigRollback {
			t.Fatalf("decision = %v, want ConfigRollback", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the rollback never reached the waiting caller")
	}
	_, updates := api.snapshot()
	if len(updates) != 1 || !strings.Contains(updates[0].Text, "Rolled back") {
		t.Errorf("card does not record the rollback: %+v", updates)
	}
}

// TestNonAdminClickIsIgnored is the security property. The router admits any
// allowlisted user to built-ins, and that is not enough here: approving adopts
// whatever is in the store, which may be an edit that widens the allowlist. An
// allowlisted user must not be able to promote themselves with one click.
func TestNonAdminClickIsIgnored(t *testing.T) {
	api := &recordingCardAPI{}
	gw := configApprovalGateway(t, api)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	decisions := awaitDecision(t, gw, ctx, approvalDiff)
	corr := pendingCorr(t, api)

	gw.handleConfigApprovalClick(ctx, clickOn(configcard.ActionID(corr, configcard.ActionApply), "U01GUEST"))

	select {
	case got := <-decisions:
		if got == ConfigApply {
			t.Fatal("a non-admin approved a configuration change")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the request neither settled nor expired")
	}
}

// TestExpiryIsNotApproval pins the default. Nobody answering must leave the
// running configuration in place — adopting an unreviewed change because the
// admin was asleep would make the review theatre.
func TestExpiryIsNotApproval(t *testing.T) {
	api := &recordingCardAPI{}
	gw := configApprovalGateway(t, api)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	decision, err := gw.RequestConfigApproval(ctx, approvalDiff)
	if err == nil {
		t.Fatal("an expired request reported success")
	}
	if decision != ConfigRollback {
		t.Errorf("decision = %v on expiry, want ConfigRollback", decision)
	}
	_, updates := api.snapshot()
	if len(updates) != 1 || !strings.Contains(updates[0].Text, "previous configuration was kept") {
		t.Errorf("expired card does not record what happened: %+v", updates)
	}
}

// TestDoubleClickSettlesOnce guards the registry against a second press — from
// an impatient admin, or Slack retrying delivery — reaching a channel nobody is
// reading any more.
func TestDoubleClickSettlesOnce(t *testing.T) {
	api := &recordingCardAPI{}
	gw := configApprovalGateway(t, api)
	ctx := context.Background()

	decisions := awaitDecision(t, gw, ctx, approvalDiff)
	corr := pendingCorr(t, api)
	click := clickOn(configcard.ActionID(corr, configcard.ActionApply), "U01ADMIN")

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); gw.handleConfigApprovalClick(ctx, click) }()
	}
	wg.Wait()

	select {
	case got := <-decisions:
		if got != ConfigApply {
			t.Fatalf("decision = %v, want ConfigApply", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no decision was delivered")
	}
}

// TestEmptyDiffIsRefused stops the daemon asking the admin to approve nothing,
// which is what a bug in change detection would look like from their side.
func TestEmptyDiffIsRefused(t *testing.T) {
	api := &recordingCardAPI{}
	gw := configApprovalGateway(t, api)

	if _, err := gw.RequestConfigApproval(context.Background(), "   "); err == nil {
		t.Fatal("an empty diff was posted for approval")
	}
	if posts, _ := api.snapshot(); len(posts) != 0 {
		t.Errorf("%d cards posted for an empty diff", len(posts))
	}
}

// TestConfigInteractionIsRecognised pins the router predicate: a missed match
// sends the click to the workflow engine, and a false match steals somebody
// else's button.
func TestConfigInteractionIsRecognised(t *testing.T) {
	if !isConfigApprovalInteraction(clickOn(configcard.ActionID("c1", configcard.ActionApply), "U1")) {
		t.Error("a config approval click was not recognised")
	}
	if isConfigApprovalInteraction(clickOn("murtaugh_restart_suggestion_confirm", "U1")) {
		t.Error("the restart card's button was claimed as a config approval")
	}
	if isConfigApprovalInteraction(slack.InteractionCallback{}) {
		t.Error("an empty callback was claimed as a config approval")
	}
}
