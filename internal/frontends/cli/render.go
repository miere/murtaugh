// CLI rendering for tool results. A tool may return a fmt.Stringer to drive
// human-readable output independently from the structured JSON shape the MCP
// frontend emits. Strings and []string are handled directly so simple tools
// don't need to allocate a Stringer.
package cli

import (
	"fmt"
	"strings"
)

// Render converts a tool result into the text the CLI writes to stdout. The
// contract per tool is documented at the tool package; this is the dispatch
// site that picks the appropriate representation.
func Render(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []string:
		return strings.Join(x, "\n")
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}
