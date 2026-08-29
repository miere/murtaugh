//go:build unix

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

func testIdentity() config.LockIdentity {
	return config.LockIdentity{TeamID: "T0000001", AppID: "A0000001"}
}

// openTestLocker opens a locker rooted at dir, failing the test on error.
func openTestLocker(t *testing.T, dir string, identity config.LockIdentity) config.Locker {
	t.Helper()
	locker, err := openLocalLocker(dir, identity)
	if err != nil {
		t.Fatalf("openLocalLocker: %v", err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	return locker
}

// TestLocalLockerExcludesSecondHolder is the whole point of this backend: a
// second gateway on the same machine, against the same Slack app, must not be
// able to take the lock while the first holds it.
func TestLocalLockerExcludesSecondHolder(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	first := openTestLocker(t, dir, testIdentity())
	lease, ok, err := first.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("first Acquire: lease=%+v ok=%v err=%v; want held", lease, ok, err)
	}
	if !lease.Held() {
		t.Fatal("first lease reports not held")
	}
	if lease.Expires() {
		t.Errorf("local lease must not expire, got ExpiresAt=%v", lease.ExpiresAt)
	}

	second := openTestLocker(t, dir, testIdentity())
	got, ok, err := second.Acquire(ctx)
	if err != nil {
		t.Fatalf("second Acquire returned an error; contention is not a failure: %v", err)
	}
	if ok {
		t.Fatalf("second Acquire succeeded while the first held the lock: %+v", got)
	}
}

// TestLocalLockerDifferentConfigDirsStillContend pins the scoping decision: the
// lock is keyed to the Slack identity in a fixed location, so a second daemon
// started under its own sandbox config directory still contends. Keying it to
// the config store instead would let this case through — and that is the case
// that actually happened.
func TestLocalLockerDifferentConfigDirsStillContend(t *testing.T) {
	dir := t.TempDir() // one lock dir stands in for the fixed state dir
	ctx := context.Background()

	live := openTestLocker(t, dir, testIdentity())
	if _, ok, err := live.Acquire(ctx); err != nil || !ok {
		t.Fatalf("live Acquire: ok=%v err=%v", ok, err)
	}

	// A different process, its own config directory, same Slack app.
	sandbox := openTestLocker(t, dir, testIdentity())
	if _, ok, err := sandbox.Acquire(ctx); err != nil || ok {
		t.Fatalf("sandbox Acquire: ok=%v err=%v; want refused", ok, err)
	}
}

// TestLocalLockerDistinctIdentitiesDoNotContend confirms the lock is scoped to
// one Slack app, not to Murtaugh globally: two gateways serving different
// workspaces on one machine are a legitimate deployment.
func TestLocalLockerDistinctIdentitiesDoNotContend(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	a := openTestLocker(t, dir, config.LockIdentity{TeamID: "T0000001", AppID: "A0000001"})
	b := openTestLocker(t, dir, config.LockIdentity{TeamID: "T0000002", AppID: "A0000001"})

	if _, ok, err := a.Acquire(ctx); err != nil || !ok {
		t.Fatalf("workspace A Acquire: ok=%v err=%v", ok, err)
	}
	if _, ok, err := b.Acquire(ctx); err != nil || !ok {
		t.Fatalf("workspace B Acquire: ok=%v err=%v; distinct apps must not contend", ok, err)
	}
}

// TestLocalLockerReleaseLetsStandbyPromote covers the clean-handover path: an
// explicit Release must free the lock immediately rather than making a standby
// wait for process exit.
func TestLocalLockerReleaseLetsStandbyPromote(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	leader := openTestLocker(t, dir, testIdentity())
	lease, ok, err := leader.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("leader Acquire: ok=%v err=%v", ok, err)
	}

	standby := openTestLocker(t, dir, testIdentity())
	if _, ok, _ := standby.Acquire(ctx); ok {
		t.Fatal("standby acquired while leader held the lock")
	}

	if err := leader.Release(ctx, lease); err != nil {
		t.Fatalf("Release: %v", err)
	}
	promoted, ok, err := standby.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("standby Acquire after release: ok=%v err=%v; want promoted", ok, err)
	}
	if promoted.Epoch <= lease.Epoch {
		t.Errorf("epoch did not advance on takeover: leader=%d standby=%d", lease.Epoch, promoted.Epoch)
	}
}

// TestLocalLockerEpochAdvancesAcrossHolders checks that the epoch is carried
// through the lock file rather than reset per process, so a takeover sequence
// stays totally ordered in the journal even across restarts.
func TestLocalLockerEpochAdvancesAcrossHolders(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	identity := testIdentity()

	var last int64
	for i := 0; i < 3; i++ {
		locker, err := openLocalLocker(dir, identity)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		lease, ok, err := locker.Acquire(ctx)
		if err != nil || !ok {
			t.Fatalf("acquire %d: ok=%v err=%v", i, ok, err)
		}
		if lease.Epoch != last+1 {
			t.Errorf("acquisition %d: epoch = %d, want %d", i, lease.Epoch, last+1)
		}
		last = lease.Epoch
		if err := locker.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

// TestLocalLockerVerifyDetectsReplacedLockFile covers the one way holding the
// descriptor is not proof of ownership: if the lock file is unlinked and
// recreated, this process guards an orphaned inode while a challenger locks the
// new one. Verify must notice.
func TestLocalLockerVerifyDetectsReplacedLockFile(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	identity := testIdentity()

	locker := openTestLocker(t, dir, identity)
	lease, ok, err := locker.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}
	if ok, err := locker.Verify(ctx, lease); err != nil || !ok {
		t.Fatalf("Verify on a freshly held lock: ok=%v err=%v", ok, err)
	}

	path := filepath.Join(dir, "leader-"+identity.Key()+".lock")
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove lock file: %v", err)
	}
	if ok, err := locker.Verify(ctx, lease); err != nil || ok {
		t.Fatalf("Verify after unlink: ok=%v err=%v; want not held", ok, err)
	}

	// Recreated by a challenger: a different inode at the same path.
	replacement, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("recreate lock file: %v", err)
	}
	defer replacement.Close()
	if ok, err := locker.Verify(ctx, lease); err != nil || ok {
		t.Fatalf("Verify after replacement: ok=%v err=%v; want not held", ok, err)
	}
}

// TestLocalLockerTTLIsZero pins the contract the election loop branches on: a
// zero TTL means the kernel owns liveness, so no renewal or self-demotion
// machinery should run for this backend.
func TestLocalLockerTTLIsZero(t *testing.T) {
	locker := openTestLocker(t, t.TempDir(), testIdentity())
	if ttl := locker.TTL(); ttl != 0 {
		t.Errorf("TTL() = %v, want 0 (liveness is the kernel's job here)", ttl)
	}
	if backend := locker.Backend(); backend != config.BackendSQLite {
		t.Errorf("Backend() = %q, want %q", backend, config.BackendSQLite)
	}
}

// TestLocalLockerRenewIsIdentity confirms Renew neither extends nor invalidates
// a non-expiring lease, so the caller can run one uniform loop across backends.
func TestLocalLockerRenewIsIdentity(t *testing.T) {
	ctx := context.Background()
	locker := openTestLocker(t, t.TempDir(), testIdentity())
	lease, ok, err := locker.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}
	renewed, ok, err := locker.Renew(ctx, lease)
	if err != nil || !ok {
		t.Fatalf("Renew: ok=%v err=%v", ok, err)
	}
	if renewed != lease {
		t.Errorf("Renew changed the lease: got %+v, want %+v", renewed, lease)
	}
}

// TestOpenLockerRefusesPostgres pins the reported-not-degraded rule: a backend
// with no election implementation must say so rather than hand back a lock that
// only looks like it works.
func TestOpenLockerRefusesPostgres(t *testing.T) {
	_, err := OpenLocker(context.Background(),
		config.DatabaseConfig{Backend: config.BackendPostgres}, testIdentity())
	if err == nil {
		t.Fatal("OpenLocker(postgres) returned no error; want ErrLockUnsupported")
	}
	if !errors.Is(err, config.ErrLockUnsupported) {
		t.Errorf("OpenLocker(postgres) error = %v, want ErrLockUnsupported", err)
	}
}

// TestOpenLockerRejectsIncompleteIdentity guards the identity rule: without a
// team and app there is nothing meaningful to make exclusive, and falling back
// to some default key would let two workspaces collide.
func TestOpenLockerRejectsIncompleteIdentity(t *testing.T) {
	for name, identity := range map[string]config.LockIdentity{
		"no team": {AppID: "A1"},
		"no app":  {TeamID: "T1"},
		"neither": {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := OpenLocker(context.Background(), config.DatabaseConfig{}, identity); err == nil {
				t.Fatal("OpenLocker accepted an incomplete identity")
			}
		})
	}
}
