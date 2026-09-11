package gateway

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/onboarding"
	"github.com/miere/murtaugh/internal/slack/agentcard"
)

func nodeSetupLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func nodeSetupGateway(t *testing.T, admin string) *Gateway {
	t.Helper()
	gw := &Gateway{logger: nodeSetupLogger(), configDir: "/etc/murtaugh"}
	gw.cfg = config.AccessConfig{AdminUser: admin}
	gw.WithNodeProfileWriter(func(context.Context, string, onboarding.Profiles) error { return nil })
	return gw
}

func TestTheAdministratorStillConfiguresTheGatewayItself(t *testing.T) {
	gw := nodeSetupGateway(t, "U0ADMIN01")

	subject, ok := gw.setupSubjectFor("U0ADMIN01")
	if !ok {
		t.Fatal("the administrator was refused the setup form")
	}
	if subject.isNode() {
		t.Errorf("the administrator's form targets node %q; it should target the gateway", subject.nodeID)
	}
	if subject.configDir != "/etc/murtaugh" {
		t.Errorf("the tweaker would be rooted at %q, want the gateway's config dir", subject.configDir)
	}
	if subject.user != "U0ADMIN01" {
		t.Errorf("the tweaker would be bound to %q, want the administrator", subject.user)
	}
}

func TestAUserWithNothingToConfigureIsRefused(t *testing.T) {
	gw := nodeSetupGateway(t, "U0ADMIN01")
	if _, ok := gw.setupSubjectFor("U0STRNGR1"); ok {
		t.Fatal("a user with no node and no admin rights reached the setup form")
	}
	if _, ok := gw.setupSubjectFor(""); ok {
		t.Fatal("an empty user id reached the setup form")
	}
}

// The config dir is empty because a node's configuration lives on its own machine,
// where the gateway's config dir does not exist.
func TestANodeOwnerConfiguresTheirNodeAndNotTheGateway(t *testing.T) {
	gw := nodeSetupGateway(t, "U0ADMIN01")
	gw.pendingNodes.offer("U0OWNER01", "node-7")

	subject, ok := gw.setupSubjectFor("U0OWNER01")
	if !ok {
		t.Fatal("the owner of an unconfigured node was refused the setup form")
	}
	if subject.nodeID != "node-7" {
		t.Errorf("the form targets node %q, want node-7", subject.nodeID)
	}
	if subject.user != "U0OWNER01" {
		t.Errorf("the tweaker would be bound to %q, want the node's owner", subject.user)
	}
	if subject.configDir != "" {
		t.Errorf("the node's tweaker would be rooted at the gateway's %q; only the node knows its own", subject.configDir)
	}
}

// A completed form consumes the invitation, so a stale card left in a DM cannot
// be clicked into a second configuration.
func TestConfiguringANodeConsumesItsInvitation(t *testing.T) {
	gw := nodeSetupGateway(t, "U0ADMIN01")
	gw.pendingNodes.offer("U0OWNER01", "node-7")
	gw.pendingNodes.clear("U0OWNER01")

	if _, ok := gw.setupSubjectFor("U0OWNER01"); ok {
		t.Fatal("a node owner could still reach the form after configuring their node")
	}
}

// A build with no way to write a node's profiles must not entitle anybody:
// offering a form that cannot be applied is worse than saying nothing.
func TestANodeOwnerIsNotEntitledWithoutAWriter(t *testing.T) {
	gw := &Gateway{logger: nodeSetupLogger()}
	gw.cfg = config.AccessConfig{AdminUser: "U0ADMIN01"}
	gw.pendingNodes.offer("U0OWNER01", "node-7")

	if _, ok := gw.setupSubjectFor("U0OWNER01"); ok {
		t.Fatal("a node owner was entitled on a build that cannot configure a node")
	}
}

// The admin is the person most likely to own both, and the node's card must never
// silently configure the gateway instead.
func TestAnAdminWhoOwnsAnUnconfiguredNodeConfiguresTheNode(t *testing.T) {
	gw := nodeSetupGateway(t, "U0ADMIN01")
	gw.pendingNodes.offer("U0ADMIN01", "node-7")

	subject, ok := gw.setupSubjectFor("U0ADMIN01")
	if !ok {
		t.Fatal("the administrator was refused")
	}
	if !subject.isNode() || subject.nodeID != "node-7" {
		t.Errorf("the form targets %+v, want the waiting node", subject)
	}
}

// Two nodes redialling on a fast backoff would otherwise DM a card on nearly every
// attach, which teaches the owner to ignore it.
func TestTwoUnconfiguredNodesGetOneCard(t *testing.T) {
	api := &recordingCardAPI{}
	gw := nodeCardGateway(t, api)

	for i := range 20 {
		nodeID := "node-A"
		if i%2 == 1 {
			nodeID = "node-B"
		}
		gw.OfferNodeSetup(context.Background(), nodeID, "U0OWNER01")
	}

	posts, _ := api.snapshot()
	if len(posts) != 1 {
		t.Fatalf("%d setup cards posted over 20 alternating attaches of two unconfigured nodes, want 1", len(posts))
	}
	if nodeID, ok := gw.pendingNodes.get("U0OWNER01"); !ok || nodeID != "node-B" {
		t.Errorf("the invitation names %q (present=%v), want the most recent node", nodeID, ok)
	}
}

// An owner away from their desk would otherwise get a new card on every redial.
func TestOneNodeRedialingGetsOneCard(t *testing.T) {
	api := &recordingCardAPI{}
	gw := nodeCardGateway(t, api)

	for range 5 {
		gw.OfferNodeSetup(context.Background(), "node-7", "U0OWNER01")
	}
	if posts, _ := api.snapshot(); len(posts) != 1 {
		t.Fatalf("%d setup cards posted over 5 attaches of one node, want 1", len(posts))
	}
}

// Without withdrawal, an admin whose node was configured by hand or unplugged could
// never reach their own gateway's setup form again.
func TestAnAdminGetsTheirGatewayBackWhenTheirNodeSettles(t *testing.T) {
	gw := nodeSetupGateway(t, "U0ADMIN01")
	gw.pendingNodes.offer("U0ADMIN01", "node-7")

	gw.WithdrawNodeSetup("node-7", "U0ADMIN01")

	subject, ok := gw.setupSubjectFor("U0ADMIN01")
	if !ok {
		t.Fatal("the administrator was refused the setup form entirely")
	}
	if subject.isNode() {
		t.Errorf("the administrator is still routed at node %q after it stopped needing the form", subject.nodeID)
	}
	if subject.configDir != "/etc/murtaugh" {
		t.Errorf("the tweaker would be rooted at %q, want the gateway's config dir", subject.configDir)
	}
}

// Only the newest of a user's unconfigured nodes is recorded, so news about the
// other must not cancel an entitlement that is still true.
func TestWithdrawingOneNodeLeavesTheOtherInvitationAlone(t *testing.T) {
	gw := nodeSetupGateway(t, "U0ADMIN01")
	gw.pendingNodes.offer("U0OWNER01", "node-A")
	gw.pendingNodes.offer("U0OWNER01", "node-B")

	gw.WithdrawNodeSetup("node-A", "U0OWNER01")

	subject, ok := gw.setupSubjectFor("U0OWNER01")
	if !ok || subject.nodeID != "node-B" {
		t.Fatalf("the owner's live invitation for node-B was cancelled by news about node-A: %+v ok=%v", subject, ok)
	}
}

// Withdrawal must not silence a node for good: one that reconnects is still
// unconfigured and its owner has not been told.
func TestAWithdrawnNodeIsOfferedAgainOnItsNextAttach(t *testing.T) {
	api := &recordingCardAPI{}
	gw := nodeCardGateway(t, api)

	gw.OfferNodeSetup(context.Background(), "node-7", "U0OWNER01")
	gw.WithdrawNodeSetup("node-7", "U0OWNER01")
	gw.OfferNodeSetup(context.Background(), "node-7", "U0OWNER01")

	if posts, _ := api.snapshot(); len(posts) != 2 {
		t.Fatalf("%d setup cards posted, want 2: a node that dropped and came back is still unconfigured", len(posts))
	}
}

func nodeCardGateway(t *testing.T, api *recordingCardAPI) *Gateway {
	t.Helper()
	gw := nodeSetupGateway(t, "U0ADMIN01")
	gw.alertAPI = api
	gw.alertEditor = api
	gw.messaging = stubMessaging{}
	gw.agentCards = agentcard.NewRenderer(t.TempDir(), assets.FS)
	return gw
}
