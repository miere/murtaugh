package slackapp

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"
)

// defaultConfigWatchInterval is the cadence at which the watcher polls
// every tracked path for mtime changes. 5s is short enough that an
// admin editing agents.yaml sees the suggestion within a coffee-sip
// while still being well below any rate limit on Slack DM posts.
const defaultConfigWatchInterval = 5 * time.Second

// configFileChangeFunc is invoked once per detected mtime change.
// The callback runs on the watcher's goroutine; long-running work
// should be dispatched to a separate goroutine by the callback.
type configFileChangeFunc func(ctx context.Context, path string, mtime time.Time)

// configWatcher polls a fixed set of file paths and invokes onChange
// whenever any tracked file's mtime advances. The first observed
// mtime for a path is treated as a baseline so a freshly started
// daemon never spuriously fires on startup; subsequent mtime changes
// fire exactly once per change.
//
// Missing files are tolerated: they are skipped silently until they
// appear, at which point they are seeded (still no fire) and tracked
// like any other path. Files that disappear after being seeded are
// also silently skipped until they reappear.
type configWatcher struct {
	paths    []string
	interval time.Duration
	onChange configFileChangeFunc
	logger   *slog.Logger

	mu   sync.Mutex
	seen map[string]time.Time
}

// newConfigWatcher constructs a watcher for the given paths. interval
// must be positive; non-positive values fall back to
// defaultConfigWatchInterval so callers can pass 0 to opt into the
// default cadence. A nil logger is tolerated; a silent fallback is
// used.
func newConfigWatcher(paths []string, interval time.Duration, onChange configFileChangeFunc, logger *slog.Logger) *configWatcher {
	if interval <= 0 {
		interval = defaultConfigWatchInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &configWatcher{
		paths:    paths,
		interval: interval,
		onChange: onChange,
		logger:   logger,
		seen:     make(map[string]time.Time),
	}
}

// Run seeds the baseline mtimes and then polls every interval until
// ctx is cancelled. The first poll never fires the callback; from
// the second poll onwards any mtime change relative to the last
// observed value fires onChange.
func (w *configWatcher) Run(ctx context.Context) {
	w.poll(ctx, true)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.poll(ctx, false)
		}
	}
}

// poll inspects every watched path exactly once. When baseline is
// true the observed mtimes are recorded without firing the callback;
// when false, a mtime that differs from the previously stored value
// fires onChange and the stored value is updated to the new mtime.
// Paths that have never been observed are always seeded silently
// regardless of baseline so a file that appears mid-flight is not
// reported as a "change".
func (w *configWatcher) poll(ctx context.Context, baseline bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, path := range w.paths {
		info, err := os.Stat(path)
		if err != nil {
			w.logger.Debug("config watcher: stat failed", "path", path, "error", err)
			continue
		}
		mtime := info.ModTime()
		prev, ok := w.seen[path]
		if !ok {
			w.seen[path] = mtime
			if !baseline {
				w.logger.Debug("config watcher: seeded new path mid-flight", "path", path, "mtime", mtime)
			}
			continue
		}
		if mtime.Equal(prev) {
			continue
		}
		w.seen[path] = mtime
		if baseline {
			continue
		}
		if w.onChange != nil {
			w.onChange(ctx, path, mtime)
		}
	}
}
