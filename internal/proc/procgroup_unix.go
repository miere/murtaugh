//go:build unix

package proc

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the child in its own process group so the whole
// group can be signalled at once, and wires ctx cancellation to that kill.
//
// Killing the group (negative PID) reaps grandchildren the child may have
// spawned — an auth helper that launches a browser, say — which would otherwise
// survive the parent and hold the output pipes open past cancellation.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		killGroup(cmd)
		return nil
	}
}

// killGroup SIGKILLs the child's whole process group. A no-op before the
// process has started or after it has been reaped, so it is safe to defer.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
