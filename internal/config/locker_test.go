package config

import (
	"strings"
	"testing"
	"time"
)

// TestLockIdentityKeyIsStableAndDistinct is the property the whole scheme rests
// on: the same workspace+app always produces the same key (so competing nodes
// contend), and any difference produces a different one (so unrelated
// deployments never do).
func TestLockIdentityKeyIsStableAndDistinct(t *testing.T) {
	base := LockIdentity{TeamID: "T0ABCDEF", AppID: "A0123456"}
	if base.Key() != base.Key() {
		t.Fatal("Key() is not deterministic")
	}
	same := LockIdentity{TeamID: "T0ABCDEF", AppID: "A0123456"}
	if base.Key() != same.Key() {
		t.Error("identical identities produced different keys; competing nodes would not contend")
	}

	for name, other := range map[string]LockIdentity{
		"other workspace": {TeamID: "T0OTHER0", AppID: "A0123456"},
		"other app":       {TeamID: "T0ABCDEF", AppID: "A0OTHER0"},
		"swapped":         {TeamID: "A0123456", AppID: "T0ABCDEF"},
	} {
		t.Run(name, func(t *testing.T) {
			if base.Key() == other.Key() {
				t.Errorf("distinct identities collided on key %q", base.Key())
			}
		})
	}
}

// TestLockIdentityKeyHidesWorkspace checks the key does not leak which
// workspace a node serves: the lock file's name sits in a predictable place and
// a Firestore document path may sit in a shared project.
func TestLockIdentityKeyHidesWorkspace(t *testing.T) {
	identity := LockIdentity{TeamID: "T0SECRET", AppID: "A0SECRET"}
	key := identity.Key()
	if strings.Contains(key, identity.TeamID) || strings.Contains(key, identity.AppID) {
		t.Errorf("Key() = %q leaks the raw identity", key)
	}
}

// TestLockIdentityValidate guards against a partially-resolved auth.test
// response producing a lock key that silently collides with another
// deployment's.
func TestLockIdentityValidate(t *testing.T) {
	if err := (LockIdentity{TeamID: "T1", AppID: "A1"}).Validate(); err != nil {
		t.Errorf("valid identity rejected: %v", err)
	}
	for name, identity := range map[string]LockIdentity{
		"empty":      {},
		"no team":    {AppID: "A1"},
		"no app":     {TeamID: "T1"},
		"blank team": {TeamID: "   ", AppID: "A1"},
		"blank app":  {TeamID: "T1", AppID: "\t"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := identity.Validate(); err == nil {
				t.Error("Validate accepted an unusable identity")
			}
		})
	}
}

// TestLeaseHeldAndExpires pins the two predicates callers branch on, including
// the distinction between a lease the OS keeps alive (no expiry) and one that
// must be renewed.
func TestLeaseHeldAndExpires(t *testing.T) {
	var zero Lease
	if zero.Held() {
		t.Error("zero Lease reports Held()")
	}

	local := Lease{Key: "k", Owner: "host/1", Epoch: 1}
	if !local.Held() {
		t.Error("acquired lease reports not Held()")
	}
	if local.Expires() {
		t.Error("a lease with no deadline reports Expires(); the caller would start a pointless renewal loop")
	}

	remote := Lease{Key: "k", Owner: "host/1", Epoch: 1, ExpiresAt: time.Unix(1, 0)}
	if !remote.Expires() {
		t.Error("a lease with a deadline reports it does not expire; the caller would skip renewal and hold a dead claim")
	}
}

// TestOwnerIDIdentifiesTheProcess checks the owner string distinguishes two
// processes on one host — precisely the collision the local backend exists to
// catch, and the thing an operator reads off a takeover notice.
func TestOwnerIDIdentifiesTheProcess(t *testing.T) {
	owner := OwnerID()
	if strings.TrimSpace(owner) == "" {
		t.Fatal("OwnerID() is empty")
	}
	if !strings.Contains(owner, "/") {
		t.Errorf("OwnerID() = %q; want host/pid so two daemons on one host are distinguishable", owner)
	}
}
