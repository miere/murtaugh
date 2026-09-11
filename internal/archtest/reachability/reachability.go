// Package reachability shells out to `go list -deps` because the rule covers a
// binary's whole import closure, which a per-package analysis pass cannot see.
package reachability

import (
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

const Module = "github.com/miere/murtaugh"

const GatewayBinary = "./cmd/murtaugh-gateway"

const RuntimeBinary = "./cmd/murtaugh-runtime"

// Forbidden leaves internal/agent out on purpose: its core is shared vocabulary,
// and internal/agent/remote is exactly how the gateway is meant to reach a node.
var Forbidden = []string{
	"internal/agentbuild",
	"internal/llm",
	"internal/agent/acp",
	"internal/agent/native",
	"internal/agent/claudecode",
}

// RuntimeForbidden bans whole subtrees, not single packages: tools run on the
// node, and any Slack client there would let a node post as the bot.
var RuntimeForbidden = []string{
	Module + "/internal/slack",
	Module + "/internal/tools/slack",
	"github.com/slack-go/slack",
}

// Pattern keeps the module path and anchors whole lines, or stdlib crypto/internal
// packages and a future internal/llmcache would match by mistake.
func Pattern() string {
	paths := make([]string, 0, len(Forbidden))
	for _, p := range Forbidden {
		paths = append(paths, Module+"/"+p)
	}
	return "^(" + alternatives(paths) + ")$"
}

// RuntimePattern stops at a path separator so a sibling such as
// internal/slackish is not mistaken for part of internal/slack.
func RuntimePattern() string {
	return "^(" + alternatives(RuntimeForbidden) + ")(/.*)?$"
}

func alternatives(paths []string) string {
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		quoted = append(quoted, strings.ReplaceAll(p, ".", `\.`))
	}
	return strings.Join(quoted, "|")
}

// Check resolves pkg against the module path rather than the working directory,
// so a test deep in the tree checks the same package CI does.
func Check(pkg string) ([]string, error) {
	banned := make(map[string]bool, len(Forbidden))
	for _, p := range Forbidden {
		banned[Module+"/"+p] = true
	}
	return reached(pkg, func(dep string) bool { return banned[dep] })
}

func CheckRuntime(pkg string) ([]string, error) {
	return reached(pkg, func(dep string) bool {
		for _, p := range RuntimeForbidden {
			if dep == p || strings.HasPrefix(dep, p+"/") {
				return true
			}
		}
		return false
	})
}

func reached(pkg string, banned func(string) bool) ([]string, error) {
	target := pkg
	if rel, ok := strings.CutPrefix(pkg, "./"); ok {
		target = Module + "/" + rel
	}
	out, err := exec.Command("go", "list", "-deps", target).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("go list -deps %s: %w: %s", target, err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("go list -deps %s: %w", target, err)
	}
	var found []string
	for _, line := range strings.Split(string(out), "\n") {
		dep := strings.TrimSpace(line)
		if banned(dep) {
			found = append(found, dep)
		}
	}
	sort.Strings(found)
	return found, nil
}
