package nodehost_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/authcard"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/display"
	authrequest "github.com/miere/murtaugh/internal/tools/auth/request"
)

type cardSlack struct {
	slacklib.SlackAPI

	mu      sync.Mutex
	posts   []slacklib.PostMessageParams
	updates []slacklib.UpdateMessageParams
}

func (s *cardSlack) PostMessage(_ context.Context, p slacklib.PostMessageParams) (slacklib.PostMessageResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts = append(s.posts, p)
	return slacklib.PostMessageResult{Channel: p.ChannelID, TS: "ts-" + p.ChannelID}, nil
}

func (s *cardSlack) UpdateMessage(_ context.Context, p slacklib.UpdateMessageParams) (slacklib.PostMessageResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, p)
	return slacklib.PostMessageResult{Channel: p.ChannelID, TS: p.TS}, nil
}

func (s *cardSlack) OpenDM(_ context.Context, userID string) (string, error) {
	return "D-" + userID, nil
}

func (s *cardSlack) OpenView(context.Context, string, slackgo.ModalViewRequest) error { return nil }

func (s *cardSlack) awaitLink(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for _, u := range s.updates {
			if strings.Contains(string(u.Blocks), url) {
				s.mu.Unlock()
				return
			}
		}
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the link never reached the owner's card")
}

func (s *cardSlack) dmCard(t *testing.T) (channel, corr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for _, p := range s.posts {
			if match := regexp.MustCompile(`murtaugh_auth:([0-9a-f]+):`).FindSubmatch(p.Blocks); match != nil {
				s.mu.Unlock()
				return p.ChannelID, string(match[1])
			}
		}
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no sign-in card was posted")
	return "", ""
}

// The whole sign-in on a node: nothing runs until the owner approves by DM, and
// the code they paste then reaches the command and settles both cards.
func TestANodeSignsInForItsOwnerEndToEnd(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	result := make(chan string, 1)
	var script *scriptedAgent
	script = newScriptedAgent(func(turn *scriptedTurn) {
		out, err := authrequest.New(agent.TurnDisplay{}).Invoke(nativeCtx(turn, script.lastPrompt()), map[string]any{
			"tool":       "gcp-mcp",
			"profile":    "custom",
			"command":    `touch "` + marker + `"; echo "Go to https://example.com/auth?x=1"; read code; [ "$code" = GOOD ]`,
			"needs_code": true,
		})
		if err != nil {
			result <- err.Error()
		} else {
			result <- out.(authrequest.Result).String()
		}
		turn.emit(agent.Event{Type: agent.EventComplete})
	})
	rig := dialLoopback(t, script)

	slack := &cardSlack{}
	flow := authcard.New(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return slack, nil }),
		authcard.NewRenderer("", assets.FS), "UADMIN", nil)
	flow.SetAuthorised(func(id string) bool { return id == nodeOwner || id == "UADMIN" })
	cards := display.New(nil, nil).WithSignIns(flow, nil)

	settled := make(chan agent.SignInSettled, 4)
	drawn := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	events, err := rig.sessions["default"].Prompt(ctx,
		agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"},
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner},
		agent.PromptRequest{Text: "deploy it", Channel: "C1", Thread: "123.4", User: "U9"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	for ev := range events {
		switch ev.Type {
		case agent.EventSignIn:
			go func() {
				defer close(drawn)
				cards.SignIn(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "123.4", UserID: "U9"}, ev.SignIn, settled)
			}()
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
		case agent.EventSignInSettled:
			settled <- *ev.SignInSettled
			if ev.SignInSettled.State != agent.SignInReady {
				continue
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("the approved command did not run")
			}
			_, corr := slack.dmCard(t)
			slack.awaitLink(t, "https://example.com/auth?x=1")
			if err := flow.HandleClick(context.Background(), corr, authcard.ActionPrimary, nodeOwner, "t"); err != nil {
				t.Fatalf("owner click: %v", err)
			}
			if err := flow.HandleCodeSubmission(corr, "GOOD", nodeOwner); err != nil {
				t.Fatalf("owner submit: %v", err)
			}
		case agent.EventError:
			t.Fatalf("the turn failed: %v", ev.Error)
		}
	}
	select {
	case <-drawn:
	case <-time.After(10 * time.Second):
		t.Fatal("the sign-in never settled")
	}

	if got := <-result; !strings.Contains(got, "Signed in for gcp-mcp") {
		t.Fatalf("the model was told %q", got)
	}
	slack.mu.Lock()
	defer slack.mu.Unlock()
	if len(slack.posts) != 2 || slack.posts[0].ChannelID != "C1" || slack.posts[0].ThreadTS != "123.4" {
		t.Fatalf("posted %d messages, the first to %q/%q; want the notice in the turn's thread and the card by DM",
			len(slack.posts), slack.posts[0].ChannelID, slack.posts[0].ThreadTS)
	}
	var thread, dm bool
	for _, u := range slack.updates {
		thread = thread || (u.ChannelID == "C1" && strings.Contains(string(u.Blocks), "completed the authentication"))
		dm = dm || (u.ChannelID == "D-"+nodeOwner && strings.Contains(string(u.Blocks), "Authentication succeeded"))
	}
	if !thread || !dm {
		t.Fatalf("success did not settle both cards (thread=%v dm=%v)", thread, dm)
	}
}

// A node whose link drops mid-sign-in kills its login command, even for a tool
// that is not waiting on the turn's context.
func TestALinkDropStopsTheNodesSignIn(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	meta := agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner}
	result := make(chan error, 1)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		stopped := make(chan error, 1)
		go func() {
			_, err := authrequest.New(agent.TurnDisplay{}).Invoke(detachedCtx(turn, meta), map[string]any{
				"tool":    "gcp-mcp",
				"profile": "custom",
				"command": `echo $$ > ` + pidFile + `; echo "Go to https://example.com/auth"; exec sleep 30`,
			})
			stopped <- err
		}()
		<-turn.ctx.Done()
		select {
		case err := <-stopped:
			result <- err
		case <-time.After(10 * time.Second):
		}
	})
	rig := dialLoopback(t, script)

	events := promptDefault(t, rig)
	for ev := range events {
		if ev.SignIn != nil {
			ev.SignIn.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayApproved}
		}
		if ev.SignInSettled != nil && ev.SignInSettled.State == agent.SignInReady {
			break
		}
	}
	rig.host.DetachAll("the test dropped the link")
	for range events {
	}

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
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			t.Fatalf("the login command (pid %d) is still running after its link dropped", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
