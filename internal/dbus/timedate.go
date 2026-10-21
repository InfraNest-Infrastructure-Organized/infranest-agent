package dbus

import "fmt"

/*
systemd-timedated, and the one question the agent asks it: is the clock synchronised (InfraNest #4391).

The kernel knows, through `adjtimex` — but the unit the installer writes forbids that syscall
(`ProtectClock=yes`, and `SystemCallFilter=@system-service` leaves out `@clock`). Seccomp filters on the
syscall number, not its arguments, so even a read-only call kills the agent with SIGSYS. v0.14.0 did exactly
that on every systemd server it reached.

timedated makes the same `adjtimex` call outside our sandbox and answers `NTPSynchronized` from it
(`TIME_ERROR` and `STA_UNSYNC`), so the meaning is unchanged: the kernel's own view, which chrony, ntpd and
timesyncd all report into. `timedatectl` asks the same property, unprivileged. It is bus-activated, so a
machine where it is not running starts it on the first question, and one where it is not installed answers
with an error rather than a value.

A read, like everything else in this package.
*/
const (
	timedateDestination = "org.freedesktop.timedate1"
	timedatePath        = "/org/freedesktop/timedate1"
	timedateInterface   = "org.freedesktop.timedate1"
)

// NTPSynchronized is whether the kernel considers the system clock synchronised, as timedated reports it.
func (c *Conn) NTPSynchronized() (bool, error) {
	body, signature, err := c.Call(
		timedateDestination, timedatePath, propertiesGet,
		"Get", "ss", timedateInterface, "NTPSynchronized",
	)
	if err != nil {
		return false, err
	}
	if signature != "v" {
		return false, fmt.Errorf("Properties.Get answered %q, expected \"v\"", signature)
	}

	return decodeVariantBool(body, "NTPSynchronized")
}

// decodeVariantBool pulls a boolean out of a `Properties.Get` reply body.
//
// A D-Bus `b` is four bytes on the wire, and only 0 and 1 are valid. Anything else is refused rather than
// read as true: a value that is not a boolean is a reply we do not understand, and "synchronised" is the
// answer that silences an alert.
func decodeVariantBool(body []byte, name string) (bool, error) {
	d := &decoder{buf: body}
	if inner := d.signature(); inner != "b" {
		return false, fmt.Errorf("%s is %q, expected a boolean", name, inner)
	}
	value := d.uint32()
	if d.err != nil {
		return false, d.err
	}
	if value > 1 {
		return false, fmt.Errorf("%s is %d, which is not a boolean", name, value)
	}

	return value == 1, nil
}
