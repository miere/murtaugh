package main

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/miere/murtaugh/internal/nodesocket"
)

// The addresses this node will try, and the rule that keeps it able to come
// home.
//
// #170: learned gateways AUGMENT the configured seeds and NEVER replace them.
// The failure that rule exists to prevent is specific and unrecoverable without
// it — a node that was asleep through a topology change wakes holding only
// addresses that no longer exist, having overwritten the address somebody typed
// on purpose. So the seeds are first, permanent, and returned to on every cycle,
// and everything learned is appended behind them.
//
// The seeds come from the node's own configuration (config.NodeConfig.Gateway)
// or from -gateway, which overrides it. Configuration rather than a flag alone
// because an installed daemon's address survives a reinstall and is the one
// thing an operator edits when a gateway moves; see internal/config/node.go.
//
// Nothing learned is written down, either. Learned addresses die with the
// process, which is the same rule stated for time instead of for order: a
// restart starts from what an operator configured, never from what some gateway
// once said.

const (
	// maxLearnedGateways bounds what a fleet can teach this node. A workspace
	// has a handful of gateways; anything past this is a redirect loop or a
	// misconfiguration, and the oldest is dropped rather than the newest
	// refused — the newest is the one that just answered.
	maxLearnedGateways = 8
	// maxConsecutiveHops bounds a chain of redirects taken without waiting.
	// Two gateways that each name the other is not a state this code can fix,
	// and following that chain at full speed turns a misconfiguration into a
	// pair of pegged CPUs. The budget is per chain, not per process: a backoff
	// or a successful attachment returns it.
	maxConsecutiveHops = 3
)

// gatewayList is the ordered set of addresses this node will dial, with the
// seeds pinned at the front.
type gatewayList struct {
	// seeds is what an operator configured, in their order. Several because a
	// gateway is often known by more than one name — a hostname and an address,
	// #170's "hostname AND IP where available" seen from the node's side — and
	// making the operator pick one means picking the one that will be wrong
	// first. None of them is ever evicted.
	seeds   []string
	learned []string
	// at is the address currently selected, held BY VALUE rather than as an
	// index: learning may evict, and an index would then quietly name a
	// different address than the one it named a moment ago.
	at string
	// hops counts redirects followed since the last successful attachment.
	hops int
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

// first is the address a cycle starts from, and the one a lost cursor falls
// back to. Empty for a list with no seeds, which is a node that was never told
// where to dial — refused at startup, so it is unreachable here.
func first(seeds []string) string {
	if len(seeds) == 0 {
		return ""
	}
	return seeds[0]
}

// addresses is the seeds followed by everything learned, in the order they will
// be tried.
func (g *gatewayList) addresses() []string {
	all := make([]string, 0, len(g.learned)+len(g.seeds))
	all = append(all, g.seeds...)
	return append(all, g.learned...)
}

// current is the address to dial now.
func (g *gatewayList) current() string {
	for _, address := range g.addresses() {
		if address == g.at {
			return address
		}
	}
	// The selected address was evicted. A seed cannot be, which is the point
	// of it.
	g.at = first(g.seeds)
	return g.at
}

// advance moves to the next address, wrapping back to the seed.
//
// This is the whole of stale-seed recovery: whatever a node has been told, it
// comes back round to the addresses it was configured with, every cycle.
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

// attached records that a connection was established, which is what ends a
// redirect chain. The CURSOR is deliberately not reset: a node that had to hop
// to find the leader should keep dialling the leader, and starting the next
// cycle at the seed again would send every node in the fleet back through the
// gateway that turned them away.
func (g *gatewayList) attached() { g.hops = 0 }

// waited records that the node has just served a backoff, which also ends a
// redirect chain.
//
// The hop budget exists to stop a hot loop between two gateways naming each
// other, and once a wait has been paid there is no hot loop left to stop.
// Without this the budget would be spent once and never returned: a node that
// met a redirect loop on its first morning would refuse to follow a redirect
// for the rest of the process's life, and would find every subsequent failover
// only by working through its address list a backoff at a time.
func (g *gatewayList) waited() { g.hops = 0 }

// learn adds addresses a gateway named, keeping order and dropping duplicates.
//
// Every learned address goes through the dialler's own resolution first. An
// address that arrived over an unauthenticated handshake response is less
// trustworthy than one an operator typed, not more, so "a gateway told me to"
// is not a reason to dial something the wss rule refuses — it is a reason to
// say so and ignore it.
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

// hop points at the first usable address of the ones just offered, and reports
// whether the node should dial it immediately rather than waiting out its
// backoff.
//
// Immediately, because a redirect is not a failure: the fleet answered, named
// the leader, and made this node's next dial a hop rather than a guess.
// Inheriting a backoff earned by unrelated failures would leave a healthy node
// idle for up to half a minute for no reason.
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
	// Everything offered was refused by the wss rule. Nothing to hop to.
	g.advance()
	return false
}

// refusal turns a failed dial into a log line a support ticket can be answered
// from, and decides whether to hop or to back off.
//
// This is what #197 asks for in as many words: a node that loses its gateway
// must be able to tell "wrong gateway" from "gateway down" from "credential
// rejected". They are three different problems with three different owners —
// the fleet's topology, the network, and whoever mints tokens — and a single
// "could not attach" line names none of them.
func (g *gatewayList) refusal(address string, err error, logger *slog.Logger) (hop bool) {
	var redirect *nodesocket.RedirectError
	switch {
	case errors.As(err, &redirect):
		if len(redirect.Addresses) == 0 {
			// The elected gateway exists and takes no nodes. Hopping is
			// pointless: there is nowhere in the fleet that would accept this
			// node right now.
			logger.Warn("this gateway is not the elected one, and the elected gateway accepts no runtime nodes",
				"gateway", address)
			g.advance()
			return false
		}
		logger.Info("redirected to the elected gateway",
			"from", address, "leader", strings.Join(redirect.Addresses, " "))
		return g.hop(redirect.Addresses, logger)

	case errors.Is(err, nodesocket.ErrCredentialRejected):
		// Loud, and deliberately not softened by the reconnect loop around it.
		// Every other failure here is something that fixes itself; this one
		// needs a person, and a node that whispered it would keep redialling
		// forever with nobody told why it never attaches.
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
