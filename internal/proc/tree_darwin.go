package proc

import "golang.org/x/sys/unix"

func childrenByParent() map[int][]int {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	children := make(map[int][]int, len(procs))
	for _, p := range procs {
		children[int(p.Eproc.Ppid)] = append(children[int(p.Eproc.Ppid)], int(p.Proc.P_pid))
	}
	return children
}
