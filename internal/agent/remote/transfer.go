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
		in.err = fmt.Errorf("the node could not read the file: %s", chunk.Error)
	case in.err != nil:
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
