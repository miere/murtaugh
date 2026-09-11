package reachability_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/archtest/reachability"
)

// The rule itself: cmd/murtaugh-gateway must not be able to reach any package
// that can run a model, by any import path.
//
// The failure this prevents is not somebody writing `import
// "internal/agentbuild"` in main.go — that is obvious in review. It is a gateway
// file importing something ordinary that turns out to reach a backend three hops
// down, which is how all five of the sites cut in this change got there.
func TestTheGatewayBinaryReachesNoAgentBackend(t *testing.T) {
	reached, err := reachability.Check(reachability.GatewayBinary)
	if err != nil {
		t.Fatalf("checking %s: %v", reachability.GatewayBinary, err)
	}
	if len(reached) > 0 {
		t.Fatalf("%s reaches packages it must not: %v", reachability.GatewayBinary, reached)
	}
}

// The control, and the reason it is not a fixture: cmd/murtaugh MUST reach these
// packages — the CLI keeps its local agent, deliberately (#170 Change E), so
// `murtaugh jobs run x` works with no gateway and no node. That makes it a
// positive control nobody can quietly delete without also deleting the feature,
// which is exactly what a check for "can this check fail?" needs.
//
// Without it, the test above passes just as happily against a Check that always
// returns nothing.
func TestTheCheckReportsABinaryThatMayRunAgents(t *testing.T) {
	reached, err := reachability.Check("./cmd/murtaugh")
	if err != nil {
		t.Fatalf("checking ./cmd/murtaugh: %v", err)
	}
	for _, want := range []string{
		reachability.Module + "/internal/agentbuild",
		reachability.Module + "/internal/llm",
	} {
		if !slices.Contains(reached, want) {
			t.Errorf("./cmd/murtaugh does not reach %s; the CLI has lost its local agent, or the check has stopped working", want)
		}
	}
}

// Pattern must match a forbidden package on a line of its own and nothing else.
// The anchoring is what stops a future sibling — internal/llmcache, say — being
// reported as internal/llm, and a guard that fires when nothing is wrong is one
// that gets deleted.
func TestPatternMatchesWholeLinesOnly(t *testing.T) {
	re, err := regexp.Compile(reachability.Pattern())
	if err != nil {
		t.Fatalf("Pattern() does not compile: %v", err)
	}
	for _, forbidden := range reachability.Forbidden {
		if !re.MatchString(reachability.Module + "/" + forbidden) {
			t.Errorf("Pattern() does not match %q, which is on the forbidden list", forbidden)
		}
	}
	for _, allowed := range []string{
		reachability.Module + "/internal/agent",
		reachability.Module + "/internal/agent/remote",
		reachability.Module + "/internal/agentruntime",
		reachability.Module + "/internal/agentwire",
		reachability.Module + "/internal/providerfail",
		reachability.Module + "/internal/llmcache",
		"crypto/internal/fips140",
	} {
		if re.MatchString(allowed) {
			t.Errorf("Pattern() matches %q, which the rule permits", allowed)
		}
	}
}

// The shell copy of each rule is the one CI runs, so it must be exactly the
// script the Go rule implies; any edit to it, however small, fails here.
func TestTheCommittedCIStepMatchesTheRule(t *testing.T) {
	const workflow = "../../../.github/workflows/ci.yml"
	raw, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatalf("reading %s: %v", workflow, err)
	}
	for _, rule := range []struct {
		step, pattern, binary, noun string
	}{
		{"Gateway reachability rule", reachability.Pattern(), reachability.GatewayBinary, "gateway"},
		{"Runtime reachability rule", reachability.RuntimePattern(), reachability.RuntimeBinary, "runtime"},
	} {
		want := strings.Join([]string{
			"set -euo pipefail",
			"forbidden='" + rule.pattern + "'",
			`deps="$(go list -deps ` + rule.binary + `)"`,
			`if reached="$(printf '%s\n' "$deps" | grep -E "$forbidden")"; then`,
			`  echo "::error::the ` + rule.noun + ` binary reaches a package it must not:"`,
			`  echo "$reached"`,
			"  exit 1",
			"fi",
		}, "\n")
		if got := runScript(t, string(raw), rule.step); got != want {
			t.Errorf("the %q step is not the script the rule implies.\n got:\n%s\nwant:\n%s", rule.step, got, want)
		}
	}
}

func runScript(t *testing.T, workflow, name string) string {
	t.Helper()
	lines := strings.Split(workflow, "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.TrimSpace(l) == "- name: "+name })
	if at < 0 {
		t.Fatalf("no CI step named %q; the reachability rule is not enforced anywhere", name)
	}
	var script []string
	indent := ""
	for _, line := range lines[at+1:] {
		trimmed := strings.TrimSpace(line)
		if indent == "" {
			if trimmed == "run: |" {
				indent = line[:len(line)-len(strings.TrimLeft(line, " "))] + "  "
			} else if strings.HasPrefix(trimmed, "- ") {
				break
			}
			continue
		}
		if trimmed != "" && !strings.HasPrefix(line, indent) {
			break
		}
		script = append(script, strings.TrimPrefix(line, indent))
	}
	for len(script) > 0 && strings.TrimSpace(script[len(script)-1]) == "" {
		script = script[:len(script)-1]
	}
	return strings.Join(script, "\n")
}

// A node that could reach a Slack client could post as the bot without the
// gateway deciding where, which is the one thing display requests exist to stop.
func TestTheRuntimeBinaryReachesNoSlack(t *testing.T) {
	reached, err := reachability.CheckRuntime(reachability.RuntimeBinary)
	if err != nil {
		t.Fatalf("checking %s: %v", reachability.RuntimeBinary, err)
	}
	if len(reached) > 0 {
		t.Fatalf("%s reaches packages it must not: %v", reachability.RuntimeBinary, reached)
	}
}

// The CLI links the Slack gateway, so it must trip the runtime rule; if it does
// not, the check has stopped working.
func TestTheRuntimeCheckReportsABinaryThatTalksToSlack(t *testing.T) {
	reached, err := reachability.CheckRuntime("./cmd/murtaugh")
	if err != nil {
		t.Fatalf("checking ./cmd/murtaugh: %v", err)
	}
	for _, want := range []string{
		"github.com/slack-go/slack",
		reachability.Module + "/internal/slack/gateway",
		reachability.Module + "/internal/tools/slack/sendmsg",
	} {
		if !slices.Contains(reached, want) {
			t.Errorf("./cmd/murtaugh does not reach %s; the check has stopped working", want)
		}
	}
}

func TestRuntimePatternMatchesWholeSubtreesOnly(t *testing.T) {
	re, err := regexp.Compile(reachability.RuntimePattern())
	if err != nil {
		t.Fatalf("RuntimePattern() does not compile: %v", err)
	}
	for _, forbidden := range []string{
		reachability.Module + "/internal/slack",
		reachability.Module + "/internal/slack/display",
		reachability.Module + "/internal/tools/slack/sendmsg",
		"github.com/slack-go/slack",
		"github.com/slack-go/slack/socketmode",
	} {
		if !re.MatchString(forbidden) {
			t.Errorf("RuntimePattern() does not match %q", forbidden)
		}
	}
	for _, allowed := range []string{
		reachability.Module + "/internal/slackish",
		reachability.Module + "/internal/tools/ask",
		reachability.Module + "/internal/agent",
		"github.com/slack-go/slacker",
	} {
		if re.MatchString(allowed) {
			t.Errorf("RuntimePattern() matches %q, which the rule permits", allowed)
		}
	}
}
