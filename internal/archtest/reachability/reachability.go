// Package reachability answers one question about a binary: which packages it
// can reach, by any import path at all.
//
// It exists for #170 Change E — "the gateway must be *incapable* of serving an
// AI-backed request by itself, enforced by CI, not by convention". That is a
// property of the whole import closure, not of one file's import block, so it
// cannot be a go/analysis pass: a pass is invoked per package and would have to
// re-walk the graph itself and be told which package is the binary. `go list
// -deps` already computes exactly this closure, so this package shells it and
// compares names.
//
// The forbidden list and the pattern CI greps for are both derived from
// Forbidden below, so the rule is written down once. A rule spelled twice — once
// in Go, once in YAML — drifts, and the copy CI runs is the one that matters.
package reachability

import (
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Module is this repository's module path. Every entry in Forbidden is
// relative to it.
const Module = "github.com/miere/murtaugh"

// GatewayBinary is the package the rule is about.
const GatewayBinary = "./cmd/murtaugh-gateway"

const RuntimeBinary = "./cmd/murtaugh-runtime"

// Forbidden lists the packages the Slack gateway binary must not reach.
//
// It is #170's list verbatim, and the exclusions are as deliberate as the
// inclusions. `internal/agent` itself is absent: it is the event and session
// vocabulary the gateway is built on, and it runs nothing. So is
// `internal/agent/remote`, which is an agent.Client over a link to a node and
// therefore exactly what the gateway is SUPPOSED to reach — which is why the
// rule enumerates three backends rather than banning the `internal/agent`
// subtree.
//
// `internal/agentdelegate` and `internal/agentruntime/local` are not listed
// either, and are already caught: both reach `internal/agentbuild` in one hop,
// so an import of either fails this check. Listing them would name the mistake
// more precisely; leaving them out keeps this list identical to the specified
// rule, which matters more.
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

// Pattern renders Forbidden as the extended regular expression the CI step
// greps `go list -deps` output with.
//
// Two details are load-bearing. Every entry carries the module path, because
// `go list -deps` prints the standard library too and a bare `internal/...`
// matches a dozen `crypto/internal/...` lines. And both ends are anchored
// against a whole line, because `go list` prints one package per line and an
// unanchored `internal/llm` would also report a future `internal/llmcache` as
// though it were the forbidden package — a guard that cries wolf gets disabled,
// which is the same failure mode the `if` form exists to avoid.
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

// Check reports which of Forbidden the given package can reach, transitively.
// The returned slice is sorted and empty when the rule holds.
//
// pkg is spelled the way the CI step spells it — "./cmd/murtaugh-gateway" — and
// is resolved against the module path rather than the process's working
// directory, so a test living several packages deep asks about the same package
// CI does.
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
