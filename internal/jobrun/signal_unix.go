//go:build !windows

package jobrun

import (
	"os/exec"
	"syscall"
)

// signalOf is the signal that ended the command, if one did.
func signalOf(exit *exec.ExitError) (syscall.Signal, bool) {
	if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal(), true
	}

	return 0, false
}
