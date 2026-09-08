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

// sqlNodeTokens holds issued node credentials in a relational store, serving
// both SQLite and Postgres through the Dialect seam.
type sqlNodeTokens struct {
	db     *sql.DB
	d      Dialect
	ownsDB bool
}

// openSQLNodeTokens prepares the credential store over an existing handle.
func openSQLNodeTokens(ctx context.Context, db *sql.DB, d Dialect, ownsDB bool) (config.NodeTokenStore, error) {
	if err := runMigrations(ctx, db, d); err != nil {
		return nil, fmt.Errorf("migrate node-token schema: %w", err)
	}
	return &sqlNodeTokens{db: db, d: d, ownsDB: ownsDB}, nil
}

// openPostgresNodeTokens opens a dedicated Postgres connection for credentials.
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

// nodeTokenColumns is the read projection, in the order scanNodeToken expects.
const nodeTokenColumns = `selector, secret_hash, node_id, user_id, label, created_at, expires_at, revoked_at`

func (s *sqlNodeTokens) Put(ctx context.Context, token config.NodeToken) error {
	if err := token.Validate(); err != nil {
		return err
	}
	// A plain INSERT, with no ON CONFLICT clause: a selector collision must
	// surface as an error rather than overwrite whatever was there. The
	// selector is 64 random bits, so a genuine collision is not a case worth
	// designing for — but a bug that reused one is, and silently replacing a
	// live node's credential is the worst possible way to find out.
	stmt := fmt.Sprintf(
		`INSERT INTO node_tokens (%s) VALUES (%s, %s, %s, %s, %s, %s, %s, %s)`,
		nodeTokenColumns,
		s.d.Placeholder(1), s.d.Placeholder(2), s.d.Placeholder(3), s.d.Placeholder(4),
		s.d.Placeholder(5), s.d.Placeholder(6), s.d.Placeholder(7), s.d.Placeholder(8))
	_, err := s.db.ExecContext(ctx, stmt,
		token.Selector, token.SecretHash, token.NodeID, token.UserID, token.Label,
		stampKey(token.CreatedAt), stampKey(token.ExpiresAt), stampKey(token.RevokedAt))
	if err != nil {
		// The selector is not a secret, but the error travels to logs and to a
		// possibly-unauthenticated caller, so it names the node instead.
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
	// Ordered in Go rather than in SQL so all three backends produce the same
	// listing; the Firestore implementation cannot ORDER BY alongside its
	// equality filter without a hand-created composite index.
	sortNodeTokens(out)
	return out, nil
}

func (s *sqlNodeTokens) Revoke(ctx context.Context, selector string, at time.Time) (config.NodeToken, bool, error) {
	// Conditional on the credential still being live, so a second revocation
	// leaves the original timestamp alone: when a credential stopped being
	// trusted is an audit fact, and the first answer is the true one.
	stmt := fmt.Sprintf(
		`UPDATE node_tokens SET revoked_at = %s WHERE selector = %s AND revoked_at = %s`,
		s.d.Placeholder(1), s.d.Placeholder(2), s.d.Placeholder(3))
	if _, err := s.db.ExecContext(ctx, stmt, stampKey(at.UTC()), selector, ""); err != nil {
		return config.NodeToken{}, false, fmt.Errorf("revoke node token: %w", err)
	}
	return s.BySelector(ctx, selector)
}

// rowScanner is the shape *sql.Row and *sql.Rows share.
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

// stampLayout is the fixed textual form of a node-token timestamp: UTC, fixed
// width, and sortable, so a column of them orders the same way the instants do.
const stampLayout = "2006-01-02T15:04:05.000Z"

// stampKey renders a timestamp for storage. The zero time becomes the empty
// string, which is how "never expires" and "not revoked" are spelled — a
// sentinel date would eventually be reached.
func stampKey(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(stampLayout)
}

// parseStamp is stampKey's inverse.
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

// sortNodeTokens orders a listing newest first, with the selector breaking ties
// so the order is total and the same on every backend.
func sortNodeTokens(tokens []config.NodeToken) {
	sort.Slice(tokens, func(i, j int) bool {
		if tokens[i].CreatedAt.Equal(tokens[j].CreatedAt) {
			return tokens[i].Selector < tokens[j].Selector
		}
		return tokens[i].CreatedAt.After(tokens[j].CreatedAt)
	})
}
