package nodeserve

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
)

const transferChunkBytes = 64 << 10

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

func (s *Server) forward(ctx context.Context, stream string, ev agent.Event) error {
	if ev.Type == agent.EventError {
		ev.Error = s.turnFailed(ev.Error)
	}
	wire, transfer, err := s.enc.Encode(ev)
	if err != nil {
		return s.sendOn(ctx, s.errorFrame(stream, fmt.Errorf("this node could not send an event: %w", err)))
	}
	if transfer != nil {
		if err := s.sendTransfer(ctx, transfer); err != nil {
			return s.sendOn(ctx, s.errorFrame(stream, fmt.Errorf("attachment %q could not be transferred: %w", wire.Attachment.Filename, err)))
		}
	}
	if wire.Permission != nil {
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
		last, err := agentwire.Chunk(agentwire.TransferChunk{
			TransferID: t.ID, Seq: seq, Last: true, Error: readErr.Error(),
		})
		if err == nil {
			_ = s.sendOn(ctx, last)
		}
		return readErr
	}
}

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
