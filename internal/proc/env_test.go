package proc

import (
	"slices"
	"testing"
)

// TestMergeEnvOverridesTheInherited is the behaviour the auth flow depends on:
// the daemon's environment already carries everything loaded from .env, and an
// override says "not that one, this one".
func TestMergeEnvOverridesTheInherited(t *testing.T) {
	got := MergeEnv(
		[]string{"PATH=/usr/bin", "CLOUDSDK_CONFIG=/home/op/.config/gcloud", "HOME=/home/op"},
		[]string{"CLOUDSDK_CONFIG=/srv/work/.gcloud"},
	)
	want := []string{"PATH=/usr/bin", "CLOUDSDK_CONFIG=/srv/work/.gcloud", "HOME=/home/op"}
	if !slices.Equal(got, want) {
		t.Errorf("MergeEnv = %v, want %v", got, want)
	}
	// One entry, not two: a duplicate key would leave the outcome depending on
	// how the platform resolves it.
	if n := count(got, "CLOUDSDK_CONFIG="); n != 1 {
		t.Errorf("CLOUDSDK_CONFIG appears %d times, want exactly 1", n)
	}
}

// TestMergeEnvAppendsNewKeys covers the variable the parent never had, which is
// the common case for GOOGLE_APPLICATION_CREDENTIALS.
func TestMergeEnvAppendsNewKeys(t *testing.T) {
	got := MergeEnv([]string{"PATH=/usr/bin"}, []string{"AWS_CONFIG_FILE=/srv/work/.aws/config"})
	want := []string{"PATH=/usr/bin", "AWS_CONFIG_FILE=/srv/work/.aws/config"}
	if !slices.Equal(got, want) {
		t.Errorf("MergeEnv = %v, want %v", got, want)
	}
}

// TestMergeEnvWithoutOverridesIsACopy. Nil in, nil out — so a caller with
// nothing to override leaves Spec.Env unset and inherits, rather than pinning a
// snapshot of the environment taken at the wrong moment.
func TestMergeEnvWithoutOverridesIsACopy(t *testing.T) {
	if got := MergeEnv(nil, nil); got != nil {
		t.Errorf("MergeEnv(nil, nil) = %v, want nil", got)
	}
	inherited := []string{"PATH=/usr/bin"}
	got := MergeEnv(inherited, nil)
	if !slices.Equal(got, inherited) {
		t.Errorf("MergeEnv = %v, want the inherited environment", got)
	}
	got[0] = "PATH=/tampered"
	if inherited[0] != "PATH=/usr/bin" {
		t.Error("MergeEnv returned an alias of its input; a caller can corrupt the parent environment")
	}
}

// TestMergeEnvDropsMalformedEntries. An entry with no "=" is malformed to the
// platform, and passing it through silently loses an unrelated variable.
func TestMergeEnvDropsMalformedEntries(t *testing.T) {
	got := MergeEnv([]string{"PATH=/usr/bin", "BROKEN"}, []string{"ALSO_BROKEN", "OK=1"})
	want := []string{"PATH=/usr/bin", "OK=1"}
	if !slices.Equal(got, want) {
		t.Errorf("MergeEnv = %v, want %v", got, want)
	}
}

// TestMergeEnvCollapsesDuplicateParentKeys guards the odd-but-legal parent
// environment holding the same key twice.
func TestMergeEnvCollapsesDuplicateParentKeys(t *testing.T) {
	got := MergeEnv([]string{"A=1", "A=2", "B=3"}, []string{"A=override"})
	want := []string{"A=override", "B=3"}
	if !slices.Equal(got, want) {
		t.Errorf("MergeEnv = %v, want %v", got, want)
	}
}

func count(entries []string, prefix string) int {
	n := 0
	for _, e := range entries {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}
