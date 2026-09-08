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

// fakeStore is an in-memory config.NodeTokenStore. The three real backends are
// held to the same contract in internal/config/store; here the store is a fake
// so these tests are about the tools.
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

// toolsFor returns the three tools bound to one fake store.
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

// TestRegisteredNames pins the dotted names the CLI resolves `murtaugh node
// token mint` to. Renaming one silently moves the command.
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

// TestMintedTokenVerifiesAgainstWhatWasStored is the end-to-end statement of
// #190's first property: the operator gets a token, the store gets a digest, and
// presenting the token alone resolves to the node and user it was minted for.
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

	// What was stored is a digest of the secret and not the secret.
	stored := store.records[res.Selector]
	if strings.Contains(stored.SecretHash, res.Token) || strings.Contains(stored.SecretHash, nodetoken.Prefix) {
		t.Fatalf("the stored hash carries the token: %q", stored.SecretHash)
	}
	if store.closes != 1 {
		t.Errorf("the store was closed %d times, want 1: a handle per invocation is the whole reason it is opened lazily", store.closes)
	}
}

// TestMintRequiresANodeAndAUser: config.NodeToken.Validate demands both, and a
// tool that let either through would create a credential resolving to nobody.
func TestMintRequiresANodeAndAUser(t *testing.T) {
	_, m, _, _ := toolsFor(t)
	for name, args := range map[string]map[string]any{
		"no args":       {},
		"no user":       {"node": "mac-mini"},
		"no node":       {"user": "U1"},
		"blank node":    {"node": "   ", "user": "U1"},
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

// TestMintExpiryIsAppliedAndValidated. Expiry is optional, but a value that was
// accepted and quietly ignored would leave an operator believing a credential
// dies on its own when nothing will ever end it but revocation.
func TestMintExpiryIsAppliedAndValidated(t *testing.T) {
	store, m, _, _ := toolsFor(t)

	before := time.Now().UTC()
	res := mint(t, m, map[string]any{"node": "n1", "user": "U1", "expires_in": "24h"})
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
	// And it really stops verifying then.
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
			if _, err := m.Invoke(context.Background(), map[string]any{"node": "n1", "user": "U1", "expires_in": raw}); err == nil {
				t.Fatalf("mint accepted --expires-in %q", raw)
			}
		})
	}
}

// TestMintToTokenFileKeepsThePlaintextOutOfTheResult is what makes minting
// through a non-CLI frontend safe: the credential lands in a 0600 file and the
// returned value — which an MCP client would render into a transcript — carries
// only metadata.
func TestMintToTokenFileKeepsThePlaintextOutOfTheResult(t *testing.T) {
	store, m, _, _ := toolsFor(t)
	path := filepath.Join(t.TempDir(), "node-token")

	res := mint(t, m, map[string]any{"node": "n1", "user": "U1", "token_file": path})
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
	// The file holds a credential that actually works.
	token, err := nodetoken.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if _, err := nodetoken.Verify(context.Background(), store, token, time.Now()); err != nil {
		t.Fatalf("the token written to the file does not verify: %v", err)
	}
}

// TestMintTellsTheOperatorTheTokenIsShownOnce. The plaintext cannot be recovered
// from the store, so a rendering that did not say so would leave an operator
// expecting to look it up later.
func TestMintTellsTheOperatorTheTokenIsShownOnce(t *testing.T) {
	_, m, _, _ := toolsFor(t)
	rendered := mint(t, m, map[string]any{"node": "n1", "user": "U1"}).String()
	if !strings.Contains(rendered, "last time it can be shown") {
		t.Fatalf("the CLI rendering does not warn that the token cannot be shown again:\n%s", rendered)
	}
}

// TestMintAndRevokeRequireApproval. Both are registry tools, so an operator may
// list them in an agent's toolset; neither may run on the agent's say-so alone.
func TestMintAndRevokeRequireApproval(t *testing.T) {
	_, m, l, r := toolsFor(t)
	for _, tool := range []tools.Tool{m, r} {
		classifier, ok := tool.(tools.ApprovalClassifier)
		if !ok {
			t.Fatalf("%s does not implement tools.ApprovalClassifier, so an agent holding it can issue or revoke credentials unattended", tool.Name())
		}
		if !classifier.RequiresApproval(map[string]any{"node": "n1", "user": "U1"}) {
			t.Errorf("%s does not require approval", tool.Name())
		}
	}
	// Listing is read-only and deliberately ungated; asserted so that a later
	// change to it is a deliberate one.
	if _, gated := any(l).(tools.ApprovalClassifier); gated {
		t.Error("node.token.list now requires approval; if that is intended, say so here")
	}
}

// TestListNeverShowsATokenOrAHash. The store cannot produce a plaintext, so the
// risk here is the digest: printed, it looks like a credential and invites being
// pasted as one.
func TestListNeverShowsATokenOrAHash(t *testing.T) {
	store, m, l, _ := toolsFor(t)
	res := mint(t, m, map[string]any{"node": "mac-mini", "user": "U1", "label": "desk"})
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

// TestListFiltersByNodeAndReportsState covers ALL THREE states the CLI help
// documents — live, expired, revoked — because two of them do not exercise
// stateOf's middle branch: with `expired` untested, replacing `case
// !t.Live(now)` with `case false` leaves this package green while every
// timed-out credential reports itself as live to the operator deciding whether
// to re-enrol a node.
func TestListFiltersByNodeAndReportsState(t *testing.T) {
	_, m, l, r := toolsFor(t)
	live := mint(t, m, map[string]any{"node": "a", "user": "U1"})
	revoked := mint(t, m, map[string]any{"node": "a", "user": "U1"})
	// The shortest lifetime the tool accepts: --expires-in must be positive, so a
	// credential can only be aged past its expiry by waiting out a real one.
	expired := mint(t, m, map[string]any{"node": "a", "user": "U1", "expires_in": "1ms"})
	other := mint(t, m, map[string]any{"node": "b", "user": "U1"})

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

// A revoked credential that has ALSO passed its expiry reports "revoked": the
// operator's question is why it stopped working, and a deliberate withdrawal is
// the more informative answer than the clock running out on it afterwards.
func TestRevocationOutranksExpiryInTheListing(t *testing.T) {
	_, m, l, r := toolsFor(t)
	res := mint(t, m, map[string]any{"node": "a", "user": "U1", "expires_in": "1ms"})
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

// TestTwoCredentialsForOneNodeThenRevokeTheOlder is the rotation case as an
// operator performs it: mint the replacement, both work, withdraw the old one.
// Named for what it checks rather than for "rotation", which would promise a
// transport this stage does not have.
func TestTwoCredentialsForOneNodeThenRevokeTheOlder(t *testing.T) {
	store, m, _, r := toolsFor(t)
	ctx := context.Background()

	old := mint(t, m, map[string]any{"node": "mac-mini", "user": "U1", "label": "old"})
	fresh := mint(t, m, map[string]any{"node": "mac-mini", "user": "U1", "label": "new"})

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

// TestRevokeByNodeTakesEveryLiveCredential is the "the node is compromised"
// case, where the operator does not know which credential leaked.
func TestRevokeByNodeTakesEveryLiveCredential(t *testing.T) {
	store, m, _, r := toolsFor(t)
	ctx := context.Background()

	first := mint(t, m, map[string]any{"node": "mac-mini", "user": "U1"})
	second := mint(t, m, map[string]any{"node": "mac-mini", "user": "U1"})
	bystander := mint(t, m, map[string]any{"node": "other", "user": "U1"})

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

	// A second sweep finds nothing live and says so rather than erroring.
	again, err := r.Invoke(ctx, map[string]any{"node": "mac-mini"})
	if err != nil {
		t.Fatalf("a second revoke by node errored: %v", err)
	}
	if got := len(again.(revokeResult).Revoked); got != 0 {
		t.Fatalf("the second sweep revoked %d credentials, want 0", got)
	}
}

// TestRevokeArgumentErrors: a revoke that guessed which of two selectors the
// operator meant, or that silently did nothing, is worse than one that refuses.
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

// TestRevokeReportsTheLimitationItCannotYetFix. Revocation stops future
// handshakes; nothing closes a connection already made, because nothing owns one
// (#193). An operator who believed otherwise would stop investigating too early.
func TestRevokeReportsTheLimitationItCannotYetFix(t *testing.T) {
	_, m, _, r := toolsFor(t)
	minted := mint(t, m, map[string]any{"node": "n1", "user": "U1"})

	res, err := r.Invoke(context.Background(), map[string]any{"selector": minted.Selector})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !strings.Contains(res.(revokeResult).String(), nodetoken.RevocationLimitation) {
		t.Fatalf("the revoke rendering does not carry the limitation:\n%s", res.(revokeResult).String())
	}
}

// TestAnUnavailableStoreFailsCleanly: the provider is a closure over a database
// that a `setup` invocation may not have, and a nil dereference there would take
// the whole CLI down instead of the one command.
func TestAnUnavailableStoreFailsCleanly(t *testing.T) {
	down := Provider(func(context.Context) (config.NodeTokenStore, error) {
		return nil, errors.New("no config directory resolved")
	})
	for _, tool := range All(down) {
		if _, err := tool.Invoke(context.Background(), map[string]any{"node": "n1", "user": "U1", "selector": "abc"}); err == nil {
			t.Errorf("%s succeeded with no store", tool.Name())
		}
	}
	for _, tool := range All(nil) {
		if _, err := tool.Invoke(context.Background(), map[string]any{"node": "n1", "user": "U1", "selector": "abc"}); err == nil {
			t.Errorf("%s succeeded with a nil provider", tool.Name())
		}
	}
}

// TestMintDoesNotOverwriteAnExistingTokenFile. The file is written before the
// record is stored, so an overwrite would destroy a working credential in
// exchange for one that may not even reach the store.
func TestMintDoesNotOverwriteAnExistingTokenFile(t *testing.T) {
	store, m, _, _ := toolsFor(t)
	path := filepath.Join(t.TempDir(), "node-token")
	if err := os.WriteFile(path, []byte("mrtg_node_existing\n"), nodetoken.FileMode); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Invoke(context.Background(), map[string]any{"node": "n1", "user": "U1", "token_file": path}); err == nil {
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

// TestEverySchemaDeclaresItsArguments: the CLI maps --kebab-case flags onto the
// declared property names, so an argument missing from the schema is an argument
// no operator can pass.
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
