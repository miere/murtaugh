package main

import (
	"log/slog"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/credwarden"
	"github.com/miere/murtaugh/internal/nodeserve"
)

type watchedCredentials interface {
	Healths() []credwarden.Health
	SetObserver(credwarden.Observer)
}

func servedIdentities(cfg config.Config, name string) []credwarden.Identity {
	if name == "" {
		return nil
	}
	return credwarden.ClaudeCodeIdentities(map[string]config.AgentProfile{name: cfg.Agents[name]})
}

func reportCredentials(watched watchedCredentials, logger *slog.Logger) *nodeserve.Credentials {
	reports := nodeserve.NewCredentials(logger, func() []agentwire.CredentialHealth {
		healths := watched.Healths()
		out := make([]agentwire.CredentialHealth, 0, len(healths))
		for _, h := range healths {
			out = append(out, wireHealth(h))
		}
		return out
	})
	watched.SetObserver(func(h credwarden.Health) { reports.Report(wireHealth(h)) })
	return reports
}

func wireHealth(h credwarden.Health) agentwire.CredentialHealth {
	return agentwire.CredentialHealth{
		Credential: h.Identity.String(),
		Degraded:   h.Degraded,
		Reason:     h.Reason,
		Since:      h.Since,
		ExpiresAt:  h.ExpiresAt,
	}
}
