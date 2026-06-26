package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrependPersona(t *testing.T) {
	got := PrependPersona("You are Murtaugh.", "Follow the rules.")
	if !strings.HasPrefix(got, "<persona>\nYou are Murtaugh.\n</persona>") {
		t.Fatalf("persona block not prepended: %q", got)
	}
	if !strings.Contains(got, "Follow the rules.") {
		t.Fatalf("base prompt lost: %q", got)
	}
	// Empty persona leaves the base untouched.
	if got := PrependPersona("", "base"); got != "base" {
		t.Fatalf("empty persona changed base: %q", got)
	}
	// Empty base with a persona yields just the block.
	if got := PrependPersona("Hi", ""); got != "<persona>\nHi\n</persona>" {
		t.Fatalf("unexpected persona-only output: %q", got)
	}
}

func TestReadSoul(t *testing.T) {
	dir := t.TempDir()
	if got := ReadSoul(dir); got != "" {
		t.Fatalf("expected empty when no SOUL.md, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("You are Murtaugh."), 0o644); err != nil {
		t.Fatalf("write SOUL.md: %v", err)
	}
	if got := ReadSoul(dir); got != "You are Murtaugh." {
		t.Fatalf("ReadSoul = %q, want the file contents", got)
	}
	if got := ReadSoul(""); got != "" {
		t.Fatalf("empty dir should yield empty, got %q", got)
	}
}
