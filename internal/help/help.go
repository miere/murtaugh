// Package help assembles Murtaugh's CLI/MCP command reference from two
// sources: the prose that ships in assets/cli-help.md, and the tool registry
// itself. Prose carries what only a human can say — worked examples, the
// consequences of a flag, which changes need a daemon restart. The registry
// carries what a human should never have to retype — the flag names, their
// types, which are required, which repeat, which are enums.
//
// Every flag table in the rendered document is generated from the owning
// tool's InputSchema, so a tool that grows a parameter documents it by
// existing. A hand-written table in cli-help.md is replaced at render time
// rather than trusted, and a tool with no prose at all still gets a complete
// generated section — the failure mode that left `ask`, `attach`, `terminal`,
// `restart` and the whole `files.*` group undocumented.
package help

import (
	"strings"

	"github.com/miere/murtaugh/assets"
)

// fileName is the embedded prose document.
const fileName = "cli-help.md"

// commandPrefix marks a per-command section header in cli-help.md. Everything
// from such a header up to the next header (any `## ` or `# ` line) is that
// command's prose block.
const commandPrefix = "## murtaugh "

// Reference renders the command reference for a set of documented tools.
// Construct it with New; the zero value renders prose only, which is what a
// caller that cannot build a registry (an early bootstrap failure) falls back
// to.
type Reference struct {
	docs []Doc
}

// New returns a Reference that documents docs, in the order given — the
// registry's registration order, which groups related tools together.
func New(docs []Doc) *Reference { return &Reference{docs: docs} }

// Full returns the entire reference: the prose preamble, every hand-written
// section with its flag table regenerated, then a generated section for each
// tool the prose never mentioned.
func (r *Reference) Full() string {
	preamble, sections := parse(prose())
	byCommand := r.index()
	documented := make(map[string]bool, len(sections))

	var b strings.Builder
	b.WriteString(preamble)
	for _, s := range sections {
		if d, ok := byCommand[s.command]; ok {
			documented[s.command] = true
			b.WriteString(splice(s.text, d))
			continue
		}
		b.WriteString(s.text)
	}
	for _, d := range r.docs {
		if !documented[normalizeKey(d.Name())] {
			b.WriteString("\n" + RenderSection(d))
		}
	}
	return b.String()
}

// Section returns the reference block for a single command, keyed by its CLI
// invocation ("jobs run", "slack send-msg", "ping"). The dotted registry form
// ("jobs.run") and the MCP underscore form ("jobs_run") are accepted too. The
// boolean reports whether the command is known — as a registered tool, as a
// prose section, or both.
func (r *Reference) Section(command string) (string, bool) {
	key := normalizeKey(command)
	if key == "" {
		return "", false
	}
	_, sections := parse(prose())
	for _, s := range sections {
		if s.command == key {
			if d, ok := r.index()[key]; ok {
				return splice(s.text, d), true
			}
			return s.text, true
		}
	}
	if d, ok := r.index()[key]; ok {
		return RenderSection(d), true
	}
	return "", false
}

// Render resolves the reference text for the given command tokens. With no
// tokens it returns the full document. When the tokens name a known command
// its section is returned; otherwise the full document is returned behind a
// short notice so callers still get something useful.
func (r *Reference) Render(tokens []string) string {
	key := normalizeKey(strings.Join(tokens, " "))
	if key == "" {
		return r.Full()
	}
	if sec, ok := r.Section(key); ok {
		return sec
	}
	return "No help section for \"" + key + "\". Showing the full reference.\n\n" + r.Full()
}

// Commands returns every command the reference can render a section for, as
// CLI invocations. Callers use it to list what exists without rendering it.
func (r *Reference) Commands() []string {
	seen := map[string]bool{}
	var out []string
	_, sections := parse(prose())
	for _, s := range sections {
		if !seen[s.command] {
			seen[s.command] = true
			out = append(out, s.command)
		}
	}
	for _, d := range r.docs {
		if k := normalizeKey(d.Name()); !seen[k] {
			seen[k] = true
			out = append(out, commandOf(d.Name()))
		}
	}
	return out
}

// index maps each documented tool to its normalized command key, so a tool
// whose registry name carries an underscore ("present_plan") is found by the
// same lookup that finds a dotted one.
func (r *Reference) index() map[string]Doc {
	out := make(map[string]Doc, len(r.docs))
	for _, d := range r.docs {
		out[normalizeKey(d.Name())] = d
	}
	return out
}

// prose returns the embedded document, or "" when the binary was assembled
// without it — a build fault, not a runtime condition, so it degrades to the
// generated sections rather than panicking.
func prose() string {
	b, err := assets.FS.ReadFile(fileName)
	if err != nil {
		return ""
	}
	return string(b)
}

// section is one `## murtaugh <command>` block of the prose document, with its
// header line included in text.
type section struct {
	command string
	text    string
}

// parse splits the prose document into the preamble (everything before the
// first command header) and the per-command sections that follow, preserving
// the document's own ordering.
func parse(doc string) (string, []section) {
	if doc == "" {
		return "", nil
	}
	lines := strings.Split(doc, "\n")
	starts := []int{}
	for i, ln := range lines {
		if strings.HasPrefix(ln, commandPrefix) {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		return doc, nil
	}
	preamble := strings.Join(lines[:starts[0]], "\n")
	var out []section
	for n, start := range starts {
		end := len(lines)
		// A section runs to the next header of any level, which may be another
		// command or a top-level `# ` heading that groups what follows.
		for i := start + 1; i < len(lines); i++ {
			if ln := lines[i]; strings.HasPrefix(ln, "## ") || strings.HasPrefix(ln, "# ") {
				end = i
				break
			}
		}
		if n+1 < len(starts) && starts[n+1] < end {
			end = starts[n+1]
		}
		out = append(out, section{
			command: normalizeKey(strings.TrimPrefix(lines[start], commandPrefix)),
			text:    strings.Join(lines[start:end], "\n"),
		})
	}
	return preamble, out
}

// splice replaces the flag table in a hand-written section with the one
// generated from the tool's schema. A section with no table gets the generated
// one inserted after its opening prose; a tool that takes no flags has any
// stale table removed. The hand-written prose either side is untouched, so the
// examples and warnings a schema cannot express survive.
func splice(text string, d Doc) string {
	table := FlagTable(d.InputSchema())
	lines := strings.Split(text, "\n")
	start, end := findTable(lines)
	if start < 0 {
		return insertAfterOpening(lines, table)
	}
	var out []string
	out = append(out, lines[:start]...)
	if table != "" {
		out = append(out, strings.Split(strings.TrimRight(table, "\n"), "\n")...)
	}
	// findTable consumed the blank line that closed the old table, so put one
	// back: without it the prose below fuses onto the last table row.
	if end < len(lines) {
		out = append(out, "")
	}
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n")
}

// findTable locates the first markdown table in a section and returns its
// half-open line range, or (-1, -1) when the section has none. A table is a
// run of lines starting with "|"; the blank line that follows it is consumed
// so a replacement does not double up.
func findTable(lines []string) (int, int) {
	for i, ln := range lines {
		if !strings.HasPrefix(strings.TrimSpace(ln), "|") {
			continue
		}
		end := i
		for end < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[end]), "|") {
			end++
		}
		if end < len(lines) && strings.TrimSpace(lines[end]) == "" {
			end++
		}
		return i, end
	}
	return -1, -1
}

// insertAfterOpening puts the generated table after a section's header and
// first prose paragraph, which is where the hand-written sections already put
// theirs. An empty table is a no-op.
func insertAfterOpening(lines []string, table string) string {
	if strings.TrimSpace(table) == "" {
		return strings.Join(lines, "\n")
	}
	// Skip the header and the blank line under it, then run to the end of the
	// opening paragraph.
	i := 1
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
		i++
	}
	var out []string
	out = append(out, lines[:i]...)
	out = append(out, "")
	out = append(out, strings.Split(strings.TrimRight(table, "\n"), "\n")...)
	out = append(out, lines[i:]...)
	return strings.Join(out, "\n")
}

// normalizeKey lower-cases the input, treats the dotted registry form and the
// MCP underscore form as the spaced CLI form, and collapses runs of whitespace
// so "jobs.run", "jobs_run", "jobs run" and "  Jobs   Run " all resolve to the
// same key.
func normalizeKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, ".", " ")
	s = strings.ReplaceAll(s, "_", " ")
	return strings.Join(strings.Fields(s), " ")
}
