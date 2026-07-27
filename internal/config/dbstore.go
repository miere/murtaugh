package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadBootstrap parses only the on-disk bootstrap file (the `oauth:` and
// `database:` blocks of config.yaml), loads the sibling .env, and expands the
// Slack tokens' ${VAR} references. It does NOT read the DB or validate — it is
// the minimal step that yields the credentials and the store connection. The
// returned Config carries OAuth, Database, and BaseDir; every other section is
// left zero for the store to fill (Store.Load) or the importer to migrate.
func LoadBootstrap(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, err
	}
	cfg.BaseDir = filepath.Dir(path)
	if err := LoadDotEnv(cfg.BaseDir); err != nil {
		return Config{}, err
	}
	cfg.OAuth.AppToken = os.ExpandEnv(cfg.OAuth.AppToken)
	cfg.OAuth.BotToken = os.ExpandEnv(cfg.OAuth.BotToken)
	cfg.OAuth.UserToken = os.ExpandEnv(cfg.OAuth.UserToken)
	// The database connection is a credential too: the Postgres DSN is referenced
	// as ${VAR} (and the SQLite path may use one), so expand them against .env
	// before the store opens.
	cfg.Database.Postgres.DSN = os.ExpandEnv(cfg.Database.Postgres.DSN)
	cfg.Database.SQLite.Path = os.ExpandEnv(cfg.Database.SQLite.Path)
	return cfg, nil
}

// This file defines the database-backed configuration seam. The bulk of
// Murtaugh's configuration (agents, MCP servers, jobs, rules, and the
// access/chat/defaults/journal/troubleshoot singletons) lives in a database
// behind the Store interface; only credentials stay on disk in the bootstrap
// config.yaml (the `oauth:` and `database:` blocks) and .env. The concrete
// SQLite/Postgres implementation lives in internal/config/store; this package
// owns the contract and the assembly-from-rows logic so that Validate (which
// lives here) stays the single validated core.

// DatabaseConfig is the `database:` block of the bootstrap config.yaml. It
// selects the config-store backend and carries its connection settings. The
// SQLite path is defined here (per the "location in the config file" rule); the
// Postgres DSN is referenced from .env via ${VAR} and never stored literally.
type DatabaseConfig struct {
	// Backend selects the store implementation: "sqlite" (default) or "postgres".
	Backend string `yaml:"backend" json:"backend"`
	// SQLite carries the SQLite-backend settings.
	SQLite SQLiteConfig `yaml:"sqlite" json:"sqlite"`
	// Postgres carries the Postgres-backend settings.
	Postgres PostgresConfig `yaml:"postgres" json:"postgres"`
}

// SQLiteConfig is the `database.sqlite:` block.
type SQLiteConfig struct {
	// Path is the config database file location. Empty defaults to `config.db`
	// in the config directory (beside config.yaml).
	Path string `yaml:"path" json:"path"`
}

// PostgresConfig is the `database.postgres:` block.
type PostgresConfig struct {
	// DSN is the libpq/pgx connection string. It is referenced from .env as
	// ${VAR}; the literal secret never lives in YAML.
	DSN string `yaml:"dsn" json:"dsn"`
}

// Backend names for DatabaseConfig.Backend.
const (
	BackendSQLite   = "sqlite"
	BackendPostgres = "postgres"
)

// IsZero reports whether the database block is absent/unset. A bootstrap
// config.yaml written before this feature has no `database:` block, which is
// the signal the startup path uses to run the one-shot YAML→DB migration.
func (d DatabaseConfig) IsZero() bool {
	return strings.TrimSpace(d.Backend) == "" &&
		strings.TrimSpace(d.SQLite.Path) == "" &&
		strings.TrimSpace(d.Postgres.DSN) == ""
}

// EffectiveBackend resolves the store backend, defaulting to SQLite.
func (d DatabaseConfig) EffectiveBackend() string {
	if b := strings.ToLower(strings.TrimSpace(d.Backend)); b != "" {
		return b
	}
	return BackendSQLite
}

// EffectiveSQLitePath resolves the SQLite config-database path: the configured
// value (with ~ expansion) or, by default, `config.db` in the config directory
// (beside config.yaml), so the store travels with the config it belongs to.
// configDir is the directory holding the bootstrap file; when it is empty (the
// path is unknown) it falls back to the XDG state dir.
func (d DatabaseConfig) EffectiveSQLitePath(configDir string) string {
	if p := strings.TrimSpace(d.SQLite.Path); p != "" {
		return expandHome(p)
	}
	if strings.TrimSpace(configDir) != "" {
		return filepath.Join(configDir, "config.db")
	}
	return filepath.Join(journalStateDir(), "config.db")
}

// Sections group the collection-typed config entities in config_items. Each is
// a map[string]<Type> in Config; a DB row is keyed by (section, name).
const (
	SectionAgent        = "agent"
	SectionMCP          = "mcp"
	SectionJob          = "job"
	SectionWorkflowRule = "workflow_rule"
	SectionUnfurlRule   = "unfurl_rule"
)

// Singleton keys group the single-valued config blocks in config_singletons.
const (
	SingletonAccess       = "access"
	SingletonChat         = "chat"
	SingletonDefaults     = "defaults"
	SingletonJournal      = "journal"
	SingletonTroubleshoot = "troubleshoot"
)

// AllSections and AllSingletons enumerate the valid keys, in a stable order
// suitable for import/export and dump.
var (
	AllSections   = []string{SectionAgent, SectionMCP, SectionJob, SectionWorkflowRule, SectionUnfurlRule}
	AllSingletons = []string{SingletonAccess, SingletonChat, SingletonDefaults, SingletonJournal, SingletonTroubleshoot}
)

// ValidSection reports whether s is a known config_items section.
func ValidSection(s string) bool {
	for _, k := range AllSections {
		if k == s {
			return true
		}
	}
	return false
}

// ValidSingleton reports whether k is a known config_singletons key.
func ValidSingleton(k string) bool {
	for _, s := range AllSingletons {
		if s == k {
			return true
		}
	}
	return false
}

// Store is the backend-agnostic configuration store. It is implemented by
// internal/config/store over SQLite and Postgres. Load assembles and validates
// the whole tree; the granular methods back the `murtaugh cfg …` admin tools.
// Bodies are the JSON serialization of the config structs in this package.
type Store interface {
	// Load reads every row and assembles a validated Config. base supplies the
	// file-sourced fields the DB does not hold — OAuth, BaseDir, Database — which
	// are carried through onto the result.
	Load(ctx context.Context, base Config) (Config, error)

	// UpsertItem inserts or replaces one collection entity (agent/mcp/job/rule).
	UpsertItem(ctx context.Context, section, name string, body any) error
	// DeleteItem removes one entity, reporting whether a row existed.
	DeleteItem(ctx context.Context, section, name string) (bool, error)
	// GetItem returns one entity's raw JSON body, and whether it existed.
	GetItem(ctx context.Context, section, name string) (json.RawMessage, bool, error)
	// ListItems returns every entity in a section keyed by name.
	ListItems(ctx context.Context, section string) (map[string]json.RawMessage, error)

	// PutSingleton inserts or replaces one singleton block.
	PutSingleton(ctx context.Context, key string, body any) error
	// GetSingleton returns a singleton's raw JSON body, and whether it existed.
	GetSingleton(ctx context.Context, key string) (json.RawMessage, bool, error)

	// Snapshot returns every row for backup / cross-backend migration.
	Snapshot(ctx context.Context) (Snapshot, error)
	// Restore writes every row from a snapshot (replacing existing rows).
	Restore(ctx context.Context, snap Snapshot) error

	// Backend reports the store backend name ("sqlite"/"postgres").
	Backend() string
	// Close releases the underlying database handle.
	Close() error
}

// Snapshot is a full dump of the config store, used by export/import and the
// SQLite→Postgres migration tool.
type Snapshot struct {
	Items      []SnapshotItem      `json:"items"`
	Singletons []SnapshotSingleton `json:"singletons"`
}

// SnapshotItem is one config_items row.
type SnapshotItem struct {
	Section string          `json:"section"`
	Name    string          `json:"name"`
	Body    json.RawMessage `json:"body"`
}

// SnapshotSingleton is one config_singletons row.
type SnapshotSingleton struct {
	Key  string          `json:"key"`
	Body json.RawMessage `json:"body"`
}

// AssembleFromRows builds a Config from raw JSON rows plus a base carrying the
// file-sourced fields (OAuth, BaseDir, Database). It unmarshals each row into
// its typed struct, runs the standard Validate, and bakes the global
// defaults.approval into each agent — mirroring the file loader's tail so a
// DB-sourced Config is indistinguishable from a file-sourced one. Store
// implementations SELECT the rows and delegate here.
func AssembleFromRows(base Config, items map[string]map[string]json.RawMessage, singletons map[string]json.RawMessage) (Config, error) {
	// Carry through ONLY the file-sourced fields. Everything else comes from the
	// DB, so a pre-migration base that still carries access:/chat: blocks in its
	// config.yaml can't leak stale values past the store.
	cfg := Config{
		BaseDir:  base.BaseDir,
		OAuth:    base.OAuth,
		Database: base.Database,
	}

	if err := decodeItemMap(items[SectionAgent], func() *AgentProfile { return new(AgentProfile) }, func(m map[string]AgentProfile) { cfg.Agents = m }); err != nil {
		return Config{}, err
	}
	if err := decodeItemMap(items[SectionMCP], func() *MCPServerConfig { return new(MCPServerConfig) }, func(m map[string]MCPServerConfig) { cfg.MCPServers = m }); err != nil {
		return Config{}, err
	}
	if err := decodeItemMap(items[SectionJob], func() *JobProfile { return new(JobProfile) }, func(m map[string]JobProfile) { cfg.Jobs = m }); err != nil {
		return Config{}, err
	}
	if err := decodeItemMap(items[SectionWorkflowRule], func() *WorkflowRuleConfig { return new(WorkflowRuleConfig) }, func(m map[string]WorkflowRuleConfig) { cfg.WorkflowRules = m }); err != nil {
		return Config{}, err
	}
	if err := decodeItemMap(items[SectionUnfurlRule], func() *UnfurlRuleConfig { return new(UnfurlRuleConfig) }, func(m map[string]UnfurlRuleConfig) { cfg.UnfurlRules = m }); err != nil {
		return Config{}, err
	}

	if err := decodeSingleton(singletons, SingletonAccess, &cfg.Access); err != nil {
		return Config{}, err
	}
	if err := decodeSingleton(singletons, SingletonChat, &cfg.Chat); err != nil {
		return Config{}, err
	}
	if err := decodeSingleton(singletons, SingletonDefaults, &cfg.Defaults); err != nil {
		return Config{}, err
	}
	if err := decodeSingleton(singletons, SingletonJournal, &cfg.Journal); err != nil {
		return Config{}, err
	}
	if err := decodeSingleton(singletons, SingletonTroubleshoot, &cfg.Troubleshoot); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	// Bake the global defaults.approval into each agent so every downstream
	// consumer sees the resolved (per-agent over default) policy — identical to
	// the file loader's tail.
	for name, p := range cfg.Agents {
		p.Approval = cfg.EffectiveApproval(p)
		cfg.Agents[name] = p
	}
	return cfg, nil
}

// decodeItemMap unmarshals a section's raw rows into a typed map and hands it to
// assign. A nil/empty section leaves the target map nil (matching the file
// loader, which leaves an absent sibling's map nil).
func decodeItemMap[T any](rows map[string]json.RawMessage, alloc func() *T, assign func(map[string]T)) error {
	if len(rows) == 0 {
		return nil
	}
	out := make(map[string]T, len(rows))
	for name, body := range rows {
		v := alloc()
		if err := json.Unmarshal(body, v); err != nil {
			return fmt.Errorf("decode %q: %w", name, err)
		}
		out[name] = *v
	}
	assign(out)
	return nil
}

// decodeSingleton unmarshals one singleton row into out when present. An absent
// key leaves out at its zero value (matching the file loader).
func decodeSingleton(singletons map[string]json.RawMessage, key string, out any) error {
	body, ok := singletons[key]
	if !ok || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode singleton %q: %w", key, err)
	}
	return nil
}
