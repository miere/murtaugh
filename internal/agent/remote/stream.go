package remote

import (
	"sync"

	"github.com/miere/murtaugh/internal/agent"
)

type stream struct {
	sessionID string
	location  agent.TurnLocation

	events chan agent.Event

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	quit   chan struct{}
}

func newStream(sessionID string, buffer int) *stream {
	return &stream{sessionID: sessionID, events: make(chan agent.Event, buffer), quit: make(chan struct{})}
}

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

func (s *stream) done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
