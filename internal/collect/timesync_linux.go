//go:build linux

package collect

import "syscall"

// readTimeSync asks the kernel whether the clock is synchronised (InfraNest #3832).
//
// adjtimex with no modes set only reads: no privilege, nothing changed. It is the kernel's own view, and
// what chrony, ntpd and systemd-timesyncd all report into, so it answers for whichever is installed.
func readTimeSync() (*bool, *int64) {
	var tx syscall.Timex // Modes zero: read only.
	state, err := syscall.Adjtimex(&tx)
	if err != nil {
		return nil, nil
	}

	return timeSyncFrom(state, int64(tx.Status), int64(tx.Maxerror))
}
