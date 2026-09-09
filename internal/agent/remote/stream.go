package remote

import (
	"sync"

	"github.com/miere/murtaugh/internal/agent"
)

// stream is one turn's event channel plus a drain barrier.
//
// The barrier exists for the same reason the ACP client's does: the sender is a
// long-lived read loop that cannot be stopped per turn, so tearing a turn down
// while a send is in flight would close the channel underneath it and panic the
// process. finish therefore stops new sends, releases any parked one, waits for
// the in-flight ones, and only then closes — which is what makes `for range
// events {}` terminate.
type stream struct {
	// sessionID is the session this turn belongs to, kept so a cancelled
	// context can tell the node to abandon the turn.
	sessionID string
	// location is where in Slack this turn is happening, taken from the prompt
	// on the way out and put back on the context of anything that has to ask
	// the user something on the way in. The gateway's approver refuses to post
	// a card without it — correctly, because a run with no thread has nobody
	// watching — so a GateTool request arriving with a bare context would be
	// auto-approved rather than asked.
	// A headless turn — a job, an unfurl, a workflow trigger — happens nowhere,
	// and its location is simply the zero value. There is no second flag saying
	// so, because agent.TurnLocationFromContext already defines presence as
	// `ok && loc.ChannelID != ""`: a zero-valued location reads as ABSENT to
	// every consumer, which is the answer they are all written for. A parallel
	// `located bool` would be a second, quieter definition of the same word, and
	// the two would eventually disagree.
	location agent.TurnLocation

	events chan agent.Event

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	quit   chan struct{}
}

func newStream(sessionID string, buffer int) *stream {
	return &stream{sessionID: sessionID, events: make(chan agent.Event, buffer), quit: make(chan struct{})}
}

// send delivers one event, blocking while the consumer is behind.
//
// Blocking is deliberate: it is the backpressure that keeps a slow renderer
// from being outrun, and it is bounded by finish, which every teardown path
// calls. A dropped event would be the hole this whole seam exists to prevent.
func (s *stream) send(ev agent.Event) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	select {
	case s.events <- ev:
	case <-s.quit:
	}
}

// finish closes the turn's channel exactly once, after the senders have left.
func (s *stream) finish() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.quit)
	s.mu.Unlock()
	s.wg.Wait()
	close(s.events)
}

// done reports whether this stream has already been finished.
func (s *stream) done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
