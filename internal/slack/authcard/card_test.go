package authcard

import (
	"context"
	"strings"
	"testing"
	"time"
)

const ownerID = "UOWNER"

func showing() Showing {
	return Showing{
		ToolName:        "gcp-mcp",
		ProfileName:     "gcloud",
		URL:             "https://accounts.example.com/o/oauth2?x=1",
		NeedsCode:       true,
		Requester:       Destination{ChannelID: "C1", ThreadTS: "100.1"},
		RequesterUserID: requesterID,
		Recipient:       ownerID,
	}
}

func ownerFlow(api *syncAPI, allowed *bool) *Flow {
	f := newTestFlow(api)
	f.SetAuthorised(func(id string) bool { return *allowed && (id == ownerID || id == adminID) })
	return f
}

func awaitReply(t *testing.T, c *Card) Reply {
	t.Helper()
	select {
	case r := <-c.Replies():
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no reply arrived")
		return Reply{}
	}
}

// A sign-in drawn for a node goes to the node's owner by DM, with the notice in
// the turn's own thread; the gateway admin is not involved.
func TestShowSendsTheCardToItsRecipientNotTheAdmin(t *testing.T) {
	api := newSyncAPI()
	allowed := true
	card, err := ownerFlow(api, &allowed).Show(context.Background(), showing())
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	defer card.Settle(StateCancelled, "")

	posts, _, _ := api.snapshot()
	if len(posts) != 2 {
		t.Fatalf("expected a thread notice and a DM card, got %d posts", len(posts))
	}
	if posts[0].ChannelID != "C1" || posts[0].ThreadTS != "100.1" {
		t.Fatalf("the notice went to %q/%q, want the turn's thread", posts[0].ChannelID, posts[0].ThreadTS)
	}
	if !strings.Contains(string(posts[0].Blocks), ownerID) {
		t.Fatalf("the notice does not say who was sent the DM:\n%s", posts[0].Blocks)
	}
	if posts[1].ChannelID != "D-"+ownerID {
		t.Fatalf("the sign-in card went to %q, want the owner's DM", posts[1].ChannelID)
	}
	if !strings.Contains(string(posts[1].Blocks), "https://accounts.example.com/o/oauth2?x=1") {
		t.Fatalf("the card does not carry the node's sign-in link:\n%s", posts[1].Blocks)
	}
}

// An owner who may not use the gateway is not sent a card at all.
func TestShowRefusesARecipientWhoMayNotUseTheGateway(t *testing.T) {
	api := newSyncAPI()
	allowed := false
	if _, err := ownerFlow(api, &allowed).Show(context.Background(), showing()); err == nil {
		t.Fatal("a sign-in was drawn for somebody who may not use this gateway")
	}
	if posts, _, _ := api.snapshot(); len(posts) != 0 {
		t.Fatalf("a refused sign-in still posted %d messages", len(posts))
	}
}

// The owner's code comes back as a reply for the node, and neither the admin
// nor the requester can answer in their place.
func TestOnlyTheRecipientCanAnswerTheCard(t *testing.T) {
	api := newSyncAPI()
	allowed := true
	f := ownerFlow(api, &allowed)
	card, err := f.Show(context.Background(), showing())
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	defer card.Settle(StateCancelled, "")
	posts, _, _ := api.snapshot()
	corr := corrOf(t, posts[1].Blocks)

	for _, other := range []string{adminID, requesterID} {
		if err := f.HandleClick(context.Background(), corr, ActionDeny, other, "t"); err == nil {
			t.Fatalf("%s denied a sign-in sent to %s", other, ownerID)
		}
		if err := f.HandleCodeSubmission(corr, "stolen", other); err == nil {
			t.Fatalf("%s submitted a code for a sign-in sent to %s", other, ownerID)
		}
	}

	if err := f.HandleClick(context.Background(), corr, ActionPrimary, ownerID, "t"); err != nil {
		t.Fatalf("owner click: %v", err)
	}
	if _, _, views := api.snapshot(); len(views) != 1 {
		t.Fatalf("the code modal was opened %d times, want once", len(views))
	}
	if err := f.HandleCodeSubmission(corr, " 4/0Ab-code ", ownerID); err != nil {
		t.Fatalf("owner submit: %v", err)
	}
	if r := awaitReply(t, card); r.Kind != ReplyCode || r.Code != "4/0Ab-code" || r.UserID != ownerID {
		t.Fatalf("the owner's code came back as %+v", r)
	}
}

// Access withdrawn while the card is open wins: the submission is refused, the
// code goes nowhere, and whoever runs the sign-in is told to stop.
func TestARecipientWhoLostAccessIsRefusedAtSubmit(t *testing.T) {
	api := newSyncAPI()
	allowed := true
	f := ownerFlow(api, &allowed)
	card, err := f.Show(context.Background(), showing())
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	defer card.Settle(StateCancelled, "")
	posts, _, _ := api.snapshot()
	corr := corrOf(t, posts[1].Blocks)

	allowed = false
	if err := f.HandleCodeSubmission(corr, "4/0Ab-code", ownerID); err == nil {
		t.Fatal("a code from somebody who lost access was accepted")
	}
	r := awaitReply(t, card)
	if r.Kind != ReplyRefused || r.Code != "" {
		t.Fatalf("a refused submission came back as %+v", r)
	}
	if r.Reason != RefusedReason || r.UserID != ownerID {
		t.Fatalf("the refusal came back as %+v", r)
	}
}

// Settling closes both cards, and a click after that finds nothing to answer.
func TestSettleClosesBothCards(t *testing.T) {
	api := newSyncAPI()
	allowed := true
	f := ownerFlow(api, &allowed)
	card, err := f.Show(context.Background(), showing())
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	posts, _, _ := api.snapshot()
	corr := corrOf(t, posts[1].Blocks)

	card.Settle(StateSuccess, "")
	_, updates, _ := api.snapshot()
	channels := map[string]bool{}
	for _, u := range updates {
		channels[u.ChannelID] = true
	}
	if !channels["C1"] || !channels["D-"+ownerID] {
		t.Fatalf("settling updated %v, want both the thread notice and the DM card", channels)
	}
	if err := f.HandleClick(context.Background(), corr, ActionDeny, ownerID, "t"); err == nil {
		t.Fatal("a settled card still took a click")
	}
}

func approvalShowing() Showing {
	s := showing()
	s.URL = ""
	s.Command = `vendor-cli login --headless && touch "$HOME/.vendor"`
	return s
}

// A command the owner must approve is shown to them exactly, with nothing to
// open until they have approved it.
func TestAnApprovalCardShowsTheCommandAndNoLink(t *testing.T) {
	api := newSyncAPI()
	allowed := true
	card, err := ownerFlow(api, &allowed).Show(context.Background(), approvalShowing())
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	defer card.Settle(StateCancelled, "")
	posts, _, _ := api.snapshot()
	dm := string(posts[1].Blocks)
	if !strings.Contains(dm, `vendor-cli login --headless \u0026\u0026 touch \"$HOME/.vendor\"`) && !strings.Contains(dm, `vendor-cli login --headless && touch \"$HOME/.vendor\"`) {
		t.Fatalf("the card does not show the exact command:\n%s", dm)
	}
	var actions []Action
	for _, b := range buttons(t, posts[1].Blocks) {
		_, action, _ := ParseActionID(b.ActionID)
		actions = append(actions, action)
	}
	if len(actions) != 2 || actions[0] != ActionApprove || actions[1] != ActionDeny {
		t.Fatalf("the approval card offers %v, want approve then deny", actions)
	}
}

// Only the owner's approval starts the command, and only once; the link then
// arrives on the same card.
func TestOnlyTheOwnersApprovalStartsTheCommand(t *testing.T) {
	api := newSyncAPI()
	allowed := true
	f := ownerFlow(api, &allowed)
	card, err := f.Show(context.Background(), approvalShowing())
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	defer card.Settle(StateCancelled, "")
	posts, _, _ := api.snapshot()
	corr := corrOf(t, posts[1].Blocks)

	for _, other := range []string{adminID, requesterID} {
		if err := f.HandleClick(context.Background(), corr, ActionApprove, other, "t"); err == nil {
			t.Fatalf("%s approved a command sent to %s", other, ownerID)
		}
	}
	if err := f.HandleClick(context.Background(), corr, ActionPrimary, ownerID, "t"); err == nil {
		t.Fatal("a sign-in with no link yet took a click on its link")
	}
	if err := f.HandleClick(context.Background(), corr, ActionApprove, ownerID, "t"); err != nil {
		t.Fatalf("owner approve: %v", err)
	}
	if r := awaitReply(t, card); r.Kind != ReplyApproved || r.UserID != ownerID {
		t.Fatalf("the approval came back as %+v", r)
	}
	if err := f.HandleClick(context.Background(), corr, ActionApprove, ownerID, "t"); err == nil {
		t.Fatal("a command was approved twice")
	}

	card.Link(context.Background(), "https://vendor.example.com/device")
	_, updates, _ := api.snapshot()
	last := updates[len(updates)-1]
	if last.ChannelID != "D-"+ownerID || !strings.Contains(string(last.Blocks), "https://vendor.example.com/device") {
		t.Fatalf("the link did not reach the owner's card:\n%s", last.Blocks)
	}
	if err := f.HandleClick(context.Background(), corr, ActionPrimary, ownerID, "t"); err != nil {
		t.Fatalf("owner click on the link: %v", err)
	}
}

// Access withdrawn before the owner approves wins: nothing is approved.
func TestAnOwnerWhoLostAccessCannotApprove(t *testing.T) {
	api := newSyncAPI()
	allowed := true
	f := ownerFlow(api, &allowed)
	card, err := f.Show(context.Background(), approvalShowing())
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	defer card.Settle(StateCancelled, "")
	posts, _, _ := api.snapshot()
	allowed = false
	if err := f.HandleClick(context.Background(), corrOf(t, posts[1].Blocks), ActionApprove, ownerID, "t"); err == nil {
		t.Fatal("an owner who lost access approved a command")
	}
	if r := awaitReply(t, card); r.Kind != ReplyRefused {
		t.Fatalf("the refused approval came back as %+v", r)
	}
}

// The thread hears where the card went even when the requester is the owner,
// so their turn never looks stalled.
func TestTheThreadIsToldEvenWhenTheRequesterIsTheOwner(t *testing.T) {
	api := newSyncAPI()
	allowed := true
	s := showing()
	s.RequesterUserID = ownerID
	card, err := ownerFlow(api, &allowed).Show(context.Background(), s)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	defer card.Settle(StateCancelled, "")
	posts, _, _ := api.snapshot()
	if len(posts) != 2 || posts[0].ChannelID != "C1" || posts[1].ChannelID != "D-"+ownerID {
		t.Fatalf("posted %d messages; want the thread notice and the DM card", len(posts))
	}
}

func TestLosingAccessWithdrawsTheCardsAlreadyOpen(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	f.SetAuthorised(func(id string) bool { return id == ownerID })
	card, err := f.Show(context.Background(), showing())
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	defer card.Settle(StateCancelled, "")

	f.SetAuthorised(func(id string) bool { return id == adminID })
	if got := awaitReply(t, card); got.Kind != ReplyRefused || got.UserID != ownerID {
		t.Fatalf("an open card for someone who lost access got %+v", got)
	}
	if card.Allowed() {
		t.Fatal("the card still counts its recipient as allowed")
	}
}
