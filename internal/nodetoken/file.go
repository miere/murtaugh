package nodetoken

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileName is what a node's credential file is called inside the Murtaugh
// config directory.
const FileName = "node-token"

// FileMode is the only mode the credential file may have: readable and
// writable by its owner and by nobody else.
const FileMode os.FileMode = 0o600

// PathFor returns the credential file's location for a config directory.
//
// The config directory, and NOT .env: config.LoadDotEnv calls godotenv.Load,
// which puts every value into the DAEMON'S OWN process environment.
// agent.SpawnEnvFor filters that environment only when a sandbox is resolved,
// and the default sandbox mode is off — so with the shipped defaults a token in
// .env is inherited by every agent the node spawns. Even under seatbelt, where
// defaultEnvAllow would drop it, a profile's own `env:` map is layered on
// afterwards unconditionally. A file is the shape a sandbox rule can act on at
// all: sandbox.Spec.NodeTokenPath denies this exact path, for reads and for
// writes, unconditionally and last.
//
// # What this location does NOT buy
//
// It is not outside the agent's workspace, and nothing here should be read as
// saying so. An agent with no `workdir:` of its own is resolved onto the runtime
// base directory (agentbuild.Resolve), and the gateway and the delegate runner
// both pass the SAME base directory here — so for a default-configured agent
// this file is a direct child of its workdir. The project's own onboarding ships
// one in exactly that shape (an unsandboxed admin agent rooted in the config
// directory).
//
// So with the sandbox off — the default — and on every non-darwin host, any
// agent the node spawns can read this file. FileMode is not a second control
// against it: 0600 keeps other UNIX users out, and the agent is not another
// user, it runs as the daemon's own uid. The seatbelt rule is the whole of the
// mitigation, and where it does not apply there is none.
func PathFor(configDir string) string {
	dir := strings.TrimSpace(configDir)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, FileName)
}

// WriteFile stores a plaintext token at path with FileMode.
//
// It refuses to overwrite an existing file. Replacing a node's credential in
// place is how a rotation loses the old token before the new one has been
// accepted, which is exactly the downtime the two-live-credentials rule exists
// to avoid; the operator removes the old file deliberately or writes the new
// one somewhere else.
func WriteFile(path, token string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("node token: no file path given")
	}
	// O_EXCL, not a Stat-then-Write: the check and the create have to be the
	// same operation or two mints racing both believe they created the file.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if err != nil {
		return fmt.Errorf("node token: write %s: %w", path, err)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		_ = f.Close()
		return fmt.Errorf("node token: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("node token: write %s: %w", path, err)
	}
	return nil
}

// ReadFile reads a node's credential from disk.
//
// It refuses a file any group or other user can read. On a node whose whole
// purpose is to run a model with a shell, a 0644 credential is readable by
// every process on the box, sandbox or no sandbox — and unlike a bad token that
// fails loudly at the handshake, a permissive mode fails silently forever. The
// error names the mode and the path and never the contents.
//
// It has no caller in this stage: nothing dials the gateway yet (#193). It is
// here because the file's location and posture are decided by this issue, and a
// reader written later would be written without them.
func ReadFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("node token: no file path given")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("node token: read %s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("node token: %s is mode %#o; it must be %#o (owner only)", path, perm, FileMode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("node token: read %s: %w", path, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("node token: %s is empty", path)
	}
	return token, nil
}
