package agentwire

import (
	"go/build"
	"slices"
	"strings"
	"testing"
)

// TestPackageImportsAreWhatTheDocSays pins the sentence this package's doc makes
// its placement argument with: "it imports internal/agent and
// internal/providerfail and nothing else of ours".
//
// A prose import list is the claim that rots with nobody noticing. This one
// named internal/llm — and went on naming it after the import was cut — while
// the whole point of the list is that a reader checks the gateway's "cannot run
// an agent" rule against it. The CI reachability guard covers the forbidden
// packages and not this: a new import of some third ordinary package of ours
// would pass CI and silently make the doc false again.
//
// Direct imports settle it, the way internal/nodelink's own guard does: no
// package of ours is reachable through a third-party dependency, so a transitive
// Murtaugh import requires a direct one. Test files are excluded by
// build.ImportDir, which is correct — roundtrip_test.go reaches for internal/llm
// deliberately, to prove a classified provider failure survives the wire.
func TestPackageImportsAreWhatTheDocSays(t *testing.T) {
	const prefix = "github.com/miere/murtaugh/"
	// Sorted, because the comparison below is on sorted output.
	want := []string{"internal/agent", "internal/providerfail"}

	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("read package: %v", err)
	}
	var got []string
	for _, imported := range pkg.Imports {
		if rel, ok := strings.CutPrefix(imported, prefix); ok {
			got = append(got, rel)
		}
	}
	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Fatalf("agentwire imports %v of ours; doc.go says %v — correct whichever of the two is wrong", got, want)
	}
}
