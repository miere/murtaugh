package agent

import (
	"os/exec"
	"syscall"
)

// SetProcessGroup makes cmd start in its own process group, so its whole
// descendant tree can be torn down together with KillProcessGroup. External agent
// backends spawn grandchildren the gateway never sees — an ACP adapter forks the
// `murtaugh mcp-bridge` and a `claude` CLI; the claude_code backend's `claude`
// process forks its own MCP servers. Without a dedicated group, killing only the
// direct child leaves those orphaned when the gateway shuts down or restarts.
func SetProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillProcessGroup kills the entire process group led by cmd's process — the agent
// and every descendant it spawned. It group-kills ONLY when the process leads its
// own group (i.e. it was started with SetProcessGroup); this guards against ever
// signalling the gateway's own group. Otherwise it falls back to killing just the
// process. A nil cmd or unstarted process is a no-op.
func KillProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err == nil {
			return
		}
	}
	_ = cmd.Process.Kill()
}
