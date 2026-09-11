package renderclockanalyzer_test

import (
	"testing"

	"github.com/miere/murtaugh/internal/archtest/renderclockanalyzer"
	"golang.org/x/tools/go/analysis/analysistest"
)

// The fixture's unflagged cases are assertions too: analysistest fails on any
// diagnostic it did not expect.
func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), renderclockanalyzer.Analyzer, "render")
}

// Renaming chatRenderer would silently turn the guard into a no-op, so the pass
// must report a guarded package that no longer declares it.
func TestAnalyzerReportsItsOwnDecay(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), renderclockanalyzer.Analyzer, "murtaugh/internal/slack/gateway")
}
