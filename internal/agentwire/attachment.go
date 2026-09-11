package agentwire

import (
	"context"
	"io"
)

// Attachment carries a reference, never the bytes: files go up to 100 MiB while a frame caps
// at 8 MiB, so bytes always ride a side transfer, with no inline fast path.
type Attachment struct {
	Filename   string `json:"filename,omitempty"`
	Title      string `json:"title,omitempty"`
	Comment    string `json:"comment,omitempty"`
	Mimetype   string `json:"mimetype,omitempty"`
	Size       int64  `json:"size"`
	TransferID string `json:"transfer_id"`
}

// 4 MiB base64-encodes to about 5.3 MiB, which fits the 8 MiB frame ceiling with room for
// the envelope.
const MaxTransferChunkBytes = 4 << 20

// Seq lets a receiver spot a missing chunk instead of delivering a truncated file.
type TransferChunk struct {
	TransferID string `json:"transfer_id"`
	Seq        int    `json:"seq"`
	Data       []byte `json:"data,omitempty"`
	Last       bool   `json:"last,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Size is already on the wire, so a Body of a different length is a transfer failure.
type Transfer struct {
	ID   string
	Size int64
	Body io.ReadCloser
}

// Return a path for anything large: it keeps a 100 MiB file out of gateway memory.
type AttachmentDeliverer interface {
	DeliverAttachment(ctx context.Context, a Attachment) (path string, data []byte, err error)
}
