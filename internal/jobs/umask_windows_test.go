//go:build windows

package jobs

func syscallUmask(m int) int { return m }
