package cfg

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/store"
	"github.com/miere/murtaugh/internal/tools"
)

// dbMigrateTool (cfg.db.migrate) copies the whole config store to another
// backend and repoints config.yaml at it. The canonical use is SQLite→Postgres:
//
//	murtaugh cfg db migrate --to postgres --dsn-env MURTAUGH_DB_DSN
//
// The DSN is read from the named .env variable (never passed on the command
// line), and config.yaml records it as ${VAR} so the secret stays out of YAML.
type dbMigrateTool struct {
	p          Provider
	configPath string
}

func (t *dbMigrateTool) Name() string { return "cfg.db.migrate" }
func (t *dbMigrateTool) Description() string {
	return "Migrate the whole config store to another backend (e.g. --to postgres --dsn-env MURTAUGH_DB_DSN)."
}
func (t *dbMigrateTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"to":          {Type: "string", Description: "target backend: postgres | sqlite"},
			"dsn_env":     {Type: "string", Description: "for postgres: name of the .env variable holding the DSN"},
			"sqlite_path": {Type: "string", Description: "for sqlite: destination database path"},
		},
	}
}

func (t *dbMigrateTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	to := strings.ToLower(strings.TrimSpace(mustString(args, "to")))
	src, err := t.p()
	if err != nil {
		return nil, err
	}

	// Resolve the target's connect config (real credentials) and the file config
	// (with a ${VAR} reference) separately, so the secret never lands in YAML.
	var connectDBC, fileDBC config.DatabaseConfig
	switch to {
	case config.BackendPostgres:
		dsnEnv, err := requireString(args, "dsn_env")
		if err != nil {
			return nil, err
		}
		dsn := strings.TrimSpace(os.Getenv(dsnEnv))
		if dsn == "" {
			return nil, fmt.Errorf("%s is empty; set the Postgres DSN in .env first", dsnEnv)
		}
		connectDBC = config.DatabaseConfig{Backend: config.BackendPostgres, Postgres: config.PostgresConfig{DSN: dsn}}
		fileDBC = config.DatabaseConfig{Backend: config.BackendPostgres, Postgres: config.PostgresConfig{DSN: "${" + dsnEnv + "}"}}
	case config.BackendSQLite:
		// An unset path defaults to `<config-basename>.db` beside the bootstrap
		// file; a given --sqlite-path pins it. Keep the same value in the file so a
		// default stays a default (portable) rather than being frozen to an
		// absolute path.
		path := strings.TrimSpace(mustString(args, "sqlite_path"))
		connectDBC = config.DatabaseConfig{Backend: config.BackendSQLite, SQLite: config.SQLiteConfig{Path: path}}
		fileDBC = connectDBC
	default:
		return nil, fmt.Errorf("--to must be postgres or sqlite (got %q)", to)
	}

	if to == src.Backend() {
		return nil, fmt.Errorf("the config store is already using the %s backend", to)
	}

	target, err := store.Open(ctx, connectDBC, filepath.Dir(t.configPath), config.BaseNameOf(t.configPath))
	if err != nil {
		return nil, fmt.Errorf("open target store: %w", err)
	}
	defer target.Close()

	snap, err := src.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if err := target.Restore(ctx, snap); err != nil {
		return nil, fmt.Errorf("copy into target: %w", err)
	}
	if _, err := target.Load(ctx, validationBase); err != nil {
		return nil, fmt.Errorf("target store is invalid after copy: %w", err)
	}

	if strings.TrimSpace(t.configPath) == "" {
		return nil, fmt.Errorf("copied %d items + %d singletons, but the config path is unknown; edit config.yaml's database block by hand", len(snap.Items), len(snap.Singletons))
	}
	if err := store.RewriteDatabaseBlock(t.configPath, fileDBC); err != nil {
		return nil, fmt.Errorf("copied data, but rewriting config.yaml failed: %w", err)
	}
	return okResult{Message: fmt.Sprintf("migrated config to the %s backend (%d items + %d singletons); restart Murtaugh to apply", to, len(snap.Items), len(snap.Singletons))}, nil
}

// mustString returns the string arg or empty (required-ness handled by callers
// that need it; some targets treat it as optional).
func mustString(args map[string]any, key string) string {
	v, _ := stringArg(args, key)
	return v
}

// DBTools returns the cross-backend migration tool.
func DBTools(p Provider, configPath string) []tools.Tool {
	return []tools.Tool{&dbMigrateTool{p: p, configPath: configPath}}
}
