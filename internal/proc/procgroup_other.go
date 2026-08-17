//go:build !unix

package proc

import "os/exec"

// configureProcessGroup is a no-op where POSIX process groups are unavailable.
// exec.CommandContext's default cancellation still kills the direct child, and
// cmd.WaitDelay bounds how long Wait blocks on a lingering pipe holder.
func configureProcessGroup(cmd *exec.Cmd) {}

// killGroup falls back to killing the direct child only; grandchildren are not
// reachable without process groups.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
