package nodehost_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	configstore "github.com/miere/murtaugh/internal/config/store"
)

func TestASignInAskedForInAConversationRunsOnItsPinnedNode(t *testing.T) {
	ctx := context.Background()
	pins, err := configstore.OpenConversationPins(ctx, config.DatabaseConfig{Backend: config.BackendSQLite,
		SQLite: config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")}}, "", "")
	if err != nil {
		t.Fatalf("open pins: %v", err)
	}
	t.Cleanup(func() { _ = pins.Close() })
	for channel, node := range map[string]string{"C1": "node-1", "C2": "node-gone"} {
		if err := pins.Put(ctx, config.ConversationPin{Conversation: config.ConversationRef{TeamID: "T1", ChannelID: channel, ThreadTS: "171.1"},
			NodeID: node, UserID: nodeOwner, ElectedAt: time.Now()}); err != nil {
			t.Fatalf("pin: %v", err)
		}
	}
	var asked atomic.Int32
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), pinning(pins),
		renewing(func(context.Context) (agentwire.CredentialRenewal, error) {
			asked.Add(1)
			return agentwire.CredentialRenewal{Status: string(agentruntime.RenewalStarted)}, nil
		}))

	if _, err := rig.pinnedNode(ctx, agent.ConversationKey{TeamID: "T1", ChannelID: "C1"}); !errors.Is(err, agentruntime.ErrNotPinned) {
		t.Fatalf("the channel outside the pinned thread was answered %v", err)
	}
	node, err := rig.pinnedNode(ctx, agent.ConversationKey{TeamID: "T1", ChannelID: "C1", ThreadTS: "171.1"})
	if err != nil || node.NodeID != "node-1" || node.Owner != nodeOwner {
		t.Fatalf("the pinned node is %+v (%v), want node-1 owned by %s", node, err, nodeOwner)
	}
	status, err := rig.renew(ctx, node.NodeID)
	if err != nil || status != agentruntime.RenewalStarted || asked.Load() != 1 {
		t.Fatalf("the node answered %q (%v) after being asked %d times", status, err, asked.Load())
	}

	if _, err := rig.pinnedNode(ctx, agent.ConversationKey{TeamID: "T1", ChannelID: "C9"}); !errors.Is(err, agentruntime.ErrNotPinned) {
		t.Fatalf("a conversation with no pin was answered %v", err)
	}
	if _, err := rig.pinnedNode(ctx, agent.ConversationKey{TeamID: "T1", ChannelID: "C2", ThreadTS: "171.1"}); err == nil || !strings.Contains(err.Error(), "node-gone") {
		t.Fatalf("a conversation pinned to a node that is gone was answered %v", err)
	}
	if nodes := rig.nodes(); len(nodes) != 1 || nodes[0].NodeID != "node-1" || nodes[0].Owner != nodeOwner {
		t.Fatalf("the gateway lists %+v as connected", nodes)
	}
	if asked.Load() != 1 {
		t.Fatalf("the node was asked %d times; only the pinned conversation may reach it", asked.Load())
	}
}
