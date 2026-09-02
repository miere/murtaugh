// Package credwarden keeps a Claude Code credential fresh from OUTSIDE the
// agent sandbox.
//
// The problem it solves: a `claude` process confined by internal/agent/sandbox
// can READ its OAuth credential but cannot WRITE it back. The macOS login
// keychain is a single file under ~/Library/Keychains, which the seatbelt
// profile's blanket `(deny file-write*)` covers and no carve-out re-admits, so
// the atomic-replace write fails part-way and leaves a stranded `.sb-*` temp
// behind. Anthropic's refresh tokens ROTATE — the server retires the old one the
// moment it issues a new one — so a refresh whose result cannot be persisted
// does not merely fail to help: it destroys the stored credential. The next
// spawn presents a refresh token the server has already retired and the agent is
// locked out. That is the "arbitrary revocation" the operator sees.
//
// The fix is not to relocate the credential (every store reachable by a boxed
// agent is equally writable AND equally readable by it) but to make sure the
// refresh happens in a process that was never boxed. The warden watches the
// credential's expiry and, shortly before it lapses, runs one minimal
// UNSANDBOXED `claude` turn. Claude Code performs its own refresh on that turn
// and persists the result normally, because nothing is denying the write. Boxed
// sessions then only ever read an already-valid credential.
//
// Three properties are load-bearing:
//
//   - It is a SINGLETON per credential. Two wardens refreshing concurrently
//     would have the second present a token the first just retired, reproducing
//     the exact bug this package exists to prevent. Refreshes are serialised.
//   - It is INTERNAL. It is not a config-store job, so no agent can enumerate,
//     run, redefine, or silently disable it (see docs/operations.md).
//   - It NEVER handles the secret. It reads `expiresAt` and discards the rest of
//     the credential blob immediately; no token value is retained, logged, or
//     returned.
package credwarden

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultForceAt is how long before expiry the warden starts forcing a
	// refresh.
	//
	// The number is measured, not guessed. Claude Code refreshes proactively
	// only once the token is inside its OWN internal threshold, and an observed
	// run pinned that threshold at five minutes: a forcing turn at 5m13s
	// remaining did nothing, and the next at 3m13s refreshed. So the effective
	// window is the last five minutes before expiry, and aiming at three minutes
	// lands inside it with margin on both sides.
	//
	// An earlier version swept a fifteen-minute margin on a two-minute ticker.
	// That spent five inference calls hitting nothing before the sixth worked,
	// and — the reason this changed — it left only about two ticks inside the
	// window that actually mattered. Losing either one to a suspend or a restart
	// lost the credential.
	DefaultForceAt = 3 * time.Minute

	// DefaultRetryInterval is the cadence while inside the window, until the
	// observed expiry actually moves.
	DefaultRetryInterval = 30 * time.Second

	// DefaultMaxSleep caps any single wait.
	//
	// It is what makes the warden robust to a suspended machine and a jumped
	// clock. Go timers run on the monotonic clock, which stops while the host
	// sleeps, so a long wait computed before a suspend fires late by however
	// long the machine was away. Capping the wait means the loop re-reads the
	// credential's real, wall-clock expiry promptly after waking and can see
	// that it has already lapsed. The read is a local keychain lookup — no
	// network, no inference — so a short cap is nearly free.
	DefaultMaxSleep = 5 * time.Minute

	// DefaultMaxAttemptsPerExpiry bounds how many forcing turns one unchanged
	// expiry may provoke before the warden backs off to DefaultMaxSleep.
	//
	// Without it a credential that can no longer be refreshed at all — a revoked
	// refresh token, a CLI that fails before reaching the network — would spend
	// an inference call every RetryInterval for as long as the daemon runs.
	DefaultMaxAttemptsPerExpiry = 6

	// DefaultRefreshTimeout bounds one forcing turn. A minimal prompt answers in
	// seconds; anything approaching this means the CLI is wedged, and holding the
	// warden goroutine open would delay every later check.
	DefaultRefreshTimeout = 90 * time.Second

	// keychainService is the login-keychain item Claude Code stores its
	// credential under on macOS.
	keychainService = "Claude Code-credentials"

	// securityBinary is the macOS keychain CLI. It is addressed by absolute path
	// so a PATH entry cannot substitute a different binary for a call that
	// handles credentials.
	securityBinary = "/usr/bin/security"
)

// Identity names one credential store. Two agent profiles that resolve to the
// same identity share a credential and therefore share a single warden.
//
// Home is part of the key because ClaudeCodeProfile.Env is layered onto the
// spawn environment unconditionally (agent.SpawnEnvFor), so a profile CAN point
// itself at a different HOME and thereby a different credential file. Keying on
// the binary alone would silently leave such an agent unwatched.
type Identity struct {
	// Command is the resolved `claude` binary.
	Command string
	// Home is the effective HOME for that agent, or "" to inherit the daemon's.
	Home string
}

// String renders the identity for logs and the diagnostics surface.
func (i Identity) String() string {
	if i.Home == "" {
		return i.Command
	}
	return i.Command + " (HOME=" + i.Home + ")"
}

// State is the warden's public, read-only view of one credential. It carries no
// secret material — only timing and the last error — so it is safe to render
// into the troubleshoot bundle and the startup routing summary.
type State struct {
	Identity Identity
	// ExpiresAt is the credential expiry last observed, zero if never read.
	ExpiresAt time.Time
	// LastChecked is when the expiry was last read.
	LastChecked time.Time
	// LastRefresh is when a forcing turn last completed successfully.
	LastRefresh time.Time
	// LastError describes the most recent failure, empty when healthy.
	LastError string
	// Refreshes counts refreshes this process actually achieved — turns after
	// which the observed expiry moved forward, not turns that merely exited 0.
	Refreshes int
	// NextCheck is when the warden intends to look again, zero if not scheduled
	// yet. Surfaced so an operator can see the warden is aimed at something
	// rather than merely alive.
	NextCheck time.Time
	// Attempts counts forcing turns spent against the CURRENT expiry without
	// moving it. Reset the moment a refresh lands.
	Attempts int
}

// Options configures a Warden. Only Identities is required.
type Options struct {
	// Identities are the credentials to watch. Duplicates are collapsed.
	Identities []Identity
	// ForceAt, RetryInterval, MaxSleep, MaxAttemptsPerExpiry and RefreshTimeout
	// override the defaults above.
	ForceAt              time.Duration
	RetryInterval        time.Duration
	MaxSleep             time.Duration
	MaxAttemptsPerExpiry int
	RefreshTimeout       time.Duration
	// Model is passed to the forcing turn as --model. Empty uses the cheapest
	// alias, since the turn's content is irrelevant — only the auth handshake
	// matters.
	Model  string
	Logger *slog.Logger
	// now is the clock; nil uses time.Now. Tests inject a fake.
	now func() time.Time
	// readExpiry and forceRefresh are the two seams onto the outside world.
	// nil installs the real implementations; tests substitute them so the suite
	// never touches a keychain or spends an inference call.
	readExpiry   func(context.Context, Identity) (time.Time, error)
	forceRefresh func(context.Context, Identity) error
}

// Warden watches one or more credentials and refreshes each before it lapses.
type Warden struct {
	identities  []Identity
	forceAt     time.Duration
	retry       time.Duration
	maxSleep    time.Duration
	maxAttempts int
	timeout     time.Duration
	model       string
	log         *slog.Logger
	now         func() time.Time

	readExpiry   func(context.Context, Identity) (time.Time, error)
	forceRefresh func(context.Context, Identity) error

	// mu serialises BOTH the forcing turns and access to state. Serialising the
	// turns is the point: concurrent refreshes race the server's token rotation.
	mu    sync.Mutex
	state map[Identity]*State
}

// New builds a Warden. It returns nil when there is nothing to watch, which the
// caller reads as "do not start" — the gate on whether any claude_code agent is
// configured lives at the call site, not in a config flag.
func New(opts Options) *Warden {
	ids := dedupe(opts.Identities)
	if len(ids) == 0 {
		return nil
	}
	w := &Warden{
		identities:   ids,
		forceAt:      orDuration(opts.ForceAt, DefaultForceAt),
		retry:        orDuration(opts.RetryInterval, DefaultRetryInterval),
		maxSleep:     orDuration(opts.MaxSleep, DefaultMaxSleep),
		maxAttempts:  orInt(opts.MaxAttemptsPerExpiry, DefaultMaxAttemptsPerExpiry),
		timeout:      orDuration(opts.RefreshTimeout, DefaultRefreshTimeout),
		model:        strings.TrimSpace(opts.Model),
		log:          opts.Logger,
		now:          opts.now,
		readExpiry:   opts.readExpiry,
		forceRefresh: opts.forceRefresh,
		state:        make(map[Identity]*State, len(ids)),
	}
	if w.log == nil {
		w.log = slog.Default()
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.readExpiry == nil {
		w.readExpiry = readExpiry
	}
	if w.forceRefresh == nil {
		w.forceRefresh = w.runForcingTurn
	}
	for _, id := range ids {
		w.state[id] = &State{Identity: id}
	}
	return w
}

// Identities returns the credentials this warden watches, for the startup log.
func (w *Warden) Identities() []Identity {
	if w == nil {
		return nil
	}
	return append([]Identity(nil), w.identities...)
}

// States returns a snapshot of every watched credential, sorted by identity, for
// the diagnostics surface. Safe to call concurrently with the run loop.
func (w *Warden) States() []State {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]State, 0, len(w.identities))
	for _, id := range w.identities {
		if s, ok := w.state[id]; ok {
			out = append(out, *s)
		}
	}
	return out
}

// Run drives the warden until ctx ends.
//
// It does not tick. It AIMS: each pass reads the credential's real expiry and
// sleeps until shortly before it, so the forcing turn lands inside the window
// where Claude Code will actually act on it. A ticker sweeping a wide margin
// spends most of its calls too early to do anything and, worse, leaves only a
// couple of chances inside the window that counts — lose one to a suspend or a
// restart and the credential is gone.
//
// Every wait is capped (see DefaultMaxSleep) and every pass re-reads the expiry
// from the credential store rather than trusting elapsed time, so a machine that
// slept through its own deadline notices on the next pass instead of waiting out
// a monotonic timer that stopped while it was away.
func (w *Warden) Run(ctx context.Context) {
	if w == nil {
		return
	}
	w.log.Info("credential warden started",
		"credentials", len(w.identities),
		"force_at", w.forceAt.String(),
		"retry", w.retry.String(),
		"max_sleep", w.maxSleep.String())

	for {
		if ctx.Err() != nil {
			w.log.Info("credential warden stopped")
			return
		}
		wait := w.checkAll(ctx)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			w.log.Info("credential warden stopped")
			return
		case <-timer.C:
		}
	}
}

// checkAll runs one pass over every identity and returns how long to wait before
// the next pass — the soonest any credential wants attention.
//
// Identities are handled sequentially, which is what keeps two forcing turns
// from overlapping: concurrent refreshes race the server's rotation of the
// refresh token, and the loser presents one that has just been retired.
func (w *Warden) checkAll(ctx context.Context) time.Duration {
	wait := w.maxSleep
	for _, id := range w.identities {
		if ctx.Err() != nil {
			return wait
		}
		if d := w.checkOne(ctx, id); d < wait {
			wait = d
		}
	}
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

// checkOne handles one credential and returns how long until it next wants
// looking at.
//
// Every failure is logged and recorded, never propagated: the warden is a
// background keeper, and one unreadable credential must not stop the loop or
// starve the others.
func (w *Warden) checkOne(ctx context.Context, id Identity) time.Duration {
	now := w.now()
	expiry, err := w.readExpiry(ctx, id)
	if err != nil {
		w.record(id, func(s *State) {
			s.LastChecked = now
			s.LastError = "read expiry: " + err.Error()
			s.NextCheck = now.Add(w.retry)
		})
		w.log.Warn("credential warden could not read expiry", "credential", id.String(), "error", err)
		return w.retry
	}

	// A new expiry retires whatever attempt count the previous one accumulated:
	// the credential moved, so the reason for backing off is gone.
	w.record(id, func(s *State) {
		if !s.ExpiresAt.Equal(expiry) {
			s.Attempts = 0
		}
		s.LastChecked = now
		s.ExpiresAt = expiry
		s.LastError = ""
	})

	remaining := expiry.Sub(now)
	if remaining > w.forceAt {
		// Still comfortable. Sleep until the window opens, capped so a suspend
		// cannot carry us past the deadline unnoticed.
		return w.schedule(id, now, capDuration(remaining-w.forceAt, w.maxSleep))
	}

	// Inside the window — or already lapsed, which a restart after a suspend is
	// the usual way to arrive at, and which is still worth one attempt.
	if attempts := w.attempts(id); attempts >= w.maxAttempts {
		// This credential is not coming back by being asked again. Back off
		// rather than spend an inference call every retry for as long as the
		// daemon runs; a re-authentication is what fixes it now.
		w.log.Warn("credential warden backing off; the expiry has not moved",
			"credential", id.String(),
			"attempts", attempts,
			"expires_in", remaining.Round(time.Second).String())
		return w.schedule(id, now, w.maxSleep)
	}

	w.log.Info("credential warden forcing refresh",
		"credential", id.String(),
		"expires_in", remaining.Round(time.Second).String())

	turnCtx, cancel := context.WithTimeout(ctx, w.timeout)
	err = w.forceRefresh(turnCtx, id)
	cancel()
	if err != nil {
		w.record(id, func(s *State) {
			s.LastError = "force refresh: " + err.Error()
			s.Attempts++
		})
		w.log.Warn("credential warden refresh failed", "credential", id.String(), "error", err)
		return w.schedule(id, w.now(), w.retry)
	}

	// Exit status 0 is NOT success. Claude Code refreshes only once the token is
	// inside its own threshold, so a turn can complete perfectly and leave the
	// credential exactly as it was. The only honest evidence is the stored expiry
	// moving forward, so read it back and say which happened.
	after, readErr := w.readExpiry(ctx, id)
	now = w.now()
	if readErr != nil {
		w.record(id, func(s *State) {
			s.LastError = "verify refresh: " + readErr.Error()
			s.Attempts++
		})
		w.log.Warn("credential warden could not verify the refresh", "credential", id.String(), "error", readErr)
		return w.schedule(id, now, w.retry)
	}
	if !after.After(expiry) {
		w.record(id, func(s *State) { s.Attempts++ })
		w.log.Info("credential warden turn completed but the expiry did not move; retrying",
			"credential", id.String(),
			"expires_in", after.Sub(now).Round(time.Second).String())
		return w.schedule(id, now, w.retry)
	}

	w.record(id, func(s *State) {
		s.ExpiresAt = after
		s.LastRefresh = now
		s.Refreshes++
		s.Attempts = 0
		s.LastError = ""
	})
	w.log.Info("credential warden refreshed the credential",
		"credential", id.String(),
		"valid_for", after.Sub(now).Round(time.Second).String())
	return w.schedule(id, now, capDuration(after.Sub(now)-w.forceAt, w.maxSleep))
}

// schedule records when the warden next intends to look and returns the wait.
func (w *Warden) schedule(id Identity, now time.Time, wait time.Duration) time.Duration {
	if wait < time.Second {
		wait = time.Second
	}
	w.record(id, func(s *State) { s.NextCheck = now.Add(wait) })
	return wait
}

func (w *Warden) attempts(id Identity) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s, ok := w.state[id]; ok {
		return s.Attempts
	}
	return 0
}

// runForcingTurn is the real refresh mechanism: one minimal UNSANDBOXED `claude`
// turn.
//
// It is deliberately not `claude auth status`, which reads local state and
// returns without touching the network — measured, it leaves expiresAt
// untouched. Only a turn that actually authenticates makes the CLI refresh and
// persist. The prompt is a single token and the model the cheapest available,
// because the answer is discarded: the auth handshake is the entire point.
//
// Nothing here is wrapped in a sandbox. That is the whole reason the package
// exists, so it is stated rather than implied.
func (w *Warden) runForcingTurn(ctx context.Context, id Identity) error {
	args := []string{"-p", "ok"}
	if w.model != "" {
		args = append(args, "--model", w.model)
	}
	cmd := exec.CommandContext(ctx, id.Command, args...)
	cmd.Env = environFor(id)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, oneLine(out))
	}
	return nil
}

func (w *Warden) record(id Identity, mutate func(*State)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s, ok := w.state[id]; ok {
		mutate(s)
	}
}

// --- credential reading ----------------------------------------------------

// credentialBlob is the shape the warden parses out of Claude Code's stored
// credential. Only the expiry is declared: the access and refresh tokens are
// present in the source bytes but are deliberately not given a field, so no code
// path here can retain, log, or return them.
type credentialBlob struct {
	ClaudeAIOAuth struct {
		// ExpiresAt is milliseconds since the Unix epoch.
		ExpiresAt int64 `json:"expiresAt"`
	} `json:"claudeAiOauth"`
}

// readExpiry returns when the credential for id lapses.
//
// On macOS the live store is the login keychain, so that is tried first;
// ~/.claude/.credentials.json is the fallback and is the only store on other
// platforms. A machine can have both, with the file left stale from an earlier
// login, so preferring the keychain is what keeps the warden from acting on a
// months-old expiry.
func readExpiry(ctx context.Context, id Identity) (time.Time, error) {
	var errs []error
	if runtime.GOOS == "darwin" {
		expiry, err := readExpiryFromKeychain(ctx, id)
		if err == nil {
			return expiry, nil
		}
		errs = append(errs, fmt.Errorf("keychain: %w", err))
	}
	expiry, err := readExpiryFromFile(id)
	if err == nil {
		return expiry, nil
	}
	errs = append(errs, fmt.Errorf("credentials file: %w", err))
	return time.Time{}, errors.Join(errs...)
}

// readExpiryFromKeychain shells out to `security`, which is how the credential
// is reachable without linking the Security framework.
func readExpiryFromKeychain(ctx context.Context, id Identity) (time.Time, error) {
	cmd := exec.CommandContext(ctx, securityBinary,
		"find-generic-password", "-w", "-s", keychainService)
	cmd.Env = environFor(id)
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, fmt.Errorf("find-generic-password: %w", err)
	}
	return parseExpiry(out)
}

func readExpiryFromFile(id Identity) (time.Time, error) {
	home := id.Home
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return time.Time{}, fmt.Errorf("resolve home: %w", err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return time.Time{}, err
	}
	return parseExpiry(raw)
}

// parseExpiry extracts the expiry from a credential blob. The caller's byte
// slice still holds token material, so nothing derived from it escapes this
// function beyond the timestamp.
func parseExpiry(raw []byte) (time.Time, error) {
	var blob credentialBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		// The error deliberately does not quote the input: it is a credential.
		return time.Time{}, errors.New("credential is not valid JSON")
	}
	ms := blob.ClaudeAIOAuth.ExpiresAt
	if ms <= 0 {
		return time.Time{}, errors.New("credential carries no expiresAt")
	}
	return time.UnixMilli(ms), nil
}

// --- helpers ---------------------------------------------------------------

// environFor builds the child environment, overriding HOME when the identity
// names one. The daemon's environment is inherited wholesale: this is the
// unsandboxed path by design, and the child is Claude Code itself rather than an
// agent-controlled command.
func environFor(id Identity) []string {
	if id.Home == "" {
		return nil
	}
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if key, _, ok := strings.Cut(kv, "="); ok && key == "HOME" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+id.Home)
}

// dedupe collapses identities to the distinct set, preserving first-seen order
// so logs and the diagnostics surface are stable across restarts.
func dedupe(in []Identity) []Identity {
	seen := make(map[Identity]bool, len(in))
	out := make([]Identity, 0, len(in))
	for _, id := range in {
		id.Command = strings.TrimSpace(id.Command)
		id.Home = strings.TrimSpace(id.Home)
		if id.Command == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// capDuration clamps d to at most limit, and never below zero.
func capDuration(d, limit time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > limit {
		return limit
	}
	return d
}

func orInt(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

func orDuration(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}

// oneLine flattens child output into a short single line for an error message.
// Claude Code does not print credentials on the failure path, but the output is
// clamped regardless rather than relying on that.
func oneLine(b []byte) string {
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return "no output"
	}
	s := strings.Join(fields, " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
