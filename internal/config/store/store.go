// Package store is the database-backed implementation of config.Store. It holds
// Murtaugh's configuration (agents, MCP servers, jobs, workflow/unfurl rules,
// and the access/chat/defaults/journal/troubleshoot singletons) as JSON
// documents in a small relational schema, behind a Dialect seam so the same
// code serves SQLite (the default) and Postgres. Credentials never live here:
// Slack tokens and the database connection stay in the on-disk bootstrap
// gateway.yaml + .env.
package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGo)

	"github.com/miere/murtaugh/internal/config"
)

// sqlstore implements config.Store over a *sql.DB, parameterized by a Dialect.
type sqlstore struct {
	db *sql.DB
	d  Dialect
}

// Open opens (creating if absent) the config store described by dbc and brings
// its schema up to date. The SQLite backend creates its parent directory and
// opens the file in WAL mode; the Postgres backend connects via the DSN. The
// returned Store owns the handle — call Close when done. configDir is the
// directory holding the bootstrap file: a SQLite store with no explicit path
// defaults to `config.db` there.
func Open(ctx context.Context, dbc config.DatabaseConfig, configDir string) (config.Store, error) {
	switch dbc.EffectiveBackend() {
	case config.BackendSQLite:
		return openSQLite(ctx, dbc.EffectiveSQLitePath(configDir))
	case config.BackendPostgres:
		return openPostgres(ctx, dbc.Postgres.DSN)
	default:
		return nil, fmt.Errorf("unknown database backend %q (want sqlite or postgres)", dbc.Backend)
	}
}

// openSQLite opens the SQLite config database at path. It mirrors the journal's
// connection setup: WAL for concurrent readers, a busy_timeout so a brief write
// lock is waited out, and MaxOpenConns=1 so the pragmas stay deterministic.
func openSQLite(ctx context.Context, path string) (config.Store, error) {
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create config db dir %q: %w", dir, err)
			}
		}
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open config db %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return newStore(ctx, db, sqliteDialect{})
}

// newStore runs migrations against an opened handle and returns the store.
func newStore(ctx context.Context, db *sql.DB, d Dialect) (config.Store, error) {
	if err := runMigrations(ctx, db, d); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate config store: %w", err)
	}
	return &sqlstore{db: db, d: d}, nil
}

// sqliteDSN attaches the pragmas every connection must carry (WAL, busy_timeout,
// NORMAL sync), matching the journal store. A ":memory:" path is used verbatim.
func sqliteDSN(path string) string {
	pragmas := url.Values{}
	pragmas.Add("_pragma", "journal_mode(WAL)")
	pragmas.Add("_pragma", "busy_timeout(5000)")
	pragmas.Add("_pragma", "synchronous(NORMAL)")
	pragmas.Add("_pragma", "foreign_keys(0)")
	if strings.HasPrefix(path, "file:") {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		return path + sep + pragmas.Encode()
	}
	return "file:" + path + "?" + pragmas.Encode()
}

func (s *sqlstore) Backend() string { return s.d.Name() }

func (s *sqlstore) Close() error { return s.db.Close() }

// Load reads every row and delegates to config.AssembleFromRows, which decodes,
// validates, and bakes the result into a Config indistinguishable from a
// file-sourced one.
func (s *sqlstore) Load(ctx context.Context, base config.Config) (config.Config, error) {
	items := make(map[string]map[string]json.RawMessage, len(config.AllSections))
	for _, section := range config.AllSections {
		rows, err := s.ListItems(ctx, section)
		if err != nil {
			return config.Config{}, err
		}
		items[section] = rows
	}
	singletons := make(map[string]json.RawMessage, len(config.AllSingletons))
	for _, key := range config.AllSingletons {
		body, ok, err := s.GetSingleton(ctx, key)
		if err != nil {
			return config.Config{}, err
		}
		if ok {
			singletons[key] = body
		}
	}
	return config.AssembleFromRows(base, items, singletons)
}

func (s *sqlstore) UpsertItem(ctx context.Context, section, name string, body any) error {
	if !config.ValidSection(section) {
		return fmt.Errorf("unknown config section %q", section)
	}
	if strings.TrimSpace(name) == "" {
		return errors.New("config item name must not be blank")
	}
	raw, err := marshalBody(body)
	if err != nil {
		return err
	}
	stmt := fmt.Sprintf(
		`INSERT INTO config_items (section, name, body, updated_at) VALUES (%s, %s, %s, %s) `+
			`ON CONFLICT (section, name) DO UPDATE SET body = excluded.body, updated_at = excluded.updated_at`,
		s.d.Placeholder(1), s.d.Placeholder(2), s.d.JSONValue(3), s.d.Now())
	if _, err := s.db.ExecContext(ctx, stmt, section, name, string(raw)); err != nil {
		return fmt.Errorf("upsert %s/%s: %w", section, name, err)
	}
	return nil
}

func (s *sqlstore) DeleteItem(ctx context.Context, section, name string) (bool, error) {
	stmt := fmt.Sprintf(`DELETE FROM config_items WHERE section = %s AND name = %s`, s.d.Placeholder(1), s.d.Placeholder(2))
	res, err := s.db.ExecContext(ctx, stmt, section, name)
	if err != nil {
		return false, fmt.Errorf("delete %s/%s: %w", section, name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil
	}
	return n > 0, nil
}

func (s *sqlstore) GetItem(ctx context.Context, section, name string) (json.RawMessage, bool, error) {
	stmt := fmt.Sprintf(`SELECT body FROM config_items WHERE section = %s AND name = %s`, s.d.Placeholder(1), s.d.Placeholder(2))
	var body string
	err := s.db.QueryRowContext(ctx, stmt, section, name).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get %s/%s: %w", section, name, err)
	}
	return json.RawMessage(body), true, nil
}

func (s *sqlstore) ListItems(ctx context.Context, section string) (map[string]json.RawMessage, error) {
	stmt := fmt.Sprintf(`SELECT name, body FROM config_items WHERE section = %s ORDER BY name`, s.d.Placeholder(1))
	rows, err := s.db.QueryContext(ctx, stmt, section)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", section, err)
	}
	defer rows.Close()
	out := make(map[string]json.RawMessage)
	for rows.Next() {
		var name, body string
		if err := rows.Scan(&name, &body); err != nil {
			return nil, fmt.Errorf("scan %s row: %w", section, err)
		}
		out[name] = json.RawMessage(body)
	}
	return out, rows.Err()
}

func (s *sqlstore) PutSingleton(ctx context.Context, key string, body any) error {
	if !config.ValidSingleton(key) {
		return fmt.Errorf("unknown config singleton %q", key)
	}
	raw, err := marshalBody(body)
	if err != nil {
		return err
	}
	stmt := fmt.Sprintf(
		`INSERT INTO config_singletons (key, body, updated_at) VALUES (%s, %s, %s) `+
			`ON CONFLICT (key) DO UPDATE SET body = excluded.body, updated_at = excluded.updated_at`,
		s.d.Placeholder(1), s.d.JSONValue(2), s.d.Now())
	if _, err := s.db.ExecContext(ctx, stmt, key, string(raw)); err != nil {
		return fmt.Errorf("put singleton %s: %w", key, err)
	}
	return nil
}

func (s *sqlstore) GetSingleton(ctx context.Context, key string) (json.RawMessage, bool, error) {
	stmt := fmt.Sprintf(`SELECT body FROM config_singletons WHERE key = %s`, s.d.Placeholder(1))
	var body string
	err := s.db.QueryRowContext(ctx, stmt, key).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get singleton %s: %w", key, err)
	}
	return json.RawMessage(body), true, nil
}

func (s *sqlstore) Snapshot(ctx context.Context) (config.Snapshot, error) {
	var snap config.Snapshot
	for _, section := range config.AllSections {
		rows, err := s.ListItems(ctx, section)
		if err != nil {
			return config.Snapshot{}, err
		}
		names := make([]string, 0, len(rows))
		for name := range rows {
			names = append(names, name)
		}
		sortStrings(names)
		for _, name := range names {
			snap.Items = append(snap.Items, config.SnapshotItem{Section: section, Name: name, Body: rows[name]})
		}
	}
	for _, key := range config.AllSingletons {
		body, ok, err := s.GetSingleton(ctx, key)
		if err != nil {
			return config.Snapshot{}, err
		}
		if ok {
			snap.Singletons = append(snap.Singletons, config.SnapshotSingleton{Key: key, Body: body})
		}
	}
	return snap, nil
}

func (s *sqlstore) Restore(ctx context.Context, snap config.Snapshot) error {
	for _, item := range snap.Items {
		if err := s.UpsertItem(ctx, item.Section, item.Name, item.Body); err != nil {
			return err
		}
	}
	for _, single := range snap.Singletons {
		if err := s.PutSingleton(ctx, single.Key, single.Body); err != nil {
			return err
		}
	}
	return nil
}

// marshalBody renders a body value to compact JSON. A json.RawMessage / []byte
// is passed through (already-encoded, e.g. from a Snapshot or a --from-file
// path); everything else is marshalled.
func marshalBody(body any) ([]byte, error) {
	switch v := body.(type) {
	case json.RawMessage:
		return compactJSON(v)
	case []byte:
		return compactJSON(v)
	default:
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal config body: %w", err)
		}
		return raw, nil
	}
}

// compactJSON validates and compacts pre-encoded JSON so stored bodies are
// uniform and malformed input is rejected at the write boundary.
func compactJSON(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	return buf.Bytes(), nil
}

// sortStrings is a tiny local sort to avoid importing sort into the hot path of
// a package that otherwise has no need for it.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
