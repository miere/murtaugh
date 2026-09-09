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
// backend and repoints config.yaml at it:
//
//	murtaugh cfg db migrate --to postgres  --dsn-env MURTAUGH_DB_DSN
//	murtaugh cfg db migrate --to firestore --firestore-project my-project
//
// For Postgres the DSN is read from the named .env variable (never passed on
// the command line), and config.yaml records it as ${VAR} so the secret stays
// out of YAML. Firestore has no such secret — it authenticates via ADC — so its
// settings are written literally.
//
// The Firestore target is what a deployment adopting the leader-election
// fallback runs: election needs a store every node can reach, so moving the
// config store is the prerequisite, not a separate choice.
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
			"to":                    {Type: "string", Enum: []any{"postgres", "sqlite", "firestore"}, Description: "target backend"},
			"dsn_env":               {Type: "string", Description: "for postgres: name of the .env variable holding the DSN"},
			"sqlite_path":           {Type: "string", Description: "for sqlite: destination database path"},
			"firestore_project":     {Type: "string", Description: "for firestore: GCP project ID (omit to let ADC decide)"},
			"firestore_database":    {Type: "string", Description: "for firestore: database ID (omit for the project's default database)"},
			"firestore_collection":  {Type: "string", Description: "for firestore: root collection (omit for \"murtaugh\")"},
			"firestore_credentials": {Type: "string", Description: "for firestore: service-account key file (omit to use ADC)"},
		},
		Required: []string{"to"},
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
	case config.BackendFirestore:
		// Firestore has no DSN and no secret to keep out of YAML: it
		// authenticates via Application Default Credentials, and the optional
		// credentials override is a file path rather than a key. So the connect
		// and file configs are the same value — unlike Postgres, there is
		// nothing to split.
		fsc := config.FirestoreConfig{
			ProjectID:       strings.TrimSpace(mustString(args, "firestore_project")),
			DatabaseID:      strings.TrimSpace(mustString(args, "firestore_database")),
			Collection:      strings.TrimSpace(mustString(args, "firestore_collection")),
			CredentialsFile: strings.TrimSpace(mustString(args, "firestore_credentials")),
		}
		connectDBC = config.DatabaseConfig{Backend: config.BackendFirestore, Firestore: fsc}
		fileDBC = connectDBC
	default:
		return nil, fmt.Errorf("--to must be postgres, sqlite, or firestore (got %q)", to)
	}

	if to == src.Backend() {
		return nil, fmt.Errorf("the config store is already using the %s backend", to)
	}

	// Refused BEFORE anything is written, and under THIS process's role.
	//
	// Both halves matter. The role, because a broker gateway is allowed to name
	// an agent profile whose body lives on a node — that is #198's behavioural
	// change, and `validationBase` alone holds the write to the combined rules,
	// so a broker gateway accepted `chat.defaults.agent` at write time and was
	// then refused by the migration, every time, with no way through. And the
	// order, because the only other place to find out is after Restore has
	// copied the whole store into the target and before config.yaml has been
	// rewritten — which leaves a fully populated store nothing points at, and
	// an operator who runs the command again gets a second one.
	if _, err := src.Load(ctx, validationBaseFor()); err != nil {
		return nil, fmt.Errorf("this config store is not valid for a %s install, so copying it would only move the problem: %w", role, err)
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
	// A failure here is the copy, not the configuration — the source passed the
	// same rules a moment ago. Say where the half-finished store is: it is
	// populated, config.yaml does not point at it, and nothing else names it.
	if _, err := target.Load(ctx, validationBaseFor()); err != nil {
		return nil, fmt.Errorf("the copy into the %s backend did not survive being read back, so config.yaml was NOT changed and still points at %s; the %s store now holds a copy that nothing uses: %w",
			to, src.Backend(), to, err)
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
