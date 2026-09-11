package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent/remote"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/credwarden"
	"github.com/miere/murtaugh/internal/nodelink"
	"github.com/miere/murtaugh/internal/nodeserve"
)

type fakeWarden struct {
	healths  []credwarden.Health
	observer credwarden.Observer
}

func (w *fakeWarden) Healths() []credwarden.Health             { return w.healths }
func (w *fakeWarden) SetObserver(observer credwarden.Observer) { w.observer = observer }

func TestANodeWatchesOnlyTheCredentialOfTheAgentItServes(t *testing.T) {
	cfg := config.Config{Agents: map[string]config.AgentProfile{
		"coder":  {ClaudeCode: &config.ClaudeCodeProfile{Command: "/fake/claude"}},
		"other":  {ClaudeCode: &config.ClaudeCodeProfile{Command: "/other/claude"}},
		"native": {},
	}}
	if ids := servedIdentities(cfg, "coder"); len(ids) != 1 || ids[0].Command != "/fake/claude" {
		t.Fatalf("serving coder watches %v, want only /fake/claude", ids)
	}
	if ids := servedIdentities(cfg, "native"); len(ids) != 0 {
		t.Fatalf("serving a native agent watches %v", ids)
	}
	if ids := servedIdentities(cfg, ""); len(ids) != 0 {
		t.Fatalf("an unconfigured node watches %v", ids)
	}
}

func TestANodeTellsItsGatewayHowItsCredentialIsDoing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	id := credwarden.Identity{Command: "/fake/claude"}
	warden := &fakeWarden{healths: []credwarden.Health{{Identity: id, ExpiresAt: time.Now().Add(time.Hour)}}}
	reports := reportCredentials(warden, logger)

	heard := make(chan agentwire.CredentialHealth, 16)
	gatewaySide, nodeSide := nodelink.Pipe(32)
	gateway := remote.New(gatewaySide, remote.Options{Logger: logger, Owner: "UOWNER", Credentials: func(h agentwire.CredentialHealth) { heard <- h }})
	t.Cleanup(func() { _ = gateway.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = nodeserve.Serve(ctx, nodeSide, nodeserve.UnconfiguredClient{}, nodeserve.Options{Logger: logger, Credentials: reports})
	}()

	if first := awaitReport(t, heard); first.Credential != "/fake/claude" || first.Degraded || first.ExpiresAt.IsZero() {
		t.Fatalf("the connect-time report was %+v", first)
	}
	warden.observer(credwarden.Health{Identity: id, Degraded: true, Reason: "keychain is locked", Since: time.Now()})
	if down := awaitReport(t, heard); !down.Degraded || down.Reason != "keychain is locked" {
		t.Fatalf("the failure reached the gateway as %+v", down)
	}
	warden.observer(credwarden.Health{Identity: id, ExpiresAt: time.Now().Add(time.Hour)})
	if up := awaitReport(t, heard); up.Degraded {
		t.Fatalf("the recovery reached the gateway as %+v", up)
	}
}

func awaitReport(t *testing.T, heard <-chan agentwire.CredentialHealth) agentwire.CredentialHealth {
	t.Helper()
	select {
	case h := <-heard:
		return h
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway never heard the report")
		return agentwire.CredentialHealth{}
	}
}
