package interaction

import (
	"strings"
	"sync"
)

// Grants is the set of tool calls a user chose to always allow, shared by both
// approval paths for one agent.
//
// There are two of those paths, and before this they could not agree. Murtaugh's
// own gate (the native loop's tool calls, and the registry tools an ACP or
// claude_code agent reaches over the MCP bridge) offers "Approve & always allow"
// and remembered the answer. The reflection path — where an ACP or Claude Code
// agent asks Murtaugh to approve one of *its* tools — had no memory at all, so
// the same command already allowed once through Murtaugh's terminal tool was
// asked about again the moment the agent ran it itself.
//
// One store per agent fixes that: whichever path records the grant, both honour
// it. It is deliberately not persisted — a grant lasts as long as the daemon, in
// keeping with the session-scoped set it replaces — and deliberately not shared
// between agents, so a permissive agent cannot widen a strict one.
type Grants struct {
	mu      sync.Mutex
	allowed map[string]bool
}

// NewGrants builds an empty grant set.
func NewGrants() *Grants { return &Grants{allowed: make(map[string]bool)} }

// Remember records key as always-allowed for the rest of this run.
func (g *Grants) Remember(key string) {
	if g == nil || strings.TrimSpace(key) == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.allowed == nil {
		g.allowed = make(map[string]bool)
	}
	g.allowed[key] = true
}

// Allowed reports whether key was previously granted.
func (g *Grants) Allowed(key string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.allowed[key]
}

// GrantKey derives the key a tool call is remembered under.
//
// A shell call keys on the command line *alone*, with no tool name in it — that
// is what lets a grant cross between the two paths, because the same command
// arrives as Murtaugh's "terminal" tool on one and as Claude Code's "Bash" on the
// other. Anything else keys on the tool name as well, so allowing an edit of one
// path says nothing about a read of another.
//
// Matching stays exact (after trimming), like the set it replaces: no
// normalising, no prefix matching. A grant means the command the user actually
// looked at, not a family of commands resembling it.
func GrantKey(toolName, detail string) string {
	d := strings.TrimSpace(detail)
	if d == "" {
		return ""
	}
	if isShellTool(toolName) {
		return d
	}
	return strings.ToLower(strings.TrimSpace(toolName)) + "\x00" + d
}
