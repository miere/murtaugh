package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/miere/murtaugh/internal/config"
)

// legacySiblings are the sibling config files a pre-database install keeps
// alongside config.yaml. On migration their contents move into the store and
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

	s, err := Open(ctx, boot.Database, filepath.Dir(configPath), config.BaseNameOf(configPath))
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
	cfg = ensureAgentIcons(ctx, s, cfg)
	return cfg, s, nil
}

// ensureAgentIcons gives every agent that has no icon a random one from the
// palette and writes it back, so the face an agent shows is decided once and
// then stays put — across surfaces and across restarts. Agents created before
// the feature are backfilled here on the next start.
//
// It writes the profile as it is stored, re-read from the row, NOT the profile
// off cfg: Load bakes the global defaults.approval into each in-memory agent,
// and persisting that would silently freeze today's default into the agent row.
//
// An icon is cosmetic, so a store that refuses the write degrades the icon
// rather than the daemon: the failure is logged and the profile is left
// iconless, both in the store and in memory, for the next start to retry.
// Assigning it in memory only would hand out a different face on every boot.
func ensureAgentIcons(ctx context.Context, s config.Store, cfg config.Config) config.Config {
	if len(cfg.Agents) == 0 {
		return cfg
	}
	rows, err := s.ListItems(ctx, config.SectionAgent)
	if err != nil {
		slog.Default().Warn("could not read agents to assign icons", "error", err)
		return cfg
	}
	for name, body := range rows {
		var stored config.AgentProfile
		if err := json.Unmarshal(body, &stored); err != nil {
			slog.Default().Warn("could not decode agent to assign an icon", "agent", name, "error", err)
			continue
		}
		if strings.TrimSpace(stored.Icon) != "" {
			continue
		}
		stored.Icon = config.PickAgentIcon()
		if err := s.UpsertItem(ctx, config.SectionAgent, name, stored); err != nil {
			slog.Default().Warn("could not persist the agent icon", "agent", name, "error", err)
			continue
		}
		if live, ok := cfg.Agents[name]; ok {
			live.Icon = stored.Icon
			cfg.Agents[name] = live
		}
	}
	return cfg
}

// bootstrapFile is the slim, post-migration shape of config.yaml: credentials
// plus the store connection, nothing else. The sqlite/postgres sub-blocks are
// pointers so an unset one is omitted — a default SQLite store writes just
// `database: { backend: sqlite }`, letting the path default to config.db beside
// this file.
type bootstrapFile struct {
	OAuth    config.OAuthConfig `yaml:"oauth"`
	Database databaseBlock      `yaml:"database"`
}

type databaseBlock struct {
	Backend  string         `yaml:"backend"`
	SQLite   *sqliteBlock   `yaml:"sqlite,omitempty"`
	Postgres *postgresBlock `yaml:"postgres,omitempty"`
}

type sqliteBlock struct {
	Path string `yaml:"path"`
}

type postgresBlock struct {
	DSN string `yaml:"dsn"`
}

// toBlock renders a DatabaseConfig for writing, omitting empty sub-blocks so a
// default store produces a clean bootstrap file.
func toBlock(d config.DatabaseConfig) databaseBlock {
	b := databaseBlock{Backend: d.EffectiveBackend()}
	if p := strings.TrimSpace(d.SQLite.Path); p != "" {
		b.SQLite = &sqliteBlock{Path: d.SQLite.Path}
	}
	if dsn := strings.TrimSpace(d.Postgres.DSN); dsn != "" {
		b.Postgres = &postgresBlock{DSN: d.Postgres.DSN}
	}
	return b
}

// migrateFilesToStore performs the one-shot YAML→database migration. It loads
// the full legacy config from disk (validating it), writes every non-credential
// section into a fresh SQLite store, rewrites config.yaml down to oauth +
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

	// Load the full legacy tree (config.yaml + siblings), validated.
	legacy, err := config.Load(configPath)
	if err != nil {
		return err
	}

	dir := filepath.Dir(configPath)
	dbc := config.DatabaseConfig{Backend: config.BackendSQLite}
	s, err := Open(ctx, dbc, dir, config.BaseNameOf(configPath))
	if err != nil {
		return err
	}
	defer s.Close()

	if err := importConfig(ctx, s, legacy); err != nil {
		return err
	}

	// Rewrite config.yaml to the slim bootstrap shape, keeping the raw oauth
	// references. The SQLite path is left unset so it defaults to `config.db`
	// beside this file — the store travels with its config.
	if err := writeBootstrap(configPath, rawBoot.OAuth, dbc); err != nil {
		return err
	}

	if err := archiveSiblings(dir); err != nil {
		return err
	}
	return nil
}

// RewriteDatabaseBlock rewrites config.yaml's `database:` block to dbc while
// preserving the raw oauth references (never the expanded secrets). It backs the
// `cfg db migrate` tool's final step of pointing the bootstrap at the new store.
func RewriteDatabaseBlock(configPath string, dbc config.DatabaseConfig) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read %q: %w", configPath, err)
	}
	boot, err := config.Parse(raw)
	if err != nil {
		return err
	}
	return writeBootstrap(configPath, boot.OAuth, dbc)
}

// SetBootstrapOAuth writes config.yaml's `oauth:` block while preserving the
// existing `database:` block (a missing file defaults to the SQLite backend). It
// is the symmetric counterpart to RewriteDatabaseBlock, used by setup.slack so
// writing Slack credentials never clobbers the store selection.
func SetBootstrapOAuth(configPath string, oauth config.OAuthConfig) error {
	var dbc config.DatabaseConfig
	if raw, err := os.ReadFile(configPath); err == nil {
		if boot, perr := config.Parse(raw); perr == nil {
			dbc = boot.Database
		}
	}
	return writeBootstrap(configPath, oauth, dbc)
}

// writeBootstrap renders and writes the slim config.yaml (oauth + database).
func writeBootstrap(configPath string, oauth config.OAuthConfig, dbc config.DatabaseConfig) error {
	out, err := yaml.Marshal(bootstrapFile{OAuth: oauth, Database: toBlock(dbc)})
	if err != nil {
		return fmt.Errorf("render bootstrap: %w", err)
	}
	header := "# Murtaugh bootstrap. Credentials live in .env and are referenced as ${VAR}.\n" +
		"# All other configuration lives in the database below — manage it with\n" +
		"# `murtaugh cfg …` (e.g. `murtaugh cfg agent list`).\n"
	if err := os.WriteFile(configPath, append([]byte(header), out...), 0o644); err != nil {
		return fmt.Errorf("write %q: %w", configPath, err)
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
// migrated-<ts>/ directory beside config.yaml. Files are moved (never deleted)
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
