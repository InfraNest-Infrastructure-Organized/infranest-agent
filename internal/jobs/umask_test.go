//go:build !windows

package jobs

import "syscall"

func syscallUmask(m int) int { return syscall.Umask(m) }
