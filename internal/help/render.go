// Flag documentation is rendered from a tool's own InputSchema rather than
// transcribed into prose. The schema already carries every fact a flag table
// needs — name, type, requiredness, enum values, repeatability and a
// description — so generating the table here makes the schema the single
// source of truth and removes the hand cross-check that used to be needed
// whenever a tool grew a parameter.
package help

import (
	"fmt"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// Doc is the metadata subset the reference needs from a tool. Every
// tools.Tool satisfies it structurally, so internal/help documents the
// registry without importing it (and without depending on Invoke).
type Doc interface {
	Name() string
	Description() string
	InputSchema() *jsonschema.Schema
}

// wrapWidth is the column the generated prose wraps at, matching the
// hand-written body of cli-help.md.
const wrapWidth = 76

// RenderSection returns the full generated reference block for one tool: the
// `## murtaugh …` header, the tool's description, a usage line and the flag
// table. It is what a command's help looks like with no hand-written prose
// backing it; Reference.Section splices the table alone into a section that
// does have prose.
func RenderSection(d Doc) string {
	var b strings.Builder
	b.WriteString("## murtaugh " + commandOf(d.Name()) + "\n\n")
	if desc := strings.TrimSpace(d.Description()); desc != "" {
		b.WriteString(wrap(desc, wrapWidth) + "\n\n")
	}
	if table := FlagTable(d.InputSchema()); table != "" {
		b.WriteString(table + "\n")
	}
	b.WriteString("```\n" + UsageLine(d) + "\n```\n")
	if mcp := mcpNameOf(d.Name()); mcp != commandOf(d.Name()) {
		b.WriteString("\nOver MCP this tool is called `" + mcp + "`.\n")
	}
	return b.String()
}

// UsageLine renders the one-line invocation form: required flags spelled out
// with their type placeholder, optional ones collapsed into `[flags]`.
func UsageLine(d Doc) string {
	line := "murtaugh " + commandOf(d.Name())
	props := orderedProps(d.InputSchema())
	req := requiredSet(d.InputSchema())
	optional := false
	for _, p := range props {
		if !req[p.name] {
			optional = true
			continue
		}
		line += fmt.Sprintf(" --%s <%s>", flagOf(p.name), placeholder(p.schema))
	}
	if optional {
		line += " [flags]"
	}
	return line
}

// FlagTable renders the markdown flag table for a schema, or "" when the tool
// takes no parameters. Columns are padded so the raw markdown stays readable
// in a terminal — `murtaugh help` prints this document unrendered.
func FlagTable(schema *jsonschema.Schema) string {
	props := orderedProps(schema)
	if len(props) == 0 {
		return ""
	}
	req := requiredSet(schema)
	rows := make([][4]string, 0, len(props))
	for _, p := range props {
		required := "no"
		if req[p.name] {
			required = "yes"
		}
		rows = append(rows, [4]string{"`--" + flagOf(p.name) + "`", required, typeLabel(p.schema), notes(p.schema)})
	}
	header := [4]string{"Flag", "Required", "Type", "Notes"}
	width := [4]int{}
	for i, h := range header {
		width[i] = len(h)
	}
	// The Notes column is last and left unpadded: padding it to the widest
	// cell would produce lines hundreds of columns wide for no gain.
	for _, r := range rows {
		for i := 0; i < 3; i++ {
			if len(r[i]) > width[i] {
				width[i] = len(r[i])
			}
		}
	}
	var b strings.Builder
	writeRow := func(cells [4]string) {
		b.WriteString("|")
		for i, c := range cells {
			if i == 3 {
				b.WriteString(" " + c + " |")
				continue
			}
			b.WriteString(" " + c + strings.Repeat(" ", width[i]-len(c)) + " |")
		}
		b.WriteString("\n")
	}
	writeRow(header)
	b.WriteString("|")
	for i := range header {
		n := width[i]
		if i == 3 {
			n = len(header[i])
		}
		b.WriteString("-" + strings.Repeat("-", n) + "-|")
	}
	b.WriteString("\n")
	for _, r := range rows {
		writeRow(r)
	}
	return b.String()
}

// prop pairs a schema property with its name so the set can be ordered.
type prop struct {
	name   string
	schema *jsonschema.Schema
}

// orderedProps returns a tool's properties in a stable documentation order:
// required flags first (in the order the schema declares them, which is the
// order the tool author considered primary), then the rest alphabetically.
// Map iteration order would otherwise reshuffle the table on every render.
func orderedProps(schema *jsonschema.Schema) []prop {
	if schema == nil || len(schema.Properties) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(schema.Properties))
	out := make([]prop, 0, len(schema.Properties))
	for _, name := range schema.Required {
		if s, ok := schema.Properties[name]; ok && !seen[name] {
			seen[name] = true
			out = append(out, prop{name: name, schema: s})
		}
	}
	rest := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		out = append(out, prop{name: name, schema: schema.Properties[name]})
	}
	return out
}

// requiredSet indexes a schema's Required list for lookup.
func requiredSet(schema *jsonschema.Schema) map[string]bool {
	if schema == nil {
		return nil
	}
	out := make(map[string]bool, len(schema.Required))
	for _, name := range schema.Required {
		out[name] = true
	}
	return out
}

// typeOf returns a property's JSON Schema type, consulting the legacy single
// Type field before the Types array — the same precedence the CLI flag parser
// applies, so the documented type is the one that will actually be coerced.
func typeOf(s *jsonschema.Schema) string {
	if s == nil {
		return "string"
	}
	if s.Type != "" {
		return s.Type
	}
	if len(s.Types) > 0 {
		return s.Types[0]
	}
	return "string"
}

// typeLabel is the Type column's value: `enum` when the property constrains
// its values, `string[]` for a repeatable array, otherwise the raw type.
func typeLabel(s *jsonschema.Schema) string {
	if s != nil && len(s.Enum) > 0 {
		return "enum"
	}
	if typeOf(s) == "array" {
		return itemTypeOf(s) + "[]"
	}
	return typeOf(s)
}

// itemTypeOf returns the element type of an array property.
func itemTypeOf(s *jsonschema.Schema) string {
	if s == nil || s.Items == nil {
		return "string"
	}
	return typeOf(s.Items)
}

// placeholder is the `<…>` token a required flag shows in the usage line.
func placeholder(s *jsonschema.Schema) string {
	if s != nil && len(s.Enum) > 0 {
		return strings.Join(enumValues(s), "|")
	}
	return typeLabel(s)
}

// notes is the Notes column: the property's own description, followed by the
// two facts the CLI enforces but a schema description never states — that an
// array flag is repeated rather than comma-joined, and which values an enum
// accepts.
func notes(s *jsonschema.Schema) string {
	parts := []string{}
	if s != nil {
		if d := strings.TrimSpace(s.Description); d != "" {
			parts = append(parts, ensureSentence(d))
		}
		if len(s.Enum) > 0 {
			vals := enumValues(s)
			for i, v := range vals {
				vals[i] = "`" + v + "`"
			}
			parts = append(parts, "One of: "+strings.Join(vals, ", ")+".")
		}
		// Only state the repetition rule when the author has not already said
		// it in their own words, so the column does not read "(repeatable).
		// Repeatable."
		if typeOf(s) == "array" && !strings.Contains(strings.ToLower(s.Description), "repeat") {
			parts = append(parts, "Repeatable — pass the flag once per value.")
		}
		if typeOf(s) == "boolean" {
			parts = append(parts, "Needs an explicit value (`true`/`false`).")
		}
	}
	if len(parts) == 0 {
		return "—"
	}
	// Descriptions routinely spell alternatives as "native | acp | claude_code".
	// An unescaped pipe closes the cell, so the rest of the sentence lands in
	// phantom columns.
	return strings.ReplaceAll(strings.Join(parts, " "), "|", `\|`)
}

// enumValues renders a property's enum as display strings.
func enumValues(s *jsonschema.Schema) []string {
	out := make([]string, 0, len(s.Enum))
	for _, v := range s.Enum {
		out = append(out, fmt.Sprintf("%v", v))
	}
	return out
}

// ensureSentence gives a description a trailing period so the Notes column
// reads consistently whether or not the tool author punctuated it.
func ensureSentence(s string) string {
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") {
		return s
	}
	return s + "."
}

func commandOf(name string) string { return strings.ReplaceAll(name, ".", " ") }

// flagOf converts a snake_case schema property into its kebab-case flag
// ("attachment_type" → "attachment-type"), inverting the CLI parser's
// snakeFromKebab.
func flagOf(name string) string { return strings.ReplaceAll(name, "_", "-") }

// mcpNameOf mirrors the MCP frontend's name normalisation (every character
// outside [A-Za-z0-9_-] becomes '_') so the reference can tell the reader
// which id the same tool answers to over MCP.
func mcpNameOf(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, name)
}

// wrap hard-wraps text at width columns on whitespace, leaving existing
// newlines (paragraph breaks in a description) intact.
func wrap(s string, width int) string {
	var out []string
	for _, para := range strings.Split(s, "\n") {
		line := ""
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case len(line)+1+len(word) <= width:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
