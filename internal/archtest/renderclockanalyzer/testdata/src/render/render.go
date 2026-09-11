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

type clean struct {
	written []string
}

func (r *clean) Text(_ context.Context, text string) error {
	r.written = append(r.written, text)
	return nil
}
func (r *clean) Finish(context.Context) error  { return nil }
func (r *clean) EnsureStopped(context.Context) {}

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

func (r *stalling) sealIfQuiet() bool {
	return time.Since(r.lastWrite) > 30*time.Second // want `a chatRenderer implementation must not observe time` `a chatRenderer implementation must not observe time`
}

func (r *stalling) sealAfter(_ time.Duration) {} // want `a chatRenderer implementation must not observe time`

type delegating struct {
	h *staleness
}

func (r *delegating) Text(context.Context, string) error { return nil }
func (r *delegating) Finish(context.Context) error       { return nil }
func (r *delegating) EnsureStopped(context.Context)      {}
func (r *delegating) sealIfQuiet() bool                  { return r.h.stale() }

type staleness struct {
	last time.Time
}

func (h *staleness) stale() bool { return time.Since(h.last) > 30*time.Second }

type throttle struct {
	interval time.Duration
	lastPost time.Time
}

func (t *throttle) shouldFlush() bool {
	return time.Since(t.lastPost) >= t.interval
}
