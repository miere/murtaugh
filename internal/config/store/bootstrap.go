package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/miere/murtaugh/internal/config"
)

// legacySiblings are the sibling config files a pre-database install keeps
// alongside gateway.yaml. On migration their contents move into the store and
// the files themselves are archived (moved, never deleted).
var legacySiblings = []string{
	"agents.yaml",
	"jobs.yaml",
	"journal.yaml",
	"workflow-rules.yaml",
	"unfurl-rules.yaml",
	"troubleshoot.yaml",
}

// Bootstrap resolves the running configuration from the on-disk bootstrap file
// at configPath. It is the single startup entrypoint that replaces the old
// config.Load: it parses the credentials + database block, migrates a legacy
// YAML tree into the store on first upgrade, opens the store, and (unless setup
// is true) loads and validates the whole config from it.
//
// It returns the assembled Config and the open Store. The caller owns the store
// and must Close it. When setup is true the store is opened but the config is
// NOT loaded/validated (setup tools run before a valid config exists); the
// returned Config carries only the bootstrap fields.
func Bootstrap(ctx context.Context, configPath string, setup bool) (config.Config, config.Store, error) {
	boot, err := config.LoadBootstrap(configPath)
	if err != nil {
		return config.Config{}, nil, err
	}

	// A bootstrap file predating this feature has no `database:` block: migrate
	// its YAML siblings into a fresh SQLite store, then re-read the (rewritten)
	// bootstrap so it now points at that store. Setup invocations skip this —
	// they are actively constructing config and may be partial/token-less.
	if boot.Database.IsZero() && !setup {
		if err := migrateFilesToStore(ctx, configPath); err != nil {
			return config.Config{}, nil, fmt.Errorf("migrate config to database: %w", err)
		}
		if boot, err = config.LoadBootstrap(configPath); err != nil {
			return config.Config{}, nil, err
		}
	}

	s, err := Open(ctx, boot.Database)
	if err != nil {
		return config.Config{}, nil, err
	}
	if setup {
		return boot, s, nil
	}
	cfg, err := s.Load(ctx, boot)
	if err != nil {
		_ = s.Close()
		return config.Config{}, nil, err
	}
	return cfg, s, nil
}

// bootstrapFile is the slim, post-migration shape of gateway.yaml: credentials
// plus the store connection, nothing else. Access/chat and the siblings now
// live in the store.
type bootstrapFile struct {
	OAuth    config.OAuthConfig    `yaml:"oauth"`
	Database config.DatabaseConfig `yaml:"database"`
}

// migrateFilesToStore performs the one-shot YAML→database migration. It loads
// the full legacy config from disk (validating it), writes every non-credential
// section into a fresh SQLite store, rewrites gateway.yaml down to oauth +
// database, and archives the now-migrated siblings. The credentials never enter
// the store: only the oauth block stays on disk (referencing .env).
func migrateFilesToStore(ctx context.Context, configPath string) error {
	// Read the RAW oauth block (with ${VAR} intact) for the rewrite, before the
	// expanding loader runs — the file must keep references, not secrets.
	rawData, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read %q: %w", configPath, err)
	}
	rawBoot, err := config.Parse(rawData)
	if err != nil {
		return err
	}

	// Load the full legacy tree (gateway.yaml + siblings), validated.
	legacy, err := config.Load(configPath)
	if err != nil {
		return err
	}

	dir := filepath.Dir(configPath)
	dbc := config.DatabaseConfig{Backend: config.BackendSQLite}
	s, err := Open(ctx, dbc)
	if err != nil {
		return err
	}
	defer s.Close()

	if err := importConfig(ctx, s, legacy); err != nil {
		return err
	}

	// Rewrite gateway.yaml to the slim bootstrap shape, keeping the raw oauth
	// references and recording the SQLite path so it lives in the config file.
	dbc.SQLite.Path = dbc.EffectiveSQLitePath()
	out, err := yaml.Marshal(bootstrapFile{OAuth: rawBoot.OAuth, Database: dbc})
	if err != nil {
		return fmt.Errorf("render bootstrap: %w", err)
	}
	header := "# Murtaugh bootstrap. Credentials live in .env and are referenced as ${VAR}.\n" +
		"# All other configuration now lives in the database below — manage it with\n" +
		"# `murtaugh cfg …` (e.g. `murtaugh cfg agent list`).\n"
	if err := os.WriteFile(configPath, append([]byte(header), out...), 0o644); err != nil {
		return fmt.Errorf("write %q: %w", configPath, err)
	}

	if err := archiveSiblings(dir); err != nil {
		return err
	}
	return nil
}

// importConfig writes every non-credential section of cfg into the store. It is
// non-destructive per row via UpsertItem/PutSingleton (idempotent re-runs), and
// deliberately omits OAuth — credentials never enter the store.
func importConfig(ctx context.Context, s config.Store, cfg config.Config) error {
	for name, p := range cfg.Agents {
		if err := s.UpsertItem(ctx, config.SectionAgent, name, p); err != nil {
			return err
		}
	}
	for name, m := range cfg.MCPServers {
		if err := s.UpsertItem(ctx, config.SectionMCP, name, m); err != nil {
			return err
		}
	}
	for name, j := range cfg.Jobs {
		if err := s.UpsertItem(ctx, config.SectionJob, name, j); err != nil {
			return err
		}
	}
	for name, r := range cfg.WorkflowRules {
		if err := s.UpsertItem(ctx, config.SectionWorkflowRule, name, r); err != nil {
			return err
		}
	}
	for name, r := range cfg.UnfurlRules {
		if err := s.UpsertItem(ctx, config.SectionUnfurlRule, name, r); err != nil {
			return err
		}
	}
	singletons := []struct {
		key  string
		body any
	}{
		{config.SingletonAccess, cfg.Access},
		{config.SingletonChat, cfg.Chat},
		{config.SingletonDefaults, cfg.Defaults},
		{config.SingletonJournal, cfg.Journal},
		{config.SingletonTroubleshoot, cfg.Troubleshoot},
	}
	for _, sg := range singletons {
		if err := s.PutSingleton(ctx, sg.key, sg.body); err != nil {
			return err
		}
	}
	return nil
}

// archiveSiblings moves the migrated sibling YAMLs into a timestamped
// migrated-<ts>/ directory beside gateway.yaml. Files are moved (never deleted)
// so the operator keeps a copy; a missing sibling is skipped.
func archiveSiblings(dir string) error {
	archive := filepath.Join(dir, "migrated-"+time.Now().Format("20060102-150405"))
	var moved bool
	for _, name := range legacySiblings {
		src := filepath.Join(dir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if !moved {
			if err := os.MkdirAll(archive, 0o755); err != nil {
				return fmt.Errorf("create archive dir: %w", err)
			}
			moved = true
		}
		if err := os.Rename(src, filepath.Join(archive, name)); err != nil {
			return fmt.Errorf("archive %q: %w", name, err)
		}
	}
	return nil
}
