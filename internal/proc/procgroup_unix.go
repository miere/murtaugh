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

func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	var tree []int
	if cmd.Process.Signal(syscall.SIGSTOP) == nil {
		tree = stopDescendants(pid)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = cmd.Process.Signal(syscall.SIGKILL)
	for _, p := range tree {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
}

func stopDescendants(root int) []int {
	stopped := map[int]bool{root: true}
	var found []int
	for range 8 {
		children := childrenByParent()
		fresh := 0
		queue := []int{root}
		for len(queue) > 0 {
			parent := queue[0]
			queue = queue[1:]
			for _, child := range children[parent] {
				if !stopped[child] {
					stopped[child] = true
					found = append(found, child)
					_ = syscall.Kill(child, syscall.SIGSTOP)
					fresh++
				}
				queue = append(queue, child)
			}
		}
		if fresh == 0 {
			break
		}
	}
	return found
}
