package nodelink

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// sink records what the link handed to its consumer, in order.
type sink struct {
	mu   sync.Mutex
	got  []string
	fail error
	// block, when non-nil, parks the handler on its first frame — the "the
	// consumer is behind" case, which is meant to stop acknowledgements rather
	// than lose frames. entered is signalled just before parking, so a test can
	// assert on the link's state while the handler is provably still running
	// rather than racing it.
	block   chan struct{}
	entered chan struct{}
}

func (s *sink) handle(payload []byte) error {
	if s.block != nil {
		select {
		case s.entered <- struct{}{}:
		default: // nobody is watching this frame; never park the link on the signal
		}
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.got = append(s.got, string(payload))
	return nil
}

func (s *sink) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

// lossyConn drops one outbound frame, which is the only interesting way an
// ordered transport can fail: the frames either side of the hole still arrive.
type lossyConn struct {
	Conn
	drop  int
	count int
}

func (c *lossyConn) WriteMessage(raw []byte) error {
	c.count++
	if c.count == c.drop {
		return nil
	}
	return c.Conn.WriteMessage(raw)
}

func payload(n int) []byte { return []byte(fmt.Sprintf("%q", fmt.Sprintf("frame-%d", n))) }

func writeRaw(t *testing.T, conn Conn, env Envelope) {
	t.Helper()
	raw, err := Encode(env)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := conn.WriteMessage(raw); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readEnv(t *testing.T, conn Conn) Envelope {
	t.Helper()
	raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	env, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSequenceNumbersAreContiguousAndAcksConsumeNone(t *testing.T) {
	ctx := context.Background()
	a, b := Pipe(32)
	link := New(a, Options{Handler: (&sink{}).handle, AckThreshold: 1})
	defer link.Close()

	for i := 1; i <= 3; i++ {
		if err := link.Send(ctx, payload(i)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	for i := uint64(1); i <= 3; i++ {
		env := readEnv(t, b)
		if env.Kind != KindMessage || env.Seq != i {
			t.Fatalf("frame %d came out as %s seq %d", i, env.Kind, env.Seq)
		}
	}

	// Make the link acknowledge something, so a standalone ack is emitted
	// between two message frames.
	writeRaw(t, b, Message(1, 0, payload(99)))
	ack := readEnv(t, b)
	if ack.Kind != KindAck {
		t.Fatalf("expected a standalone ack, got %s", ack.Kind)
	}
	if ack.Seq != 0 {
		t.Fatalf("the ack consumed sequence %d; acks must consume none", ack.Seq)
	}
	if ack.Ack != 1 {
		t.Fatalf("ack watermark = %d, want 1", ack.Ack)
	}

	if err := link.Send(ctx, payload(4)); err != nil {
		t.Fatalf("send 4: %v", err)
	}
	next := readEnv(t, b)
	if next.Seq != 4 {
		t.Fatalf("the ack moved the sequence counter: next frame is %d, want 4", next.Seq)
	}
	if next.Ack != 1 {
		t.Fatalf("the message frame did not carry the watermark: ack = %d", next.Ack)
	}
}

// TestFrameIsAcknowledgedOnlyAfterTheHandlerReturns pins the guarantee doc.go
// states exactly — a frame is acknowledged when the local Handler has RETURNED,
// not when it was decoded — which is the property the reconnect loop (#193,
// #197) is built on: whatever is not acknowledged is replayed, so a frame the
// consumer never finished must still be owed.
//
// The handler is parked mid-frame, which is the "the consumer is behind" case:
// the watermark must not move and no acknowledgement may go out until it
// returns. Advancing the watermark before the call — the obvious tidy-up in
// receive — makes this fail here rather than lose a frame across a reconnect.
func TestFrameIsAcknowledgedOnlyAfterTheHandlerReturns(t *testing.T) {
	a, b := Pipe(32)
	consumer := &sink{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	link := New(a, Options{Handler: consumer.handle, AckThreshold: 1})
	defer link.Close()

	writeRaw(t, b, Message(1, 0, payload(1)))
	select {
	case <-consumer.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler was never called")
	}

	// The handler is provably inside the call at this point.
	if got := link.LastSeen(); got != 0 {
		t.Fatalf("watermark advanced to %d while the handler was still running; "+
			"an unfinished frame would not be replayed after a reconnect", got)
	}
	acked := make(chan Envelope, 1)
	go func() {
		raw, err := b.ReadMessage()
		if err != nil {
			return
		}
		env, err := Decode(raw)
		if err != nil {
			return
		}
		acked <- env
	}()
	select {
	case env := <-acked:
		t.Fatalf("the link acknowledged %d before the handler returned", env.Ack)
	case <-time.After(50 * time.Millisecond):
	}

	close(consumer.block)
	waitFor(t, "the watermark to advance once the handler returned", func() bool { return link.LastSeen() == 1 })
	select {
	case env := <-acked:
		if env.Kind != KindAck || env.Ack != 1 {
			t.Fatalf("after the handler returned the link sent %s ack=%d, want an ack of frame 1", env.Kind, env.Ack)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the frame was never acknowledged after the handler returned")
	}
}

func TestAckPrunesTheRetransmitBuffer(t *testing.T) {
	ctx := context.Background()
	a, b := Pipe(32)
	link := New(a, Options{Handler: (&sink{}).handle})
	defer link.Close()

	for i := 1; i <= 3; i++ {
		if err := link.Send(ctx, payload(i)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		readEnv(t, b)
	}
	if frames, _ := link.Pending(); frames != 3 {
		t.Fatalf("retransmit buffer holds %d frames, want 3", frames)
	}

	writeRaw(t, b, Ack(2))
	waitFor(t, "the acknowledged prefix to be released", func() bool {
		frames, _ := link.Pending()
		return frames == 1
	})

	// An older acknowledgement carries no information and must not resurrect
	// what has already been released. The buffer alone cannot show that — the
	// prune loop is a no-op for a lower ack either way — so the real assertion
	// is on peerAck: roll it back and a resume from frame 1 starts answering OK
	// and replaying a suffix with frame 2 missing, which is the hole this
	// package exists to prevent.
	writeRaw(t, b, Ack(1))
	writeRaw(t, b, Message(1, 0, payload(50)))
	waitFor(t, "the stale ack to be processed", func() bool { return link.LastSeen() == 1 })
	if frames, _ := link.Pending(); frames != 1 {
		t.Fatalf("a stale ack changed the buffer: %d frames", frames)
	}
	if _, err := link.ReplayFrom(1); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("ReplayFrom(1) after a stale ack = %v, want %v: the acknowledged prefix is gone "+
			"and a resume from before it cannot be answered", err, ErrNotResumable)
	}
}

func TestWindowBlocksUntilAcknowledged(t *testing.T) {
	ctx := context.Background()
	a, b := Pipe(32)
	link := New(a, Options{Handler: (&sink{}).handle, WindowBytes: len(payload(1)) + 1})
	defer link.Close()

	if err := link.Send(ctx, payload(1)); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	readEnv(t, b)

	blocked := make(chan error, 1)
	go func() { blocked <- link.Send(ctx, payload(2)) }()
	select {
	case err := <-blocked:
		t.Fatalf("the second send did not wait for room: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	writeRaw(t, b, Ack(1))
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("send after the window opened: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the acknowledgement did not open the window")
	}
	if env := readEnv(t, b); env.Seq != 2 {
		t.Fatalf("the released frame is seq %d, want 2", env.Seq)
	}
}

// A few large frames must be acknowledged on bytes, not only on a frame count.
//
// This is a deadlock, not a stall, and it was found by sending a real
// attachment over a real socket: four 64 KiB chunks fill a 256 KiB window while
// the frame count is still three short of its threshold, so the sender waits
// for an acknowledgement that consuming more frames would trigger — and no more
// frames can be sent. The consumed-byte trigger is what breaks the cycle.
func TestLargeFramesAreAcknowledgedBeforeTheWindowFills(t *testing.T) {
	ctx := context.Background()
	a, b := Pipe(32)

	// A window of four frames, and an ack threshold that would never be reached
	// within it.
	frame := payload(1)
	window := len(frame) * 4
	consumer := New(b, Options{Handler: (&sink{}).handle, WindowBytes: window, AckThreshold: 1000})
	defer consumer.Close()
	sender := New(a, Options{Handler: (&sink{}).handle, WindowBytes: window, AckThreshold: 1000})
	defer sender.Close()

	for i := range 12 {
		send, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := sender.Send(send, frame)
		cancel()
		if err != nil {
			t.Fatalf("frame %d wedged the window: %v", i+1, err)
		}
	}
}

// A blocked send must come back when its caller gives up, or a cancelled turn
// leaks the goroutine that was writing it.
func TestBlockedSendReturnsOnContextCancel(t *testing.T) {
	a, b := Pipe(32)
	link := New(a, Options{Handler: (&sink{}).handle, WindowBytes: len(payload(1)) + 1})
	defer link.Close()

	if err := link.Send(context.Background(), payload(1)); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	readEnv(t, b)

	ctx, cancel := context.WithCancel(context.Background())
	blocked := make(chan error, 1)
	go func() { blocked <- link.Send(ctx, payload(2)) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-blocked:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked send returned %v, want a cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a cancelled send stayed parked on the window")
	}
}

// The named acceptance test: a dropped frame is DETECTED, not swallowed.
//
// The two assertions that matter are the second and the fourth. A transport
// that reported the gap and then carried on delivering would leave a hole in a
// sentence with a warning line nobody reads; a transport that stayed open would
// keep every later frame in the same fiction.
func TestDroppedFrameIsDetectedNotSwallowed(t *testing.T) {
	ctx := context.Background()
	a, b := Pipe(32)
	consumer := &sink{}
	sender := New(&lossyConn{Conn: a, drop: 2}, Options{})
	receiver := New(b, Options{Handler: consumer.handle})
	defer sender.Close()
	defer receiver.Close()

	for i := 1; i <= 3; i++ {
		if err := sender.Send(ctx, payload(i)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	select {
	case <-receiver.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the gap was swallowed: the link is still running")
	}

	err := receiver.Err()
	if !errors.Is(err, ErrSequenceGap) {
		t.Fatalf("link failed with %v, want a sequence gap", err)
	}
	var gap *SequenceGapError
	if !errors.As(err, &gap) {
		t.Fatalf("the failure does not name the hole: %v", err)
	}
	if gap.Want != 2 || gap.Got != 3 {
		t.Fatalf("gap = %+v, want frame 2, received 3", gap)
	}
	if got := consumer.seen(); len(got) != 1 || got[0] != string(payload(1)) {
		t.Fatalf("consumer saw %v; nothing after the gap may be delivered", got)
	}
	waitFor(t, "the connection to be torn down", func() bool {
		return sender.Send(ctx, payload(4)) != nil
	})
}

// A duplicate is only legal after a resume replayed a suffix this side had
// already consumed. It must not be delivered a second time, and it must not
// move the watermark backwards.
func TestDuplicateFramesAreDroppedNotDelivered(t *testing.T) {
	a, b := Pipe(32)
	consumer := &sink{}
	link := New(a, Options{Handler: consumer.handle})
	defer link.Close()

	writeRaw(t, b, Message(1, 0, payload(1)))
	writeRaw(t, b, Message(2, 0, payload(2)))
	writeRaw(t, b, Message(2, 0, payload(2)))
	writeRaw(t, b, Message(1, 0, payload(1)))
	writeRaw(t, b, Message(3, 0, payload(3)))

	waitFor(t, "the third frame to be consumed", func() bool { return len(consumer.seen()) == 3 })
	time.Sleep(20 * time.Millisecond)
	got := consumer.seen()
	if len(got) != 3 {
		t.Fatalf("consumer saw %v; a replayed frame was delivered twice", got)
	}
	for i, want := range []int{1, 2, 3} {
		if got[i] != string(payload(want)) {
			t.Fatalf("consumer saw %v, want frames 1..3 in order", got)
		}
	}
	if link.LastSeen() != 3 {
		t.Fatalf("watermark = %d after a duplicate, want 3", link.LastSeen())
	}
	select {
	case <-link.Done():
		t.Fatalf("a duplicate killed the link: %v", link.Err())
	default:
	}
}

func TestReplayFromReturnsTheUnacknowledgedSuffix(t *testing.T) {
	ctx := context.Background()
	a, b := Pipe(32)
	link := New(a, Options{Handler: (&sink{}).handle, Epoch: 7})
	defer link.Close()

	for i := 1; i <= 4; i++ {
		if err := link.Send(ctx, payload(i)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		readEnv(t, b)
	}
	writeRaw(t, b, Ack(2))
	waitFor(t, "the acknowledged prefix to be released", func() bool {
		frames, _ := link.Pending()
		return frames == 2
	})

	frames, err := link.ReplayFrom(3)
	if err != nil {
		t.Fatalf("replay from 3: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("replay returned %d frames, want 1", len(frames))
	}
	if env, err := Decode(frames[0]); err != nil || env.Seq != 4 {
		t.Fatalf("replayed frame is %+v (%v), want seq 4", env, err)
	}

	// Below the floor: the peer is asking for frames it already acknowledged,
	// and pretending otherwise would hand it a stream with a hole in it.
	if _, err := link.ReplayFrom(1); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("replay below the floor returned %v, want not-resumable", err)
	}
	// Above what was ever sent: not the same stream.
	if _, err := link.ReplayFrom(99); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("replay beyond the stream returned %v, want not-resumable", err)
	}
}

func TestAcceptResumeRefusesAnotherEpoch(t *testing.T) {
	ctx := context.Background()
	a, b := Pipe(32)
	link := New(a, Options{Handler: (&sink{}).handle, Epoch: 7})
	defer link.Close()
	if err := link.Send(ctx, payload(1)); err != nil {
		t.Fatalf("send: %v", err)
	}
	readEnv(t, b)

	answer, frames := link.AcceptResume(Resume{LastSeen: 0, Epoch: 6})
	if answer.Kind != KindResumed || answer.Resume.OK || frames != nil {
		t.Fatalf("a resume from another epoch was accepted: %+v", answer.Resume)
	}
	if answer.Resume.Reason == "" {
		t.Fatal("the refusal says nothing; an unresumable link must say why")
	}

	answer, frames = link.AcceptResume(Resume{LastSeen: 0, Epoch: 7})
	if !answer.Resume.OK || len(frames) != 1 {
		t.Fatalf("a resume in the same epoch was refused: %+v", answer.Resume)
	}
}

// The other half of a resume: the side that continues must recognise the
// replayed prefix as something it has already consumed.
func TestResumedLinkTreatsTheReplayedPrefixAsDuplicate(t *testing.T) {
	a, b := Pipe(32)
	consumer := &sink{}
	link := New(a, Options{Handler: consumer.handle, ResumeFrom: 2})
	defer link.Close()

	writeRaw(t, b, Message(1, 0, payload(1)))
	writeRaw(t, b, Message(2, 0, payload(2)))
	writeRaw(t, b, Message(3, 0, payload(3)))

	waitFor(t, "the frame after the replay to arrive", func() bool { return len(consumer.seen()) == 1 })
	if got := consumer.seen(); got[0] != string(payload(3)) {
		t.Fatalf("consumer saw %v, want only the frame after the resume point", got)
	}
	if req := link.ResumeRequest().Resume; req.LastSeen != 3 {
		t.Fatalf("a further resume would ask from %d, want 3", req.LastSeen)
	}
}

// A consumer that cannot take a frame has left the same hole a lost frame
// would, so it fails the link rather than being logged and stepped over.
func TestHandlerRefusalFailsTheLink(t *testing.T) {
	a, b := Pipe(32)
	link := New(a, Options{Handler: (&sink{fail: errors.New("no room")}).handle})
	defer link.Close()

	writeRaw(t, b, Message(1, 0, payload(1)))
	select {
	case <-link.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a refused frame left the link running")
	}
	if err := link.Err(); err == nil {
		t.Fatal("the link failed with no reason")
	}
}

func TestVersionMismatchIsRefusedAtTheFirstFrame(t *testing.T) {
	a, b := Pipe(32)
	link := New(a, Options{Handler: (&sink{}).handle})
	defer link.Close()

	writeRaw(t, b, Envelope{V: Version + 1, Kind: KindMessage, Seq: 1, Payload: payload(1)})
	select {
	case <-link.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a frame from another protocol version was accepted")
	}
	if !errors.Is(link.Err(), ErrVersionMismatch) {
		t.Fatalf("link failed with %v, want a version mismatch", link.Err())
	}
}
