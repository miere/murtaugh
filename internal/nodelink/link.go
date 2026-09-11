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
	ErrLinkClosed      = errors.New("nodelink: link closed")
	ErrSequenceGap     = errors.New("nodelink: sequence gap")
	ErrNotResumable    = errors.New("nodelink: not resumable")
	ErrVersionMismatch = errors.New("nodelink: envelope version mismatch")
)

// Carries both sequence numbers because the gap size is what a maintainer needs first, and a re-run
// will not reproduce it.
type SequenceGapError struct {
	Want uint64
	Got  uint64
}

func (e *SequenceGapError) Error() string {
	return fmt.Sprintf("nodelink: sequence gap: expected frame %d, received %d", e.Want, e.Got)
}

func (e *SequenceGapError) Unwrap() error { return ErrSequenceGap }

// Blocking here is intended backpressure, since the frame is acked only on return, but bound the
// wait: no acks are processed meanwhile. An error fails the link, as a skipped frame is a hole.
type Handler func(payload []byte) error

type Options struct {
	Handler Handler
	Logger  *slog.Logger
	// Bytes, not frames: an attachment chunk is 4 MB and a text event is 40 bytes, so a frame count
	// bounds nothing useful.
	WindowBytes int
	// A frame count alone deadlocks: a few large frames fill the byte window before it is reached, so
	// a consumed-byte trigger runs alongside it.
	AckThreshold uint64
	// A transport keepalive only: it proves the socket is alive, not that the agent is still working.
	AckInterval time.Duration
	Epoch       uint64
	// Seeds the receive watermark so a replayed prefix is dropped as a duplicate, not delivered twice.
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

// Lock order is sendMu, mu, connMu. mu is never held across a write and the read loop takes only
// connMu, so neither a stalled write nor a sender parked on a full window can hold up an ack.
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
	sinceAck     int
	room         chan struct{}
	err          error

	done      chan struct{}
	closeOnce sync.Once
}

// Takes ownership of conn: Close, or any failure, closes it.
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

func (l *Link) Done() <-chan struct{} { return l.done }

func (l *Link) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *Link) LastSeen() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastSeen
}

func (l *Link) Epoch() uint64 { return l.epoch }

func (l *Link) Pending() (frames, bytes int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.unacked), l.unackedBytes
}

// Returns once the frame is written, not once the peer consumed it; the retransmit buffer, pruned
// by the peer's ack, is what carries it across a reconnect.
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
		case KindResume, KindResumed:
			l.log.Warn("nodelink: resume frame arrived on a link with no reconnect driver", "kind", env.Kind)
		}
	}
}

func (l *Link) receive(env Envelope) error {
	l.mu.Lock()
	last := l.lastSeen
	l.mu.Unlock()

	switch {
	case env.Seq <= last:
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
	if behind >= l.ackAt || bytesBehind >= l.window/2 {
		l.sendAck()
	}
	return nil
}

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
		l.unacked = append([]frame(nil), l.unacked[cut:]...)
		close(l.room)
		l.room = make(chan struct{})
	}
	l.mu.Unlock()
}

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

// Refuses rather than guesses, so the caller fails its open turns explicitly instead of carrying
// on past a hole.
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

// Unlike ReplayFrom it checks the epoch: a gateway re-promoted since never held the buffer, and
// saying yes would silently drop the frames the resume exists to recover.
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

func (l *Link) ResumeRequest() Envelope {
	return Envelope{V: Version, Kind: KindResume, Resume: &Resume{LastSeen: l.LastSeen(), Epoch: l.epoch}}
}
