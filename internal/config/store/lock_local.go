//go:build unix

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// localLocker is the leader lock for a single machine, backed by a BSD advisory
// file lock (flock) on a lock file outside the config directory.
//
// It is deliberately the whole of what the SQLite backend promises. SQLite was
// never designed for remote access, so its store is local by construction and
// cannot arbitrate between machines; what it CAN do is stop a second gateway
// starting on the same host against the same Slack app, which is a real and
// already-observed accident (an installer test bootstrapping a second daemon
// beside a live one). Distributed election is Firestore's job.
//
// Two properties follow from using flock rather than a lease, and both are the
// reason to prefer it here:
//
//   - The kernel releases the lock when the holding process dies, however it
//     dies. There is no expiry to tune, no renewal traffic, and no window where
//     a dead holder still looks alive to a challenger.
//   - There is consequently no zombie-leader problem. A lease-based lock has to
//     defend against a holder that was suspended and wakes up still believing it
//     leads; flock has nothing to defend, because a suspended process still
//     genuinely holds its lock and a dead one genuinely does not.
//
// Scope note: the lock file lives in the XDG state directory, NOT beside the
// config store. A lock keyed to the store file would only catch two gateways
// sharing one config.db, and would sail straight past the case that actually
// happened — a second daemon coming up under its own sandbox config directory.
// Keying it to the Slack identity in a fixed location catches both. The scope
// is per-user (the state dir is under $HOME), which covers every realistic case
// on a machine where Murtaugh runs as a launchd user agent.
type localLocker struct {
	path     string
	identity config.LockIdentity

	mu    sync.Mutex
	file  *os.File
	epoch int64
}

// lockFileBody is the diagnostic payload written into the lock file after a
// successful acquisition. Nothing reads it to decide ownership — flock alone
// does that — but it makes a held lock legible to an operator running `cat`,
// and it carries the epoch across process restarts.
type lockFileBody struct {
	Owner    string `json:"owner"`
	Epoch    int64  `json:"epoch"`
	TeamID   string `json:"team_id"`
	AppID    string `json:"app_id"`
	Acquired string `json:"acquired_at"`
}

// openLocalLocker prepares the local leader lock for identity. It creates the
// lock file if absent but does NOT take the lock — Acquire does that, so a
// standby can hold an open locker without contending.
func openLocalLocker(dir string, identity config.LockIdentity) (config.Locker, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, "leader-"+identity.Key()+".lock")
	// O_NOFOLLOW: the lock file is opened by a predictable path, so refuse to
	// follow a symlink planted where it should be.
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %q: %w", path, err)
	}
	return &localLocker{path: path, identity: identity, file: file}, nil
}

func (l *localLocker) Backend() string { return config.BackendSQLite }

// TTL is zero: the lock does not expire, because the kernel drops it when this
// process dies. The election loop reads this and skips renewal entirely.
func (l *localLocker) TTL() time.Duration { return 0 }

// Acquire takes the advisory lock without blocking. A lock held by another
// process on this machine yields ok=false and a nil error — the ordinary
// standby outcome, not a failure.
func (l *localLocker) Acquire(ctx context.Context) (config.Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return config.Lease{}, false, errors.New("local locker is closed")
	}
	if err := ctx.Err(); err != nil {
		return config.Lease{}, false, err
	}

	if err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return config.Lease{}, false, nil
		}
		return config.Lease{}, false, fmt.Errorf("acquire local lock %q: %w", l.path, err)
	}

	// The lock is ours. Carry the epoch forward from whatever the previous
	// holder wrote, so takeovers stay totally ordered across restarts. A
	// corrupt or empty body just restarts the count — the epoch is diagnostic
	// ordering, never a correctness input on this backend.
	prior := l.readBody()
	l.epoch = prior.Epoch + 1
	owner := config.OwnerID()
	if err := l.writeBody(lockFileBody{
		Owner:    owner,
		Epoch:    l.epoch,
		TeamID:   l.identity.TeamID,
		AppID:    l.identity.AppID,
		Acquired: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		// The lock is genuinely held; failing to describe it is not a reason to
		// give it up. Report the lease and let the caller log the write failure.
		return config.Lease{Key: l.identity.Key(), Owner: owner, Epoch: l.epoch}, true, err
	}
	return config.Lease{Key: l.identity.Key(), Owner: owner, Epoch: l.epoch}, true, nil
}

// Renew is a no-op for a non-expiring lock: it re-verifies that this process
// still holds the file and returns the lease unchanged.
func (l *localLocker) Renew(ctx context.Context, lease config.Lease) (config.Lease, bool, error) {
	ok, err := l.Verify(ctx, lease)
	if err != nil || !ok {
		return config.Lease{}, false, err
	}
	return lease, true, nil
}

// Verify confirms this process still holds the lock. Holding the descriptor is
// almost the whole answer — but not quite: if the lock file was unlinked and
// recreated, this process holds a lock on an orphaned inode while a challenger
// happily locks the new one. Comparing inodes closes that gap.
func (l *localLocker) Verify(_ context.Context, lease config.Lease) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil || !lease.Held() || lease.Key != l.identity.Key() {
		return false, nil
	}
	held, err := l.file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat held lock file: %w", err)
	}
	onDisk, err := os.Stat(l.path)
	if err != nil {
		// The path is gone: our lock now guards an inode nobody else will find.
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat lock path %q: %w", l.path, err)
	}
	return sameFile(held, onDisk), nil
}

// Release drops the advisory lock so a standby on this machine can promote at
// once, rather than waiting for this process to exit.
func (l *localLocker) Release(_ context.Context, lease config.Lease) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil || !lease.Held() {
		return nil
	}
	if err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("release local lock %q: %w", l.path, err)
	}
	return nil
}

// Close drops the lock (implicitly, by closing the descriptor) and frees the
// handle. Safe to call more than once.
func (l *localLocker) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	return file.Close()
}

// readBody reads the diagnostic payload, tolerating an empty or malformed file.
func (l *localLocker) readBody() lockFileBody {
	var body lockFileBody
	raw, err := os.ReadFile(l.path)
	if err != nil || len(raw) == 0 {
		return body
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return lockFileBody{}
	}
	return body
}

// writeBody replaces the lock file's contents in place. It writes through the
// held descriptor rather than via a temp-file rename, because renaming would
// swap the inode this process holds its lock on.
func (l *localLocker) writeBody(body lockFileBody) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal lock body: %w", err)
	}
	if err := l.file.Truncate(0); err != nil {
		return fmt.Errorf("truncate lock file %q: %w", l.path, err)
	}
	if _, err := l.file.WriteAt(raw, 0); err != nil {
		return fmt.Errorf("write lock file %q: %w", l.path, err)
	}
	return l.file.Sync()
}

// sameFile reports whether two FileInfos name the same inode.
func sameFile(a, b os.FileInfo) bool { return os.SameFile(a, b) }
