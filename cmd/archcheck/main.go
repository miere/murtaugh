// Command archcheck runs Murtaugh's architecture-guard analyzers over the tree.
// It is wired into CI (`go run ./cmd/archcheck ./...`) so a violation fails the
// build. Today it carries three guards — the workdir rule (no downstream reads
// of config.AgentProfile.WorkDir), the renderer-clock rule (no chatRenderer
// implementation may observe time) and the node-token rule (no digest compared
// with ==) — and is a multichecker so adding the next arch invariant means one
// more analyzer here.
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
