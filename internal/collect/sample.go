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
	s.Failed[collector] = err.Error()
}

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }
