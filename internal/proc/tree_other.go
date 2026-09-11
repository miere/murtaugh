//go:build unix && !darwin && !linux

package proc

func childrenByParent() map[int][]int { return nil }
