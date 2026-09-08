package nodeserve

import (
	"context"
	"strings"
	"testing"
)

// An unbound proxy — no gateway attached — must answer, not vanish and not
// block. A tool that disappears between reconnects changes the model's tool list
// underneath it, and a tool that hangs parks the turn; an answer saying the node
// is disconnected is the only one a model can act on.
func TestAnUnboundProxyAnswersRatherThanVanishing(t *testing.T) {
	proxy := NewToolProxy(nil)
	note, err := proxy.Call(context.Background(), "ping", nil)
	if err != nil {
		t.Fatalf("an unbound proxy failed with an error rather than a note: %v", err)
	}
	if !strings.Contains(note, "not currently connected") {
		t.Fatalf("the note does not say why: %q", note)
	}
	// The certainty matters. Nothing was sent, so nothing ran, and telling a
	// model "it may have happened" here would make it needlessly cautious about
	// an action that definitely did not.
	if !strings.Contains(note, "Nothing happened") {
		t.Fatalf("the unbound note leaves the model unsure whether the action ran: %q", note)
	}
	if strings.Contains(note, "may or may not") {
		t.Fatalf("the unbound note borrowed the dropped-connection wording, which is a different and weaker claim: %q", note)
	}
}

// The two abort notes must stay distinguishable. They describe different states
// of the world — one certain, one unknowable — and collapsing them would either
// over-warn about a call that never left the node or under-warn about one that
// may have taken effect on the gateway.
//
// This pins the WORDING and nothing more. What stops a dropped call being
// repeated is the turn's context being cancelled before the note is produced
// (see droppedNote); the note matters for a caller whose context outlives the
// link. Reading these assertions as the enforcement would be reading them wrong.
func TestTheTwoAbortNotesSayDifferentThings(t *testing.T) {
	if droppedNote == unboundNote {
		t.Fatal("the dropped and unbound notes are the same string")
	}
	for _, clause := range []string{"Do not retry", "may or may not have taken effect"} {
		if !strings.Contains(droppedNote, clause) {
			t.Errorf("droppedNote omits %q; it is the clause that stops a model retrying a side-effecting tool", clause)
		}
	}
}

// A proxy with nothing published is still a usable registry: the node's runtime
// builder is handed it at construction, before any gateway has been asked.
func TestAFreshProxyHandsOverAnEmptyRegistry(t *testing.T) {
	proxy := NewToolProxy(nil)
	registry := proxy.Registry()
	if registry == nil {
		t.Fatal("a fresh proxy has no registry; the node's runtime cannot be built")
	}
	if n := len(registry.All()); n != 0 {
		t.Fatalf("a fresh proxy published %d tools before asking any gateway", n)
	}
}
