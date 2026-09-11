// Command archcheck runs Murtaugh's architecture-guard analyzers over the tree.
// It is wired into CI (`go run ./cmd/archcheck ./...`) so a violation fails the
// build.
package main

import (
	"github.com/miere/murtaugh/internal/archtest/nodetokenanalyzer"
	"github.com/miere/murtaugh/internal/archtest/renderclockanalyzer"
	"github.com/miere/murtaugh/internal/archtest/workdiranalyzer"
	"golang.org/x/tools/go/analysis/multichecker"
)

func main() {
	multichecker.Main(
		workdiranalyzer.Analyzer,
		renderclockanalyzer.Analyzer,
		nodetokenanalyzer.Analyzer,
	)
}
