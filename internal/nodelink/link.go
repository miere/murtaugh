package nodelink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

var (
	// ErrLinkClosed is what an operation on a link that has stopped returns.
	ErrLinkClosed = errors.New("nodelink: link closed")
	// ErrSequenceGap is the discriminant for a detected hole in the stream.
	// Everything above a failed link is failed with an error that satisfies
	// errors.Is against it, so "an event went missing" is one identifiable
	// condition rather than a string in a log.
	ErrSequenceGap = errors.New("nodelink: sequence gap")
	// ErrNotResumable answers a resume whose suffix is no longer held.
	ErrNotResumable = errors.New("nodelink: not resumable")
	// ErrVersionMismatch is a peer speaking a different envelope version.
	ErrVersionMismatch = errors.New("nodelink: envelope version mismatch")
)

// SequenceGapError names the hole: the sequence expected and the one that
// arrived. Both numbers are in the message because the size of the gap is the
// first thing a maintainer wants and the last thing a re-run will reproduce.
type SequenceGapError struct {
	Want uint64
	Got  uint64
}

func (e *SequenceGapError) Error() string {
	return fmt.Sprintf("nodelink: sequence gap: expected frame %d, received %d", e.Want, e.Got)
}

func (e *SequenceGapError) Unwrap() error { return ErrSequenceGap }

// Handler consumes one delivered payload.
//
// It is called from the read loop, in sequence order, and the frame is
// acknowledged only once it RETURNS — so a handler that hands the payload
// straight to a full channel is applying backpressure to the peer, which is the
// intended behaviour and not a bug. It must not block forever: while it is
// running no acknowledgement is processed, exactly as the ACP transport's read
// loop already behaves when a turn's event channel is full. Bound every wait
// inside it on a context.
//
// Returning an error fails the LINK. A consumer that cannot take a frame has
// left the same hole a dropped frame would, so it is not something to log and
// step over.
type Handler func(payload []byte) error

// Options configures a Link. The zero value of every field is usable.
type Options struct {
	// Handler receives delivered payloads. A nil handler discards them, which
	// is only useful in a test.
	Handler Handler
	Logger  *slog.Logger
	// WindowBytes bounds the unacknowledged bytes in flight before Send blocks.
	// Bytes rather than frames: an attachment chunk is four megabytes and a
	// text event is forty, so a frame count bounds nothing useful.
	WindowBytes int
	// AckThreshold is how many consumed frames may go unacknowledged before a
	// standalone ack is sent. Acks piggyback on outbound traffic; this covers
	// the direction that is only listening.
	//
	// It is a frame count, and frame counts alone are not enough: the WINDOW is
	// measured in bytes, so a peer sending a few large frames — four 64 KiB
	// attachment chunks against a 256 KiB window — fills the window before this
	// many frames have been consumed, and waits for an acknowledgement that a
	// frame count will never trigger. That is a deadlock, not a stall, and it
	// was found by sending a real attachment over a real socket. A consumed-byte
	// trigger runs alongside this one; see receive.
	AckThreshold uint64
	// AckInterval sends a standalone ack when nothing has been sent for that
	// long. It is a TRANSPORT keepalive — it proves the socket is alive and
	// says nothing about whether the agent is still working, which is a
	// different timer with a different owner. Zero disables it.
	AckInterval time.Duration
	// Epoch is the leadership term this link belongs to. A resume carrying a
	// different one is refused.
	Epoch uint64
	// ResumeFrom seeds the receive watermark when this link continues a
	// previous one, so the replayed prefix the peer re-sends is recognised as
	// duplicate rather than delivered twice.
	ResumeFrom uint64
}

const (
	defaultWindowBytes  = 4 << 20
	defaultAckThreshold = 8
)

type frame struct {
	seq  uint64
	raw  []byte
	size int
}

// Link is one connection's delivery state: a sequenced, acknowledged,
// flow-controlled stream of opaque payloads in each direction.
//
// Locking, because it is the part that goes wrong. Three separate mutexes, in
// this order where more than one is held:
//
//   - sendMu serialises Send end to end (window wait, sequence assignment,
//     write) so two concurrent senders cannot write frames out of sequence
//     order — which the peer would report as a gap and kill the link over.
//   - mu guards the shared counters and buffers. It is NEVER held across a
//     write to the transport, so a stalled write cannot stop an arriving
//     acknowledgement from opening the window.
//   - connMu guards the transport write itself, and is the only thing the read
//     loop takes in order to emit a standalone ack. It is deliberately not
//     sendMu: a sender parked on a full window holds sendMu for as long as it
//     takes, and the read loop must not queue behind it.
type Link struct {
	conn    Conn
	handler Handler
	log     *slog.Logger
	window  int
	ackAt   uint64
	epoch   uint64

	sendMu sync.Mutex
	connMu sync.Mutex

	mu           sync.Mutex
	nextSeq      uint64
	unacked      []frame
	unackedBytes int
	peerAck      uint64
	lastSeen     uint64
	ackedThrough uint64
	// sinceAck is the payload bytes delivered since the last acknowledgement
	// this side emitted, piggybacked or standalone. It is what makes a
	// byte-sized window and a frame-counted ack policy agree.
	sinceAck int
	room     chan struct{}
	err      error

	done      chan struct{}
	closeOnce sync.Once
}

// New starts a link over conn and begins reading immediately. The caller owns
// conn no longer: Close, and any failure, closes it.
func New(conn Conn, opts Options) *Link {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	window := opts.WindowBytes
	if window <= 0 {
		window = defaultWindowBytes
	}
	ackAt := opts.AckThreshold
	if ackAt == 0 {
		ackAt = defaultAckThreshold
	}
	l := &Link{
		conn:     conn,
		handler:  opts.Handler,
		log:      log,
		window:   window,
		ackAt:    ackAt,
		epoch:    opts.Epoch,
		lastSeen: opts.ResumeFrom,
		room:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go l.readLoop()
	if opts.AckInterval > 0 {
		go l.keepalive(opts.AckInterval)
	}
	return l
}

// Done closes when the link stops, for any reason.
func (l *Link) Done() <-chan struct{} { return l.done }

// Err reports why the link stopped, or nil while it is running. It is
// ErrLinkClosed for an orderly Close.
func (l *Link) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// LastSeen is the highest contiguous sequence delivered to the handler — what
// a resume request carries.
func (l *Link) LastSeen() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastSeen
}

// Epoch is the leadership term this link was opened in.
func (l *Link) Epoch() uint64 { return l.epoch }

// Pending reports the retransmit buffer's depth: frames sent and not yet
// acknowledged, and their bytes. A number that only grows is a peer that has
// stopped consuming.
func (l *Link) Pending() (frames, bytes int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.unacked), l.unackedBytes
}

// Send delivers one payload, blocking while the unacknowledged window is full.
//
// It returns when the frame has been written to the transport, not when the
// peer has consumed it: the retransmit buffer is what carries the frame across
// a reconnect, and it is pruned by the peer's acknowledgement.
func (l *Link) Send(ctx context.Context, payload []byte) error {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	if err := l.awaitRoom(ctx, len(payload)); err != nil {
		return err
	}

	l.mu.Lock()
	if l.err != nil {
		err := l.err
		l.mu.Unlock()
		return err
	}
	seq := l.nextSeq + 1
	raw, err := Encode(Message(seq, l.lastSeen, json.RawMessage(payload)))
	if err != nil {
		l.mu.Unlock()
		return err
	}
	l.nextSeq = seq
	l.ackedThrough = l.lastSeen
	l.sinceAck = 0
	l.unacked = append(l.unacked, frame{seq: seq, raw: raw, size: len(payload)})
	l.unackedBytes += len(payload)
	l.mu.Unlock()

	if err := l.write(raw); err != nil {
		l.fail(fmt.Errorf("nodelink: write frame %d: %w", seq, err))
		return err
	}
	return nil
}

// awaitRoom blocks until the window has space for another n bytes.
//
// The `already in flight` guard is what stops a payload larger than the whole
// window from waiting forever for room that can never appear: an oversized
// frame goes out alone.
func (l *Link) awaitRoom(ctx context.Context, n int) error {
	for {
		l.mu.Lock()
		if l.err != nil {
			err := l.err
			l.mu.Unlock()
			return err
		}
		if l.unackedBytes == 0 || l.unackedBytes+n <= l.window {
			l.mu.Unlock()
			return nil
		}
		room := l.room
		l.mu.Unlock()
		select {
		case <-room:
		case <-ctx.Done():
			return ctx.Err()
		case <-l.done:
			return l.Err()
		}
	}
}

// Close stops the link and the transport under it. Safe to call twice.
func (l *Link) Close() error {
	l.fail(ErrLinkClosed)
	return nil
}

func (l *Link) fail(err error) {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		if l.err == nil {
			l.err = err
		}
		l.mu.Unlock()
		close(l.done)
		_ = l.conn.Close()
	})
}

func (l *Link) write(raw []byte) error {
	l.connMu.Lock()
	defer l.connMu.Unlock()
	select {
	case <-l.done:
		return ErrLinkClosed
	default:
	}
	return l.conn.WriteMessage(raw)
}

func (l *Link) readLoop() {
	for {
		raw, err := l.conn.ReadMessage()
		if err != nil {
			if isClosedConn(err) {
				l.fail(ErrLinkClosed)
				return
			}
			l.fail(fmt.Errorf("nodelink: read frame: %w", err))
			return
		}
		env, err := Decode(raw)
		if err != nil {
			l.fail(err)
			return
		}
		if env.V != Version {
			l.fail(fmt.Errorf("%w: peer speaks %d, this build speaks %d", ErrVersionMismatch, env.V, Version))
			return
		}
		l.applyAck(env.Ack)
		switch env.Kind {
		case KindMessage:
			if err := l.receive(env); err != nil {
				l.fail(err)
				return
			}
		case KindAck:
			// The acknowledgement above was the whole frame.
		case KindResume, KindResumed:
			// The reconnect exchange is driven by the owner of the connection
			// (ReplayFrom / AcceptResume), not from inside the read loop: it
			// arrives on a link that has just been built around a NEW transport,
			// and only the owner knows which streams it belongs to.
			l.log.Warn("nodelink: resume frame arrived on a link with no reconnect driver", "kind", env.Kind)
		}
	}
}

// receive applies the sequencing rules and, when the frame is the next one,
// hands it to the consumer before advancing the watermark.
func (l *Link) receive(env Envelope) error {
	l.mu.Lock()
	last := l.lastSeen
	l.mu.Unlock()

	switch {
	case env.Seq <= last:
		// A duplicate. Legal only after a resume replayed a suffix this side
		// had already consumed; delivering it twice would double a sentence.
		return nil
	case env.Seq > last+1:
		return &SequenceGapError{Want: last + 1, Got: env.Seq}
	}

	if l.handler != nil {
		if err := l.handler(env.Payload); err != nil {
			return fmt.Errorf("nodelink: consume frame %d: %w", env.Seq, err)
		}
	}

	l.mu.Lock()
	l.lastSeen = env.Seq
	l.sinceAck += len(env.Payload)
	behind := l.lastSeen - l.ackedThrough
	bytesBehind := l.sinceAck
	l.mu.Unlock()
	// Either trigger. The frame count keeps a chatty stream's acknowledgements
	// timely; the byte count is what stops a peer sending large frames from
	// filling the window before the frame count is reached, which is a deadlock
	// because the acknowledgement that would open it is the one being waited
	// for. Half the window, so the peer has room to keep sending while the
	// acknowledgement is in flight.
	if behind >= l.ackAt || bytesBehind >= l.window/2 {
		l.sendAck()
	}
	return nil
}

// applyAck prunes the retransmit buffer and opens the window.
//
// A lower or repeated acknowledgement is ignored rather than rolling the
// watermark back: acknowledgements are cumulative, so an older one carries no
// information, and un-pruning would resurrect frames the peer has consumed.
func (l *Link) applyAck(ack uint64) {
	if ack == 0 {
		return
	}
	l.mu.Lock()
	if ack <= l.peerAck {
		l.mu.Unlock()
		return
	}
	l.peerAck = ack
	cut := 0
	for cut < len(l.unacked) && l.unacked[cut].seq <= ack {
		l.unackedBytes -= l.unacked[cut].size
		cut++
	}
	if cut > 0 {
		// Re-slice into a fresh backing array so an acknowledged frame's bytes
		// are collectable rather than pinned by the buffer's capacity.
		l.unacked = append([]frame(nil), l.unacked[cut:]...)
		close(l.room)
		l.room = make(chan struct{})
	}
	l.mu.Unlock()
}

// sendAck emits a standalone acknowledgement. Failures are logged and not
// fatal: the next message frame carries the same watermark.
func (l *Link) sendAck() {
	l.mu.Lock()
	if l.err != nil || l.lastSeen == l.ackedThrough {
		l.mu.Unlock()
		return
	}
	ack := l.lastSeen
	l.ackedThrough = ack
	l.sinceAck = 0
	l.mu.Unlock()
	raw, err := Encode(Ack(ack))
	if err != nil {
		l.log.Warn("nodelink: encode ack", "error", err)
		return
	}
	if err := l.write(raw); err != nil && !errors.Is(err, ErrLinkClosed) {
		l.log.Warn("nodelink: write ack", "error", err)
	}
}

func (l *Link) keepalive(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			l.sendAck()
		}
	}
}

// ReplayFrom returns the frames the peer must be re-sent after it resumes from
// lastSeen: every frame this side sent after that sequence, in order, exactly
// as they were first written.
//
// It refuses rather than guesses. Below the buffer's floor means the peer is
// asking for frames it had already acknowledged, so they are gone; above what
// was ever sent means it is not the same stream. Both are answered with
// ErrNotResumable so the caller fails its open turns explicitly instead of
// continuing into a hole.
func (l *Link) ReplayFrom(lastSeen uint64) ([][]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lastSeen > l.nextSeq {
		return nil, fmt.Errorf("%w: peer claims frame %d, only %d were sent", ErrNotResumable, lastSeen, l.nextSeq)
	}
	if lastSeen < l.peerAck {
		return nil, fmt.Errorf("%w: frames after %d were acknowledged and released", ErrNotResumable, lastSeen)
	}
	out := make([][]byte, 0, len(l.unacked))
	for _, f := range l.unacked {
		if f.seq > lastSeen {
			out = append(out, f.raw)
		}
	}
	return out, nil
}

// AcceptResume answers a peer's resume request: the answer frame to send, and
// the frames to replay behind it.
//
// The epoch check is the reason this is not just ReplayFrom. A node resuming
// into a gateway that was re-promoted since is talking to a process that never
// held the buffer; saying yes there would silently drop the frames the resume
// exists to recover.
func (l *Link) AcceptResume(req Resume) (Envelope, [][]byte) {
	if req.Epoch != l.epoch {
		return Envelope{V: Version, Kind: KindResumed, Resume: &Resume{
			LastSeen: req.LastSeen,
			Epoch:    l.epoch,
			Reason:   fmt.Sprintf("resume is for epoch %d, this link holds epoch %d", req.Epoch, l.epoch),
		}}, nil
	}
	frames, err := l.ReplayFrom(req.LastSeen)
	if err != nil {
		return Envelope{V: Version, Kind: KindResumed, Resume: &Resume{
			LastSeen: req.LastSeen,
			Epoch:    l.epoch,
			Reason:   err.Error(),
		}}, nil
	}
	return Envelope{V: Version, Kind: KindResumed, Resume: &Resume{
		LastSeen: req.LastSeen,
		Epoch:    l.epoch,
		OK:       true,
	}}, frames
}

// ResumeRequest is what this side asks for after its transport was replaced.
func (l *Link) ResumeRequest() Envelope {
	return Envelope{V: Version, Kind: KindResume, Resume: &Resume{LastSeen: l.LastSeen(), Epoch: l.epoch}}
}
