package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/miere/murtaugh/internal/config"
)

// firestoreJobRuns records scheduled-run claims in Firestore.
//
// The claim is a Create on a document whose ID encodes (job, occurrence).
// Create fails with AlreadyExists if the document is there, which makes it the
// compare-and-swap — the same primitive the leader lock uses for an uncontested
// acquisition, and for the same reason: it needs no read first, so there is no
// window between checking and claiming.
type firestoreJobRuns struct {
	client *firestore.Client
	root   string
}

// Firestore job-run document fields.
const (
	fsRunJob        = "job"
	fsRunOccurrence = "occurrence"
	fsRunNode       = "node"
	fsRunClaimedAt  = "claimed_at"
)

// openFirestoreJobRuns connects to the Firestore claim store.
func openFirestoreJobRuns(ctx context.Context, fsc config.FirestoreConfig) (config.JobRunStore, error) {
	client, err := newFirestoreClient(ctx, fsc)
	if err != nil {
		return nil, err
	}
	return &firestoreJobRuns{client: client, root: fsc.EffectiveCollection()}, nil
}

func (s *firestoreJobRuns) Close() error { return s.client.Close() }

func (s *firestoreJobRuns) runs() *firestore.CollectionRef {
	return s.client.Collection(s.root + "_job_runs")
}

// runDocID renders a claim as a document ID. The job name is escaped for the
// same reasons config item names are (see itemDocID), and the occurrence is
// appended in the fixed sortable layout so the ID is identical on every node.
func runDocID(claim config.JobRunClaim) string {
	return itemDocID("run", claim.Job) + "~" + occurrenceKey(claim.Occurrence)
}

// claimAttempts bounds the retries Claim makes when Firestore aborts the write
// under contention. Contention only happens when several nodes fire the same
// trigger at the same instant, so the losers are resolved within a round or two
// — this is a ceiling, not an expected cost.
const claimAttempts = 5

func (s *firestoreJobRuns) Claim(ctx context.Context, claim config.JobRunClaim, node string) (bool, error) {
	if err := claim.Validate(); err != nil {
		return false, err
	}
	doc := s.runs().Doc(runDocID(claim))
	var err error
	for attempt := 0; attempt < claimAttempts; attempt++ {
		_, err = doc.Create(ctx, map[string]any{
			fsRunJob:        claim.Job,
			fsRunOccurrence: occurrenceKey(claim.Occurrence),
			fsRunNode:       node,
			fsRunClaimedAt:  firestore.ServerTimestamp,
		})
		switch {
		case err == nil:
			return true, nil
		case status.Code(err) == codes.AlreadyExists:
			return false, nil // another node, or an earlier run, has this slot
		case status.Code(err) == codes.Aborted:
			// Our write did not land because someone else held the document at
			// that instant — which says nothing about who ends up owning the
			// slot. Retrying resolves it: the next attempt either creates the
			// document or sees AlreadyExists. Returning "not ours" here would
			// instead let every contender lose and the run be skipped entirely.
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 20 * time.Millisecond):
			}
		default:
			return false, fmt.Errorf("claim run of %q: %w", claim.Job, err)
		}
	}
	return false, fmt.Errorf("claim run of %q: %w", claim.Job, err)
}

func (s *firestoreJobRuns) LastRun(ctx context.Context, job string) (time.Time, bool, error) {
	// An equality filter plus an OrderBy on a different field would require a
	// composite index that an operator would have to create by hand before this
	// worked at all. Equality alone is served by the automatic single-field
	// index, so this scans one job's claims and takes the maximum in Go. The
	// occurrence layout sorts lexically, so string comparison is date ordering.
	//
	// The scan is bounded by retention: Prune keeps a job to at most a week of
	// claims, and this is a diagnostic path, not the scheduler's hot path —
	// Claim never reads.
	iter := s.runs().Where(fsRunJob, "==", job).Documents(ctx)
	defer iter.Stop()

	var latest string
	for {
		snap, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return time.Time{}, false, fmt.Errorf("last run of %q: %w", job, err)
		}
		value, err := docString(snap, fsRunOccurrence)
		if err != nil {
			continue // a hand-edited document is not worth failing the scheduler
		}
		if value > latest {
			latest = value
		}
	}
	if latest == "" {
		return time.Time{}, false, nil
	}
	at, err := time.Parse(occurrenceLayout, latest)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("last run of %q: unrecognised occurrence %q", job, latest)
	}
	return at.UTC(), true, nil
}

func (s *firestoreJobRuns) Prune(ctx context.Context, before time.Time) (int64, error) {
	cutoff := occurrenceKey(before)
	iter := s.runs().Where(fsRunOccurrence, "<", cutoff).Documents(ctx)
	defer iter.Stop()

	var (
		deleted int64
		batch   = s.client.BulkWriter(ctx)
	)
	for {
		snap, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return deleted, fmt.Errorf("prune job runs: %w", err)
		}
		if _, err := batch.Delete(snap.Ref); err != nil {
			return deleted, fmt.Errorf("prune job runs: %w", err)
		}
		deleted++
	}
	batch.End()
	return deleted, nil
}
