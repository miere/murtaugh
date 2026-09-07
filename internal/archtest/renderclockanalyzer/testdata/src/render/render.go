// Package render is a self-contained fixture mirroring the real
// internal/slack/gateway seam: a package declaring a `chatRenderer` interface,
// one implementation that reaches for a clock (flagged) and one that does not,
// plus a non-renderer type that uses time freely (not flagged, proving the
// scoping is by receiver rather than blanket).
//
// Its package path does not end in /internal/slack/gateway, so the "the
// interface is gone" diagnostic does not apply here; that half is covered by
// the fixture at testdata/src/murtaugh/internal/slack/gateway, which sits at
// the required path and declares no chatRenderer (TestAnalyzerReportsItsOwnDecay).
package render

import (
	"context"
	"time"
)

type chatRenderer interface {
	Text(ctx context.Context, text string) error
	Finish(ctx context.Context) error
	EnsureStopped(ctx context.Context)
}

// clean is the shape the rule wants: it segments output and nothing more.
type clean struct {
	written []string
}

func (r *clean) Text(_ context.Context, text string) error {
	r.written = append(r.written, text)
	return nil
}
func (r *clean) Finish(context.Context) error  { return nil }
func (r *clean) EnsureStopped(context.Context) {}

// stalling is the mistake the rule exists to make impossible: a renderer that
// decides for itself when a turn has gone quiet.
type stalling struct {
	lastWrite time.Time   // want `a chatRenderer implementation must not observe time`
	idle      *time.Timer // want `a chatRenderer implementation must not observe time`
	written   []string
}

func (r *stalling) Text(_ context.Context, text string) error {
	r.written = append(r.written, text)
	r.lastWrite = time.Now() // want `a chatRenderer implementation must not observe time`
	return nil
}

func (r *stalling) Finish(context.Context) error { return nil }

func (r *stalling) EnsureStopped(context.Context) {}

// sealIfQuiet is a HELPER method, not one of the interface methods — exactly
// where a clock would be smuggled in, so it is covered too.
func (r *stalling) sealIfQuiet() bool {
	return time.Since(r.lastWrite) > 30*time.Second // want `a chatRenderer implementation must not observe time` `a chatRenderer implementation must not observe time`
}

// sealAfter is the SIGNATURE half of "inside any method": a clock handed in as
// a parameter needs no reference to time in a body at all.
func (r *stalling) sealAfter(_ time.Duration) {} // want `a chatRenderer implementation must not observe time`

// delegating is the limit the package doc states, written down so it is a known
// gap rather than a surprise: the clock lives on a collaborator and the renderer
// only calls a method on it, so the pass — which follows receivers, not call
// graphs — reports NOTHING here. The absence of a `want` marker on the next
// three declarations is the assertion.
type delegating struct {
	h *staleness
}

func (r *delegating) Text(context.Context, string) error { return nil }
func (r *delegating) Finish(context.Context) error       { return nil }
func (r *delegating) EnsureStopped(context.Context)      {}
func (r *delegating) sealIfQuiet() bool                  { return r.h.stale() }

// staleness is not a chatRenderer, so its clock is invisible to the rule even
// though a renderer is asking it the liveness question.
type staleness struct {
	last time.Time
}

func (h *staleness) stale() bool { return time.Since(h.last) > 30*time.Second }

// throttle is not a chatRenderer: it is the delivery layer below one, where a
// wall clock is rate limiting rather than liveness. It must not be flagged.
type throttle struct {
	interval time.Duration
	lastPost time.Time
}

func (t *throttle) shouldFlush() bool {
	return time.Since(t.lastPost) >= t.interval
}
