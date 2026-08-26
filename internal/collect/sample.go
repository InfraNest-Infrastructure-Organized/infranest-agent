// Package collect reads a handful of numbers from the machine the agent runs on.
//
// Nothing here executes anything or opens a socket: every collector reads a file under /proc, calls
// statfs, or gives up. That is the whole of what the agent is permitted to do locally.
package collect

import "time"

// Sample is one reading of the machine, and is exactly what gets posted.
//
// Every field is a pointer or a slice so that "not measured" and "measured as zero" stay different
// claims all the way to the wire. The server's window skips nulls rather than averaging them, so a
// collector that failed abstains instead of voting zero — a memory reading of 0% from a broken collector
// would otherwise look like a machine with nothing running on it.
type Sample struct {
	CollectedAt time.Time `json:"collected_at"`

	CPUPercent    *float64 `json:"cpu_percent,omitempty"`
	MemoryPercent *float64 `json:"memory_percent,omitempty"`
	MemoryUsed    *int64   `json:"memory_used_bytes,omitempty"`
	MemoryTotal   *int64   `json:"memory_total_bytes,omitempty"`
	SwapUsed      *int64   `json:"swap_used_bytes,omitempty"`
	Load1         *float64 `json:"load_1,omitempty"`
	Load5         *float64 `json:"load_5,omitempty"`
	Load15        *float64 `json:"load_15,omitempty"`
	Uptime        *int64   `json:"uptime_seconds,omitempty"`

	Mounts    []Mount   `json:"mounts,omitempty"`
	Processes []Process `json:"processes,omitempty"`

	// The watched service units (#774). Absent when the collector could not ask — which the server reads
	// as "no answer" and not as "nothing has failed", because those need opposite things said about them.
	Services []Service `json:"services,omitempty"`

	// What the machine says about itself: kernel, OS, pending updates, reboot required (#767). Changes
	// rarely, so it rides on every sample rather than having a cadence of its own — it is four short
	// strings and two integers, which is cheaper than the bookkeeping a schedule would need.
	System *System `json:"system,omitempty"`

	AgentVersion string `json:"agent_version,omitempty"`

	// Collectors that failed, by name, with the reason. Reported rather than hidden: a collector that
	// stopped working is something the server should be able to show, and silently sending fewer fields
	// looks identical to a machine that simply has less to say.
	Failed map[string]string `json:"failed,omitempty"`
}

// Mount is one filesystem. UsedPercent is deliberately absent — the server computes it from the bytes,
// because a percentage that disagrees with its own bytes is what an alert would then fail to fire on.
type Mount struct {
	MountPoint string `json:"mount_point"`
	Device     string `json:"device,omitempty"`
	UsedBytes  int64  `json:"used_bytes"`
	TotalBytes int64  `json:"total_bytes"`
}

// Service is one unit the machine was told to run, and what became of it.
//
// systemd's own vocabulary, sent verbatim rather than mapped to ours: ActiveState is one of
// active/inactive/failed/activating/deactivating, and SubState is the per-unit-type detail. Deciding what
// "exited" means for a oneshot unit is a judgement the page can make with the whole picture, and the
// collector cannot.
type Service struct {
	Unit        string `json:"unit"`
	Description string `json:"description,omitempty"`
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state,omitempty"`
	// systemd's record of when the unit entered its current state. Only fetched for units that have
	// failed — it is one call each, and on a healthy machine there are none.
	StateChangedAt *time.Time `json:"state_changed_at,omitempty"`

	// How many times systemd has restarted this unit since it was loaded (#774).
	//
	// The one thing systemd knows that nothing else here surfaces. A unit in a crash-restart loop reads as
	// `active` every time anyone looks: the page says it is running, no alert fires, and the only symptom
	// is whatever the restarts are costing. Absent for units that cannot have it — a timer is not a
	// service — rather than zero, because "never restarted" and "cannot restart" are different facts.
	Restarts *uint64 `json:"restarts,omitempty"`

	// What this unit's cgroup is using, in bytes.
	//
	// Per *unit*, which is what makes it worth carrying next to the process list: a service that forks
	// twenty workers is twenty rows there and one number here. Absent where systemd has no accounting for
	// the unit — see dbus.MemoryUnknown, which is `(uint64) -1` and reads as sixteen exabytes if believed.
	MemoryBytes *uint64 `json:"memory_bytes,omitempty"`
}

// Process is one of the busiest few. Command carries no arguments unless the operator turned them on:
// command lines routinely contain credentials, and the default must not be to ship them to us.
type Process struct {
	Command     string     `json:"command"`
	CPUPercent  *float64   `json:"cpu_percent,omitempty"`
	MemoryBytes *int64     `json:"memory_bytes,omitempty"`
	PID         int        `json:"pid,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
}

// fail records a collector that did not work, without disturbing the ones that did.
func (s *Sample) fail(collector string, err error) {
	if s.Failed == nil {
		s.Failed = map[string]string{}
	}
	// An error string is whatever the failing call produced — a D-Bus refusal quotes the bus, and a file
	// error carries a path. Neither has a bound, and an over-long one would fail validation for the whole
	// push: the collector-failure field would take the reading down with it, which is precisely backwards.
	s.Failed[collector] = clip(err.Error(), maxFailReason)
}

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }
