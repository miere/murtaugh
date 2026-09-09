// Package persona resolves an agent's SOUL.md — the single source of Murtaugh's
// voice — and strips the Claude Code output-style frontmatter so only the prose
// ever reaches a system prompt.
//
// SOUL.md is dual-purpose by design: the same file is the persona Murtaugh
// injects (native's <persona> block, claude_code's --append-system-prompt) AND a
// Claude Code output style an admin can select from the interactive CLI. The
// output-style role is what requires the YAML frontmatter; the injection role is
// what requires stripping it. Both backends resolve through here, so the two can
// never drift.
package persona

import (
	"os"
	"path/filepath"
	"strings"
)

// File is the conventional persona filename, in both the workspace/config root
// (the node default, seeded by bootstrap) and an agent's workdir (the override
// onboarding writes).
const File = "SOUL.md"

// Resolve returns the persona prose for an agent, frontmatter already stripped,
// by precedence:
//
//  1. profileFile — the agent profile's soul_file; a relative path resolves
//     against workspaceDir. The only knob that separates two profiles sharing
//     one workdir.
//  2. <workDir>/SOUL.md — the per-workdir override the onboarding flow writes,
//     so an agent's voice can follow the context it works in.
//  3. <workspaceDir>/SOUL.md — the node default bootstrap seeds.
//
// Best-effort throughout: a missing or unreadable file falls through to the next
// step, and a file that is nothing but frontmatter (the seeded default, before
// onboarding) yields "" — which callers treat as "no persona", not an error.
func Resolve(profileFile, workDir, workspaceDir string) string {
	for _, path := range candidates(profileFile, workDir, workspaceDir) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if body := StripFrontmatter(string(data)); body != "" {
			return body
		}
	}
	return ""
}

// candidates lists the persona files to try, in precedence order, skipping the
// steps that are not configured.
func candidates(profileFile, workDir, workspaceDir string) []string {
	var paths []string
	if f := strings.TrimSpace(profileFile); f != "" {
		if !filepath.IsAbs(f) && strings.TrimSpace(workspaceDir) != "" {
			f = filepath.Join(workspaceDir, f)
		}
		paths = append(paths, f)
	}
	if d := strings.TrimSpace(workDir); d != "" {
		paths = append(paths, filepath.Join(d, File))
	}
	if d := strings.TrimSpace(workspaceDir); d != "" {
		p := filepath.Join(d, File)
		// When an agent's workdir IS the workspace root (the common single-node
		// setup) the two candidates collide; listing it twice would just re-read
		// the same file.
		if len(paths) == 0 || paths[len(paths)-1] != p {
			paths = append(paths, p)
		}
	}
	return paths
}

// StripFrontmatter removes a leading YAML frontmatter block (--- fenced) and
// returns the remaining prose, trimmed. A document with no frontmatter is
// returned trimmed but otherwise untouched, and so is one whose opening fence is
// never closed — an unterminated block is far more likely to be prose that
// happens to start with a rule than a truncated header, and swallowing the whole
// file would silently erase the agent's voice.
func StripFrontmatter(doc string) string {
	rest, ok := strings.CutPrefix(strings.TrimLeft(doc, " \t\r\n"), "---")
	if !ok {
		return strings.TrimSpace(doc)
	}
	// The opening fence must be alone on its line, so `--- not frontmatter` is
	// treated as prose.
	line, rest, found := strings.Cut(rest, "\n")
	if !found || strings.TrimSpace(line) != "" {
		return strings.TrimSpace(doc)
	}
	for {
		line, remainder, more := strings.Cut(rest, "\n")
		if strings.TrimSpace(line) == "---" {
			return strings.TrimSpace(remainder)
		}
		if !more {
			return strings.TrimSpace(doc)
		}
		rest = remainder
	}
}
