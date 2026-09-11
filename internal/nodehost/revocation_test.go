package nodehost_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/journal"
)

const recheckEvery = 20 * time.Millisecond

// The CLI revokes from another process, so only the gateway's re-check can
// close the connection; before it, a revoked node stayed attached for good.
func TestRevokingInTheStoreClosesTheLiveConnection(t *testing.T) {
	logs := &gatewayLog{}
	rec := &recordingJournal{}
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}),
		rechecking(recheckEvery), logging(logs), journalling(rec))

	if _, _, err := rig.store.Revoke(context.Background(), rig.selector, time.Now()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	waitForStop(t, rig.nodeStopped, "the revoked node's connection to close")
	waitFor(t, "the node to leave the registry", func() bool {
		_, ok := rig.host.Attached()
		return !ok
	})

	line := logs.String()
	for _, want := range []string{"level=WARN", "node_id=node-1", "selector=" + rig.selector} {
		if !strings.Contains(line, want) {
			t.Fatalf("the drop was not logged with %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, rig.token) {
		t.Fatalf("the drop logged the token itself:\n%s", line)
	}
	revoked := rec.find("revoked")
	if revoked.Level != journal.LevelWarn || revoked.Payload["selector"] != rig.selector {
		t.Fatalf("the drop was journalled as %+v, want a warning naming selector %q", revoked, rig.selector)
	}
}

func TestAnExpiredOrVanishedCredentialLosesItsLiveConnection(t *testing.T) {
	for name, tc := range map[string]struct {
		state  string
		change func(store *memTokens, selector string)
	}{
		"expired": {state: "expired", change: func(store *memTokens, selector string) {
			store.mu.Lock()
			defer store.mu.Unlock()
			record := store.records[selector]
			record.ExpiresAt = time.Now().Add(-time.Second)
			store.records[selector] = record
		}},
		"gone": {state: "credential_gone", change: func(store *memTokens, selector string) {
			store.mu.Lock()
			defer store.mu.Unlock()
			delete(store.records, selector)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recordingJournal{}
			rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), rechecking(recheckEvery), journalling(rec))

			tc.change(rig.store, rig.selector)

			waitForStop(t, rig.nodeStopped, "the connection to close")
			waitFor(t, "the drop to be journalled as "+tc.state, func() bool { return rec.has(tc.state) })
		})
	}
}

// Mid-rotation the node holds two credentials; revoking the old one must not
// take down the new one.
func TestTheRecheckLeavesARotatingNodesOtherCredentialUp(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), rechecking(recheckEvery))
	rotated := attachScripted(t, rig, "node-1", newScriptedAgent(func(*scriptedTurn) {}))

	if _, _, err := rig.store.Revoke(context.Background(), rig.selector, time.Now()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	waitForStop(t, rig.nodeStopped, "the retired credential's connection to close")
	waitForRechecks(t, rig.store, 3)

	if isStopped(rotated.stopped) {
		t.Fatal("revoking the retired credential closed the node's new one")
	}
	nodes := rig.host.Nodes()
	if len(nodes) != 1 || nodes[0].Selector != rotated.selector {
		t.Fatalf("after the retired credential dropped the registry holds %+v, want the new credential alone", nodes)
	}
}

// A database hiccup is not a revocation; dropping on it would take the whole
// fleet down with the database.
func TestAStoreFailureDuringTheRecheckClosesNothing(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), rechecking(recheckEvery))

	rig.store.fail(errors.New("connection refused"))
	waitForRechecks(t, rig.store, 3)
	if isStopped(rig.nodeStopped) {
		t.Fatal("a store outage closed a live connection")
	}
	if _, ok := rig.host.Attached(); !ok {
		t.Fatal("a store outage removed the node from the registry")
	}

	rig.store.fail(nil)
	if _, _, err := rig.store.Revoke(context.Background(), rig.selector, time.Now()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	waitForStop(t, rig.nodeStopped, "the re-check to resume once the store answered")
}

func (m *memTokens) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failWith = err
}

func (m *memTokens) lookupCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lookups
}

func waitForRechecks(t *testing.T, store *memTokens, n int) {
	t.Helper()
	target := store.lookupCount() + n
	waitFor(t, "the gateway to re-check the store", func() bool { return store.lookupCount() >= target })
}

func waitForStop(t *testing.T, stopped <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func isStopped(stopped <-chan struct{}) bool {
	select {
	case <-stopped:
		return true
	default:
		return false
	}
}
