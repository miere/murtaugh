package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/store"
)

func testStore(t *testing.T) config.Store {
	t.Helper()
	s, err := store.Open(context.Background(), config.DatabaseConfig{
		Backend: config.BackendSQLite,
		SQLite:  config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}, "", "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func storedJob(t *testing.T, s config.Store, name string) config.JobProfile {
	t.Helper()
	cfg, err := s.Load(context.Background(), config.Config{OAuth: config.OAuthConfig{AppToken: "x", BotToken: "x"}})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	job, ok := cfg.Jobs[name]
	if !ok {
		t.Fatalf("job %q not in the loaded config", name)
	}
	return job
}

// Approving a held job must clear the hold in the store, so the next daemon
// loads it already confirmed and never re-prompts.
func TestNewJobConfirmer_MarksJobConfirmed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	unconfirmed := false
	held := config.JobProfile{Command: "/bin/echo", Args: []string{"hi"}, Every: "1h", Confirmed: &unconfirmed}
	if err := s.UpsertItem(ctx, config.SectionJob, "held", held); err != nil {
		t.Fatal(err)
	}

	if err := newJobConfirmer(s)(ctx, "held"); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	job := storedJob(t, s, "held")
	if job.AwaitingConfirmation() {
		t.Fatalf("job is still held after confirmation (Confirmed=%v)", job.Confirmed)
	}
	// The rest of the entry must survive the flag flip untouched.
	if job.Command != "/bin/echo" || job.Every != "1h" || len(job.Args) != 1 || job.Args[0] != "hi" {
		t.Fatalf("confirmation rewrote the job definition: %+v", job)
	}
}

func TestNewJobConfirmer_UnknownJob(t *testing.T) {
	if err := newJobConfirmer(testStore(t))(context.Background(), "ghost"); err == nil {
		t.Fatal("confirming a job that is not in the store should error")
	}
}
