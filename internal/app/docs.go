package app

import (
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/help"
	"github.com/miere/murtaugh/internal/tools"
)

// HelpReference builds the command reference from the real tool registry.
//
// It is safe to call before config bootstrap — which is exactly why it exists.
// `murtaugh help` must work on a machine that has never been configured, and
// every tool defers its dependencies behind a closure or a lazy client, so a
// registry built from a zero config resolves no store, opens no journal and
// dials no Slack. Only Name/Description/InputSchema are read from it.
func HelpReference(version string) *help.Reference {
	return help.New(HelpDocs(docsRegistry(version)))
}

// docsRegistry builds the registry from a zero config, for metadata only.
func docsRegistry(version string) *tools.Registry {
	return buildRegistry(config.Config{}, nil, "", version, nil, nil, nil, Agents{})
}

// HelpDocs adapts a registry to the reference's Doc view. Every tools.Tool
// satisfies help.Doc structurally; this is the one place that conversion
// happens, so internal/help never imports the registry.
func HelpDocs(reg *tools.Registry) []help.Doc {
	all := reg.All()
	out := make([]help.Doc, 0, len(all))
	for _, t := range all {
		out = append(out, t)
	}
	return out
}
