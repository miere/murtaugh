package store

// Dialect abstracts the only SQL differences between the SQLite and Postgres
// config-store backends: bind-placeholder style, the JSON and timestamp column
// types used in DDL, and the "current timestamp" expression. Everything else —
// table shape, upsert via ON CONFLICT, excluded.* references — is standard SQL
// both engines accept, so the store body is written once and parameterized by a
// Dialect value.
type Dialect interface {
	// Name is the backend identifier ("sqlite" / "postgres").
	Name() string
	// Placeholder returns the bind marker for the n-th parameter (1-based):
	// "?" for SQLite, "$n" for Postgres.
	Placeholder(n int) string
	// JSONValue is the bind marker for a JSON body parameter, cast to the JSON
	// column type where the driver needs it (Postgres: "$n::jsonb"; SQLite: "?").
	JSONValue(n int) string
	// JSONType is the column type for a JSON document body.
	JSONType() string
	// TimestampType is the column type for the updated_at bookkeeping column.
	TimestampType() string
	// Now is the SQL expression yielding the current timestamp.
	Now() string
	// LeaseExpired is the boolean SQL expression "the lease recorded in
	// acquiredCol, lasting secondsCol seconds, has lapsed" — evaluated by the
	// DATABASE, never by the client.
	//
	// This is the one place the leader lease's correctness meets SQL dialect,
	// and it lives behind the seam rather than inline because getting it wrong
	// is invisible: a comparison against a client-supplied timestamp would
	// compile, pass tests on one machine, and then let two nodes with skewed
	// clocks disagree about whether the incumbent still holds the lock. Both
	// sides of this comparison come from the server.
	LeaseExpired(acquiredCol, secondsCol string) string
}

// sqliteDialect targets modernc.org/sqlite (pure-Go). SQLite has no native JSON
// column type, so bodies live in TEXT; timestamps are ISO-8601 TEXT.
type sqliteDialect struct{}

func (sqliteDialect) Name() string           { return "sqlite" }
func (sqliteDialect) Placeholder(int) string { return "?" }
func (sqliteDialect) JSONValue(int) string   { return "?" }
func (sqliteDialect) JSONType() string       { return "TEXT" }
func (sqliteDialect) TimestampType() string  { return "TEXT" }
func (sqliteDialect) Now() string            { return "CURRENT_TIMESTAMP" }

// LeaseExpired uses SQLite's datetime() modifier arithmetic. Timestamps are
// ISO-8601 text here, which orders correctly under lexicographic comparison as
// long as both sides are produced by SQLite itself — which they are.
func (sqliteDialect) LeaseExpired(acquiredCol, secondsCol string) string {
	return "datetime(" + acquiredCol + ", '+' || " + secondsCol + " || ' seconds') <= CURRENT_TIMESTAMP"
}
