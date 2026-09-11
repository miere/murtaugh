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

// No config backend offers a change feed the others share, so polling is the only uniform option;
// config edits are rare, human-scale events.
const DefaultInterval = 30 * time.Second

type Options struct {
	Store    config.Store
	Base     config.Config
	Serving  []string
	Publish  func(context.Context, agentwire.Advertisement)
	Interval time.Duration
	Logger   *slog.Logger
}

type Watcher struct {
	store    config.Store
	base     config.Config
	serving  []string
	publish  func(context.Context, agentwire.Advertisement)
	interval time.Duration
	log      *slog.Logger
	baseline config.Snapshot
}

// The baseline is read here, not in Run, so a config written right after this returns is still
// seen as a change.
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

func (w *Watcher) poll(ctx context.Context, baseline config.Snapshot) (config.Snapshot, bool) {
	current, err := w.store.Snapshot(ctx)
	if err != nil {
		w.log.Warn("could not read this node's configuration store", "error", err)
		return baseline, false
	}
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
	w.log.Info("this node's configuration changed; re-advertising")
	w.publish(ctx, Advertise(cfg, w.serving))
	return current, true
}
