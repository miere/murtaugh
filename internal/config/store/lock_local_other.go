//go:build !unix

package store

import (
	"fmt"

	"github.com/miere/murtaugh/internal/config"
)

// openLocalLocker is unavailable off unix: the local backend's guarantee rests
// on flock, and without an equivalent there is no honest way to promise it.
// Murtaugh's daemon targets macOS and Linux, so this exists to keep the package
// building rather than to serve a supported deployment.
func openLocalLocker(string, config.LockIdentity) (config.Locker, error) {
	return nil, fmt.Errorf("%w: local file locking requires a unix platform", config.ErrLockUnsupported)
}
