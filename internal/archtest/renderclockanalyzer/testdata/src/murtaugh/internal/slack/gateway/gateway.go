// Package gateway is a fixture at the ONE package path the guard requires the
// chatRenderer interface to exist at. It deliberately does not declare it, so
// the pass reports the guard's own decay: a rename that quietly turned the rule
// into a no-op would otherwise pass CI.
package gateway // want `the renderclock guard is scoped to the chatRenderer interface, which this package no longer declares`

import "time"

type replyWriter struct {
	lastPost time.Time
}

func (w *replyWriter) idle() time.Duration { return time.Since(w.lastPost) }
