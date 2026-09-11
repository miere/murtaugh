package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

const roleGatedChecks = 8

// If this count changed, you added or removed an agent-name check in Validate: update
// Config.AgentReferences and everyDistinctReferenceConfig to match before bumping it.
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
			"See the note on this test.", found, roleGatedChecks, where)
	}
}

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
	cfg.Agents = map[string]AgentProfile{}
	cfg.OAuth = OAuthConfig{AppToken: "x", BotToken: "x"}
	return cfg
}

// A name AgentReferences lists that Validate never checks stops being rejected on a node; one
// Validate checks that the list omits is never reported by a gateway.
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
