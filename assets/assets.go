package assets

import (
	"embed"
	"io/fs"
	"sort"
)

// FS contains reference assets that are also used as built-in defaults: the
// seed bootstrap file (config.yaml — oauth + database) and .env template, the
// default system prompt, the Block Kit templates under templates/, the bundled
// agent skills under skills/ (each a SKILL.md + reference/ + examples/ tree),
// and cli-help.md (the canonical CLI/MCP command reference surfaced by
// `murtaugh help`). Both templates and skills are embedded recursively, as is
// troubleshoot/ (the diagnostics-bundle instructions surfaced by
// `troubleshoot.bundle`).
//
// The former per-vertical config templates (agents.yaml, jobs.yaml, …) are gone:
// that configuration lives in the database now and is authored via `murtaugh
// cfg …`, so there is nothing to seed.
//
//go:embed config.yaml env.example system-prompt.md slack-format.md AGENTS.md cli-help.md templates skills troubleshoot
var FS embed.FS

// slackFormatFile holds the Slack formatting dialect rules.
const slackFormatFile = "slack-format.md"

// SlackFormat returns the canonical Slack formatting rules, appended to every
// agent's system prompt regardless of backend.
//
// It is a separate file rather than a paragraph inside system-prompt.md because
// the two backends reach it differently — native folds it into the resolved
// prompt, claude_code passes it as --append-system-prompt — and because it is a
// fact about the transport, not a persona choice. An operator who replaces the
// whole system prompt should still get correct formatting.
func SlackFormat() string {
	data, err := FS.ReadFile(slackFormatFile)
	if err != nil {
		return ""
	}
	return string(data)
}

// skillsRoot is the embedded directory holding one subdirectory per bundled
// (murtaugh-*) skill.
const skillsRoot = "skills"

// Skills returns an fs.FS rooted at the embedded skills tree, so each bundled
// skill is a top-level directory (e.g. "murtaugh-slack/SKILL.md"). This is the
// managed, in-binary source the skills tool serves from — the bundled skills
// never need to touch disk. Returns nil only if the embed is malformed (a build
// error), which never happens in practice.
func Skills() fs.FS {
	sub, err := fs.Sub(FS, skillsRoot)
	if err != nil {
		return nil
	}
	return sub
}

// SkillNames returns the sorted directory names of the bundled skills (e.g.
// "murtaugh-slack"). It is the authority for validating an agent's
// export_skills_to_fs list and for the export reconcile.
func SkillNames() []string {
	entries, err := fs.ReadDir(FS, skillsRoot)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}
