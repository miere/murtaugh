package nodeserve

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
)

// transferChunkBytes is one attachment chunk's payload.
//
// It is far below agentwire.MaxTransferChunkBytes (4 MiB) on purpose: a chunk
// larger than the link's window can only ever travel alone, so a 100 MiB file
// would become twenty-five serialised round trips. Sized under the window, the
// transfer streams and the window does the pacing.
const transferChunkBytes = 64 << 10

// pump forwards one turn's events and closes its stream.
//
// It drains `events` to completion even after the transport has failed. Both
// in-process consumers do `cancel(); for range events {}` and the backends are
// written to that contract: abandoning the channel parks the backend's own
// emitter goroutine, which on the native loop means the turn never ends.
func (s *Server) pump(ctx context.Context, stream string, events <-chan agent.Event) {
	healthy := true
	for ev := range events {
		if !healthy {
			continue
		}
		if err := s.forward(ctx, stream, ev); err != nil {
			healthy = false
			if !errors.Is(err, context.Canceled) {
				s.log.Warn("nodeserve: could not forward an event", "stream", stream, "error", err)
			}
		}
	}
	s.endTurn(stream, healthy)
}

// forward puts one event on the wire, with its bytes ahead of it when it has
// any.
func (s *Server) forward(ctx context.Context, stream string, ev agent.Event) error {
	wire, transfer, err := s.enc.Encode(ev)
	if err != nil {
		// An event this build cannot serialise is reported ON the turn. Dropping
		// it would leave a hole in a sentence that nothing detects, which is the
		// failure mode the whole envelope exists to prevent.
		return s.sendOn(ctx, s.errorFrame(stream, fmt.Errorf("this node could not send an event: %w", err)))
	}
	if transfer != nil {
		if err := s.sendTransfer(ctx, transfer); err != nil {
			return s.sendOn(ctx, s.errorFrame(stream, fmt.Errorf("attachment %q could not be transferred: %w", wire.Attachment.Filename, err)))
		}
	}
	if wire.Permission != nil {
		// A backend-raised (GateAgent) request: the Encoder holds the channel
		// its goroutine is blocked on, so this side only records which turn it
		// belongs to, for the teardown that has to answer it.
		s.register(wire.Permission.ID, stream, nil)
	}
	switch {
	case wire.Question != nil:
		s.registerDisplay(wire.Question.ID, stream)
	case wire.Plan != nil:
		s.registerDisplay(wire.Plan.ID, stream)
	case wire.SignIn != nil:
		s.registerSignIn(wire.SignIn.ID, stream)
	case wire.SignInSettled != nil && agent.SignInState(wire.SignInSettled.State).Terminal():
		s.forget(wire.SignInSettled.ID)
	}
	msg, err := agentwire.StreamEvent(stream, wire)
	if err != nil {
		return err
	}
	return s.sendOn(ctx, msg)
}

// sendTransfer streams an attachment's bytes, BEFORE the event that references
// them.
//
// Order is the whole design. The gateway decodes an attachment event by asking
// its deliverer for the bytes, on the read loop; a deliverer that had to pull
// chunks that arrive on that same read loop would deadlock it. Sending the
// chunks first means the bytes are already on disk when the event lands, and
// the deliverer is a map lookup.
func (s *Server) sendTransfer(ctx context.Context, t *agentwire.Transfer) error {
	defer func() { _ = t.Body.Close() }()
	buf := make([]byte, transferChunkBytes)
	seq := 0
	for {
		n, readErr := t.Body.Read(buf)
		if n > 0 {
			chunk, err := agentwire.Chunk(agentwire.TransferChunk{
				TransferID: t.ID,
				Seq:        seq,
				Data:       append([]byte(nil), buf[:n]...),
			})
			if err != nil {
				return err
			}
			if err := s.sendOn(ctx, chunk); err != nil {
				return err
			}
			seq++
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			last, err := agentwire.Chunk(agentwire.TransferChunk{TransferID: t.ID, Seq: seq, Last: true})
			if err != nil {
				return err
			}
			return s.sendOn(ctx, last)
		}
		// A file that stopped being readable is a fact the consumer must be
		// told, or it waits for chunks that will never come.
		last, err := agentwire.Chunk(agentwire.TransferChunk{
			TransferID: t.ID, Seq: seq, Last: true, Error: readErr.Error(),
		})
		if err == nil {
			_ = s.sendOn(ctx, last)
		}
		return readErr
	}
}

// errorFrame renders err as an error event on a turn's stream, through the
// Encoder so the discriminant survives the hop.
func (s *Server) errorFrame(stream string, err error) agentwire.Message {
	wire, _, encodeErr := s.enc.Encode(agent.Event{Type: agent.EventError, Error: err})
	if encodeErr != nil {
		wire = agentwire.Event{Type: agentwire.EventError, Text: err.Error()}
	}
	msg, buildErr := agentwire.StreamEvent(stream, wire)
	if buildErr != nil {
		return agentwire.StreamEnd(stream)
	}
	return msg
}
