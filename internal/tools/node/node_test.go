package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/nodetoken"
	"github.com/miere/murtaugh/internal/tools"
)

type fakeStore struct {
	records map[string]config.NodeToken
	closes  int
	putErr  error
}

func newFake() *fakeStore { return &fakeStore{records: map[string]config.NodeToken{}} }

func (s *fakeStore) Put(_ context.Context, token config.NodeToken) error {
	if s.putErr != nil {
		return s.putErr
	}
	if err := token.Validate(); err != nil {
		return err
	}
	if _, exists := s.records[token.Selector]; exists {
		return errors.New("selector already exists")
	}
	s.records[token.Selector] = token
	return nil
}

func (s *fakeStore) BySelector(_ context.Context, selector string) (config.NodeToken, bool, error) {
	record, ok := s.records[selector]
	return record, ok, nil
}

func (s *fakeStore) List(_ context.Context, nodeID string) ([]config.NodeToken, error) {
	var out []config.NodeToken
	for _, r := range s.records {
		if nodeID == "" || r.NodeID == nodeID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeStore) Revoke(_ context.Context, selector string, at time.Time) (config.NodeToken, bool, error) {
	record, ok := s.records[selector]
	if !ok {
		return config.NodeToken{}, false, nil
	}
	if record.RevokedAt.IsZero() {
		record.RevokedAt = at.UTC()
		s.records[selector] = record
	}
	return record, true, nil
}

func (s *fakeStore) Close() error { s.closes++; return nil }

func toolsFor(t *testing.T) (*fakeStore, *mintTool, *listTool, *revokeTool) {
	t.Helper()
	store := newFake()
	p := Provider(func(context.Context) (config.NodeTokenStore, error) { return store, nil })
	return store, &mintTool{p: p}, &listTool{p: p}, &revokeTool{p: p}
}

func mint(t *testing.T, tool *mintTool, args map[string]any) mintResult {
	t.Helper()
	res, err := tool.Invoke(context.Background(), args)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	out, ok := res.(mintResult)
	if !ok {
		t.Fatalf("mint returned %T, want mintResult", res)
	}
	return out
}

// The CLI resolves `murtaugh node token mint` to these names, so renaming one silently
// moves the command.
func TestRegisteredNames(t *testing.T) {
	_, m, l, r := toolsFor(t)
	for tool, want := range map[tools.Tool]string{
		m: "node.token.mint",
		l: "node.token.list",
		r: "node.token.revoke",
	} {
		if got := tool.Name(); got != want {
			t.Errorf("Name() = %q, want %q", got, want)
		}
	}
	if len(All(nil)) != 3 {
		t.Errorf("All returned %d tools, want 3", len(All(nil)))
	}
}

func TestMintedTokenVerifiesAgainstWhatWasStored(t *testing.T) {
	store, m, _, _ := toolsFor(t)

	res := mint(t, m, map[string]any{"node": "mac-mini", "user": "U123", "label": "desk"})
	if res.Token == "" {
		t.Fatal("mint returned no token")
	}

	record, err := nodetoken.Verify(context.Background(), store, res.Token, time.Now())
	if err != nil {
		t.Fatalf("the minted token does not verify: %v", err)
	}
	if record.NodeID != "mac-mini" || record.UserID != "U123" {
		t.Fatalf("verified to node %q / user %q, want mac-mini / U123", record.NodeID, record.UserID)
	}
	if record.Label != "desk" {
		t.Errorf("label = %q, want desk", record.Label)
	}

	stored := store.records[res.Selector]
	if strings.Contains(stored.SecretHash, res.Token) || strings.Contains(stored.SecretHash, nodetoken.Prefix) {
		t.Fatalf("the stored hash carries the token: %q", stored.SecretHash)
	}
	if store.closes != 1 {
		t.Errorf("the store was closed %d times, want 1: a handle per invocation is the whole reason it is opened lazily", store.closes)
	}
}

// A credential missing either would resolve to nobody.
func TestMintRequiresANodeAndAUser(t *testing.T) {
	_, m, _, _ := toolsFor(t)
	for name, args := range map[string]map[string]any{
		"no args":       {},
		"no user":       {"node": "mac-mini"},
		"no node":       {"user": "U012ABCDEF"},
		"blank node":    {"node": "   ", "user": "U012ABCDEF"},
		"blank user":    {"node": "mac-mini", "user": " "},
		"wrong types":   {"node": 7, "user": true},
		"empty strings": {"node": "", "user": ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := m.Invoke(context.Background(), args); err == nil {
				t.Fatal("mint issued a credential without a node and a user")
			}
		})
	}
}

// A node's owner is who its sign-ins are sent to and whose clicks are checked,
// and both match Slack user IDs only, so any other name would reach nobody.
func TestMintRefusesAnOwnerThatIsNotASlackUserID(t *testing.T) {
	store, m, _, _ := toolsFor(t)
	for _, user := range []string{"@miere", "miere", "u012abcdef", "C012ABCDEF", "U1", "U012-ABCDEF"} {
		t.Run(user, func(t *testing.T) {
			_, err := m.Invoke(context.Background(), map[string]any{"node": "mac-mini", "user": user})
			if err == nil {
				t.Fatalf("mint issued a credential for owner %q", user)
			}
			if !strings.Contains(err.Error(), "Slack user ID") || !strings.Contains(err.Error(), user) {
				t.Fatalf("the refusal does not say what was wrong: %v", err)
			}
		})
	}
	if len(store.records) != 0 {
		t.Fatalf("a refused mint stored %d credentials", len(store.records))
	}
	for _, user := range []string{"U012ABCDEF", "W012ABCDEF", " U012ABCDEF "} {
		if _, err := m.Invoke(context.Background(), map[string]any{"node": "mac-mini", "user": user}); err != nil {
			t.Fatalf("mint refused the Slack user ID %q: %v", user, err)
		}
	}
}

// An expiry accepted but ignored would leave an operator believing a credential dies on
// its own, when only revocation will ever end it.
func TestMintExpiryIsAppliedAndValidated(t *testing.T) {
	store, m, _, _ := toolsFor(t)

	before := time.Now().UTC()
	res := mint(t, m, map[string]any{"node": "n1", "user": "U012ABCDEF", "expires_in": "24h"})
	stored := store.records[res.Selector]
	if stored.ExpiresAt.IsZero() {
		t.Fatal("--expires-in was accepted but no expiry was stored")
	}
	if got := stored.ExpiresAt.Sub(before); got < 23*time.Hour || got > 25*time.Hour {
		t.Fatalf("expiry is %s away, want about 24h", got)
	}
	if res.ExpiresAt == "" {
		t.Error("the result does not report the expiry it just set")
	}
	if _, err := nodetoken.Verify(context.Background(), store, res.Token, stored.ExpiresAt); !errors.Is(err, nodetoken.ErrExpired) {
		t.Fatalf("Verify at the expiry instant = %v, want ErrExpired", err)
	}

	for name, raw := range map[string]string{
		"nonsense":   "soon",
		"negative":   "-1h",
		"zero":       "0s",
		"bare digit": "24",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := m.Invoke(context.Background(), map[string]any{"node": "n1", "user": "U012ABCDEF", "expires_in": raw}); err == nil {
				t.Fatalf("mint accepted --expires-in %q", raw)
			}
		})
	}
}

// An MCP client renders the result into a transcript, so with a token file the result
// must carry only metadata.
func TestMintToTokenFileKeepsThePlaintextOutOfTheResult(t *testing.T) {
	store, m, _, _ := toolsFor(t)
	path := filepath.Join(t.TempDir(), "node-token")

	res := mint(t, m, map[string]any{"node": "n1", "user": "U012ABCDEF", "token_file": path})
	if res.Token != "" {
		t.Fatal("the result carries the plaintext even though it was written to a file")
	}

	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), nodetoken.Prefix) {
		t.Fatalf("the marshalled result carries something token-shaped: %s", encoded)
	}
	if strings.Contains(res.String(), nodetoken.Prefix) {
		t.Fatalf("the rendered result carries something token-shaped: %s", res.String())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != nodetoken.FileMode {
		t.Fatalf("token file is mode %#o, want %#o", perm, nodetoken.FileMode)
	}
	token, err := nodetoken.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if _, err := nodetoken.Verify(context.Background(), store, token, time.Now()); err != nil {
		t.Fatalf("the token written to the file does not verify: %v", err)
	}
}

// The store cannot give the plaintext back, so an operator not told this would expect
// to look it up later.
func TestMintTellsTheOperatorTheTokenIsShownOnce(t *testing.T) {
	_, m, _, _ := toolsFor(t)
	rendered := mint(t, m, map[string]any{"node": "n1", "user": "U012ABCDEF"}).String()
	if !strings.Contains(rendered, "last time it can be shown") {
		t.Fatalf("the CLI rendering does not warn that the token cannot be shown again:\n%s", rendered)
	}
}

// Both can be put in an agent's toolset; neither may run on the agent's say-so alone.
func TestMintAndRevokeRequireApproval(t *testing.T) {
	_, m, l, r := toolsFor(t)
	for _, tool := range []tools.Tool{m, r} {
		classifier, ok := tool.(tools.ApprovalClassifier)
		if !ok {
			t.Fatalf("%s does not implement tools.ApprovalClassifier, so an agent holding it can issue or revoke credentials unattended", tool.Name())
		}
		if !classifier.RequiresApproval(map[string]any{"node": "n1", "user": "U012ABCDEF"}) {
			t.Errorf("%s does not require approval", tool.Name())
		}
	}
	if _, gated := any(l).(tools.ApprovalClassifier); gated {
		t.Error("node.token.list now requires approval; if that is intended, say so here")
	}
}

// The store holds no plaintext, so the risk is the digest: printed, it looks like a
// credential and invites being pasted as one.
func TestListNeverShowsATokenOrAHash(t *testing.T) {
	store, m, l, _ := toolsFor(t)
	res := mint(t, m, map[string]any{"node": "mac-mini", "user": "U012ABCDEF", "label": "desk"})
	hash := store.records[res.Selector].SecretHash

	listed, err := l.Invoke(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	encoded, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	rendered := listed.(listResult).String()
	for _, text := range []string{string(encoded), rendered} {
		if strings.Contains(text, res.Token) || strings.Contains(text, nodetoken.Prefix) {
			t.Errorf("a listing carries something token-shaped:\n%s", text)
		}
		if strings.Contains(text, hash) {
			t.Errorf("a listing carries the stored digest:\n%s", text)
		}
		if !strings.Contains(text, res.Selector) {
			t.Errorf("a listing omits the selector, which is what revoke is addressed by:\n%s", text)
		}
	}
}

// Covers all three states, because without an expired credential a broken expiry check
// still passes and timed-out credentials show as live.
func TestListFiltersByNodeAndReportsState(t *testing.T) {
	_, m, l, r := toolsFor(t)
	live := mint(t, m, map[string]any{"node": "a", "user": "U012ABCDEF"})
	revoked := mint(t, m, map[string]any{"node": "a", "user": "U012ABCDEF"})
	expired := mint(t, m, map[string]any{"node": "a", "user": "U012ABCDEF", "expires_in": "1ms"})
	other := mint(t, m, map[string]any{"node": "b", "user": "U012ABCDEF"})

	if _, err := r.Invoke(context.Background(), map[string]any{"selector": revoked.Selector}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	res, err := l.Invoke(context.Background(), map[string]any{"node": "a"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	listed := res.(listResult)
	if len(listed.Credentials) != 3 {
		t.Fatalf("listing node a returned %d credentials, want 3", len(listed.Credentials))
	}
	states := map[string]string{}
	for _, c := range listed.Credentials {
		if c.Selector == other.Selector {
			t.Fatal("node b's credential appeared in node a's listing")
		}
		states[c.Selector] = c.State
	}
	if states[live.Selector] != "live" {
		t.Errorf("the live credential is reported %q", states[live.Selector])
	}
	if states[revoked.Selector] != "revoked" {
		t.Errorf("the revoked credential is reported %q", states[revoked.Selector])
	}
	if states[expired.Selector] != "expired" {
		t.Errorf("the timed-out credential is reported %q, want \"expired\"", states[expired.Selector])
	}
}

// The operator is asking why it stopped working, and a deliberate revocation answers
// that better than an expiry that came after it.
func TestRevocationOutranksExpiryInTheListing(t *testing.T) {
	_, m, l, r := toolsFor(t)
	res := mint(t, m, map[string]any{"node": "a", "user": "U012ABCDEF", "expires_in": "1ms"})
	if _, err := r.Invoke(context.Background(), map[string]any{"selector": res.Selector}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	listed, err := l.Invoke(context.Background(), map[string]any{"node": "a"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	creds := listed.(listResult).Credentials
	if len(creds) != 1 {
		t.Fatalf("listing returned %d credentials, want 1", len(creds))
	}
	if creds[0].State != "revoked" {
		t.Errorf("a revoked credential past its expiry is reported %q, want \"revoked\"", creds[0].State)
	}
}

// Not named for rotation on purpose: that would promise a rotation mechanism that does
// not exist yet.
func TestTwoCredentialsForOneNodeThenRevokeTheOlder(t *testing.T) {
	store, m, _, r := toolsFor(t)
	ctx := context.Background()

	old := mint(t, m, map[string]any{"node": "mac-mini", "user": "U012ABCDEF", "label": "old"})
	fresh := mint(t, m, map[string]any{"node": "mac-mini", "user": "U012ABCDEF", "label": "new"})

	at := time.Now()
	for name, token := range map[string]string{"the old credential": old.Token, "the new credential": fresh.Token} {
		record, err := nodetoken.Verify(ctx, store, token, at)
		if err != nil {
			t.Fatalf("%s does not verify during the overlap: %v", name, err)
		}
		if record.NodeID != "mac-mini" {
			t.Fatalf("%s verified to node %q", name, record.NodeID)
		}
	}

	if _, err := r.Invoke(ctx, map[string]any{"selector": old.Selector}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := nodetoken.Verify(ctx, store, old.Token, at); !errors.Is(err, nodetoken.ErrRevoked) {
		t.Fatalf("the withdrawn credential = %v, want ErrRevoked", err)
	}
	if _, err := nodetoken.Verify(ctx, store, fresh.Token, at); err != nil {
		t.Fatalf("revoking the old credential took the new one down too: %v", err)
	}
}

// For a compromised node, when the operator does not know which credential leaked.
func TestRevokeByNodeTakesEveryLiveCredential(t *testing.T) {
	store, m, _, r := toolsFor(t)
	ctx := context.Background()

	first := mint(t, m, map[string]any{"node": "mac-mini", "user": "U012ABCDEF"})
	second := mint(t, m, map[string]any{"node": "mac-mini", "user": "U012ABCDEF"})
	bystander := mint(t, m, map[string]any{"node": "other", "user": "U012ABCDEF"})

	res, err := r.Invoke(ctx, map[string]any{"node": "mac-mini"})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := len(res.(revokeResult).Revoked); got != 2 {
		t.Fatalf("revoked %d credentials, want 2", got)
	}
	for _, token := range []string{first.Token, second.Token} {
		if _, err := nodetoken.Verify(ctx, store, token, time.Now()); !errors.Is(err, nodetoken.ErrRevoked) {
			t.Errorf("a credential of the revoked node still verifies: %v", err)
		}
	}
	if _, err := nodetoken.Verify(ctx, store, bystander.Token, time.Now()); err != nil {
		t.Errorf("another node's credential was revoked too: %v", err)
	}

	again, err := r.Invoke(ctx, map[string]any{"node": "mac-mini"})
	if err != nil {
		t.Fatalf("a second revoke by node errored: %v", err)
	}
	if got := len(again.(revokeResult).Revoked); got != 0 {
		t.Fatalf("the second sweep revoked %d credentials, want 0", got)
	}
}

// Guessing which selector the operator meant, or silently doing nothing, is worse than
// refusing.
func TestRevokeArgumentErrors(t *testing.T) {
	_, _, _, r := toolsFor(t)
	for name, args := range map[string]map[string]any{
		"neither":       {},
		"both":          {"selector": "abc", "node": "mac-mini"},
		"blank both":    {"selector": "  ", "node": "  "},
		"unknown":       {"selector": "0123456789abcdef"},
		"unknown types": {"selector": 12},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Invoke(context.Background(), args); err == nil {
				t.Fatalf("revoke accepted %v", args)
			}
		})
	}
}

// The drop lags the revoke by up to one gateway re-check; an operator who
// expected it at once would chase a node that is about to go.
func TestRevokeSaysWhenLiveConnectionsDrop(t *testing.T) {
	_, m, _, r := toolsFor(t)
	minted := mint(t, m, map[string]any{"node": "n1", "user": "U012ABCDEF"})

	res, err := r.Invoke(context.Background(), map[string]any{"selector": minted.Selector})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !strings.Contains(res.(revokeResult).String(), nodetoken.RevocationNotice) {
		t.Fatalf("the revoke rendering does not say when live connections drop:\n%s", res.(revokeResult).String())
	}
}

// A `setup` invocation may have no database, and a nil dereference there would take
// the whole CLI down instead of one command.
func TestAnUnavailableStoreFailsCleanly(t *testing.T) {
	down := Provider(func(context.Context) (config.NodeTokenStore, error) {
		return nil, errors.New("no config directory resolved")
	})
	for _, tool := range All(down) {
		if _, err := tool.Invoke(context.Background(), map[string]any{"node": "n1", "user": "U012ABCDEF", "selector": "abc"}); err == nil {
			t.Errorf("%s succeeded with no store", tool.Name())
		}
	}
	for _, tool := range All(nil) {
		if _, err := tool.Invoke(context.Background(), map[string]any{"node": "n1", "user": "U012ABCDEF", "selector": "abc"}); err == nil {
			t.Errorf("%s succeeded with a nil provider", tool.Name())
		}
	}
}

// The file is written before the record is stored, so an overwrite could trade a
// working credential for one that never reaches the store.
func TestMintDoesNotOverwriteAnExistingTokenFile(t *testing.T) {
	store, m, _, _ := toolsFor(t)
	path := filepath.Join(t.TempDir(), "node-token")
	if err := os.WriteFile(path, []byte("mrtg_node_existing\n"), nodetoken.FileMode); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Invoke(context.Background(), map[string]any{"node": "n1", "user": "U012ABCDEF", "token_file": path}); err == nil {
		t.Fatal("mint overwrote an existing credential file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "mrtg_node_existing" {
		t.Fatal("the existing credential file was modified")
	}
	if len(store.records) != 0 {
		t.Fatal("a credential was stored even though the file could not be written")
	}
}

// The CLI maps --kebab-case flags onto declared property names, so an argument missing
// from the schema is one no operator can pass.
func TestEverySchemaDeclaresItsArguments(t *testing.T) {
	_, m, l, r := toolsFor(t)
	want := map[string][]string{
		"node.token.mint":   {"node", "user", "label", "expires_in", "token_file"},
		"node.token.list":   {"node"},
		"node.token.revoke": {"selector", "node"},
	}
	for _, tool := range []tools.Tool{m, l, r} {
		schema := tool.InputSchema()
		if schema == nil || schema.Type != "object" {
			t.Fatalf("%s has no object schema", tool.Name())
		}
		for _, prop := range want[tool.Name()] {
			if _, ok := schema.Properties[prop]; !ok {
				t.Errorf("%s does not declare %q", tool.Name(), prop)
			}
		}
		if got, expected := len(schema.Properties), len(want[tool.Name()]); got != expected {
			t.Errorf("%s declares %d properties, want %d — update this test with the new flag and cli-help.md with its description", tool.Name(), got, expected)
		}
		if strings.TrimSpace(tool.Description()) == "" {
			t.Errorf("%s has no description", tool.Name())
		}
	}
}
