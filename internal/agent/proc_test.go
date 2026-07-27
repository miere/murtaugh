package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSetProcessGroup_MakesOwnGroupLeader(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { KillProcessGroup(cmd) })

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}
	if pgid != cmd.Process.Pid {
		t.Fatalf("pgid=%d, want it to equal pid %d (its own group leader)", pgid, cmd.Process.Pid)
	}
}

// TestKillProcessGroup_KillsTheWholeTree is the core guarantee: killing the group
// reaps not just the direct child but the grandchildren it spawned — the exact
// orphaning we saw with mcp-bridge / claude subprocesses.
func TestKillProcessGroup_KillsTheWholeTree(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "grandchild.pid")
	// sh becomes the group leader and forks a background `sleep` (the grandchild),
	// records its pid, then waits — so the process tree is: us → sh → sleep.
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $! > "+pidfile+"; wait")
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { KillProcessGroup(cmd) })
	go func() { _ = cmd.Wait() }()

	grandchild := waitForPID(t, pidfile)
	if !processAlive(grandchild) {
		t.Fatalf("grandchild %d should be alive right after start", grandchild)
	}

	KillProcessGroup(cmd)

	// The grandchild shared the group, so it must die too — not just sh.
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(grandchild) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still alive after KillProcessGroup — the tree was not killed", grandchild)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestKillProcessGroup_NilAndUnstartedAreSafe(t *testing.T) {
	KillProcessGroup(nil)
	KillProcessGroup(&exec.Cmd{}) // never started: no Process — must not panic
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				if pid, perr := strconv.Atoi(s); perr == nil {
					return pid
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("grandchild pid was not recorded in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// processAlive reports whether pid is still a live process, via a signal-0 probe.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
