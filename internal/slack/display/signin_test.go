package display

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/authcard"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

type cardAPI struct {
	slacklib.SlackAPI

	mu      sync.Mutex
	posts   []slacklib.PostMessageParams
	updates []slacklib.UpdateMessageParams
	posted  chan struct{}
}

func (a *cardAPI) PostMessage(_ context.Context, p slacklib.PostMessageParams) (slacklib.PostMessageResult, error) {
	a.mu.Lock()
	a.posts = append(a.posts, p)
	a.mu.Unlock()
	a.posted <- struct{}{}
	return slacklib.PostMessageResult{Channel: p.ChannelID, TS: "ts-" + p.ChannelID}, nil
}

func (a *cardAPI) UpdateMessage(_ context.Context, p slacklib.UpdateMessageParams) (slacklib.PostMessageResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updates = append(a.updates, p)
	return slacklib.PostMessageResult{Channel: p.ChannelID, TS: p.TS}, nil
}

func (a *cardAPI) OpenDM(_ context.Context, userID string) (string, error) { return "D-" + userID, nil }

func (a *cardAPI) OpenView(context.Context, string, slackgo.ModalViewRequest) error { return nil }

func (a *cardAPI) snapshot() ([]slacklib.PostMessageParams, []slacklib.UpdateMessageParams) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]slacklib.PostMessageParams(nil), a.posts...), append([]slacklib.UpdateMessageParams(nil), a.updates...)
}

type signInRig struct {
	api     *cardAPI
	flow    *authcard.Flow
	allowed *bool
	prompt  *agent.SignInPrompt
	settled chan agent.SignInSettled
	done    chan struct{}
	cancel  context.CancelFunc
}

func drawSignIn(t *testing.T) *signInRig {
	t.Helper()
	return drawSignInFor(t, agent.SignInRequest{Tool: "gcp-mcp", Profile: "gcloud", URL: "https://accounts.example.com/o/oauth2", NeedsCode: true})
}

func drawSignInFor(t *testing.T, req agent.SignInRequest) *signInRig {
	t.Helper()
	allowed := true
	api := &cardAPI{posted: make(chan struct{}, 8)}
	flow := authcard.New(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return api, nil }),
		authcard.NewRenderer("", assets.FS), "UADMIN", nil)
	flow.SetAuthorised(func(id string) bool { return allowed && (id == "UOWNER" || id == "UADMIN") })
	r := &signInRig{
		api:     api,
		flow:    flow,
		allowed: &allowed,
		prompt: &agent.SignInPrompt{
			Request: req,
			Owner:   "UOWNER",
			Answer:  make(chan agent.DisplayAnswer, 2),
		},
		settled: make(chan agent.SignInSettled),
		done:    make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	go func() {
		defer close(r.done)
		New(nil, nil).WithSignIns(flow, nil).SignIn(ctx, agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1", UserID: "UREQ"}, r.prompt, r.settled)
	}()
	for range 2 {
		select {
		case <-api.posted:
		case <-time.After(5 * time.Second):
			t.Fatal("the sign-in was never drawn")
		}
	}
	return r
}

func (r *signInRig) corr(t *testing.T) string {
	t.Helper()
	posts, _ := r.api.snapshot()
	match := regexp.MustCompile(`murtaugh_auth:([0-9a-f]+):`).FindSubmatch(posts[1].Blocks)
	if match == nil {
		t.Fatalf("the DM card carries no buttons:\n%s", posts[1].Blocks)
	}
	return string(match[1])
}

func (r *signInRig) answer(t *testing.T) agent.DisplayAnswer {
	t.Helper()
	select {
	case a := <-r.prompt.Answer:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was sent back to whoever runs the sign-in")
		return agent.DisplayAnswer{}
	}
}

func (r *signInRig) finished(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drawing did not finish")
	}
}

// The owner is DMed, the requester's thread gets the notice, the owner's code
// goes back, and the node's word on how it ended settles both cards.
func TestASignInIsDrawnForItsOwnerAndSettledByTheNode(t *testing.T) {
	r := drawSignIn(t)
	posts, _ := r.api.snapshot()
	if posts[0].ChannelID != "C1" || posts[0].ThreadTS != "t1" {
		t.Fatalf("the notice went to %q/%q, want the turn's thread", posts[0].ChannelID, posts[0].ThreadTS)
	}
	if posts[1].ChannelID != "D-UOWNER" {
		t.Fatalf("the sign-in card went to %q, want the owner's DM and not the admin's", posts[1].ChannelID)
	}

	corr := r.corr(t)
	if err := r.flow.HandleClick(context.Background(), corr, authcard.ActionPrimary, "UOWNER", "t"); err != nil {
		t.Fatalf("owner click: %v", err)
	}
	if err := r.flow.HandleCodeSubmission(corr, "4/0Ab-code", "UOWNER"); err != nil {
		t.Fatalf("owner submit: %v", err)
	}
	if got := r.answer(t); got.Outcome != agent.DisplayAnswered || got.Code != "4/0Ab-code" || got.UserID != "UOWNER" {
		t.Fatalf("the code went back as %+v", got)
	}

	r.settled <- agent.SignInSettled{Prompt: r.prompt, State: agent.SignInSuccess}
	r.finished(t)
	_, updates := r.api.snapshot()
	var thread, dm bool
	for _, u := range updates {
		thread = thread || (u.ChannelID == "C1" && strings.Contains(string(u.Blocks), "completed the authentication"))
		dm = dm || (u.ChannelID == "D-UOWNER" && strings.Contains(string(u.Blocks), "Authentication succeeded"))
	}
	if !thread || !dm {
		t.Fatalf("success did not settle both cards (thread=%v dm=%v)", thread, dm)
	}
}

// Access withdrawn before the owner submits wins: no code goes back, the node is
// told to stop, and the drawing ends.
func TestASubmissionFromAnOwnerWhoLostAccessStopsTheSignIn(t *testing.T) {
	r := drawSignIn(t)
	*r.allowed = false
	if err := r.flow.HandleCodeSubmission(r.corr(t), "4/0Ab-code", "UOWNER"); err == nil {
		t.Fatal("a code from an owner who lost access was accepted")
	}
	got := r.answer(t)
	if got.Outcome == agent.DisplayAnswered || got.Code != "" {
		t.Fatalf("the node was sent %+v; it must be told to stop, with no code", got)
	}
	if got.Note != "the person asked to sign in lost access to this gateway, so the sign-in was stopped" {
		t.Fatalf("the node was not told why, in words fit for it: %+v", got)
	}
	r.finished(t)
}

// The owner declining is told apart from a turn ending, so the model hears who
// said no.
func TestADeclinedSignInTellsTheNode(t *testing.T) {
	r := drawSignIn(t)
	if err := r.flow.HandleClick(context.Background(), r.corr(t), authcard.ActionDeny, "UOWNER", "t"); err != nil {
		t.Fatalf("owner deny: %v", err)
	}
	if got := r.answer(t); got.Outcome != agent.DisplayDenied {
		t.Fatalf("a declined sign-in went back as %+v", got)
	}
	r.finished(t)
}

// A drawing withdrawn with its turn tells whoever runs the sign-in to stop.
func TestAWithdrawnSignInTellsTheNodeToStop(t *testing.T) {
	r := drawSignIn(t)
	r.cancel()
	if got := r.answer(t); got.Outcome != agent.DisplayDismissed {
		t.Fatalf("a withdrawn sign-in went back as %+v", got)
	}
	r.finished(t)
}

// An owner who may not use the gateway is never sent a card, and the node hears why.
func TestASignInForAnOwnerWhoMayNotUseTheGatewayIsRefused(t *testing.T) {
	api := &cardAPI{posted: make(chan struct{}, 8)}
	flow := authcard.New(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return api, nil }),
		authcard.NewRenderer("", assets.FS), "UADMIN", nil)
	flow.SetAuthorised(func(id string) bool { return id == "UADMIN" })
	prompt := &agent.SignInPrompt{Request: agent.SignInRequest{Tool: "x", Profile: "gcloud", URL: "https://x"}, Owner: "UOWNER", Answer: make(chan agent.DisplayAnswer, 2)}
	New(nil, nil).WithSignIns(flow, nil).SignIn(context.Background(), here, prompt, nil)
	if got := <-prompt.Answer; got.Outcome != agent.DisplayUnavailable || got.Note != "the owner of this machine may not use this gateway, so nobody was asked to sign in" {
		t.Fatalf("a refused sign-in went back as %+v", got)
	}
	if posts, _ := api.snapshot(); len(posts) != 0 {
		t.Fatalf("a refused sign-in posted %d messages", len(posts))
	}
}

// Raw gateway failures stay on the gateway: the node's model only hears that
// the sign-in could not be shown.
func TestAGatewayFailureReachesTheNodeAsAFixedReason(t *testing.T) {
	flow := authcard.New(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) {
		return nil, errors.New("slack: invalid_auth for xoxb-secret at /etc/murtaugh/templates")
	}),
		authcard.NewRenderer("", assets.FS), "UADMIN", nil)
	flow.SetAuthorised(func(string) bool { return true })
	prompt := &agent.SignInPrompt{Request: agent.SignInRequest{Tool: "x", Profile: "gcloud", URL: "https://x"}, Owner: "UOWNER", Answer: make(chan agent.DisplayAnswer, 2)}
	New(nil, nil).WithSignIns(flow, nil).SignIn(context.Background(), here, prompt, nil)
	if got := <-prompt.Answer; got.Outcome != agent.DisplayUnavailable || got.Note != "the sign-in could not be shown in Slack" {
		t.Fatalf("a gateway failure went back as %+v", got)
	}
}

// A command the owner approves only then gets its link shown, and the code
// typed after that still goes back.
func TestACommandIsApprovedBeforeItsLinkIsShown(t *testing.T) {
	r := drawSignInFor(t, agent.SignInRequest{Tool: "vendor-mcp", Profile: "custom", Command: "vendor-cli login", NeedsCode: true})
	corr := r.corr(t)
	if err := r.flow.HandleClick(context.Background(), corr, authcard.ActionApprove, "UOWNER", "t"); err != nil {
		t.Fatalf("owner approve: %v", err)
	}
	if got := r.answer(t); got.Outcome != agent.DisplayApproved || got.UserID != "UOWNER" {
		t.Fatalf("the approval went back as %+v", got)
	}
	r.settled <- agent.SignInSettled{Prompt: r.prompt, State: agent.SignInReady, URL: "https://vendor.example.com/device"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, updates := r.api.snapshot()
		if len(updates) > 0 && strings.Contains(string(updates[len(updates)-1].Blocks), "https://vendor.example.com/device") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the link never reached the owner's card")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.flow.HandleCodeSubmission(corr, "123-456", "UOWNER"); err != nil {
		t.Fatalf("owner submit: %v", err)
	}
	if got := r.answer(t); got.Outcome != agent.DisplayAnswered || got.Code != "123-456" {
		t.Fatalf("the code went back as %+v", got)
	}
	r.settled <- agent.SignInSettled{Prompt: r.prompt, State: agent.SignInSuccess}
	r.finished(t)
}

// Declining the command tells the node before anything has run.
func TestADeclinedCommandTellsTheNode(t *testing.T) {
	r := drawSignInFor(t, agent.SignInRequest{Tool: "vendor-mcp", Profile: "custom", Command: "vendor-cli login"})
	if err := r.flow.HandleClick(context.Background(), r.corr(t), authcard.ActionDeny, "UOWNER", "t"); err != nil {
		t.Fatalf("owner deny: %v", err)
	}
	if got := r.answer(t); got.Outcome != agent.DisplayDenied {
		t.Fatalf("a declined command went back as %+v", got)
	}
	r.finished(t)
}

func TestAFinishedSignInIsConfirmedOnlyWhileItsOwnerMayUseTheGateway(t *testing.T) {
	r := drawSignIn(t)
	r.settled <- agent.SignInSettled{Prompt: r.prompt, State: agent.SignInConfirming}
	if got := r.answer(t); got.Outcome != agent.DisplayApproved {
		t.Fatalf("a sign-in finished by an owner who still has access was answered %+v", got)
	}
	r.settled <- agent.SignInSettled{Prompt: r.prompt, State: agent.SignInSuccess}
	r.finished(t)

	r = drawSignIn(t)
	*r.allowed = false
	r.settled <- agent.SignInSettled{Prompt: r.prompt, State: agent.SignInConfirming}
	if got := r.answer(t); got.Outcome == agent.DisplayApproved {
		t.Fatal("a sign-in finished after its owner lost access was confirmed")
	}
	r.finished(t)
	_, updates := r.api.snapshot()
	for _, u := range updates {
		if u.ChannelID == "D-UOWNER" && strings.Contains(string(u.Blocks), "lost access") {
			return
		}
	}
	t.Fatal("the owner's card does not say the sign-in was stopped for losing access")
}
