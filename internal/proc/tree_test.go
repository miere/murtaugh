//go:build unix

package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestKillReachesADescendantThatLeftTheGroup(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl is needed to detach a grandchild from the group")
	}
	marker := filepath.Join(t.TempDir(), "escaped")
	h, err := Start(context.Background(), Spec{Command: "sh", Args: []string{"-c",
		`perl -e 'setpgrp(0, 0); print "detached\n"; STDOUT->flush; sleep 2; open(my $f, ">", $ARGV[0]); close $f' "$0" & exec sleep 30`,
		marker}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case line := <-h.Lines():
		if line.Text != "detached" {
			t.Fatalf("unexpected output %q", line.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the grandchild never detached")
	}
	h.Kill()
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a grandchild that left the process group survived Kill and acted afterwards")
	}
}
