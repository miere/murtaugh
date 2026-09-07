package agentwire

import (
	"context"
	"io"
)

// Attachment is the serialisable form of agent.AttachmentEvent: the metadata,
// the exact byte count, and a reference to the bytes. The bytes themselves ride
// a side transfer and are never in this frame.
//
// # Why a side transfer rather than inline
//
// This was the open decision in the spec. It is settled here as: a side
// transfer, uniformly, with no inline fast path.
//
//  1. Inline cannot carry today's maximum. The attach tool caps a file at 100
//     MiB (chosen to mirror Slack's per-file ceiling) while both agent protocols
//     already in this repository cap a single frame at 8 MiB. A fully inline
//     attachment is ~12x the house frame budget at the documented maximum, and
//     "nothing that works today may stop working".
//  2. Inline destroys a property the producing side was built around. The attach
//     tool deliberately never reads the file — it hands over a path precisely so
//     a large attachment is never buffered in the conversation. Inlining buffers
//     it on the node AND on the gateway.
//  3. The consumer gains nothing from inline. Slack's external-upload flow needs
//     a byte source plus an exact size up front; a sized reference satisfies
//     that by streaming, which inline does not improve on.
//  4. It is not an architecture. Nodes dial inward and the gateway never dials
//     out, so this is a correlated chunk stream on the SAME inbound connection —
//     not a second endpoint, not a blob store. It costs a frame type.
//  5. Scoped to the turn, so there is no lifetime to manage: the reference is
//     valid while the turn's stream is open and the consumer pulls immediately.
//     No expiry, no garbage collection, no "blob not found".
//  6. The hybrid (inline under N, a reference over N) is rejected on purpose. A
//     threshold means the common small-file case exercises one code path and the
//     rare large-file case exercises the other — the shape of bug that only
//     appears in production. One path, always exercised.
//
// The residual risk, stated rather than hidden: inline is genuinely simpler and
// this buys a frame type up front. If the chunk stream proves disproportionate
// later, the fallback is inline with a hard cap plus a legible "file too large
// to deliver" alert — but that is a capability regression and has to be argued
// as one.
type Attachment struct {
	Filename string `json:"filename,omitempty"`
	Title    string `json:"title,omitempty"`
	Comment  string `json:"comment,omitempty"`
	Mimetype string `json:"mimetype,omitempty"`
	// Size is the exact byte count, known before the first chunk. Slack's
	// external-upload flow demands it up front and rejects a zero-length upload,
	// so it is a field rather than something discovered by reading.
	Size int64 `json:"size"`
	// TransferID correlates this attachment with its TransferChunk stream.
	TransferID string `json:"transfer_id"`
}

// MaxTransferChunkBytes bounds one chunk's payload.
//
// 4 MiB of raw bytes becomes ~5.3 MiB once base64-encoded into JSON, which sits
// inside the 8 MiB single-frame ceiling both existing agent protocols in this
// repository already use, with room for the envelope. Doubling it would not.
const MaxTransferChunkBytes = 4 << 20

// TransferChunk is one frame of an attachment's bytes.
//
// Seq is the chunk's position, so a receiver can detect a hole rather than
// silently deliver a truncated file. Last marks the final chunk. Error is the
// producer giving up mid-stream — a file that stopped being readable is a fact
// the consumer must be told, not a stream that merely stops.
//
// Nothing in this package produces or consumes these: the transport does. They
// are defined now because the transfer decision shapes the transport, and
// deciding it without writing down the frame would leave the decision unmade in
// practice.
type TransferChunk struct {
	TransferID string `json:"transfer_id"`
	Seq        int    `json:"seq"`
	Data       []byte `json:"data,omitempty"`
	Last       bool   `json:"last,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Transfer is the byte stream that accompanies an encoded attachment event: the
// producer side of the side transfer. The caller streams Body as TransferChunks
// and closes it.
//
// Size is authoritative and already on the wire in the Attachment, so a Body
// that turns out to be a different length is a transfer failure rather than a
// surprise for the uploader.
type Transfer struct {
	ID   string
	Size int64
	Body io.ReadCloser
}

// AttachmentDeliverer materialises a side-transferred attachment on the
// consuming side. It is handed the metadata (including the transfer id and the
// exact size) and returns the byte source in one of the two forms
// agent.AttachmentEvent already understands: a path to a file on the CONSUMING
// host, or the bytes themselves.
//
// A path is preferred for anything large — it is what keeps a 100 MiB file out
// of gateway memory, and the uploader stats it for the size Slack demands.
// Returning bytes is right for the small in-memory case.
//
// The transport implements this in a later stage; the round-trip test supplies
// an in-memory one. A nil deliverer makes decoding an attachment an error
// rather than a silently dropped file.
type AttachmentDeliverer interface {
	DeliverAttachment(ctx context.Context, a Attachment) (path string, data []byte, err error)
}
