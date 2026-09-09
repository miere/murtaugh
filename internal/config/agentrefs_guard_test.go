package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// The invariant agentrefs.go rests on is symmetric — "this list and the
// name→body checks in Validate are the same set" — and until this file only one
// half of it was guarded.
//
// TestAgentReferencesNamesEveryNameToBodySite compares the list against a
// hard-coded seven, so REMOVING a site from AgentReferences fails. ADDING an
// eighth check to Validate and not adding it here changed nothing any test
// observed — and that is the direction a contributor actually takes, because
// Validate is where checks live. The cost of missing it is stated in the commit
// that introduced the split: a reference added to Validate and not here is a
// name whose typo the gateway silently never reports.
//
// So both halves are guarded here, by two different means, because they are two
// different questions:
//
//   - The sites that exist today are checked BEHAVIOURALLY, by running Validate
//     under both roles over a configuration that names a different agent at
//     every one of them.
//   - A site that does not exist yet cannot be run, so it is caught in the
//     SOURCE: every role-gated name→body check in this package is counted, and
//     the count is pinned.

// roleGatedChecks is how many places in this package defer a name→body lookup
// on the process's role.
//
// There are exactly two ways to write one — the `resolvesNames` flag inside
// Validate, and agentSet.unknown, which is that flag carried into the helpers —
// so counting both counts all of them.
//
// If you are here because this number is wrong: you added or removed a check
// that resolves an agent NAME against a profile BODY. Add or remove the matching
// site in Config.AgentReferences, extend everyDistinctReferenceConfig below so
// the behavioural test covers it, and then update this number. A check that is
// deferred on a gateway and not carried by AgentReferences is never made
// anywhere.
const roleGatedChecks = 8

func TestEveryRoleGatedCheckIsAccountedFor(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(f fs.FileInfo) bool {
		return !strings.HasSuffix(f.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse the config package: %v", err)
	}
	pkg, ok := pkgs["config"]
	if !ok {
		t.Fatal("the config package did not parse; this guard reads its own source")
	}

	// The flag's own declaration is not a check, so its identifier is located
	// first and skipped by position when the uses are counted.
	declared := map[token.Pos]bool{}
	for _, file := range pkg.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || assign.Tok != token.DEFINE {
				return true
			}
			for _, lhs := range assign.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == "resolvesNames" {
					declared[id.Pos()] = true
				}
			}
			return true
		})
	}

	found := 0
	where := map[string]int{}
	for name, file := range pkg.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.Ident:
				if node.Name == "resolvesNames" && !declared[node.Pos()] {
					found++
					where[name]++
				}
			case *ast.SelectorExpr:
				if node.Sel.Name == "unknown" {
					found++
					where[name]++
				}
			}
			return true
		})
	}
	if found != roleGatedChecks {
		t.Fatalf("this package has %d role-gated name→body checks, want %d (%v).\n"+
			"Config.AgentReferences must name every one of them: it is the only thing that carries "+
			"these names to internal/nodehost, which is where a gateway resolves them now. "+
			"See the note on roleGatedChecks.", found, roleGatedChecks, where)
	}
}

// everyDistinctReferenceConfig names a DIFFERENT agent at every reference site,
// so a name found in Validate's output can only have come from one of them.
//
// everyReferenceConfig reuses names across sites, which is right for the list
// test next door and useless here.
func everyDistinctReferenceConfig() Config {
	cfg := everyReferenceConfig()
	cfg.Chat.Defaults.Agent = "site-chat-default"
	cfg.Chat.Defaults.DMAgent = "site-dm-agent"
	cfg.Chat.Defaults.DMAgents = map[string]string{"U1": "site-dm-agents"}
	cfg.Chat.Channels = ChannelRules{{Match: "nc-*", Agent: "site-channel"}}
	cfg.Jobs = map[string]JobProfile{
		"nightly": {Agent: "site-job", Prompt: "sweep", Schedule: "0 3 * * *"},
	}
	cfg.WorkflowRules = map[string]WorkflowRuleConfig{
		"deploy": {
			RequestEvent: "interactive",
			Match:        map[string]any{"callback_id": "deploy"},
			Triggers: []TriggerConfig{
				{Type: "delegate-to-agent", DelegateToAgent: &DelegateToAgentConfig{Agent: "site-workflow", Prompt: "go"}},
			},
		},
	}
	cfg.UnfurlRules = map[string]UnfurlRuleConfig{
		"tickets": {
			Match:  UnfurlMatchConfig{Domain: "example.com"},
			Unfurl: UnfurlActionConfig{DelegateToAgent: &DelegateToAgentConfig{Agent: "site-unfurl", Prompt: "summarise"}},
		},
	}
	// No bodies at all, so every one of those names is unresolvable and the
	// combined install has to complain about each.
	cfg.Agents = map[string]AgentProfile{}
	cfg.OAuth = OAuthConfig{AppToken: "x", BotToken: "x"}
	return cfg
}

// TestTheTwoHalvesOfTheInvariantAgreeSiteBySite is the behavioural half.
//
// Every name AgentReferences reports must be one a combined install refuses and
// a gateway defers. A site in the list that Validate never checked would be a
// name a node's own configuration stopped rejecting; a site Validate checks that
// the list omits would be a name the gateway never reports.
func TestTheTwoHalvesOfTheInvariantAgreeSiteBySite(t *testing.T) {
	cfg := everyDistinctReferenceConfig()

	refs := cfg.AgentReferences()
	if len(refs) != 7 {
		t.Fatalf("the fixture exercises %d reference sites, want 7: %+v", len(refs), refs)
	}

	combined := cfg
	combined.Role = RoleCombined
	combinedErr := combined.Validate()
	if combinedErr == nil {
		t.Fatal("a combined install accepted seven agent names with no agents defined")
	}

	gateway := cfg
	gateway.Role = RoleGateway
	gatewayText := ""
	if err := gateway.Validate(); err != nil {
		gatewayText = err.Error()
	}

	for _, ref := range refs {
		if !strings.Contains(combinedErr.Error(), ref.Name) {
			t.Errorf("%s names %q and a combined install did not refuse it; "+
				"AgentReferences carries a site Validate never checked:\n%v", ref.Field, ref.Name, combinedErr)
		}
		if strings.Contains(gatewayText, ref.Name) {
			t.Errorf("%s names %q and a gateway still refused it; the check did not move to connect time:\n%s",
				ref.Field, ref.Name, gatewayText)
		}
	}
}
