// Package helptool exposes Murtaugh's own command reference as a tool, so an
// agent that is unsure how to call something can ask instead of guessing —
// or, as it does today, shelling out to `murtaugh --help` and probing.
//
// It is deliberately the cheap half of a two-tier documentation split. Every
// tool's Description and parameter descriptions ship in the model's context on
// every single turn, so they have to stay short. The long form — worked
// examples, mutually exclusive flags, which changes need a daemon restart —
// lives here and costs nothing until the agent actually asks for it.
package helptool

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/help"
)

// Tool is the `help` capability.
type Tool struct {
	// docs is resolved on each call rather than captured at construction:
	// the tool is registered into the very registry it documents, so the set
	// does not exist yet when New runs.
	docs func() []help.Doc
}

// New constructs a Tool that documents whatever docs returns at call time.
func New(docs func() []help.Doc) *Tool { return &Tool{docs: docs} }

// Name returns the registry key.
func (t *Tool) Name() string { return "help" }

// Description returns the human-facing summary used by MCP clients. It names
// the failure it exists to prevent, because a description that only says what
// a tool does gives a model no cue for when to reach for it.
func (t *Tool) Description() string {
	return "Look up how to call any Murtaugh tool: its exact flags, their types, which are required, and worked examples. Call this instead of guessing at arguments or running `murtaugh --help` in a terminal. With no command, lists every command available."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"command": {
				Type:        "string",
				Description: "Command to document, in any spelling: \"slack send_msg\", \"slack.send_msg\" or \"slack_send_msg\". Omit to list every command.",
			},
		},
	}
}

// Invoke renders the reference for the requested command, or the command list
// when none was given. An unknown command returns the list too, rather than an
// error: the caller asked a reasonable question with the wrong name, and the
// list is the answer that unblocks it.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	ref := help.New(t.resolve())
	command, _ := args["command"].(string)
	if strings.TrimSpace(command) == "" {
		return "Available commands:\n\n" + bullets(ref.Commands()), nil
	}
	section, ok := ref.Section(command)
	if !ok {
		return fmt.Sprintf("No command named %q. Available commands:\n\n%s", command, bullets(ref.Commands())), nil
	}
	return section, nil
}

// resolve returns the documented tool set, tolerating a nil source so a
// misconfigured wiring degrades to the prose-only reference instead of
// panicking mid-conversation.
func (t *Tool) resolve() []help.Doc {
	if t.docs == nil {
		return nil
	}
	return t.docs()
}

// bullets renders command names as a markdown list.
func bullets(commands []string) string {
	var b strings.Builder
	for _, c := range commands {
		b.WriteString("- `murtaugh " + c + "`\n")
	}
	return b.String()
}
