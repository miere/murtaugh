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
