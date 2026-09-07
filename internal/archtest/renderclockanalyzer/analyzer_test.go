package renderclockanalyzer_test

import (
	"testing"

	"github.com/miere/murtaugh/internal/archtest/renderclockanalyzer"
	"golang.org/x/tools/go/analysis/analysistest"
)

// TestAnalyzer runs the guard over the `render` fixture: a renderer that reaches
// for a clock is flagged (in a field, in an interface method, in a helper
// method, and in a method signature), a renderer that does not is left alone,
// and a non-renderer type using time freely is left alone — which is the scoping
// claim, that the rule follows the receiver rather than the package.
//
// The fixture also pins the stated LIMIT: `delegating` asks a collaborator the
// liveness question and is deliberately not flagged. The fixture uses `want`
// markers, so an unexpected diagnostic fails — which makes that absence an
// assertion about the boundary rather than an oversight.
func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), renderclockanalyzer.Analyzer, "render")
}

// TestAnalyzerReportsItsOwnDecay covers the half a rule keyed off a NAME
// otherwise loses: rename chatRenderer and the guard becomes a no-op that still
// passes CI. The fixture sits at the one package path the interface is required
// to exist at and does not declare it, so the pass reports that instead of
// silently finding nothing.
func TestAnalyzerReportsItsOwnDecay(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), renderclockanalyzer.Analyzer, "murtaugh/internal/slack/gateway")
}
