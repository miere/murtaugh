package nodetoken

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const FileName = "node-token"

const FileMode os.FileMode = 0o600

// Not .env, whose values leak into every agent the node spawns. Only the
// seatbelt sandbox rule keeps agents from reading this file.
func PathFor(configDir string) string {
	dir := strings.TrimSpace(configDir)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, FileName)
}

// Never overwrites: replacing a credential in place loses the old token before
// the new one is accepted, which breaks rotation.
func WriteFile(path, token string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("node token: no file path given")
	}
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

// A bad token fails loudly at the handshake, but a readable credential file
// leaks silently, so a permissive mode is refused.
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
