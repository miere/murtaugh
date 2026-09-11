package nodetoken

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathForIsInsideTheConfigDirAndNamed(t *testing.T) {
	if got := PathFor("/etc/murtaugh"); got != filepath.Join("/etc/murtaugh", FileName) {
		t.Fatalf("PathFor = %q, want the config dir + %q", got, FileName)
	}
	if got := PathFor(""); got != "" {
		t.Fatalf("PathFor(\"\") = %q, want the empty string", got)
	}
	if got := PathFor("   "); got != "" {
		t.Fatalf("PathFor(whitespace) = %q, want the empty string", got)
	}
}

func TestWriteFileUsesOwnerOnlyMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	minted, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := WriteFile(path, minted.Token); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != FileMode {
		t.Fatalf("mode = %#o, want %#o; a node's credential must not be readable by other users", perm, FileMode)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != minted.Token {
		t.Fatalf("ReadFile round-trip changed the token")
	}
}

func TestWriteFileRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := WriteFile(path, "mrtg_node_first"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := WriteFile(path, "mrtg_node_second"); err == nil {
		t.Fatal("WriteFile overwrote an existing credential file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "mrtg_node_first" {
		t.Fatal("the original credential was replaced")
	}
}

func TestReadFileRefusesAWorldReadableCredential(t *testing.T) {
	for name, mode := range map[string]os.FileMode{
		"group readable": 0o640,
		"world readable": 0o644,
		"world writable": 0o666,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			if err := os.WriteFile(path, []byte("mrtg_node_abc"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			_, err := ReadFile(path)
			if err == nil {
				t.Fatalf("ReadFile accepted a %#o credential file", mode)
			}
			if strings.Contains(err.Error(), "mrtg_node_abc") {
				t.Fatalf("the error quotes the credential: %v", err)
			}
		})
	}
}

func TestReadFileRejectsMissingAndEmpty(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadFile(filepath.Join(dir, "absent")); err == nil {
		t.Error("ReadFile invented a credential for a file that does not exist")
	}

	empty := filepath.Join(dir, FileName)
	if err := os.WriteFile(empty, []byte("  \n"), FileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(empty); err == nil {
		t.Error("ReadFile accepted an empty credential file")
	}
	if _, err := ReadFile(""); err == nil {
		t.Error("ReadFile accepted an empty path")
	}
}
