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

// Call the returned cleanup before exit or buffered events are lost. A store that fails to
// open degrades to a no-op rather than an error, so journalling never blocks start.
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

// Lives under XDG state rather than config because the marker is runtime state.
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

// The sweep runs in the daemon because it is the journal's only writer, and only the
// writer may delete.
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
