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
	// DefaultCheckEvery is how often the warden reads the credential's expiry.
	// The read is local and cheap (no network, no inference), so this is
	// deliberately frequent: it bounds how late the warden can notice that a
	// token is about to lapse.
	DefaultCheckEvery = 2 * time.Minute

	// DefaultMargin is how long before expiry the warden starts forcing a
	// refresh.
	//
	// It must exceed Claude Code's OWN proactive-refresh threshold, which is
	// internal and unpublished. That is why the warden re-attempts on every tick
	// while inside the margin rather than firing once: whatever the CLI's
	// threshold turns out to be, one of those attempts lands inside it, and the
	// attempts stop as soon as the observed expiry moves forward. The design
	// therefore does not depend on guessing a number we cannot see.
	DefaultMargin = 15 * time.Minute

	// DefaultRefreshTimeout bounds one forcing turn. A minimal prompt answers in
	// seconds; anything approaching this means the CLI is wedged, and holding the
	// warden goroutine open would delay every later check.
	DefaultRefreshTimeout = 90 * time.Second

	// minForceInterval floors the gap between two forcing turns for the same
	// credential. Without it, a credential whose expiry never advances (a revoked
	// refresh token, a CLI that fails before reaching the network) would burn one
	// inference call per tick for as long as the daemon runs.
	minForceInterval = 60 * time.Second

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
	// Refreshes counts successful forcing turns this process.
	Refreshes int
}

// Options configures a Warden. Only Identities is required.
type Options struct {
	// Identities are the credentials to watch. Duplicates are collapsed.
	Identities []Identity
	// CheckEvery, Margin and RefreshTimeout override the defaults above.
	CheckEvery     time.Duration
	Margin         time.Duration
	RefreshTimeout time.Duration
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
	identities []Identity
	checkEvery time.Duration
	margin     time.Duration
	timeout    time.Duration
	model      string
	log        *slog.Logger
	now        func() time.Time

	readExpiry   func(context.Context, Identity) (time.Time, error)
	forceRefresh func(context.Context, Identity) error

	// mu serialises BOTH the forcing turns and access to state. Serialising the
	// turns is the point: concurrent refreshes race the server's token rotation.
	mu        sync.Mutex
	state     map[Identity]*State
	lastForce map[Identity]time.Time
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
		checkEvery:   orDuration(opts.CheckEvery, DefaultCheckEvery),
		margin:       orDuration(opts.Margin, DefaultMargin),
		timeout:      orDuration(opts.RefreshTimeout, DefaultRefreshTimeout),
		model:        strings.TrimSpace(opts.Model),
		log:          opts.Logger,
		now:          opts.now,
		readExpiry:   opts.readExpiry,
		forceRefresh: opts.forceRefresh,
		state:        make(map[Identity]*State, len(ids)),
		lastForce:    make(map[Identity]time.Time, len(ids)),
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

// Run drives the warden until ctx ends. It performs one pass immediately — a
// daemon that has been down over an expiry boundary must not wait a full tick to
// discover it — and then ticks.
func (w *Warden) Run(ctx context.Context) {
	if w == nil {
		return
	}
	w.log.Info("credential warden started",
		"credentials", len(w.identities),
		"check_every", w.checkEvery.String(),
		"margin", w.margin.String())

	w.checkAll(ctx)
	ticker := time.NewTicker(w.checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("credential warden stopped")
			return
		case <-ticker.C:
			w.checkAll(ctx)
		}
	}
}

// checkAll runs one pass over every identity. Identities are handled
// sequentially, which is what keeps two refreshes from overlapping.
func (w *Warden) checkAll(ctx context.Context) {
	for _, id := range w.identities {
		if ctx.Err() != nil {
			return
		}
		w.checkOne(ctx, id)
	}
}

// checkOne reads one credential's expiry and forces a refresh when it is inside
// the margin.
//
// Every failure is logged and recorded, never propagated: the warden is a
// background keeper, and one unreadable credential must not stop the ticker or
// starve the other identities.
func (w *Warden) checkOne(ctx context.Context, id Identity) {
	now := w.now()
	expiry, err := w.readExpiry(ctx, id)
	if err != nil {
		w.record(id, func(s *State) {
			s.LastChecked = now
			s.LastError = "read expiry: " + err.Error()
		})
		w.log.Warn("credential warden could not read expiry", "credential", id.String(), "error", err)
		return
	}

	w.record(id, func(s *State) {
		s.LastChecked = now
		s.ExpiresAt = expiry
		s.LastError = ""
	})

	remaining := expiry.Sub(now)
	if remaining > w.margin {
		return
	}

	// Inside the margin. Re-attempt on each tick until the observed expiry moves
	// forward, floored by minForceInterval so a credential that can no longer be
	// refreshed at all does not spend an inference call every tick.
	if last, ok := w.lastForced(id); ok && now.Sub(last) < minForceInterval {
		return
	}
	w.noteForced(id, now)

	w.log.Info("credential warden forcing refresh",
		"credential", id.String(),
		"expires_in", remaining.Round(time.Second).String())

	turnCtx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	if err := w.forceRefresh(turnCtx, id); err != nil {
		w.record(id, func(s *State) { s.LastError = "force refresh: " + err.Error() })
		w.log.Warn("credential warden refresh failed", "credential", id.String(), "error", err)
		return
	}
	w.record(id, func(s *State) {
		s.LastRefresh = w.now()
		s.Refreshes++
		s.LastError = ""
	})
	w.log.Info("credential warden refresh completed", "credential", id.String())
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

func (w *Warden) lastForced(id Identity) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.lastForce[id]
	return t, ok
}

func (w *Warden) noteForced(id Identity, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastForce[id] = at
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
