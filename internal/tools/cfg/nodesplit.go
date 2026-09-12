package cfg

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/store"
	"github.com/miere/murtaugh/internal/nodetoken"
	"github.com/miere/murtaugh/internal/tools"
)

type nodeSplitTool struct {
	p          Provider
	configPath string
}

func (t *nodeSplitTool) Name() string { return "cfg.node.split" }
func (t *nodeSplitTool) Description() string {
	return "Give the runtime node half of this configuration its own root (agents, MCP servers, jobs, chat, defaults). Copies; changes nothing here."
}
func (t *nodeSplitTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"dest": {Type: "string", Description: "the node's config.yaml path (default ~/.config/murtaugh/node/config.yaml). Named dest, not config: --config is a global flag and would be consumed before this tool sees it"},
			"gateway": {
				Type:        "string",
				Description: "gateway seed address to record in the node's config, ws:// or wss://",
			},
		},
	}
}

func (t *nodeSplitTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	src, err := t.p.Store()
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(mustString(args, "dest"))
	if target == "" {
		if target, err = config.DefaultNodePath(); err != nil {
			return nil, err
		}
	}
	if same, err := sameFile(target, t.configPath); err != nil {
		return nil, err
	} else if same {
		return nil, fmt.Errorf("the node configuration must not be this one (%s); give --dest a path in its own directory", target)
	}

	seed, err := nodeSeed(args)
	if err != nil {
		return nil, err
	}

	if err := config.BootstrapNode(target); err != nil {
		return nil, fmt.Errorf("prepare the node configuration directory: %w", err)
	}
	boot, err := config.LoadBootstrap(target)
	if err != nil {
		return nil, err
	}
	dst, err := store.Open(ctx, boot.Database, filepath.Dir(target), config.BaseNameOf(target))
	if err != nil {
		return nil, fmt.Errorf("open the node's config store: %w", err)
	}
	defer func() { _ = dst.Close() }()

	report, err := store.SplitForNode(ctx, src, dst)
	if err != nil {
		return nil, err
	}

	if seed != nil {
		if err := dst.PutSingleton(ctx, config.SingletonNode, *seed); err != nil {
			return nil, fmt.Errorf("record the gateway address: %w", err)
		}
	}

	return okResult{Message: fmt.Sprintf(
		"copied %d rows into %s (%s); kept on the gateway: %s. Nothing here changed — mint the node's credential with "+
			"`murtaugh node token mint --node <id> --user <U…> --token-file %s`, then start murtaugh-runtime.",
		report.Total(), target, describe(report.Copied), describe(report.Kept), nodetoken.PathFor(filepath.Dir(target)))}, nil
}

func nodeSeed(args map[string]any) (*config.NodeConfig, error) {
	address := strings.TrimSpace(mustString(args, "gateway"))
	if address == "" {
		return nil, nil
	}
	node := config.NodeConfig{Gateway: []string{address}}
	if err := node.Validate(); err != nil {
		return nil, err
	}
	return &node, nil
}

func describe(counts map[string]int) string {
	if len(counts) == 0 {
		return "nothing"
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", key, counts[key]))
	}
	return strings.Join(parts, ", ")
}

func sameFile(a, b string) (bool, error) {
	if strings.TrimSpace(b) == "" {
		return false, nil
	}
	absA, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	absB, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}
	return absA == absB, nil
}

func NodeSplitTools(p Provider, configPath string) []tools.Tool {
	return []tools.Tool{&nodeSplitTool{p: p, configPath: configPath}}
}
