package nodehost_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/slack/authcard"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/display"
	authrequest "github.com/miere/murtaugh/internal/tools/auth/request"
)

func ownerCards(t *testing.T) (*cardSlack, *authcard.Flow, func(context.Context, *agent.SignInPrompt, <-chan agent.SignInSettled, func(error))) {
	t.Helper()
	slack := &cardSlack{}
	flow := authcard.New(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return slack, nil }),
		authcard.NewRenderer("", assets.FS), "UADMIN", nil)
	flow.SetAuthorised(func(id string) bool { return id == nodeOwner || id == "UADMIN" })
	cards := display.New(nil, nil).WithSignIns(flow, nil)
	return slack, flow, func(ctx context.Context, prompt *agent.SignInPrompt, settled <-chan agent.SignInSettled, shown func(error)) {
		cards.ShowSignIn(ctx, agent.TurnLocation{}, prompt, settled, shown)
	}
}

func headlessSignIn(rig *loopback, args map[string]any) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := authrequest.New(agent.TurnDisplay{}).WithoutConversation(rig.signIns).Invoke(context.Background(), args)
		done <- err
	}()
	return done
}

func TestAJobOnANodeSignsInThroughItsOwnersDM(t *testing.T) {
	slack, flow, draw := ownerCards(t)
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), drawingSignIns(draw))

	marker := filepath.Join(t.TempDir(), "ran")
	result := headlessSignIn(rig, map[string]any{
		"tool":       "gcp-mcp",
		"profile":    "custom",
		"command":    `touch "` + marker + `"; host=example.com; echo "Go to https://$host/auth?x=1"; read code; [ "$code" = GOOD ]`,
		"needs_code": true,
	})

	channel, corr := slack.dmCard(t)
	if channel != "D-"+nodeOwner {
		t.Fatalf("the sign-in card went to %q, want the node owner's DM", channel)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the node ran the command before its owner approved it")
	}
	if err := flow.HandleClick(context.Background(), corr, authcard.ActionApprove, "UADMIN", "t"); err == nil {
		t.Fatal("the gateway admin approved a command sent to the node's owner")
	}
	if err := flow.HandleClick(context.Background(), corr, authcard.ActionApprove, nodeOwner, "t"); err != nil {
		t.Fatalf("owner approve: %v", err)
	}
	slack.awaitLink(t, "https://example.com/auth?x=1")
	if err := flow.HandleClick(context.Background(), corr, authcard.ActionPrimary, nodeOwner, "t"); err != nil {
		t.Fatalf("owner click: %v", err)
	}
	if err := flow.HandleCodeSubmission(corr, "GOOD", nodeOwner); err != nil {
		t.Fatalf("owner submit: %v", err)
	}

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("the job's sign-in failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the job's sign-in never finished")
	}
	waitFor(t, "the owner's card to settle", func() bool {
		slack.mu.Lock()
		defer slack.mu.Unlock()
		for _, u := range slack.updates {
			if u.ChannelID == "D-"+nodeOwner && strings.Contains(string(u.Blocks), "Authentication succeeded") {
				return true
			}
		}
		return false
	})
	slack.mu.Lock()
	defer slack.mu.Unlock()
	for _, p := range slack.posts {
		if p.ChannelID != "D-"+nodeOwner {
			t.Fatalf("a sign-in with no conversation posted to %q/%q; only the owner's DM may show it", p.ChannelID, p.ThreadTS)
		}
	}
	for _, u := range slack.updates {
		if u.ChannelID != "D-"+nodeOwner {
			t.Fatalf("a sign-in with no conversation updated %q; only the owner's DM may show it", u.ChannelID)
		}
	}
}

func TestALinkDropStopsASignInWithNoConversation(t *testing.T) {
	slack, flow, draw := ownerCards(t)
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), drawingSignIns(draw))

	pidFile := filepath.Join(t.TempDir(), "pid")
	result := headlessSignIn(rig, map[string]any{
		"tool":    "gcp-mcp",
		"profile": "custom",
		"command": `echo $$ > ` + pidFile + `; host=example.com; echo "Go to https://$host/auth"; exec sleep 30`,
	})
	_, corr := slack.dmCard(t)
	if err := flow.HandleClick(context.Background(), corr, authcard.ActionApprove, nodeOwner, "t"); err != nil {
		t.Fatalf("owner approve: %v", err)
	}
	slack.awaitLink(t, "https://example.com/auth")

	rig.host.DetachAll("the test dropped the link")

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("a sign-in whose link dropped ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the sign-in outlived its link")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse pid %q: %v", raw, err)
	}
	waitFor(t, "the login command to stop", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	waitFor(t, "the owner's card to be withdrawn", func() bool {
		slack.mu.Lock()
		defer slack.mu.Unlock()
		for _, u := range slack.updates {
			if strings.Contains(string(u.Blocks), "cancelled before it was completed") {
				return true
			}
		}
		return false
	})
}

func TestANodeCannotAimASignInAtSomeoneElse(t *testing.T) {
	drawn := make(chan *agent.SignInPrompt, 1)
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), drawingSignIns(func(ctx context.Context, prompt *agent.SignInPrompt, _ <-chan agent.SignInSettled, shown func(error)) {
		drawn <- prompt
		shown(nil)
		<-ctx.Done()
	}))
	node := attachRaw(t, rig, "node-raw")

	request, err := agentwire.Request("1", agentwire.MethodSignIn, map[string]any{
		"id": "sign-in-1", "tool": "gcp-mcp", "profile": "gcloud", "url": "https://accounts.example.com",
		"owner": "U-someone-else", "recipient": "U-someone-else", "user_id": "U-someone-else",
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	node.send(t, request)

	select {
	case prompt := <-drawn:
		if prompt.Owner != nodeOwner {
			t.Fatalf("the sign-in is for %q, want the owner the node's token names (%q)", prompt.Owner, nodeOwner)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway never drew the node's sign-in")
	}
}

func TestASignInForAnOwnerWhoMayNotUseTheGatewayIsRefusedBeforeAnythingRuns(t *testing.T) {
	slack, flow, draw := ownerCards(t)
	flow.SetAuthorised(func(string) bool { return false })
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), drawingSignIns(draw))

	marker := filepath.Join(t.TempDir(), "ran")
	result := headlessSignIn(rig, map[string]any{"tool": "gcp-mcp", "profile": "custom", "command": `touch "` + marker + `"`})
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "could not be shown to anyone") {
			t.Fatalf("a sign-in for an owner who may not use the gateway ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the refusal was not fast")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran for an owner who may not use the gateway")
	}
	slack.mu.Lock()
	defer slack.mu.Unlock()
	if len(slack.posts) != 0 {
		t.Fatalf("posted %d messages for an owner who may not use the gateway", len(slack.posts))
	}
}
