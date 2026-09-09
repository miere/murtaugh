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
	"github.com/miere/murtaugh/internal/tools"
)

// nodeSplitTool (cfg.node.split) is #170 Change I's migration: it takes a single
// combined installation and gives the runtime node half a configuration root of
// its own.
//
//	murtaugh cfg node split
//	murtaugh cfg node split --dest ~/.config/murtaugh/node/config.yaml
//
// The destination flag is `--dest` and NOT `--config`, and that is a
// correctness requirement rather than taste. `--config` is a GLOBAL flag:
// cmd/murtaugh's extractConfigFlag scans the whole argv, strips every
// occurrence and keeps the last, so `murtaugh --config <gateway> cfg node split
// --config <node>` silently ran the whole command against the NODE path — the
// source became the destination, this tool saw no argument at all and aimed at
// config.DefaultNodePath(), and the global bootstrap had already seeded a root
// at the path the operator meant as the target. A tool-scoped flag that shares
// a name with a global one cannot be passed, so it gets its own name.
//
// It copies and deletes nothing — see internal/config/store/split.go — so it is
// safe to run against a live gateway, safe to run twice, and does not switch
// anything over. The gateway keeps answering in-process until somebody starts
// murtaugh-gateway instead, which is a choice of binary rather than a
// consequence of this command.
//
// The destination is a DIRECTORY, not a second file beside config.yaml. Two
// roles in one directory share a .env, a store and a node-token — and share
// internal/config/migrate's backup/restore, which reverts every top-level file
// in the directory it runs in, so a failed migration in one role would restore
// over the other's credentials.
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
	src, err := t.p()
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

	// Every argument is checked BEFORE anything is written. The seed address used
	// to be validated last, after the skeleton had been laid down and the rows
	// copied, so `--gateway http://…` left a seeded node root behind and reported
	// a failure — an operator's next command then found a directory that looks
	// initialised and holds a half-migration. Nothing here touches the disk.
	seed, err := nodeSeed(args)
	if err != nil {
		return nil, err
	}

	// Seeded with the NODE's skeleton, which carries no oauth block: a node has
	// no Slack connection and must never hold the workspace's tokens.
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

	// Written after the split, so a store that failed to validate does not leave
	// a half-configured node pointing at a gateway. Its validity was settled
	// before the first byte was written; see nodeSeed.
	if seed != nil {
		if err := dst.PutSingleton(ctx, config.SingletonNode, *seed); err != nil {
			return nil, fmt.Errorf("record the gateway address: %w", err)
		}
	}

	return okResult{Message: fmt.Sprintf(
		"copied %d rows into %s (%s); kept on the gateway: %s. Nothing here changed — mint this node a credential on the gateway with `murtaugh node token mint --node <name> --user <slack-user-id>`, put the printed secret at %s/node-token (mode 0600), and start murtaugh-runtime.",
		report.Total(), target, describe(report.Copied), describe(report.Kept), filepath.Dir(target))}, nil
}

// nodeSeed reads and validates the optional --gateway seed address, returning
// nil when none was given. It writes nothing: it exists so an invalid address is
// refused before the destination root is seeded.
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

// describe renders a per-section count in a stable order.
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

// sameFile reports whether two config paths name the same file, comparing
// cleaned absolute paths. A blank source path (a tool invoked without one)
// cannot collide with anything.
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

// NodeSplitTools returns the combined→split migration tool.
func NodeSplitTools(p Provider, configPath string) []tools.Tool {
	return []tools.Tool{&nodeSplitTool{p: p, configPath: configPath}}
}
