package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

type sqlNodeTokens struct {
	db     *sql.DB
	d      Dialect
	ownsDB bool
}

func openSQLNodeTokens(ctx context.Context, db *sql.DB, d Dialect, ownsDB bool) (config.NodeTokenStore, error) {
	if err := runMigrations(ctx, db, d); err != nil {
		return nil, fmt.Errorf("migrate node-token schema: %w", err)
	}
	return &sqlNodeTokens{db: db, d: d, ownsDB: ownsDB}, nil
}

func openPostgresNodeTokens(ctx context.Context, dsn string) (config.NodeTokenStore, error) {
	if dsn == "" {
		return nil, errors.New("database.postgres.dsn is required for the postgres backend")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres for node tokens: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect postgres for node tokens: %w", err)
	}
	db.SetMaxOpenConns(2)

	tokens, err := openSQLNodeTokens(ctx, db, postgresDialect{}, true)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return tokens, nil
}

func (s *sqlNodeTokens) Close() error {
	if !s.ownsDB {
		return nil
	}
	return s.db.Close()
}

const nodeTokenColumns = `selector, secret_hash, node_id, user_id, label, created_at, expires_at, revoked_at`

func (s *sqlNodeTokens) Put(ctx context.Context, token config.NodeToken) error {
	if err := token.Validate(); err != nil {
		return err
	}
	stmt := fmt.Sprintf(
		`INSERT INTO node_tokens (%s) VALUES (%s, %s, %s, %s, %s, %s, %s, %s)`,
		nodeTokenColumns,
		s.d.Placeholder(1), s.d.Placeholder(2), s.d.Placeholder(3), s.d.Placeholder(4),
		s.d.Placeholder(5), s.d.Placeholder(6), s.d.Placeholder(7), s.d.Placeholder(8))
	_, err := s.db.ExecContext(ctx, stmt,
		token.Selector, token.SecretHash, token.NodeID, token.UserID, token.Label,
		stampKey(token.CreatedAt), stampKey(token.ExpiresAt), stampKey(token.RevokedAt))
	if err != nil {
		return fmt.Errorf("store node token for %q: %w", token.NodeID, err)
	}
	return nil
}

func (s *sqlNodeTokens) BySelector(ctx context.Context, selector string) (config.NodeToken, bool, error) {
	stmt := fmt.Sprintf(`SELECT %s FROM node_tokens WHERE selector = %s`, nodeTokenColumns, s.d.Placeholder(1))
	row := s.db.QueryRowContext(ctx, stmt, selector)
	token, err := scanNodeToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return config.NodeToken{}, false, nil
	}
	if err != nil {
		return config.NodeToken{}, false, fmt.Errorf("look up node token: %w", err)
	}
	return token, true, nil
}

func (s *sqlNodeTokens) List(ctx context.Context, nodeID string) ([]config.NodeToken, error) {
	stmt := fmt.Sprintf(`SELECT %s FROM node_tokens`, nodeTokenColumns)
	args := []any{}
	if nodeID != "" {
		stmt += fmt.Sprintf(` WHERE node_id = %s`, s.d.Placeholder(1))
		args = append(args, nodeID)
	}
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("list node tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []config.NodeToken
	for rows.Next() {
		token, err := scanNodeToken(rows)
		if err != nil {
			return nil, fmt.Errorf("list node tokens: %w", err)
		}
		out = append(out, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list node tokens: %w", err)
	}
	sortNodeTokens(out)
	return out, nil
}

func (s *sqlNodeTokens) Revoke(ctx context.Context, selector string, at time.Time) (config.NodeToken, bool, error) {
	stmt := fmt.Sprintf(
		`UPDATE node_tokens SET revoked_at = %s WHERE selector = %s AND revoked_at = %s`,
		s.d.Placeholder(1), s.d.Placeholder(2), s.d.Placeholder(3))
	if _, err := s.db.ExecContext(ctx, stmt, stampKey(at.UTC()), selector, ""); err != nil {
		return config.NodeToken{}, false, fmt.Errorf("revoke node token: %w", err)
	}
	return s.BySelector(ctx, selector)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanNodeToken(row rowScanner) (config.NodeToken, error) {
	var (
		token                             config.NodeToken
		created, expires, revoked, secret string
	)
	if err := row.Scan(&token.Selector, &secret, &token.NodeID, &token.UserID, &token.Label,
		&created, &expires, &revoked); err != nil {
		return config.NodeToken{}, err
	}
	token.SecretHash = secret

	var err error
	if token.CreatedAt, err = parseStamp(created); err != nil {
		return config.NodeToken{}, err
	}
	if token.ExpiresAt, err = parseStamp(expires); err != nil {
		return config.NodeToken{}, err
	}
	if token.RevokedAt, err = parseStamp(revoked); err != nil {
		return config.NodeToken{}, err
	}
	return token, nil
}

const stampLayout = "2006-01-02T15:04:05.000Z"

func stampKey(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(stampLayout)
}

func parseStamp(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(stampLayout, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("unrecognised node-token timestamp %q", raw)
	}
	return at.UTC(), nil
}

func sortNodeTokens(tokens []config.NodeToken) {
	sort.Slice(tokens, func(i, j int) bool {
		if tokens[i].CreatedAt.Equal(tokens[j].CreatedAt) {
			return tokens[i].Selector < tokens[j].Selector
		}
		return tokens[i].CreatedAt.After(tokens[j].CreatedAt)
	})
}
