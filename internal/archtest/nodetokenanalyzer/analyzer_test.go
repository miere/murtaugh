package nodetokenanalyzer_test

import (
	"testing"

	"github.com/miere/murtaugh/internal/archtest/nodetokenanalyzer"
	"golang.org/x/tools/go/analysis/analysistest"
)

// TestAnalyzer runs the guard over the `creds` fixture: a digest compared with
// ==, with !=, through a conversion, as a struct field, through an inferred
// local, and through each byte-wise standard-library helper is flagged; the
// crypto/subtle spelling is not.
//
// The fixture also pins the two stated LIMITS — a digest laundered through an
// intermediate variable, and a map keyed by Digest — plus the scoping claim that
// an ordinary non-secret identifier may be compared freely. analysistest fails
// on an unexpected diagnostic, so each of those absences is an assertion about
// the boundary rather than an oversight.
func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), nodetokenanalyzer.Analyzer, "creds")
}

// TestAnalyzerReportsItsOwnDecay covers the half a rule keyed off a NAME
// otherwise loses: rename Digest and the guard becomes a no-op that still passes
// CI. The fixture sits at the one package path the type is required to exist at
// and does not declare it, so the pass reports that instead of silently finding
// nothing.
func TestAnalyzerReportsItsOwnDecay(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), nodetokenanalyzer.Analyzer, "murtaugh/internal/nodetoken")
}
