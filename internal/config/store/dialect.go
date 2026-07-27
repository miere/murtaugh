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
	// JSONType is the column type for a JSON document body.
	JSONType() string
	// TimestampType is the column type for the updated_at bookkeeping column.
	TimestampType() string
	// Now is the SQL expression yielding the current timestamp.
	Now() string
}

// sqliteDialect targets modernc.org/sqlite (pure-Go). SQLite has no native JSON
// column type, so bodies live in TEXT; timestamps are ISO-8601 TEXT.
type sqliteDialect struct{}

func (sqliteDialect) Name() string           { return "sqlite" }
func (sqliteDialect) Placeholder(int) string { return "?" }
func (sqliteDialect) JSONType() string       { return "TEXT" }
func (sqliteDialect) TimestampType() string  { return "TEXT" }
func (sqliteDialect) Now() string            { return "CURRENT_TIMESTAMP" }
