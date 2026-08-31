package proc

import "strings"

// MergeEnv layers overrides on top of the inherited environment and returns the
// full KEY=VALUE list Spec.Env expects.
//
// Overrides win on a duplicate key, which is the whole point: the inherited
// environment already carries everything the daemon loaded from .env, and a
// caller passing CLOUDSDK_CONFIG is saying "not that one, this one". Go's
// os/exec would also resolve a duplicate last-wins, but relying on that would
// make the precedence a property the reader has to know rather than one they
// can see.
//
// An override replaces its variable IN PLACE rather than being appended, so the
// result reads like the environment the process actually gets. Entries with no
// "=" are dropped: the platform treats them as malformed, and passing one
// through would silently lose an unrelated variable.
func MergeEnv(inherited, overrides []string) []string {
	if len(overrides) == 0 {
		if inherited == nil {
			return nil
		}
		return append([]string(nil), inherited...)
	}

	replaced := make(map[string]string, len(overrides))
	order := make([]string, 0, len(overrides))
	for _, entry := range overrides {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if _, seen := replaced[key]; !seen {
			order = append(order, key)
		}
		replaced[key] = entry
	}

	out := make([]string, 0, len(inherited)+len(order))
	placed := make(map[string]bool, len(order))
	for _, entry := range inherited {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		override, shadowed := replaced[key]
		if !shadowed {
			out = append(out, entry)
			continue
		}
		// A parent environment holding the same key twice collapses to one
		// entry, not two copies of the override.
		if placed[key] {
			continue
		}
		placed[key] = true
		out = append(out, override)
	}
	// Overrides naming a variable the parent never had keep the order they were
	// given, so the result is stable across runs.
	for _, key := range order {
		if !placed[key] {
			out = append(out, replaced[key])
		}
	}
	return out
}
