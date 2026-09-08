package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
)

// The two helpers here are the daemon's startup, as opposed to the CLI's. They
// live in this package rather than in a main package because there is now more
// than one daemon binary — `murtaugh slack gateway` and cmd/murtaugh-gateway —
// and a startup sequence copied into a second main is a startup sequence that
// drifts.

// OpenJournal opens the event journal and returns the store, a recorder, and a
// cleanup that drains and closes them. The store is returned so the daemon can
// reuse it for the retention sweep (it is the single writer that may delete).
//
// A store that cannot be opened degrades to a nil store and a no-op recorder
// with a no-op cleanup, so journalling never blocks start. The caller must
// invoke the returned cleanup before exit so buffered events flush.
func OpenJournal(cfg config.Config, logger *slog.Logger) (*journal.Store, journal.Recorder, func()) {
	path := cfg.Journal.EffectivePath(cfg.BaseDir, cfg.BaseName)
	store, err := journal.Open(path, cfg.Journal.RetentionByStream(),
		journal.WithBlobDir(cfg.Journal.EffectiveBlobDir(cfg.BaseDir, cfg.BaseName)))
	if err != nil {
		logger.Warn("journal disabled: could not open event store", "path", path, "error", err)
		return nil, journal.NopRecorder{}, func() {}
	}
	recorder := journal.NewRecorder(store, cfg.Journal.EnabledStreams(), logger)
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := recorder.Close(ctx); err != nil {
			logger.Warn("journal recorder did not drain cleanly", "error", err)
		}
		if err := store.Close(); err != nil {
			logger.Warn("journal store close failed", "error", err)
		}
	}
	return store, recorder, cleanup
}

// DefaultResumeMarkerPath resolves the on-disk location for the cross-restart
// resume marker. It follows the XDG state convention (XDG_STATE_HOME overrides;
// falls back to ~/.local/state/murtaugh) because the marker is runtime state,
// not config.
func DefaultResumeMarkerPath() (string, error) {
	if v := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); v != "" {
		return filepath.Join(v, "murtaugh", "restart.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "murtaugh", "restart.json"), nil
}

// WithJournalRetentionSweep wires the retention sweeper over an open journal
// store. The daemon is the single writer, so the sweep runs there, reusing the
// recorder's store (Prune serialises with the writer on the one connection); the
// `journal.prune` tool is the manual equivalent. A nil store leaves the sweeper
// unwired.
func (a *Application) WithJournalRetentionSweep(store *journal.Store, cfg config.Config, logger *slog.Logger) *Application {
	if store == nil {
		return a
	}
	return a.WithJournalSweeper(func(ctx context.Context) error {
		res, err := store.Prune(ctx, time.Now())
		if err != nil {
			return err
		}
		if res.Total > 0 {
			logger.Info("journal swept old events", "removed", res.Total, "by_stream", res.Removed)
		}
		return nil
	}, cfg.Journal.EffectiveSweepEvery())
}
