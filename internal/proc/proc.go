// Package proc supervises a long-lived child process: it streams the child's
// output as it arrives, keeps stdin open so the caller can answer a prompt, and
// reaps the whole process group on cancel.
//
// It exists because internal/tools/terminal cannot do this and should not try.
// That tool is a bounded request/response primitive — it caps the run at five
// minutes, buffers the output, and deliberately SIGKILLs the process group to
// reap anything the shell backgrounded. An interactive auth flow needs the
// opposite of all three: an open-ended lifetime, output delivered while the
// process is still running, and a child that stays alive waiting on stdin.
//
// The intended shape is a caller that starts a process, watches Lines() for
// something interesting (a verification URL, a prompt), writes a response with
// WriteLine, and selects over Exited() alongside its own timeout:
//
//	h, err := proc.Start(ctx, proc.Spec{Command: "gcloud", Args: []string{"auth", "login"}})
//	defer h.Kill()
//	select {
//	case <-h.Exited():
//	    err = h.Wait()
//	case <-timeout:
//	    // h.Kill() via the defer
//	}
//
// Commands are executed directly, never through a shell: Spec takes an argv,
// so a value interpolated into Args is an argument and cannot become a second
// command. A caller that genuinely wants shell semantics has to ask for them
// explicitly by running sh itself.
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Stream identifies which pipe a Line came from.
type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

// Line is one piece of child output.
//
// Partial marks text that has not been newline-terminated yet. Interactive
// tools prompt without a trailing newline ("Enter verification code: "), so a
// consumer that waited for complete lines would never see the prompt it is
// supposed to answer. A partial is re-emitted as it grows and is superseded by
// the complete line once the newline arrives, so a consumer matching on
// content should tolerate seeing the same text more than once.
type Line struct {
	Stream  Stream
	Text    string
	Partial bool
}

// Spec describes the process to start.
type Spec struct {
	Command string   // argv[0]; executed directly, not through a shell
	Args    []string // argv[1:]
	Dir     string   // working directory; empty inherits the parent's
	Env     []string // full environment; nil inherits the parent's

	// MaxOutputBytes caps the retained output copy returned by Output(). It
	// bounds only the diagnostic buffer — streaming through Lines() is
	// unaffected and continues past the cap. Zero uses DefaultMaxOutputBytes.
	MaxOutputBytes int
}

const (
	// DefaultMaxOutputBytes bounds the retained diagnostic copy of the child's
	// output. Matches the terminal tool's cap; auth flows emit far less.
	DefaultMaxOutputBytes = 64 * 1024

	// lineBuffer is how many Lines may queue before the pumps start dropping.
	// See the note in emit about why dropping beats blocking.
	lineBuffer = 256

	// waitDelay bounds how long Wait blocks on a pipe still held open by a
	// grandchild after the process itself has gone.
	waitDelay = 5 * time.Second
)

// Handle is a running child process.
type Handle struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan Line

	// exited closes once the process has been reaped, so a caller can select
	// on it alongside its own timers.
	exited chan struct{}

	pumps sync.WaitGroup

	waitOnce sync.Once
	waitErr  error

	killOnce sync.Once

	mu        sync.Mutex
	output    []byte
	maxOutput int
	truncated bool
	dropped   int
}

// Start launches the process described by spec. Cancelling ctx kills the whole
// process group; so does Kill. The returned Handle is valid only if err is nil.
func Start(ctx context.Context, spec Spec) (*Handle, error) {
	if strings.TrimSpace(spec.Command) == "" {
		return nil, errors.New("proc: no command to run")
	}
	max := spec.MaxOutputBytes
	if max <= 0 {
		max = DefaultMaxOutputBytes
	}

	cmd := exec.CommandContext(ctx, spec.Command, spec.Args...)
	cmd.Dir = spec.Dir
	if spec.Env != nil {
		cmd.Env = spec.Env
	}
	configureProcessGroup(cmd)
	cmd.WaitDelay = waitDelay

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("proc: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("proc: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("proc: stderr pipe: %w", err)
	}

	h := &Handle{
		cmd:       cmd,
		stdin:     stdin,
		lines:     make(chan Line, lineBuffer),
		exited:    make(chan struct{}),
		maxOutput: max,
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("proc: start %s: %w", spec.Command, err)
	}

	h.pumps.Add(2)
	go h.pump(stdout, StreamStdout)
	go h.pump(stderr, StreamStderr)

	// Reap in the background so Exited() is meaningful even for a caller that
	// never calls Wait itself. Wait is idempotent, so an external call still
	// gets the same error back. Wait blocks on both pumps before reaping, so by
	// the time it returns no further sends can happen and lines can be closed.
	go func() {
		_ = h.Wait()
		close(h.lines)
		close(h.exited)
	}()

	return h, nil
}

// Lines returns the channel of child output. It is closed once both pipes have
// hit EOF, which happens at or just before process exit.
func (h *Handle) Lines() <-chan Line { return h.lines }

// Exited returns a channel closed once the process has been reaped. Read the
// outcome with Wait, which by then returns immediately.
func (h *Handle) Exited() <-chan struct{} { return h.exited }

// Wait blocks until the process exits and returns its result: nil for a clean
// exit, an *exec.ExitError for a non-zero status, or the context's error when
// the run was cancelled. Safe to call from several goroutines and more than
// once; every caller sees the same result.
func (h *Handle) Wait() error {
	h.waitOnce.Do(func() {
		// The pipes from StdoutPipe/StderrPipe are closed by Wait, so all reads
		// must finish first — otherwise a pump races the close and loses the
		// tail of the output.
		h.pumps.Wait()
		h.waitErr = h.cmd.Wait()
	})
	return h.waitErr
}

// WriteStdin writes s to the child's stdin, leaving the pipe open.
func (h *Handle) WriteStdin(s string) error {
	if _, err := io.WriteString(h.stdin, s); err != nil {
		return fmt.Errorf("proc: write stdin: %w", err)
	}
	return nil
}

// WriteLine writes s followed by a newline — what a child blocked on a line
// read is waiting for.
func (h *Handle) WriteLine(s string) error { return h.WriteStdin(s + "\n") }

// CloseStdin signals EOF to a child that reads until the pipe closes.
func (h *Handle) CloseStdin() error { return h.stdin.Close() }

// Kill terminates the process group. Idempotent, and safe to defer even on the
// paths where the process has already exited.
func (h *Handle) Kill() {
	h.killOnce.Do(func() { killGroup(h.cmd) })
}

// Output returns the retained copy of the child's combined output, capped at
// the spec's MaxOutputBytes. It is for diagnostics — reporting why an auth
// attempt failed — not for control flow, which should read Lines instead.
func (h *Handle) Output() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := string(h.output)
	if h.truncated {
		out += "\n[output truncated]"
	}
	return out
}

// Dropped reports how many Lines were discarded because the consumer fell
// behind. Non-zero means Lines is an incomplete view; Output still holds the
// text (up to its own cap).
func (h *Handle) Dropped() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}

// pump reads one pipe to EOF, splitting it into Lines and copying it into the
// diagnostic buffer.
func (h *Handle) pump(r io.Reader, stream Stream) {
	defer h.pumps.Done()

	buf := make([]byte, 4096)
	var pending []byte
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			h.capture(buf[:n])
			pending = append(pending, buf[:n]...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				h.emit(Line{Stream: stream, Text: strings.TrimRight(string(pending[:i]), "\r")})
				pending = pending[i+1:]
			}
			// Whatever is left is an unterminated prompt. Emit it so a consumer
			// waiting to answer one is not blocked on a newline that will only
			// arrive after it responds.
			if len(pending) > 0 {
				h.emit(Line{Stream: stream, Text: string(pending), Partial: true})
			}
		}
		if readErr != nil {
			if len(pending) > 0 {
				h.emit(Line{Stream: stream, Text: string(pending), Partial: true})
			}
			return
		}
	}
}

// emit delivers a Line, dropping it if the consumer has fallen behind.
//
// Blocking here would be worse than dropping: the pump would stop reading, the
// child's pipe would fill, and the child would block on write — deadlocking a
// process the caller is still waiting on. A consumer that stops reading (having
// found the URL it wanted, say) is a normal case, not a bug, so the supervisor
// has to stay live without it. Output() retains the text either way.
func (h *Handle) emit(l Line) {
	select {
	case h.lines <- l:
	default:
		h.mu.Lock()
		h.dropped++
		h.mu.Unlock()
	}
}

func (h *Handle) capture(b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.maxOutput - len(h.output)
	if room <= 0 {
		h.truncated = true
		return
	}
	if len(b) > room {
		b = b[:room]
		h.truncated = true
	}
	h.output = append(h.output, b...)
}
