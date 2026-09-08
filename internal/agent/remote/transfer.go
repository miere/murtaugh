package remote

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/miere/murtaugh/internal/agentwire"
)

// transfers collects attachment side transfers into files on the gateway's own
// disk and hands the finished path to the decoder.
//
// # Why this is not a pulling deliverer
//
// The obvious shape — a deliverer that reads chunks when asked — deadlocks. The
// decoder calls DeliverAttachment from inside Decoder.Decode, which runs on the
// link's read loop, and the chunks it would pull arrive on that same read loop.
// The node therefore sends every chunk BEFORE the event that references them,
// and this type is a place to put them until it does: by the time the event is
// decoded the file is complete and DeliverAttachment is a map lookup.
//
// # The temp files outlive the turn
//
// A delivered file is removed when the connection ends, not when the turn does.
// Nothing here can know when the uploader has finished with the path — the
// renderer takes a path precisely so a 100 MiB file is never buffered — and
// deleting it underneath a Slack upload would turn a working feature into an
// intermittent one. The bound is therefore one connection's worth of delivered
// attachments, which is honest but not free; giving the path an owner that can
// release it belongs with the uploader, not here.
type transfers struct {
	log *slog.Logger

	mu   sync.Mutex
	dir  string
	open map[string]*incoming
	done map[string]finished
}

type incoming struct {
	file *os.File
	path string
	next int
	err  error
}

type finished struct {
	path string
	err  error
}

func newTransfers(log *slog.Logger) *transfers {
	return &transfers{
		log:  log,
		open: make(map[string]*incoming),
		done: make(map[string]finished),
	}
}

// accept applies one chunk. It runs on the read loop and does one file write.
func (t *transfers) accept(msg agentwire.Message) {
	var chunk agentwire.TransferChunk
	if err := msg.Into(&chunk); err != nil {
		t.log.Warn("remote: undecodable transfer chunk", "error", err, "transfer_id", msg.ID)
		return
	}
	if chunk.TransferID == "" {
		chunk.TransferID = msg.ID
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	in, ok := t.open[chunk.TransferID]
	if !ok {
		file, err := t.createLocked(chunk.TransferID)
		if err != nil {
			t.done[chunk.TransferID] = finished{err: err}
			return
		}
		in = &incoming{file: file, path: file.Name()}
		t.open[chunk.TransferID] = in
	}

	switch {
	case chunk.Error != "":
		// The producer gave up. Recorded rather than ignored, or the turn waits
		// for bytes that will never come and the user is told nothing.
		in.err = fmt.Errorf("the node could not read the file: %s", chunk.Error)
	case in.err != nil:
		// Already failed; keep draining the stream without writing.
	case chunk.Seq != in.next:
		in.err = fmt.Errorf("transfer %s: expected chunk %d, received %d", chunk.TransferID, in.next, chunk.Seq)
	default:
		if _, err := in.file.Write(chunk.Data); err != nil {
			in.err = err
		}
		in.next++
	}

	if !chunk.Last {
		return
	}
	if err := in.file.Close(); err != nil && in.err == nil {
		in.err = err
	}
	delete(t.open, chunk.TransferID)
	if in.err != nil {
		_ = os.Remove(in.path)
		t.done[chunk.TransferID] = finished{err: in.err}
		return
	}
	t.done[chunk.TransferID] = finished{path: in.path}
}

// createLocked opens the file one transfer's bytes land in, making the
// per-connection directory on first use.
func (t *transfers) createLocked(id string) (*os.File, error) {
	if t.dir == "" {
		dir, err := os.MkdirTemp("", "murtaugh-node-transfer-")
		if err != nil {
			return nil, fmt.Errorf("remote: open a place to receive attachments: %w", err)
		}
		t.dir = dir
	}
	file, err := os.CreateTemp(t.dir, "transfer-*")
	if err != nil {
		return nil, fmt.Errorf("remote: receive transfer %s: %w", id, err)
	}
	return file, nil
}

// DeliverAttachment hands the decoder the finished file.
//
// An absent transfer is an error, never a wait. The chunks precede the event by
// construction, so "not here" means they were lost or the node sent the event
// without them — and blocking the read loop hoping otherwise would stop the
// very frames it is waiting for.
func (t *transfers) DeliverAttachment(_ context.Context, a agentwire.Attachment) (string, []byte, error) {
	t.mu.Lock()
	result, ok := t.done[a.TransferID]
	delete(t.done, a.TransferID)
	_, stillOpen := t.open[a.TransferID]
	t.mu.Unlock()

	switch {
	case ok && result.err != nil:
		return "", nil, result.err
	case ok:
		return result.path, nil, nil
	case stillOpen:
		return "", nil, fmt.Errorf("transfer %s arrived unfinished: the node sent the attachment before its last chunk", a.TransferID)
	default:
		return "", nil, errors.New("no bytes arrived for this attachment")
	}
}

// close releases every file this connection received.
func (t *transfers) close() {
	t.mu.Lock()
	for _, in := range t.open {
		_ = in.file.Close()
	}
	t.open = make(map[string]*incoming)
	t.done = make(map[string]finished)
	dir := t.dir
	t.dir = ""
	t.mu.Unlock()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}
