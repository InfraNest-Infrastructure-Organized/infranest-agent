//go:build linux

package collect

import "github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/dbus"

// readTimeSync asks systemd-timedated whether the clock is synchronised (InfraNest #3832, #4391).
//
// Not `adjtimex`, though that is where the answer comes from: the unit this agent runs under forbids the
// syscall (`ProtectClock=yes`), and a forbidden syscall is not an error but SIGSYS — v0.14.0 asked the
// kernel directly and was killed on every systemd server. timedated asks on our behalf; see internal/dbus.
//
// Over the same bus the services collector uses, so it rides on that collector's switch: with
// `INFRANEST_SERVICES=0` the socket is never opened, as install.sh promises.
func readTimeSync() (bool, error) {
	conn, err := dbus.Dial(busTimeout)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()

	return conn.NTPSynchronized()
}
