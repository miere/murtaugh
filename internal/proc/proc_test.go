package proc

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// collect drains lines until one satisfies match, and returns it. It fails the
// test if the channel closes or the deadline passes first.
func collect(t *testing.T, h *Handle, match func(Line) bool) Line {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-h.Lines():
			if !ok {
				t.Fatalf("output ended without a matching line; output was:\n%s", h.Output())
			}
			if match(l) {
				return l
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a matching line; output was:\n%s", h.Output())
		}
	}
}

func TestStartStreamsStdoutAndStderr(t *testing.T) {
	h, err := Start(context.Background(), Spec{
		Command: "sh",
		Args:    []string{"-c", "echo out; echo err 1>&2"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Kill()

	var sawOut, sawErr bool
	for l := range h.Lines() {
		if l.Partial {
			continue
		}
		switch {
		case l.Stream == StreamStdout && l.Text == "out":
			sawOut = true
		case l.Stream == StreamStderr && l.Text == "err":
			sawErr = true
		}
	}
	if !sawOut || !sawErr {
		t.Fatalf("sawOut=%v sawErr=%v; output was:\n%s", sawOut, sawErr, h.Output())
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// The reason this package exists: a child that prints a prompt WITHOUT a
// trailing newline, blocks on stdin, and only finishes once the caller answers.
// This is the gcloud verification-code shape.
func TestInteractiveStdinRoundTrip(t *testing.T) {
	h, err := Start(context.Background(), Spec{
		Command: "sh",
		Args:    []string{"-c", `printf 'Enter code: '; read code; echo "got:$code"`},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Kill()

	// The prompt has no newline, so it can only ever arrive as a partial.
	prompt := collect(t, h, func(l Line) bool { return strings.Contains(l.Text, "Enter code:") })
	if !prompt.Partial {
		t.Fatalf("expected the unterminated prompt to be marked Partial, got %+v", prompt)
	}

	if err := h.WriteLine("abc123"); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
	got := collect(t, h, func(l Line) bool { return strings.Contains(l.Text, "got:") })
	if !strings.Contains(got.Text, "got:abc123") {
		t.Fatalf("child did not receive the code: %q", got.Text)
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// A child that outlives the caller's interest must still be reaped, and Wait
// must return rather than hanging on the open pipe.
func TestKillTerminatesLongRunningChild(t *testing.T) {
	h, err := Start(context.Background(), Spec{
		Command: "sh",
		Args:    []string{"-c", "sleep 300"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- h.Wait() }()

	h.Kill()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after Kill")
	}
}

func TestKillIsIdempotent(t *testing.T) {
	h, err := Start(context.Background(), Spec{Command: "sh", Args: []string{"-c", "sleep 300"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.Kill()
	h.Kill() // must not panic or block
	<-h.Exited()
}

// Cancelling the context must reap the child even when it is parked on stdin —
// the leak the auth flow would otherwise produce on timeout.
func TestContextCancelReapsChildBlockedOnStdin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h, err := Start(ctx, Spec{Command: "sh", Args: []string{"-c", "read x; echo never"}})
	if err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	defer h.Kill()

	cancel()
	select {
	case <-h.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("child was not reaped after context cancellation")
	}
	if err := h.Wait(); err == nil {
		t.Fatal("expected a non-nil error for a cancelled run")
	}
}

// A backgrounded grandchild holding the output pipe must not keep Wait blocked
// past cancellation — the case the process-group kill exists for.
func TestCancelReapsBackgroundedGrandchild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h, err := Start(ctx, Spec{Command: "sh", Args: []string{"-c", "sleep 300 & echo started; wait"}})
	if err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	defer h.Kill()

	collect(t, h, func(l Line) bool { return strings.Contains(l.Text, "started") })

	start := time.Now()
	cancel()
	select {
	case <-h.Exited():
	case <-time.After(20 * time.Second):
		t.Fatal("Wait did not return; the grandchild kept the pipe open")
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("reaping took %v — the process group was not killed", elapsed)
	}
}

func TestWaitReportsExitStatus(t *testing.T) {
	h, err := Start(context.Background(), Spec{Command: "sh", Args: []string{"-c", "exit 7"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Kill()

	err = h.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an *exec.ExitError, got %v", err)
	}
	if code := exitErr.ExitCode(); code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
}

func TestWaitIsRepeatableAndConcurrent(t *testing.T) {
	h, err := Start(context.Background(), Spec{Command: "sh", Args: []string{"-c", "exit 3"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Kill()

	const callers = 4
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() { results <- h.Wait() }()
	}
	first := <-results
	for i := 1; i < callers; i++ {
		if got := <-results; !errors.Is(got, first) && got.Error() != first.Error() {
			t.Fatalf("Wait returned inconsistent results: %v vs %v", got, first)
		}
	}
	if h.Wait().Error() != first.Error() {
		t.Fatal("a later Wait disagreed with the first")
	}
}

// A consumer that stops reading must not wedge the child. The pumps drop lines
// rather than blocking, so the process still runs to completion.
func TestSlowConsumerDoesNotDeadlockChild(t *testing.T) {
	h, err := Start(context.Background(), Spec{
		Command: "sh",
		Args:    []string{"-c", "i=0; while [ $i -lt 5000 ]; do echo line-$i; i=$((i+1)); done"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Kill()

	// Deliberately never read h.Lines().
	select {
	case <-h.Exited():
	case <-time.After(20 * time.Second):
		t.Fatal("child deadlocked against an unread Lines channel")
	}
	if err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if h.Dropped() == 0 {
		t.Log("no lines dropped — buffer absorbed the run; the deadlock property still holds")
	}
	if h.Output() == "" {
		t.Fatal("Output should retain the text even when Lines were dropped")
	}
}

func TestOutputIsCappedAndFlagged(t *testing.T) {
	h, err := Start(context.Background(), Spec{
		Command:        "sh",
		Args:           []string{"-c", "i=0; while [ $i -lt 2000 ]; do echo 0123456789012345678901234567890123456789; i=$((i+1)); done"},
		MaxOutputBytes: 1024,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Kill()
	<-h.Exited()

	out := h.Output()
	if !strings.Contains(out, "[output truncated]") {
		t.Fatalf("expected a truncation marker, got %d bytes", len(out))
	}
	if len(out) > 1024+len("\n[output truncated]") {
		t.Fatalf("output exceeded the cap: %d bytes", len(out))
	}
}

func TestLinesChannelClosesOnExit(t *testing.T) {
	h, err := Start(context.Background(), Spec{Command: "sh", Args: []string{"-c", "echo done"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Kill()

	for range h.Lines() { //nolint:revive // draining to EOF is the assertion
	}
	// Reaching here means the channel closed; a second receive confirms it.
	if _, ok := <-h.Lines(); ok {
		t.Fatal("Lines channel was not closed")
	}
}

func TestStartRejectsEmptyCommand(t *testing.T) {
	if _, err := Start(context.Background(), Spec{Command: "   "}); err == nil {
		t.Fatal("expected an error for an empty command")
	}
}

func TestStartReportsMissingBinary(t *testing.T) {
	if _, err := Start(context.Background(), Spec{Command: "murtaugh-no-such-binary-xyz"}); err == nil {
		t.Fatal("expected an error for a missing binary")
	}
}

// Args are arguments, never a second command: proc runs argv directly with no
// shell, so shell metacharacters in an arg stay inert.
func TestArgsAreNotShellInterpreted(t *testing.T) {
	h, err := Start(context.Background(), Spec{
		Command: "echo",
		Args:    []string{"a; touch /tmp/murtaugh-proc-should-not-exist"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Kill()

	got := collect(t, h, func(l Line) bool { return !l.Partial && l.Text != "" })
	if !strings.Contains(got.Text, "touch") {
		t.Fatalf("expected the metacharacters echoed literally, got %q", got.Text)
	}
}

func TestSpecDirIsHonoured(t *testing.T) {
	dir := t.TempDir()
	h, err := Start(context.Background(), Spec{Command: "pwd", Dir: dir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Kill()

	got := collect(t, h, func(l Line) bool { return !l.Partial && l.Text != "" })
	// macOS reports /private/var… for a /var… temp dir, so match on the suffix.
	if !strings.HasSuffix(got.Text, strings.TrimPrefix(dir, "/private")) {
		t.Fatalf("pwd = %q, want something ending in %q", got.Text, dir)
	}
}

func TestCloseStdinSignalsEOF(t *testing.T) {
	h, err := Start(context.Background(), Spec{
		Command: "sh",
		Args:    []string{"-c", "cat; echo eof-seen"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Kill()

	if err := h.WriteStdin("payload\n"); err != nil {
		t.Fatalf("WriteStdin: %v", err)
	}
	if err := h.CloseStdin(); err != nil {
		t.Fatalf("CloseStdin: %v", err)
	}
	collect(t, h, func(l Line) bool { return strings.Contains(l.Text, "eof-seen") })
	if err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}
