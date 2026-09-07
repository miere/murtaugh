// Package nodelink is the envelope a runtime node and the gateway wrap their
// protocol in: sequencing, acknowledgement, backpressure and loss detection,
// and nothing else.
//
// # Why this is a separate package
//
// In process, delivery cannot fail: a send on a Go channel either happens or
// the program is deadlocked. Over a network it can fail silently, and the
// consumer downstream of this seam is append-only — a Slack streamed message
// whose writer frees its buffer once a paint lands. A dropped event is
// therefore a hole in a sentence that nothing notices.
//
// Reliability lives HERE, wrapping the payload, and never inside it. Keeping it
// out of internal/agentwire is what makes the two concerns separable: the codec
// can be tested for meaning with no transport in sight (it already is), and this
// package can be tested for delivery with no idea what it is delivering. The
// separation is enforced rather than asserted — this package imports nothing of
// ours, and TestEnvelopeKnowsNothingOfMurtaugh fails the build if that changes.
//
// # What is guaranteed, stated exactly
//
// A frame is acknowledged when the local Handler has RETURNED, not when it was
// decoded. That is the wire's analogue of the stream writer's rule ("consume the
// buffer only once the paint lands"), moved to the receiving side of the
// connection, and it is where the guarantee stops: nodelink delivers to the
// consumer, and the consumer's own append-only retry covers the last hop to
// Slack. It is not an end-to-end acknowledgement of a rendered message and does
// not pretend to be one.
//
// # The invariants
//
//   - Seq is per connection and per direction, starts at 1 and increments by
//     exactly one for every KindMessage frame.
//   - Only a KindMessage carries a Seq. An ack, a resume and a resume answer
//     carry Seq 0 and are never themselves acknowledged — otherwise two idle
//     peers acknowledge each other's acknowledgements forever.
//   - Ack is cumulative: the highest CONTIGUOUS sequence the sender has handed
//     to its consumer. It piggybacks on any outbound message frame and is sent
//     standalone once the unacknowledged count crosses a threshold.
//   - A gap (Seq > lastSeen+1) is fatal to the connection. On an ordered byte
//     transport a gap means the framing is broken, so waiting cannot repair it
//     and per-stream recovery would be a fiction. The link fails with
//     ErrSequenceGap and every stream above it is failed with that error.
//   - A duplicate (Seq <= lastSeen) is dropped silently, advances nothing and is
//     never delivered twice. That case is legal only after a resume replays a
//     suffix the receiver had already consumed.
//
// # Backpressure
//
// Send blocks while the unacknowledged window is full, which is the pacing
// signal a Go channel gave away for free. The window is measured in BYTES
// rather than frames because one attachment chunk is four megabytes and one
// text event is forty, so a frame count bounds nothing useful.
//
// # Two things this deliberately does not do
//
// It does not conflate the transport keepalive with agent liveness. A
// standalone ack on an idle timer proves the socket is alive; it says nothing
// about whether the agent is still producing events, which is a different
// timer with a different owner (#192). Conflating them re-creates exactly the
// blindness the split is meant to remove.
//
// It does not drive a reconnect. The retransmit buffer, ReplayFrom and
// AcceptResume are the machinery a reconnect needs and are tested here; the
// loop that dials again and performs the exchange belongs to the stage that
// owns the session channel (#193/#197).
package nodelink
