package claudecode

import (
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

func TestDeriveSessionIDDeterministicAndDistinct(t *testing.T) {
	a := agent.SessionMetadata{TeamID: "T1", ChannelID: "C1", ThreadTS: "111.1"}

	// Deterministic: same conversation → same id (the whole stateless premise).
	if deriveSessionID(a) != deriveSessionID(a) {
		t.Fatal("derivation is not deterministic")
	}

	id := deriveSessionID(a)
	if len(id) != 36 {
		t.Fatalf("not a UUID: %q", id)
	}
	if id[14] != '5' { // version nibble
		t.Fatalf("expected a v5 UUID, got version %c in %q", id[14], id)
	}

	// Distinct conversations must not collide.
	diffThread := agent.SessionMetadata{TeamID: "T1", ChannelID: "C1", ThreadTS: "222.2"}
	diffChannel := agent.SessionMetadata{TeamID: "T1", ChannelID: "C2", ThreadTS: "111.1"}
	diffTeam := agent.SessionMetadata{TeamID: "T2", ChannelID: "C1", ThreadTS: "111.1"}
	for _, other := range []agent.SessionMetadata{diffThread, diffChannel, diffTeam} {
		if deriveSessionID(a) == deriveSessionID(other) {
			t.Fatalf("distinct conversation collided: %+v", other)
		}
	}
}
