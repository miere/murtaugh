package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/slack/approvalcard"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/client/slacktest"
	"github.com/miere/murtaugh/internal/slack/interaction"
)

// signalingSlackAPI wraps the shared fake to announce each post so the test can
// learn the broker's correlation id and resolve the confirmation prompt.
type signalingSlackAPI struct {
	*slacktest.FakeAPI
	posted chan slacklib.PostMessageParams
}

func (s *signalingSlackAPI) PostMessage(ctx context.Context, p slacklib.PostMessageParams) (slacklib.PostMessageResult, error) {
	res, err := s.FakeAPI.PostMessage(ctx, p)
	s.posted <- p
	return res, err
}

// dmMessaging is a minimal slackMessagingAPI that opens a fixed admin DM.
type dmMessaging struct{ dm string }

func (d *dmMessaging) PostMessageContext(context.Context, string, ...slackgo.MsgOption) (string, string, error) {
	return "", "", nil
}
func (d *dmMessaging) UpdateMessageContext(context.Context, string, string, ...slackgo.MsgOption) (string, string, string, error) {
	return "", "", "", nil
}
func (d *dmMessaging) OpenConversationContext(context.Context, *slackgo.OpenConversationParameters) (*slackgo.Channel, bool, bool, error) {
	ch := &slackgo.Channel{}
	ch.ID = d.dm
	return ch, false, false, nil
}

func newConfirmGateway(t *testing.T) (*Gateway, *signalingSlackAPI, *interaction.Broker) {
	t.Helper()
	sig := &signalingSlackAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "DADMIN", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))
	a := &Gateway{
		logger:       discardLogger(),
		interactions: broker,
		messaging:    &dmMessaging{dm: "DADMIN"},
		cfg:          config.AccessConfig{AdminUser: "UADMIN"},
		// Wired as production wires it, so the tests drive the card the admin
		// actually sees rather than the plain button-row fallback.
		approvalCards: approvalcard.NewRenderer("", assets.FS),
	}
	return a, sig, broker
}

func heldJob() config.JobProfile {
	unconfirmed := false
	return config.JobProfile{Command: "/bin/echo", Args: []string{"hi"}, Every: "1h", Confirmed: &unconfirmed}
}

func TestConfirmHeldJob_ApprovedRunsAndRemembers(t *testing.T) {
	a, sig, broker := newConfirmGateway(t)

	out := make(chan bool, 1)
	go func() { out <- a.confirmHeldJob(context.Background(), "myjob", heldJob()) }()

	posted := <-sig.posted
	if posted.ChannelID != "DADMIN" {
		t.Fatalf("confirmation posted to %q, want the admin DM DADMIN", posted.ChannelID)
	}
	broker.Resolve(corrFromBlocks(t, posted.Blocks), interaction.Decision{OptionID: "approve", UserID: "UADMIN"})

	if !<-out {
		t.Fatal("approval should return true (run the job)")
	}
	if !a.isJobConfirmed("myjob") {
		t.Fatal("approved job should be remembered as confirmed for this session")
	}
}

// The approval must outlive the process, otherwise every restart re-prompts for
// jobs the admin already signed off on.
func TestConfirmHeldJob_ApprovedPersistsConfirmation(t *testing.T) {
	a, sig, broker := newConfirmGateway(t)
	persisted := make(chan string, 1)
	a.persistJobConfirmation = func(_ context.Context, name string) error {
		persisted <- name
		return nil
	}

	out := make(chan bool, 1)
	go func() { out <- a.confirmHeldJob(context.Background(), "myjob", heldJob()) }()

	posted := <-sig.posted
	broker.Resolve(corrFromBlocks(t, posted.Blocks), interaction.Decision{OptionID: "approve", UserID: "UADMIN"})

	if !<-out {
		t.Fatal("approval should return true (run the job)")
	}
	select {
	case name := <-persisted:
		if name != "myjob" {
			t.Fatalf("persisted confirmation for %q, want myjob", name)
		}
	default:
		t.Fatal("approval was not persisted to the config store")
	}
}

// A store that cannot be written must not block the run the admin just
// approved: it degrades to the old session-scoped behaviour.
func TestConfirmHeldJob_PersistFailureStillRuns(t *testing.T) {
	a, sig, broker := newConfirmGateway(t)
	a.persistJobConfirmation = func(context.Context, string) error {
		return errors.New("store is read-only")
	}

	out := make(chan bool, 1)
	go func() { out <- a.confirmHeldJob(context.Background(), "myjob", heldJob()) }()

	posted := <-sig.posted
	broker.Resolve(corrFromBlocks(t, posted.Blocks), interaction.Decision{OptionID: "approve", UserID: "UADMIN"})

	if !<-out {
		t.Fatal("a failed confirmation write must not cancel an approved run")
	}
	if !a.isJobConfirmed("myjob") {
		t.Fatal("approval should still hold for this session")
	}
}

func TestConfirmHeldJob_DeniedDoesNotRun(t *testing.T) {
	a, sig, broker := newConfirmGateway(t)
	denyPersisted := false
	a.persistJobConfirmation = func(context.Context, string) error {
		denyPersisted = true
		return nil
	}

	out := make(chan bool, 1)
	go func() { out <- a.confirmHeldJob(context.Background(), "myjob", heldJob()) }()

	posted := <-sig.posted
	broker.Resolve(corrFromBlocks(t, posted.Blocks), interaction.Decision{OptionID: "deny", UserID: "UADMIN"})

	if <-out {
		t.Fatal("denial should return false (do not run)")
	}
	if a.isJobConfirmed("myjob") {
		t.Fatal("denied job must not be marked confirmed")
	}
	if denyPersisted {
		t.Fatal("denied job must not be persisted as confirmed")
	}
}

func TestConfirmHeldJob_NoBrokerDoesNotRun(t *testing.T) {
	a := &Gateway{logger: discardLogger(), cfg: config.AccessConfig{AdminUser: "UADMIN"}}
	if a.confirmHeldJob(context.Background(), "myjob", heldJob()) {
		t.Fatal("with no broker wired, a held job must not be confirmed")
	}
}

// corrFromBlocks parses the correlation id out of the posted prompt's first
// button (action_id = "murtaugh_interaction:<corr>:<idx>"). The buttons sit
// inside the approval card's container child_blocks, a block type newer than the
// pinned slack-go, so the payload is walked as raw JSON rather than decoded into
// typed blocks — and the walk finds the button wherever the card puts it.
func corrFromBlocks(t *testing.T, raw []byte) string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("posted blocks are not valid JSON: %v", err)
	}
	actionID, ok := firstActionID(doc)
	if !ok {
		t.Fatalf("no broker button found in posted blocks:\n%s", raw)
	}
	parts := strings.Split(actionID, ":")
	if len(parts) < 3 {
		t.Fatalf("action_id %q is not a broker button", actionID)
	}
	return parts[1]
}

// firstActionID returns the first action_id found anywhere in the decoded JSON.
// Both buttons of a prompt carry the same correlation id, so which one the walk
// reaches first does not matter.
func firstActionID(node any) (string, bool) {
	switch n := node.(type) {
	case map[string]any:
		if id, ok := n["action_id"].(string); ok && id != "" {
			return id, true
		}
		for _, child := range n {
			if id, ok := firstActionID(child); ok {
				return id, true
			}
		}
	case []any:
		for _, child := range n {
			if id, ok := firstActionID(child); ok {
				return id, true
			}
		}
	}
	return "", false
}

// The hold is an approval, and it must look like every other approval Murtaugh
// asks for: one container card, not the loose section-and-buttons prompt the
// broker falls back to. The subtitle has to name the job, because the admin sees
// this in a DM with no thread around it to say what it is about.
func TestConfirmHeldJob_RendersTheApprovalCard(t *testing.T) {
	a, sig, broker := newConfirmGateway(t)

	out := make(chan bool, 1)
	go func() { out <- a.confirmHeldJob(context.Background(), "myjob", heldJob()) }()

	posted := <-sig.posted
	card := containerCard(t, posted.Blocks)
	if card.Type != "container" {
		t.Fatalf("posted a %q block, want the approval card's container", card.Type)
	}
	if card.BlockID != approvalcard.ContainerBlockID {
		t.Errorf("container block_id = %q, want %q", card.BlockID, approvalcard.ContainerBlockID)
	}
	if !strings.Contains(card.Subtitle.Text, "'myjob'") {
		t.Errorf("subtitle = %q, want it to name the job", card.Subtitle.Text)
	}
	// The command, the schedule note and the buttons: what the admin needs to
	// decide, and the thing that makes the card clickable.
	if !strings.Contains(string(posted.Blocks), "/bin/echo hi") {
		t.Errorf("card does not show the command it is asking about:\n%s", posted.Blocks)
	}
	if !strings.Contains(string(posted.Blocks), "every 1h") {
		t.Errorf("card does not show the schedule being approved:\n%s", posted.Blocks)
	}
	// A push notification is the least private surface Slack has: it may name
	// the job, never the command.
	if strings.Contains(posted.Text, "/bin/echo") {
		t.Errorf("notification text leaks the command: %q", posted.Text)
	}

	broker.Resolve(corrFromBlocks(t, posted.Blocks), interaction.Decision{OptionID: "approve", UserID: "UADMIN"})
	if !<-out {
		t.Fatal("approval should return true (run the job)")
	}

	// The settled card replaces the prompt and stays put: the confirmation is
	// persisted and the job runs unattended from here on, so who allowed it is
	// worth keeping in the DM.
	if len(sig.Updated) != 1 {
		t.Fatalf("settled prompt was updated %d times, want 1", len(sig.Updated))
	}
	settled := containerCard(t, sig.Updated[0].Blocks)
	if !settled.IsCollapsible {
		t.Error("a settled card must be collapsible")
	}
	if _, ok := firstActionID(rawJSON(t, sig.Updated[0].Blocks)); ok {
		t.Error("settled card still carries a clickable button")
	}
	if !strings.Contains(string(sig.Updated[0].Blocks), "<@UADMIN>") {
		t.Errorf("settled card does not name the decider:\n%s", sig.Updated[0].Blocks)
	}
	if len(sig.Deleted) != 0 {
		t.Error("the settled first-run card must not be swept from the DM")
	}
}

// cardBlock is the approval card decoded far enough to assert on it.
type cardBlock struct {
	Type          string `json:"type"`
	BlockID       string `json:"block_id"`
	IsCollapsible bool   `json:"is_collapsible"`
	Subtitle      struct {
		Text string `json:"text"`
	} `json:"subtitle"`
}

func containerCard(t *testing.T, raw []byte) cardBlock {
	t.Helper()
	var doc struct {
		Blocks []cardBlock `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode posted blocks: %v\n%s", err, raw)
	}
	if len(doc.Blocks) != 1 {
		t.Fatalf("want exactly one top-level block, got %d:\n%s", len(doc.Blocks), raw)
	}
	return doc.Blocks[0]
}

func rawJSON(t *testing.T, raw []byte) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode blocks: %v", err)
	}
	return doc
}
