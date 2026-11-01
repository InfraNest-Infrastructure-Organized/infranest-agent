//go:build windows

package jobrun

import (
	"os/exec"
	"syscall"
)

// Windows has no signals that end a process the way POSIX's do: an exit code is all there is.
func signalOf(*exec.ExitError) (syscall.Signal, bool) {
	return 0, false
}
