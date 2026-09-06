package agent

import "testing"

func TestDeriveSessionIDDeterministicAndDistinct(t *testing.T) {
	a := SessionMetadata{TeamID: "T1", ChannelID: "C1", ThreadTS: "111.1"}

	// Deterministic: same conversation → same id (the stateless premise).
	if DeriveSessionID(a) != DeriveSessionID(a) {
		t.Fatal("derivation is not deterministic")
	}
	id := DeriveSessionID(a)
	if len(id) != 36 || id[14] != '5' { // v5 UUID
		t.Fatalf("expected a v5 UUID, got %q", id)
	}

	// Distinct conversations must not collide.
	for _, other := range []SessionMetadata{
		{TeamID: "T1", ChannelID: "C1", ThreadTS: "222.2"},
		{TeamID: "T1", ChannelID: "C2", ThreadTS: "111.1"},
		{TeamID: "T2", ChannelID: "C1", ThreadTS: "111.1"},
	} {
		if DeriveSessionID(a) == DeriveSessionID(other) {
			t.Fatalf("distinct conversation collided: %+v", other)
		}
	}
}

// TestDeriveSessionIDEphemeralIsUniquePerCall covers the one case where
// determinism is the bug. An ephemeral session has no conversation, so the
// triple is empty and every caller would otherwise share a single derived id —
// which a --resume backend turns into "replay the last run's transcript".
func TestDeriveSessionIDEphemeralIsUniquePerCall(t *testing.T) {
	meta := SessionMetadata{Source: "delegate", Ephemeral: true}

	first, second := DeriveSessionID(meta), DeriveSessionID(meta)
	if first == second {
		t.Fatalf("ephemeral sessions shared an id: %q", first)
	}
	if len(first) != 36 || first[14] != '4' { // v4 UUID (random)
		t.Fatalf("expected a v4 UUID, got %q", first)
	}

	// It must not collide with the id the empty conversation used to derive —
	// that id may still name a transcript on disk from before this fix.
	if empty := DeriveSessionID(SessionMetadata{}); first == empty || second == empty {
		t.Fatal("ephemeral id collided with the derived empty-conversation id")
	}
}
