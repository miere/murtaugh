package pingcard

import (
	"strings"
	"testing"
)

// The package is a routing contract now that the button lives in the App Home
// control row rather than on a card of its own, so what is worth pinning is the
// contract itself: the ids are namespaced (the package doc's promise that a
// user-defined workflow rule can never shadow the self-test), and the copy the
// gateway renders is present.
func TestActionIDsAreNamespaced(t *testing.T) {
	for name, id := range map[string]string{"BlockID": BlockID, "ActionPing": ActionPing} {
		if !strings.HasPrefix(id, "murtaugh_") {
			t.Errorf("%s = %q: must carry the murtaugh_ namespace so no workflow rule can shadow it", name, id)
		}
	}
}

func TestCopyIsPresent(t *testing.T) {
	for name, text := range map[string]string{
		"ButtonLabel":  ButtonLabel,
		"PongTitle":    PongTitle,
		"PongSubtitle": PongSubtitle,
	} {
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s is empty; the self-test would render with no copy", name)
		}
	}
}
