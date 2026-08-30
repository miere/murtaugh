package config

import (
	"context"
	"encoding/json"
	"fmt"
)

// RevertToSnapshot makes the store's contents match snap exactly.
//
// This is the rollback the change-approval flow needs when an admin rejects an
// edit, and it is deliberately NOT Store.Restore. Restore upserts: it writes
// every row in the snapshot and leaves anything else alone, which is right for
// copying a store into another backend and wrong for reverting. Rejecting the
// addition of an agent has to REMOVE that agent, and an upsert-only revert
// would leave it in place — the admin would be told the change was rolled back
// while the thing they refused kept running.
//
// So this reverts in both directions: rows in snap are written, and rows in the
// store that snap does not have are deleted.
//
// Singletons are handled by writing rather than deleting. Every singleton key
// is known and every one has a meaningful zero value, so an absent key and a
// zero-valued key mean the same thing to AssembleFromRows; writing the zero
// value is a faithful revert and avoids widening the Store interface with a
// delete verb that has exactly one caller.
//
// It is not atomic. A failure partway leaves the store between the two states,
// which the caller must report rather than swallow — a half-reverted config is
// the one outcome worse than either whole one.
func RevertToSnapshot(ctx context.Context, store Store, snap Snapshot) error {
	target := indexSnapshot(snap)

	for _, section := range AllSections {
		current, err := store.ListItems(ctx, section)
		if err != nil {
			return fmt.Errorf("revert %s: %w", section, err)
		}
		for name := range current {
			if _, keep := target.items[section][name]; keep {
				continue
			}
			if _, err := store.DeleteItem(ctx, section, name); err != nil {
				return fmt.Errorf("revert %s/%s: %w", section, name, err)
			}
		}
		for name, body := range target.items[section] {
			if err := store.UpsertItem(ctx, section, name, body); err != nil {
				return fmt.Errorf("revert %s/%s: %w", section, name, err)
			}
		}
	}

	for _, key := range AllSingletons {
		body, ok := target.singletons[key]
		if !ok {
			// Absent from the target: write the zero value, which is what an
			// absent row assembles to anyway.
			body = json.RawMessage("{}")
		}
		if err := store.PutSingleton(ctx, key, body); err != nil {
			return fmt.Errorf("revert singleton %s: %w", key, err)
		}
	}
	return nil
}

// snapshotIndex is a snapshot keyed for lookup.
type snapshotIndex struct {
	items      map[string]map[string]json.RawMessage
	singletons map[string]json.RawMessage
}

func indexSnapshot(snap Snapshot) snapshotIndex {
	idx := snapshotIndex{
		items:      make(map[string]map[string]json.RawMessage, len(AllSections)),
		singletons: make(map[string]json.RawMessage, len(snap.Singletons)),
	}
	for _, item := range snap.Items {
		if idx.items[item.Section] == nil {
			idx.items[item.Section] = map[string]json.RawMessage{}
		}
		idx.items[item.Section][item.Name] = item.Body
	}
	for _, single := range snap.Singletons {
		idx.singletons[single.Key] = single.Body
	}
	return idx
}
