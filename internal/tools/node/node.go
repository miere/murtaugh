// Package node implements the `murtaugh node token …` admin surface: minting,
// listing and revoking the bearer credentials runtime nodes authenticate with
// (#190, part of the split in #170).
//
// Every tool here is a registry Tool, so it is exposed identically over the CLI
// and MCP — which is the reason for two deliberate restrictions:
//
//   - mint declares itself side-effecting (tools.ApprovalClassifier), so an
//     agent that has been given it still cannot issue a credential without a
//     human saying yes.
//   - mint's --token-file writes the plaintext to a 0600 file and returns only
//     the metadata, so an operator minting through an agent has a way to create
//     a credential without the credential landing in a transcript.
//
// list never returns a token, because nothing can: the store holds a digest.
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

// Provider opens the credential store. It returns an error when the store is
// unavailable (e.g. a setup invocation that could not open the database), so a
// tool fails cleanly rather than dereferencing nil.
//
// It hands back a fresh handle per invocation and the caller closes it. Minting
// a node credential is a rare administrative act — holding a database
// connection open for the daemon's lifetime against the chance that somebody
// enrols a node would be a cost paid by every gateway, including the ones that
// never will.
type Provider func(ctx context.Context) (config.NodeTokenStore, error)

// All returns every `node …` tool bound to the given provider.
func All(p Provider) []tools.Tool {
	return []tools.Tool{
		&mintTool{p: p},
		&listTool{p: p},
		&revokeTool{p: p},
	}
}

// open resolves the store, refusing a nil provider rather than panicking.
func (p Provider) open(ctx context.Context) (config.NodeTokenStore, error) {
	if p == nil {
		return nil, errors.New("the node credential store is unavailable")
	}
	return p(ctx)
}

// --- mint -------------------------------------------------------------------

type mintTool struct{ p Provider }

func (t *mintTool) Name() string { return "node.token.mint" }
func (t *mintTool) Description() string {
	return "Mint a bearer token for a runtime node. The token is shown once and only its hash is stored (e.g. `node token mint --node mac-mini --user U123`)."
}

// RequiresApproval satisfies tools.ApprovalClassifier: issuing a credential
// that speaks for a user is always worth a human's yes, whatever the args.
func (t *mintTool) RequiresApproval(map[string]any) bool { return true }

func (t *mintTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"node":       {Type: "string", Description: "node id this credential identifies (required)"},
			"user":       {Type: "string", Description: "Murtaugh user the node acts for (required)"},
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

	// The file is written BEFORE the record is stored. A credential in the store
	// that never reached its file is a live credential nobody holds — harmless
	// but permanent clutter that still has to be revoked; a file written for a
	// record that failed to store is a token that simply does not work, which
	// the operator finds out at the first handshake.
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

// mintResult is the one place a plaintext token is ever returned.
type mintResult struct {
	// Token is empty when --token-file was used.
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

// --- list -------------------------------------------------------------------

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

// credential is a listing row. There is no token field and no hash field: the
// hash is not useful to an operator and printing it would invite pasting it
// somewhere that treats it as a credential.
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

// stateOf renders a credential's lifecycle in one word.
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

// --- revoke -----------------------------------------------------------------

type revokeTool struct{ p Provider }

func (t *revokeTool) Name() string { return "node.token.revoke" }
func (t *revokeTool) Description() string {
	return "Revoke a node credential by selector, or every credential a node holds (e.g. `node token revoke --selector 1a2b…`)."
}

// RequiresApproval satisfies tools.ApprovalClassifier: revoking is how a node is
// cut off, and --node cuts off all of them at once.
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

	// Connections is left nil: nothing owns a live node connection yet (#193).
	// The Revoker is used anyway rather than calling the store directly, so the
	// day that changes it is one field, not a new call site.
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

// --- shared helpers ---------------------------------------------------------

// stamp renders a timestamp for display, with the zero time as the empty
// string so "never expires" and "not revoked" read as absent rather than as
// year one.
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
