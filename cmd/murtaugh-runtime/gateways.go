package main

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/miere/murtaugh/internal/nodesocket"
)

const (
	maxLearnedGateways = 8
	maxConsecutiveHops = 3
)

type gatewayList struct {
	seeds   []string
	learned []string
	at      string
	hops    int
}

func newGatewayList(seeds ...string) *gatewayList {
	kept := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		if trimmed := strings.TrimSpace(seed); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return &gatewayList{seeds: kept, at: first(kept)}
}

func first(seeds []string) string {
	if len(seeds) == 0 {
		return ""
	}
	return seeds[0]
}

func (g *gatewayList) addresses() []string {
	all := make([]string, 0, len(g.learned)+len(g.seeds))
	all = append(all, g.seeds...)
	return append(all, g.learned...)
}

func (g *gatewayList) current() string {
	for _, address := range g.addresses() {
		if address == g.at {
			return address
		}
	}
	g.at = first(g.seeds)
	return g.at
}

func (g *gatewayList) advance() {
	all := g.addresses()
	for i, address := range all {
		if address == g.at {
			g.at = all[(i+1)%len(all)]
			return
		}
	}
	g.at = first(g.seeds)
}

func (g *gatewayList) attached() { g.hops = 0 }

func (g *gatewayList) waited() { g.hops = 0 }

func (g *gatewayList) learn(addresses []string, logger *slog.Logger) {
	for _, address := range addresses {
		trimmed := strings.TrimSpace(address)
		if trimmed == "" || g.known(trimmed) {
			continue
		}
		if _, err := nodesocket.ResolveEndpoint(trimmed); err != nil {
			logger.Warn("ignoring an address the gateway offered", "address", trimmed, "error", err)
			continue
		}
		if len(g.learned) >= maxLearnedGateways {
			g.learned = g.learned[1:]
		}
		g.learned = append(g.learned, trimmed)
	}
}

func (g *gatewayList) known(address string) bool {
	for _, seed := range g.seeds {
		if seed == address {
			return true
		}
	}
	for _, learned := range g.learned {
		if learned == address {
			return true
		}
	}
	return false
}

func (g *gatewayList) hop(offered []string, logger *slog.Logger) bool {
	g.learn(offered, logger)
	if g.hops >= maxConsecutiveHops {
		logger.Warn("too many redirects in a row; backing off instead of following another",
			"hops", g.hops)
		g.advance()
		return false
	}
	for _, address := range offered {
		if g.known(strings.TrimSpace(address)) {
			g.at = strings.TrimSpace(address)
			g.hops++
			return true
		}
	}
	g.advance()
	return false
}

func (g *gatewayList) refusal(address string, err error, logger *slog.Logger) (hop bool) {
	var redirect *nodesocket.RedirectError
	switch {
	case errors.As(err, &redirect):
		if len(redirect.Addresses) == 0 {
			logger.Warn("this gateway is not the elected one, and the elected gateway accepts no runtime nodes",
				"gateway", address)
			g.advance()
			return false
		}
		logger.Info("redirected to the elected gateway",
			"from", address, "leader", strings.Join(redirect.Addresses, " "))
		return g.hop(redirect.Addresses, logger)

	case errors.Is(err, nodesocket.ErrCredentialRejected):
		logger.Error("the gateway rejected this node's credential: it is unknown, revoked, or not the one this gateway holds. Redialling will not fix it — mint a new one with `murtaugh node token`",
			"gateway", address, "error", err)
		g.advance()
		return false

	case errors.Is(err, nodesocket.ErrGatewayUnavailable):
		logger.Warn("the gateway answered but is not serving runtime nodes; no gateway may be elected yet",
			"gateway", address, "error", err)
		g.advance()
		return false

	case errors.Is(err, nodesocket.ErrGatewayUnreachable):
		logger.Warn("no answer from the gateway", "gateway", address, "error", err)
		g.advance()
		return false

	default:
		logger.Warn("could not attach to gateway", "gateway", address, "error", err)
		g.advance()
		return false
	}
}
