package dbus

import (
	"fmt"
	"time"
)

/*
The systemd manager, and the three questions the agent asks it.

All three are reads. Nothing in this file, and nothing this package can express, starts, stops, restarts
or kills anything — and that is not only our restraint: `StartUnit` and its relatives are gated behind
polkit's `manage-units` action, which requires an authenticated administrator. A system daemon with no
login session has no way to satisfy it, and on a machine with no polkit at all systemd allows uid 0 only.
So the read-only property is the operating system's, not a promise about our own good behaviour.

CI enforces the intent as well: the build fails if a unit-control method name ever appears in this
package, for the same reason it fails on `os/exec`. An invariant nobody checks is an invariant that erodes.
*/
const (
	managerDestination = "org.freedesktop.systemd1"
	managerPath        = "/org/freedesktop/systemd1"
	managerInterface   = "org.freedesktop.systemd1.Manager"
	unitInterface      = "org.freedesktop.systemd1.Unit"
	serviceInterface   = "org.freedesktop.systemd1.Service"
	propertiesGet      = "org.freedesktop.DBus.Properties"
)

// Unit is one loaded unit, as ListUnits describes it.
type Unit struct {
	Name        string
	Description string
	LoadState   string
	ActiveState string
	SubState    string
	Path        string
}

// ListUnits returns every unit systemd currently has loaded.
//
// Reply signature `a(ssssssouso)`: name, description, load state, active state, sub state, followed unit,
// object path, job id, job type, job object path. The last four are read and discarded — a job is a
// transition in progress, which is a different question from what state the unit is in.
func (c *Conn) ListUnits() ([]Unit, error) {
	body, signature, err := c.Call(managerDestination, managerPath, managerInterface, "ListUnits", "")
	if err != nil {
		return nil, err
	}

	const want = "a(ssssssouso)"
	if signature != want {
		// systemd has never changed this signature, and if it ever does, saying so is far better than
		// decoding the new shape as though it were the old one and reporting units that do not exist.
		return nil, fmt.Errorf("ListUnits answered %q, expected %q", signature, want)
	}

	return decodeUnits(body)
}

// decodeUnits reads the `a(ssssssouso)` body of a ListUnits reply.
//
// Split from the call so the wire format can be tested without a socket — the encoding is the part worth
// testing, and it has nothing to do with how the bytes arrived.
func decodeUnits(body []byte) ([]Unit, error) {
	d := &decoder{buf: body}
	count, err := checkedLen(d.uint32())
	if err != nil {
		return nil, err
	}
	if count > len(body) {
		return nil, fmt.Errorf("unit array claims %d bytes, reply has %d", count, len(body))
	}
	d.pad(8)
	end := d.pos + count

	units := make([]Unit, 0, 64)
	for d.pos < end && d.err == nil {
		d.pad(8) // each struct in the array starts on an eight-byte boundary
		unit := Unit{
			Name:        d.str(),
			Description: d.str(),
			LoadState:   d.str(),
			ActiveState: d.str(),
			SubState:    d.str(),
		}
		d.str()             // followed unit
		unit.Path = d.str() // object path, same encoding as a string
		d.uint32()          // job id
		d.str()             // job type
		d.str()             // job object path

		if unit.Name != "" {
			units = append(units, unit)
		}
	}

	if d.err != nil {
		return nil, fmt.Errorf("could not read the unit list: %w", d.err)
	}

	return units, nil
}

// UnitFileState is what `systemctl is-enabled` reports: enabled, disabled, static, masked, and so on.
//
// Asked per unit rather than through `ListUnitFiles`, which returns every unit file on the machine —
// hundreds of rows, nearly all of them irrelevant, to answer a question about the few dozen that are
// loaded.
func (c *Conn) UnitFileState(name string) (string, error) {
	body, signature, err := c.Call(
		managerDestination, managerPath, managerInterface,
		"GetUnitFileState", "s", name,
	)
	if err != nil {
		return "", err
	}
	if signature != "s" {
		return "", fmt.Errorf("GetUnitFileState answered %q, expected \"s\"", signature)
	}

	d := &decoder{buf: body}
	state := d.str()

	return state, d.err
}

// MemoryUnknown is what systemd answers for a unit with no memory accounting.
//
// `(uint64) -1`, not zero. A unit whose cgroup is not accounted returns the maximum value, and a caller
// that takes it at face value reports sixteen exabytes of memory use — which is the kind of number that
// gets believed for exactly as long as it takes somebody to act on it.
const MemoryUnknown = ^uint64(0)

// unitPropertyUint64 reads one numeric property off a unit, in either of the two widths systemd uses.
//
// `NRestarts` is a `u` and `MemoryCurrent` is a `t`; both arrive inside a variant, so the caller cannot
// know which until the inner signature is read. Returning one width keeps the call sites from caring.
func (c *Conn) unitPropertyUint64(unitPath, iface, name string) (uint64, error) {
	body, signature, err := c.Call(managerDestination, unitPath, propertiesGet, "Get", "ss", iface, name)
	if err != nil {
		return 0, err
	}
	if signature != "v" {
		return 0, fmt.Errorf("Properties.Get answered %q, expected \"v\"", signature)
	}

	d := &decoder{buf: body}

	switch inner := d.signature(); inner {
	case "t":
		value := d.uint64()

		return value, d.err
	case "u":
		value := d.uint32()

		return uint64(value), d.err
	default:
		return 0, fmt.Errorf("%s is %q, expected a number", name, inner)
	}
}

// unitPropertyString reads one string property off a unit.
//
// Separate from the numeric reader because the variant's inner type decides how the body is decoded, and
// a string is not a width of a number. Same shape otherwise: the caller names the interface, because a
// `.service` property asked of a `.timer` is an error rather than an empty answer.
func (c *Conn) unitPropertyString(unitPath, iface, name string) (string, error) {
	body, signature, err := c.Call(managerDestination, unitPath, propertiesGet, "Get", "ss", iface, name)
	if err != nil {
		return "", err
	}
	if signature != "v" {
		return "", fmt.Errorf("Properties.Get answered %q, expected \"v\"", signature)
	}

	return decodeVariantString(body, name)
}

// decodeVariantString pulls a string out of a `Properties.Get` reply body.
//
// Split from the call so the signature check has a test. The check is the whole point of it: a property
// that answers with a different type than expected must be an error, not a value read at the wrong offset
// — that path returns something rather than nothing, which is the failure that gets believed.
func decodeVariantString(body []byte, name string) (string, error) {
	d := &decoder{buf: body}
	if inner := d.signature(); inner != "s" {
		return "", fmt.Errorf("%s is %q, expected a string", name, inner)
	}
	value := d.str()

	return value, d.err
}

// unitPropertyInt32 reads one signed 32-bit property off a unit.
//
// `ExecMainStatus` and `ExecMainCode` are both `i`. On the wire that is the same four bytes as a `u` —
// D-Bus does not distinguish them in layout, only in signature — so the existing uint32 reader does the
// decoding and the conversion happens here, where the signature has been checked.
func (c *Conn) unitPropertyInt32(unitPath, iface, name string) (int32, error) {
	body, signature, err := c.Call(managerDestination, unitPath, propertiesGet, "Get", "ss", iface, name)
	if err != nil {
		return 0, err
	}
	if signature != "v" {
		return 0, fmt.Errorf("Properties.Get answered %q, expected \"v\"", signature)
	}

	return decodeVariantInt32(body, name)
}

// decodeVariantInt32 pulls a signed 32-bit value out of a `Properties.Get` reply body.
//
// The conversion is where this can go wrong quietly. D-Bus lays `i` and `u` out identically, so the bytes
// for -1 and for 4294967295 are the same four bytes: reading the field as unsigned and never converting
// turns "killed by signal 1" into a four-billion exit status, and both are numbers a page will print.
func decodeVariantInt32(body []byte, name string) (int32, error) {
	d := &decoder{buf: body}
	if inner := d.signature(); inner != "i" {
		return 0, fmt.Errorf("%s is %q, expected an int32", name, inner)
	}
	value := int32(d.uint32())

	return value, d.err
}

// Result is systemd's own word for how this unit last finished.
//
// One of a fixed set — "success", "exit-code", "signal", "core-dump", "timeout", "watchdog",
// "start-limit-hit", "oom-kill", "resources", "protocol" — and the single most useful thing that can be
// said about a failure without reading a line of its output. It is why the unit failed, as a fact with the
// same shape every time, which is what makes it filterable, alertable and translatable. A log excerpt is
// none of those, and carries whatever the service happened to print.
//
// `.service` only, like the rest of these.
func (c *Conn) Result(unitPath string) (string, error) {
	return c.unitPropertyString(unitPath, serviceInterface, "Result")
}

// ExecMainStatus is the exit status of the unit's main process, when it exited.
//
// Only meaningful alongside {@link ExecMainCode}: a status of 0 means "exited cleanly" if the code says
// exited, and means nothing at all if the process was killed by a signal — where this field carries the
// signal number instead. Sent as a pair for that reason.
func (c *Conn) ExecMainStatus(unitPath string) (int32, error) {
	return c.unitPropertyInt32(unitPath, serviceInterface, "ExecMainStatus")
}

// ExecMainCode is *how* the main process ended, as a POSIX `si_code`: 1 exited, 2 killed, 3 dumped.
//
// Zero means systemd has no record of a main process having ended — a unit that never started, or one
// still running. The caller treats that as absent rather than as "exited with 0", which is the reading
// that would turn a unit that never ran into one that succeeded.
func (c *Conn) ExecMainCode(unitPath string) (int32, error) {
	return c.unitPropertyInt32(unitPath, serviceInterface, "ExecMainCode")
}

// Restarts is how many times systemd has restarted this unit since it was loaded.
//
// The single most useful thing systemd knows that nothing else here surfaces. A unit in a crash-restart
// loop is `active` every time anyone looks at it — the page says it is running, the alert never fires, and
// the only visible symptom is whatever the restarts are costing. This is the counter that makes it
// visible.
//
// Only `.service` units have it. Asking a timer or a target answers with an unknown-property error, which
// is why the caller filters rather than treating the error as a failure.
func (c *Conn) Restarts(unitPath string) (uint64, error) {
	return c.unitPropertyUint64(unitPath, serviceInterface, "NRestarts")
}

// MemoryCurrent is what this unit's cgroup is currently using, in bytes.
//
// Per *unit* rather than per process, which is the difference that makes it worth having: a service that
// forks twenty workers shows as twenty rows in a process list and as one number here. Returns
// {@link MemoryUnknown} where accounting is off, which the caller must treat as absent.
func (c *Conn) MemoryCurrent(unitPath string) (uint64, error) {
	return c.unitPropertyUint64(unitPath, serviceInterface, "MemoryCurrent")
}

// StateChangedAt is when the unit entered the state it is in now.
//
// systemd's own record, in microseconds since the epoch, and the reason the page can say "failed 2 days
// ago" rather than "failed at some point before we noticed". Our own first-seen time would be wrong in
// the common case: the agent is often installed long after the unit gave up.
//
// Asked only for units that have actually failed. It is one call each, and on a healthy machine there are
// none — which is what keeps a per-unit property read affordable.
func (c *Conn) StateChangedAt(unitPath string) (time.Time, error) {
	body, signature, err := c.Call(
		managerDestination, unitPath, propertiesGet,
		"Get", "ss", unitInterface, "StateChangeTimestamp",
	)
	if err != nil {
		return time.Time{}, err
	}
	if signature != "v" {
		return time.Time{}, fmt.Errorf("Properties.Get answered %q, expected \"v\"", signature)
	}

	d := &decoder{buf: body}
	inner := d.signature()
	if inner != "t" {
		return time.Time{}, fmt.Errorf("StateChangeTimestamp is %q, expected a uint64", inner)
	}
	micros := d.uint64()
	if d.err != nil {
		return time.Time{}, d.err
	}

	// Zero means systemd has no record — a unit that has not changed state since boot under some
	// configurations. Absent rather than 1970, which would put a failure fifty years in the past and read
	// as a corrupt reading rather than a missing one.
	if micros == 0 {
		return time.Time{}, nil
	}

	return time.UnixMicro(int64(micros)).UTC(), nil
}
