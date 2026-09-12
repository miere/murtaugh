package nodehost

import (
	"context"
	"errors"
	"time"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/journal"
	"github.com/miere/murtaugh/internal/nodetoken"
)

func (h *Host) watchCredentials(ctx context.Context) {
	every := h.opts.RecheckInterval
	if every <= 0 {
		every = nodetoken.RecheckInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := h.recheckCredentials(ctx)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil && !failing:
			h.log.Warn("could not re-check runtime node credentials; their connections stay up until the store answers", "error", err)
		case err == nil && failing:
			h.log.Info("re-checking runtime node credentials again")
		}
		failing = err != nil
	}
}

func (h *Host) recheckCredentials(ctx context.Context) error {
	var storeErr error
	for _, selector := range h.liveSelectors() {
		err := nodetoken.Recheck(ctx, h.tokens, selector, h.now())
		switch {
		case err == nil:
		case errors.Is(err, nodetoken.ErrNotAuthorized):
			h.closeCredential(selector, err)
		default:
			storeErr = err
		}
	}
	return storeErr
}

func (h *Host) liveSelectors() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.nodes))
	for _, node := range h.nodes {
		out = append(out, node.selector)
	}
	return out
}

func (h *Host) closeCredential(selector string, reason error) {
	state, summary := "revoked", "A runtime node's credential was revoked and its connection closed"
	switch {
	case errors.Is(reason, nodetoken.ErrExpired):
		state, summary = "expired", "A runtime node's credential expired and its connection was closed"
	case errors.Is(reason, nodetoken.ErrUnknownCredential):
		state, summary = "credential_gone", "A runtime node's credential no longer exists and its connection was closed"
	}
	for _, node := range h.takeCredential(selector) {
		h.log.Warn("closing a runtime node whose credential no longer verifies",
			"node_id", node.nodeID, "selector", selector, "reason", reason)
		h.record(journal.LevelWarn, state, summary, node, agentwire.Advertisement{})
		node.close()
	}
}
