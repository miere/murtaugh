package agentwire

import (
	"go/build"
	"slices"
	"strings"
	"testing"
)

// A prose import list rots silently, and CI's reachability guard does not cover ordinary
// packages of ours, so this pins the one in doc.go.
func TestPackageImportsAreWhatTheDocSays(t *testing.T) {
	const prefix = "github.com/miere/murtaugh/"
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
