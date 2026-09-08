package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/miere/murtaugh/internal/config"
)

// sqlConversationPins holds delegated-conversation pins in a relational store,
// serving both SQLite and Postgres through the Dialect seam.
type sqlConversationPins struct {
	db     *sql.DB
	d      Dialect
	ownsDB bool
}

// openSQLConversationPins prepares the pin store over an existing handle.
func openSQLConversationPins(ctx context.Context, db *sql.DB, d Dialect, ownsDB bool) (config.ConversationPinStore, error) {
	if err := runMigrations(ctx, db, d); err != nil {
		return nil, fmt.Errorf("migrate conversation-pin schema: %w", err)
	}
	return &sqlConversationPins{db: db, d: d, ownsDB: ownsDB}, nil
}

// openPostgresConversationPins opens a dedicated Postgres connection for pins.
func openPostgresConversationPins(ctx context.Context, dsn string) (config.ConversationPinStore, error) {
	if dsn == "" {
		return nil, errors.New("database.postgres.dsn is required for the postgres backend")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres for conversation pins: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect postgres for conversation pins: %w", err)
	}
	db.SetMaxOpenConns(2)

	pins, err := openSQLConversationPins(ctx, db, postgresDialect{}, true)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return pins, nil
}

func (s *sqlConversationPins) Close() error {
	if !s.ownsDB {
		return nil
	}
	return s.db.Close()
}

// dmFlag renders the DM bool as the 0/1 the key column holds. Written as a
// helper rather than inline so the two statements that build the key cannot
// disagree about which integer means which.
func dmFlag(dm bool) int {
	if dm {
		return 1
	}
	return 0
}

func (s *sqlConversationPins) Get(ctx context.Context, ref config.ConversationRef) (config.ConversationPin, bool, error) {
	stmt := fmt.Sprintf(
		`SELECT node_id, user_id, elected_at FROM conversation_pins
		 WHERE team_id = %s AND channel_id = %s AND thread_ts = %s AND dm = %s`,
		s.d.Placeholder(1), s.d.Placeholder(2), s.d.Placeholder(3), s.d.Placeholder(4))
	var nodeID, userID, elected string
	err := s.db.QueryRowContext(ctx, stmt, ref.TeamID, ref.ChannelID, ref.ThreadTS, dmFlag(ref.DM)).
		Scan(&nodeID, &userID, &elected)
	if errors.Is(err, sql.ErrNoRows) {
		return config.ConversationPin{}, false, nil
	}
	if err != nil {
		return config.ConversationPin{}, false, fmt.Errorf("look up conversation pin: %w", err)
	}
	at, err := parseStamp(elected)
	if err != nil {
		return config.ConversationPin{}, false, fmt.Errorf("read conversation pin: %w", err)
	}
	return config.ConversationPin{Conversation: ref, NodeID: nodeID, UserID: userID, ElectedAt: at}, true, nil
}

func (s *sqlConversationPins) Put(ctx context.Context, pin config.ConversationPin) error {
	if err := pin.Validate(); err != nil {
		return err
	}
	// ON CONFLICT DO UPDATE, not INSERT: overwriting is the operation. #170
	// requires a re-election to REPLACE the stored pin rather than leave the
	// dead node's row behind, and a delete-then-insert would leave a window in
	// which a concurrent read saw the conversation as never delegated and
	// elected a third node.
	stmt := fmt.Sprintf(
		`INSERT INTO conversation_pins (team_id, channel_id, thread_ts, dm, node_id, user_id, elected_at)
		 VALUES (%s, %s, %s, %s, %s, %s, %s)
		 ON CONFLICT (team_id, channel_id, thread_ts, dm) DO UPDATE SET
		   node_id = excluded.node_id, user_id = excluded.user_id, elected_at = excluded.elected_at`,
		s.d.Placeholder(1), s.d.Placeholder(2), s.d.Placeholder(3), s.d.Placeholder(4),
		s.d.Placeholder(5), s.d.Placeholder(6), s.d.Placeholder(7))
	ref := pin.Conversation
	if _, err := s.db.ExecContext(ctx, stmt,
		ref.TeamID, ref.ChannelID, ref.ThreadTS, dmFlag(ref.DM),
		pin.NodeID, pin.UserID, stampKey(pin.ElectedAt)); err != nil {
		return fmt.Errorf("store conversation pin for %q: %w", ref.ChannelID, err)
	}
	return nil
}
