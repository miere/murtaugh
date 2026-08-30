package store

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// postgresTestDSN returns the integration DSN, skipping the test when it is
// unset so the default `go test ./...` stays green without a database. CI sets
// it to the Postgres service container; locally, `docker compose up -d postgres`
// plus the DSN in docker-compose.yml's header comment does the same job.
func postgresTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MURTAUGH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set MURTAUGH_TEST_POSTGRES_DSN (e.g. via docker compose up -d) to run Postgres tests")
	}
	return dsn
}

// uniqueAppID mints a lock identity nothing else will collide with.
//
// The leader lock is a single row keyed by Slack identity, and unlike the
// config tables it cannot simply be truncated between runs: a test that
// inherited a live lease from the previous run would see every acquisition
// refused and fail for a reason that has nothing to do with the code. Each
// caller therefore gets its own key, so runs and cases are independent even
// against a long-lived shared database.
func uniqueAppID() string {
	return fmt.Sprintf("B%d-%d", time.Now().UnixNano(), lockKeySeq.Add(1))
}

var lockKeySeq atomic.Int64
