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

// The rule is written twice — once here in Go, once as a shell step in the CI
// workflow — and the shell copy is the one CI actually runs. This is the decay
// test: it reads the committed workflow and fails when the two drift, so adding
// a package to Forbidden without regenerating the step cannot silently leave the
// guard checking the old list.
//
// It also pins the `if` form. A pipeline ending in `grep … && exit 1` fails in
// the clean case too, and a guard that fails when nothing is wrong gets disabled
// within a week.
func TestTheCommittedCIStepMatchesTheRule(t *testing.T) {
	const workflow = "../../../.github/workflows/ci.yml"
	raw, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatalf("reading %s: %v", workflow, err)
	}
	step := ciStep(t, string(raw), "Gateway reachability rule")

	if !strings.Contains(step, "forbidden='"+reachability.Pattern()+"'") {
		t.Errorf("the CI step's pattern has drifted from reachability.Pattern().\nstep:\n%s\nwant the line:\n          forbidden='%s'", step, reachability.Pattern())
	}
	if !strings.Contains(step, "go list -deps "+reachability.GatewayBinary) {
		t.Errorf("the CI step does not check %s:\n%s", reachability.GatewayBinary, step)
	}
	if !strings.Contains(step, `if reached="$(`) {
		t.Errorf("the CI step does not use the `if` form, so it fails when nothing is wrong:\n%s", step)
	}
	if strings.Contains(step, "&& exit 1") {
		t.Errorf("the CI step ends a pipeline in `&& exit 1`, which also fails the clean case:\n%s", step)
	}
}

// ciStep returns the body of the named workflow step: everything indented under
// its `run: |` up to the next step or the end of the job.
func ciStep(t *testing.T, workflow, name string) string {
	t.Helper()
	marker := "- name: " + name + "\n"
	i := strings.Index(workflow, marker)
	if i < 0 {
		t.Fatalf("no CI step named %q; the reachability rule is not enforced anywhere", name)
	}
	rest := workflow[i+len(marker):]
	if j := strings.Index(rest, "\n      - name: "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
