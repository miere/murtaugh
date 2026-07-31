package config

import (
	"path/filepath"
	"testing"
)

func TestBaseNameOf(t *testing.T) {
	cases := map[string]string{
		"/etc/murtaugh/config.yaml":             "config",
		"/etc/murtaugh/slack-nurturecloud.yaml": "slack-nurturecloud",
		"config.yml":                            "config",
		"gateway":                               "gateway",
		"":                                      "",
	}
	for in, want := range cases {
		if got := BaseNameOf(in); got != want {
			t.Errorf("BaseNameOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// The database filenames are stemmed by the bootstrap file's name so that two
// configs sharing a directory address two distinct stores. Sharing one store was
// what let a second bootstrap file's migration overwrite the first's settings.
func TestDatabaseNamesAreStemmedByConfigName(t *testing.T) {
	dir := "/etc/murtaugh"
	var dbc DatabaseConfig
	var jc JournalConfig

	cases := []struct {
		baseName   string
		wantConfig string
		wantJournl string
		wantBlobs  string
	}{
		{"config", "config.db", "config-journal.db", "config-journal-blobs"},
		{"slack-nurturecloud", "slack-nurturecloud.db", "slack-nurturecloud-journal.db", "slack-nurturecloud-journal-blobs"},
		// An unrecorded base name keeps the historical config.db.
		{"", "config.db", "config-journal.db", "config-journal-blobs"},
	}
	for _, c := range cases {
		if got, want := dbc.EffectiveSQLitePath(dir, c.baseName), filepath.Join(dir, c.wantConfig); got != want {
			t.Errorf("EffectiveSQLitePath(%q) = %q, want %q", c.baseName, got, want)
		}
		if got, want := jc.EffectivePath(dir, c.baseName), filepath.Join(dir, c.wantJournl); got != want {
			t.Errorf("journal EffectivePath(%q) = %q, want %q", c.baseName, got, want)
		}
		if got, want := jc.EffectiveBlobDir(dir, c.baseName), filepath.Join(dir, c.wantBlobs); got != want {
			t.Errorf("EffectiveBlobDir(%q) = %q, want %q", c.baseName, got, want)
		}
	}

	// Two configs in one directory must never resolve to the same file.
	if a, b := dbc.EffectiveSQLitePath(dir, "config"), dbc.EffectiveSQLitePath(dir, "slack-nurturecloud"); a == b {
		t.Errorf("two configs in %s share a store (%q)", dir, a)
	}
}

// An explicit path in the `database:`/journal block still wins over the stem.
func TestExplicitPathsOverrideStem(t *testing.T) {
	dbc := DatabaseConfig{SQLite: SQLiteConfig{Path: "/var/lib/pinned.db"}}
	if got := dbc.EffectiveSQLitePath("/etc/murtaugh", "slack-nurturecloud"); got != "/var/lib/pinned.db" {
		t.Errorf("EffectiveSQLitePath = %q, want the pinned path", got)
	}
	jc := JournalConfig{Path: "/var/lib/pinned-journal.db", BlobDir: "/var/lib/pinned-blobs"}
	if got := jc.EffectivePath("/etc/murtaugh", "slack-nurturecloud"); got != "/var/lib/pinned-journal.db" {
		t.Errorf("journal EffectivePath = %q, want the pinned path", got)
	}
	if got := jc.EffectiveBlobDir("/etc/murtaugh", "slack-nurturecloud"); got != "/var/lib/pinned-blobs" {
		t.Errorf("EffectiveBlobDir = %q, want the pinned dir", got)
	}
}
