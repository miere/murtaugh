// Package node's tools also reach agents over MCP, so minting needs a human's yes and
// --token-file keeps the plaintext token out of the agent's transcript.
package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/nodetoken"
	"github.com/miere/murtaugh/internal/tools"
)

// Provider opens a fresh store per call, closed by the caller: minting is too rare to
// justify every gateway holding a database connection open for it.
type Provider func(ctx context.Context) (config.NodeTokenStore, error)

func All(p Provider) []tools.Tool {
	return []tools.Tool{
		&mintTool{p: p},
		&listTool{p: p},
		&revokeTool{p: p},
	}
}

func (p Provider) open(ctx context.Context) (config.NodeTokenStore, error) {
	if p == nil {
		return nil, errors.New("the node credential store is unavailable")
	}
	return p(ctx)
}

type mintTool struct{ p Provider }

func (t *mintTool) Name() string { return "node.token.mint" }
func (t *mintTool) Description() string {
	return "Mint a bearer token for a runtime node. The token is shown once and only its hash is stored (e.g. `node token mint --node mac-mini --user U123`)."
}

func (t *mintTool) RequiresApproval(map[string]any) bool { return true }

func (t *mintTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"node":       {Type: "string", Description: "node id this credential identifies (required)"},
			"user":       {Type: "string", Description: "Slack user ID (U… or W…) of the node's owner, who receives its sign-in requests; a handle or name is refused (required)"},
			"label":      {Type: "string", Description: "free-text note about where this credential lives"},
			"expires_in": {Type: "string", Description: "lifetime as a Go duration (e.g. 720h); omitted means it lasts until revoked"},
			"token_file": {Type: "string", Description: "write the token to this file (mode 0600) instead of returning it"},
		},
	}
}

func (t *mintTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	nodeID, err := requireString(args, "node")
	if err != nil {
		return nil, err
	}
	userID, err := requireString(args, "user")
	if err != nil {
		return nil, err
	}
	if !config.IsSlackUserID(userID) {
		return nil, fmt.Errorf("--user must be the Slack user ID of the node's owner (like U012ABCDEF or W012ABCDEF), not %q", userID)
	}
	label, _ := stringArg(args, "label")

	now := time.Now().UTC()
	var expiresAt time.Time
	if raw, ok := stringArg(args, "expires_in"); ok && strings.TrimSpace(raw) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("--expires-in must be a Go duration (e.g. 720h): %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("--expires-in must be positive, got %s", d)
		}
		expiresAt = now.Add(d)
	}

	minted, err := nodetoken.Mint()
	if err != nil {
		return nil, err
	}
	record := config.NodeToken{
		Selector:   minted.Selector,
		SecretHash: string(minted.SecretHash),
		NodeID:     nodeID,
		UserID:     userID,
		Label:      label,
		CreatedAt:  now,
		ExpiresAt:  expiresAt,
	}

	tokenFile, _ := stringArg(args, "token_file")
	tokenFile = strings.TrimSpace(tokenFile)
	if tokenFile != "" {
		if err := nodetoken.WriteFile(tokenFile, minted.Token); err != nil {
			return nil, err
		}
	}

	store, err := t.p.open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()

	if err := store.Put(ctx, record); err != nil {
		return nil, err
	}

	result := mintResult{
		Selector:  record.Selector,
		NodeID:    record.NodeID,
		UserID:    record.UserID,
		Label:     record.Label,
		ExpiresAt: stamp(record.ExpiresAt),
		TokenFile: tokenFile,
	}
	if tokenFile == "" {
		result.Token = minted.Token
	}
	return result, nil
}

type mintResult struct {
	Token     string `json:"token,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	Selector  string `json:"selector"`
	NodeID    string `json:"node_id"`
	UserID    string `json:"user_id"`
	Label     string `json:"label,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

func (r mintResult) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "minted a credential for node %q acting for %s\n", r.NodeID, r.UserID)
	fmt.Fprintf(&b, "  selector:  %s\n", r.Selector)
	if r.Label != "" {
		fmt.Fprintf(&b, "  label:     %s\n", r.Label)
	}
	if r.ExpiresAt != "" {
		fmt.Fprintf(&b, "  expires:   %s\n", r.ExpiresAt)
	} else {
		b.WriteString("  expires:   never (revoke it to end it)\n")
	}
	if r.TokenFile != "" {
		fmt.Fprintf(&b, "  token:     written to %s (mode 0600)\n", r.TokenFile)
		return b.String()
	}
	fmt.Fprintf(&b, "  token:     %s\n", r.Token)
	b.WriteString("\nCopy the token now — only its hash is stored, so this is the last time it can be shown.\n")
	b.WriteString("Put it on the node mode 0600. A seatbelt-confined agent is denied it unconditionally;\n")
	b.WriteString("with the sandbox off (the default) an agent this node spawns can read it.\n")
	return b.String()
}

type listTool struct{ p Provider }

func (t *listTool) Name() string { return "node.token.list" }
func (t *listTool) Description() string {
	return "List issued node credentials and their state. Never shows a token — only its hash is stored."
}
func (t *listTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"node": {Type: "string", Description: "only this node's credentials (omitted lists every node's)"},
		},
	}
}

func (t *listTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	nodeID, _ := stringArg(args, "node")

	store, err := t.p.open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()

	records, err := store.List(ctx, strings.TrimSpace(nodeID))
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := listResult{Node: strings.TrimSpace(nodeID), Credentials: make([]credential, 0, len(records))}
	for _, r := range records {
		out.Credentials = append(out.Credentials, credential{
			Selector:  r.Selector,
			NodeID:    r.NodeID,
			UserID:    r.UserID,
			Label:     r.Label,
			CreatedAt: stamp(r.CreatedAt),
			ExpiresAt: stamp(r.ExpiresAt),
			RevokedAt: stamp(r.RevokedAt),
			State:     stateOf(r, now),
		})
	}
	return out, nil
}

type credential struct {
	Selector  string `json:"selector"`
	NodeID    string `json:"node_id"`
	UserID    string `json:"user_id"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at,omitempty"`
	RevokedAt string `json:"revoked_at,omitempty"`
	State     string `json:"state"`
}

type listResult struct {
	Node        string       `json:"node,omitempty"`
	Credentials []credential `json:"credentials"`
}

func (r listResult) String() string {
	if len(r.Credentials) == 0 {
		if r.Node != "" {
			return fmt.Sprintf("no credentials issued for node %q", r.Node)
		}
		return "no node credentials issued"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "node credentials (%d):\n", len(r.Credentials))
	for _, c := range r.Credentials {
		fmt.Fprintf(&b, "  %s  %-16s %-10s %s", c.Selector, c.NodeID, c.State, c.CreatedAt)
		if c.Label != "" {
			fmt.Fprintf(&b, "  (%s)", c.Label)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func stateOf(t config.NodeToken, now time.Time) string {
	switch {
	case !t.RevokedAt.IsZero():
		return "revoked"
	case !t.Live(now):
		return "expired"
	default:
		return "live"
	}
}

type revokeTool struct{ p Provider }

func (t *revokeTool) Name() string { return "node.token.revoke" }
func (t *revokeTool) Description() string {
	return "Revoke a node credential by selector, or every credential a node holds (e.g. `node token revoke --selector 1a2b…`)."
}

func (t *revokeTool) RequiresApproval(map[string]any) bool { return true }

func (t *revokeTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"selector": {Type: "string", Description: "the credential to revoke (from `node token list`)"},
			"node":     {Type: "string", Description: "revoke every credential this node holds (mutually exclusive with --selector)"},
		},
	}
}

func (t *revokeTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	selector, hasSelector := stringArg(args, "selector")
	nodeID, hasNode := stringArg(args, "node")
	selector, nodeID = strings.TrimSpace(selector), strings.TrimSpace(nodeID)
	hasSelector = hasSelector && selector != ""
	hasNode = hasNode && nodeID != ""

	switch {
	case hasSelector && hasNode:
		return nil, errors.New("--selector and --node are mutually exclusive")
	case !hasSelector && !hasNode:
		return nil, errors.New("--selector or --node is required")
	}

	store, err := t.p.open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()

	revoker := nodetoken.Revoker{Store: store}

	selectors := []string{selector}
	if hasNode {
		records, err := store.List(ctx, nodeID)
		if err != nil {
			return nil, err
		}
		selectors = selectors[:0]
		for _, r := range records {
			if r.RevokedAt.IsZero() {
				selectors = append(selectors, r.Selector)
			}
		}
		if len(selectors) == 0 {
			return revokeResult{Node: nodeID}, nil
		}
	}

	now := time.Now().UTC()
	out := revokeResult{Node: nodeID}
	for _, sel := range selectors {
		record, found, err := revoker.Revoke(ctx, sel, now)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("no credential with selector %q", sel)
		}
		out.Revoked = append(out.Revoked, credential{
			Selector:  record.Selector,
			NodeID:    record.NodeID,
			UserID:    record.UserID,
			Label:     record.Label,
			CreatedAt: stamp(record.CreatedAt),
			ExpiresAt: stamp(record.ExpiresAt),
			RevokedAt: stamp(record.RevokedAt),
			State:     stateOf(record, now),
		})
	}
	return out, nil
}

type revokeResult struct {
	Node    string       `json:"node,omitempty"`
	Revoked []credential `json:"revoked"`
}

func (r revokeResult) String() string {
	if len(r.Revoked) == 0 {
		return fmt.Sprintf("node %q has no live credentials to revoke", r.Node)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "revoked %d credential(s):\n", len(r.Revoked))
	for _, c := range r.Revoked {
		fmt.Fprintf(&b, "  %s  %s  (revoked %s)\n", c.Selector, c.NodeID, c.RevokedAt)
	}
	fmt.Fprintf(&b, "\nNote: %s\n", nodetoken.RevocationLimitation)
	return b.String()
}

func stamp(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

func stringArg(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

func requireString(args map[string]any, key string) (string, error) {
	v, ok := stringArg(args, key)
	if !ok || strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("--%s is required", strings.ReplaceAll(key, "_", "-"))
	}
	return strings.TrimSpace(v), nil
}
