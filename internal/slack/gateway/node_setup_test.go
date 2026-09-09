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

// The gate this file replaces is the one every other route into the setup form
// uses: only the gateway administrator. #170 Change I adds a second entitlement
// — the owner of a node that just attached with nothing configured — and the
// question is entirely about who may reach the form and what it configures when
// they do.

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

// The administrator configures the gateway: its own store, its own config dir,
// the tweaker bound to them. Unchanged.
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

// Somebody with no node and no admin rights is refused, exactly as before. This
// is the property the new entitlement must not weaken.
func TestAUserWithNothingToConfigureIsRefused(t *testing.T) {
	gw := nodeSetupGateway(t, "U0ADMIN01")
	if _, ok := gw.setupSubjectFor("U0STRNGR1"); ok {
		t.Fatal("a user with no node and no admin rights reached the setup form")
	}
	if _, ok := gw.setupSubjectFor(""); ok {
		t.Fatal("an empty user id reached the setup form")
	}
}

// A node owner is entitled while their node is waiting, and what they configure
// is their NODE — not the gateway.
//
// The config dir is deliberately empty: the tweaker profile is rooted where the
// configuration lives, that directory is on the node's machine, and only the
// node can fill it in. Handing over the GATEWAY's config dir would root a node's
// unsandboxed profile at a path that does not exist on it.
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

// An admin who ALSO owns an unconfigured node configures the node: what the
// node's card offers must be the node, or the one person most likely to own both
// would silently reconfigure the wrong half.
//
// The node branch is checked BEFORE the admin branch, which is what makes
// withdrawing an invitation load-bearing rather than tidy — see
// TestAnAdminGetsTheirGatewayBackWhenTheirNodeSettles.
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

// TestTwoUnconfiguredNodesGetOneCard is the DM storm.
//
// pendingNodes held one node id per user and called an invitation fresh whenever
// the stored id differed, so two unconfigured nodes overwrote each other: a
// laptop and a desktop, both freshly installed, both redialling on a
// seconds-scale backoff, produce a card on very nearly every attach. That is the
// "trains the admin to ignore it" failure #170 states for disconnects, landing
// in the one conversation that most needs to be read.
//
// The entitlement still follows the newest node — a submission configures the
// one whose card is most likely in front of the operator — but the CARD is once.
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
	// And the entitlement is still live, pointing at the node that attached last.
	if nodeID, ok := gw.pendingNodes.get("U0OWNER01"); !ok || nodeID != "node-B" {
		t.Errorf("the invitation names %q (present=%v), want the most recent node", nodeID, ok)
	}
}

// TestOneNodeRedialingGetsOneCard is the same rule for the ordinary case: a
// single node whose owner is not at their desk keeps attaching all day, and the
// card already sitting in their DM still opens the form.
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

// TestAnAdminGetsTheirGatewayBackWhenTheirNodeSettles is the withdrawal.
//
// The invitation used to end in exactly one way — a completed form — so a node
// configured in a terminal, or simply unplugged, left its owner routed at a node
// id for the life of the process. For an administrator that is the expensive
// case: setupSubjectFor checks the node branch first, so their own gateway's
// setup form becomes unreachable, and there is no App Home route into it.
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

// TestWithdrawingOneNodeLeavesTheOtherInvitationAlone is why withdraw checks the
// node id. A user with two unconfigured nodes has the newest one recorded;
// news about the other must not cancel an entitlement that is still true.
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

// TestAWithdrawnNodeIsOfferedAgainOnItsNextAttach keeps the withdrawal from
// being a way to silence a node permanently: an unconfigured node that
// disconnects and comes back is still unconfigured, and its owner has not been
// told.
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

// nodeCardGateway is nodeSetupGateway with the Slack surface a card actually
// needs, so a test can count posts rather than trusting the gate in front of
// them.
func nodeCardGateway(t *testing.T, api *recordingCardAPI) *Gateway {
	t.Helper()
	gw := nodeSetupGateway(t, "U0ADMIN01")
	gw.alertAPI = api
	gw.alertEditor = api
	gw.messaging = stubMessaging{}
	gw.agentCards = agentcard.NewRenderer(t.TempDir(), assets.FS)
	return gw
}
