package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/miere/murtaugh/internal/proc"
)

const (
	// DefaultTimeout is long enough for an owner to notice a DM and sign in, and
	// short enough that a forgotten sign-in does not hold its turn open for good.
	DefaultTimeout = 10 * time.Minute

	// DefaultURLWait fails a command that has printed no link by then, because
	// it is broken and there would be nothing to show anyone.
	DefaultURLWait = 60 * time.Second
)

// Login is a sign-in process with no Slack in it, so the machine whose
// credentials it writes is the one that runs it.
type Login struct {
	h       *proc.Handle
	cleanup func()
}

// StartLogin returns only once there is a link to show, because a sign-in with
// nothing to open cannot be finished by anyone.
func StartLogin(ctx context.Context, p Profile, env []string, urlWait time.Duration) (*Login, string, error) {
	spec, cleanup, err := commandSpec(p, env)
	if err != nil {
		cleanup()
		return nil, "", fmt.Errorf("could not prepare the authentication command: %w", err)
	}
	h, err := proc.Start(ctx, spec)
	if err != nil {
		cleanup()
		return nil, "", fmt.Errorf("could not start the authentication command: %w", err)
	}
	login := &Login{h: h, cleanup: cleanup}
	url, err := waitForURL(ctx, h, p, urlWait)
	if err != nil {
		login.Stop()
		return nil, "", err
	}
	return login, url, nil
}

func (l *Login) SendCode(code string) error { return l.h.WriteLine(code) }

func (l *Login) Exited() <-chan struct{} { return l.h.Exited() }

// Result reads the finished process; its detail quotes the command's output,
// which only the person completing the sign-in should see.
func (l *Login) Result() (ok bool, detail string) {
	if Succeeded(l.h.Wait()) {
		return true, ""
	}
	return false, describeFailure(l.h)
}

// Stop is safe to call more than once, so every exit path can stop the process
// without knowing whether another already did.
func (l *Login) Stop() {
	l.h.Kill()
	l.cleanup()
}

func commandSpec(p Profile, env []string) (proc.Spec, func(), error) {
	spec := p.Spec()
	cleanup := func() {}

	overrides := env
	if p.SuppressBrowser {
		guard, err := NewBrowserGuard()
		if err != nil {
			return proc.Spec{}, cleanup, err
		}
		cleanup = func() { _ = guard.Close() }
		effective := proc.MergeEnv(os.Environ(), overrides)
		overrides = append(append([]string(nil), overrides...), guard.Overrides(effective)...)
	}

	if len(overrides) > 0 {
		spec.Env = proc.MergeEnv(os.Environ(), overrides)
	}
	return spec, cleanup, nil
}

func waitForURL(ctx context.Context, h *proc.Handle, p Profile, wait time.Duration) (string, error) {
	if wait <= 0 {
		wait = DefaultURLWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	for {
		select {
		case line, ok := <-h.Lines():
			if !ok {
				return "", fmt.Errorf("the authentication command finished without offering a sign-in link: %s%s",
					oneLine(h.Output()), driftSuffix(ctx, p))
			}
			if url, found := p.ExtractURL(line.Text); found {
				return url, nil
			}
		case <-timer.C:
			return "", fmt.Errorf("the authentication command did not offer a sign-in link within %s: %s%s",
				wait, oneLine(h.Output()), driftSuffix(ctx, p))
		case <-ctx.Done():
			return "", errors.New("the turn was cancelled before authentication started")
		}
	}
}

func driftSuffix(ctx context.Context, p Profile) string {
	if note := p.VersionDrift(ctx); note != "" {
		return " (" + note + ")"
	}
	return ""
}

func describeFailure(h *proc.Handle) string {
	out := oneLine(h.Output())
	if out == "" {
		return "the authentication command failed"
	}
	return "the authentication command failed: " + out
}

func oneLine(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	r := []rune(strings.Join(fields, " "))
	if len(r) <= 400 {
		return string(r)
	}
	return string(r[:399]) + "…"
}
