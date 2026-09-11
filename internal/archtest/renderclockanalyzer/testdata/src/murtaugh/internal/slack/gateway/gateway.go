package gateway // want `the renderclock guard is scoped to the chatRenderer interface, which this package no longer declares`

import "time"

type replyWriter struct {
	lastPost time.Time
}

func (w *replyWriter) idle() time.Duration { return time.Since(w.lastPost) }
