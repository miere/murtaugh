package nodeclaim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
)

// DefaultInterval is how often a node re-reads its own configuration.
//
// It matches internal/app's gateway watcher, and for the same reason: none of
// the three backends offers a change feed the other two do, so a poll is the
// only uniform answer, and a configuration edit is a human-scale event. This is
// the ONLY poll in the design — the gateway does not poll nodes, and the
// staleness this one accepts is bounded by one edit. #170 rejects the
// alternative explicitly: asking every node at delegation time would put an
// N-way fan-out on the first message of every conversation, where one wedged
// node adds a timeout to every delegation in the workspace.
const DefaultInterval = 30 * time.Second

// Options configures a Watcher.
type Options struct {
	// Store is the node's own configuration store.
	Store config.Store
	// Base carries the file-sourced fields the store does not hold, and is what
	// a reload is assembled onto — the same value internal/app reloads against.
	Base config.Config
	// Serving is the profile names this process actually serves. It is fixed
	// for the process's life: changing it means rebuilding the agent, which
	// this watcher deliberately does not do. See the package doc.
	Serving []string
	// Publish receives each new claim. It is nodeserve.Advertiser.Publish in
	// production, and it is called from the watcher's own goroutine — never
	// from a link read loop, which is what lets it wait for the gateway's
	// acknowledgement.
	Publish func(context.Context, agentwire.Advertisement)
	// Interval is the poll period. Zero takes DefaultInterval; a test injects a
	// short one so it drives the node's real detector rather than reaching past
	// it.
	Interval time.Duration
	Logger   *slog.Logger
}

// Watcher notices a node-side configuration change and republishes the claim.
type Watcher struct {
	store    config.Store
	base     config.Config
	serving  []string
	publish  func(context.Context, agentwire.Advertisement)
	interval time.Duration
	log      *slog.Logger
	baseline config.Snapshot
}

// NewWatcher builds a watcher and reads the baseline it will compare against.
//
// The baseline is read HERE rather than on Run's goroutine, and that is not
// tidiness. A watcher whose first act is an asynchronous read is armed at some
// unspecified moment after the caller moves on, so a configuration written
// immediately afterwards may land inside the baseline and never register as a
// change — a race that costs an advertisement in production and produces a test
// that passes or fails by timing. Constructing it means "armed".
//
// It also takes the baseline from the STORE rather than from the configuration
// the process booted with, because those two can already differ: something may
// have been written in between, and treating that as a change would push an
// advertisement identical to the one the handshake is about to carry.
//
// A store that cannot be read is an error here, which the caller degrades: a
// node that cannot notice a configuration change still serves every
// conversation it is given, and one that refused to start serves none.
func NewWatcher(ctx context.Context, opts Options) (*Watcher, error) {
	if opts.Store == nil {
		return nil, errors.New("nodeclaim: no configuration store to watch")
	}
	if opts.Publish == nil {
		return nil, errors.New("nodeclaim: nowhere to publish a changed claim")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	baseline, err := opts.Store.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("nodeclaim: read the node's configuration: %w", err)
	}
	return &Watcher{
		store:    opts.Store,
		base:     opts.Base,
		serving:  append([]string(nil), opts.Serving...),
		publish:  opts.Publish,
		interval: interval,
		log:      log,
		baseline: baseline,
	}, nil
}

// Run polls until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	baseline := w.baseline
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if next, changed := w.poll(ctx, baseline); changed {
			baseline = next
		}
	}
}

// poll performs one read and returns the snapshot that is now the baseline.
//
// An invalid configuration advances the baseline anyway, with a warning. Not
// advancing it would re-read, re-fail and re-log every tick for as long as the
// mistake stands; advancing it costs nothing, because fixing the mistake is
// itself another change and is picked up on the next tick.
func (w *Watcher) poll(ctx context.Context, baseline config.Snapshot) (config.Snapshot, bool) {
	current, err := w.store.Snapshot(ctx)
	if err != nil {
		w.log.Warn("could not read this node's configuration store", "error", err)
		return baseline, false
	}
	// Renderings, not raw bodies: a store that re-encoded a body without
	// changing its meaning must not read as an edit, or a node would announce a
	// change every time a row was rewritten.
	changed, err := config.SnapshotChanged(baseline, current)
	if err != nil {
		w.log.Warn("could not compare this node's configuration", "error", err)
		return baseline, false
	}
	if !changed {
		return baseline, false
	}

	cfg, err := w.store.Load(ctx, w.base)
	if err != nil {
		w.log.Warn("this node's configuration changed but does not assemble; the gateway keeps the previous claim", "error", err)
		return current, true
	}
	// Applied unconditionally: a node admin editing their own node is the
	// authority on that node. There is no approval card here and there must not
	// be one — that flow belongs to the gateway admin, over gateway
	// configuration, and it needs a Slack surface a node does not have.
	//
	// What is re-read is the CLAIM and nothing else. The agent this node serves
	// is not rebuilt; see the package doc for what that would strand.
	w.log.Info("this node's configuration changed; re-advertising")
	w.publish(ctx, Advertise(cfg, w.serving))
	return current, true
}
