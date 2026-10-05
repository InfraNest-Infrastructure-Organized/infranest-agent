//go:build linux

package collect

import (
	"sort"
	"strings"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/dbus"
)

// maxServices bounds what one collection reports, matching the server's own cap.
//
// The watch set below is a few dozen units on an ordinary machine. The cap is here so a box with an
// unusual number of enabled units sends a truncated list rather than one the server refuses whole —
// losing the tail is better than losing the failure at the front, which is why failures are sorted first.
const maxServices = 200

// busTimeout bounds the whole exchange. The collection cycle is on a schedule and a bus that accepts the
// connection and then says nothing must not hold it open.
const busTimeout = 5 * time.Second

/*
CollectServices asks systemd what it was told to run, and what became of it.

**The watch set is "enabled, plus anything that has failed".** Enabled is the machine's own statement
about what should be running, which is what makes this work with no configuration — the bar the feature
was written to. The union with failed units matters because a static unit is not "enabled" and can still
be the thing that broke; excluding it would mean a failure the operator can see in `systemctl` and we
report as a healthy machine.

Templates and generated units are excluded. `getty@.service` is a template with no state of its own, and
`.scope` and `.mount` units are the kernel's bookkeeping rather than anything anybody chose to run.
*/
func CollectServices() ([]Service, error) {
	services, _, err := CollectUnits(nil)

	return services, err
}

// CollectUnits is CollectServices plus the running containers (#2809), from the same unit list — one bus
// connection and one ListUnits for both. Containers are reported only when a tracker is passed, because
// without one there is nothing to count restarts against.
func CollectUnits(tracker *ContainerTracker) ([]Service, []Container, error) {
	conn, err := dbus.Dial(busTimeout)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = conn.Close() }()

	units, err := conn.ListUnits()
	if err != nil {
		return nil, nil, err
	}

	var containers []Container
	if tracker != nil {
		containers = collectContainers(conn, units, tracker, time.Now())
	}

	services := make([]Service, 0, 64)
	for _, unit := range units {
		if !watchable(unit) {
			continue
		}

		failed := unit.ActiveState == "failed"

		if !failed {
			// One call per candidate, and only for units that have not failed — the failed ones are in
			// regardless, so asking whether they are enabled would be a call whose answer changes nothing.
			state, err := conn.UnitFileState(unit.Name)
			// An error here is ordinary: a unit with no unit file on disk answers with one. Treated as
			// "not enabled" rather than fatal — one unreadable unit must not cost the whole list.
			if err != nil || !isEnabled(state) {
				continue
			}
		}

		// Clipped to what the ingest accepts. A unit description is free text from a unit file and has
		// no length systemd enforces, so this is the field most likely to exceed it — and an over-long
		// one would fail validation for the whole push rather than for itself. See limits.go.
		service := Service{
			Unit:        clip(unit.Name, maxUnit),
			Description: clip(unit.Description, maxDescription),
			ActiveState: clip(unit.ActiveState, maxState),
			SubState:    clip(unit.SubState, maxState),
		}

		if unit.Path != "" {
			// For every unit, not only the failures. It used to be failures alone, on the grounds that a
			// per-unit property read is the expensive part — but that made "what changed recently" a
			// question the payload could not answer, because every healthy unit arrived with no timestamp
			// at all. A list of seventy units nobody can sort by *when they last did something* is
			// inventory; this is what makes it a diagnosis.
			if at, err := conn.StateChangedAt(unit.Path); err == nil && !at.IsZero() {
				service.StateChangedAt = &at
			}

			// `.service` only. A timer, target or mount has neither property, and asking answers with an
			// unknown-property error — so this filters rather than treating a normal answer as a fault.
			if strings.HasSuffix(unit.Name, ".service") {
				if n, err := conn.Restarts(unit.Path); err == nil {
					service.Restarts = &n
				}

				// `MemoryUnknown` is `(uint64) -1` — what systemd answers when the unit has no memory
				// accounting. Dropped rather than stored: believed, it is sixteen exabytes.
				if b, err := conn.MemoryCurrent(unit.Path); err == nil && b != dbus.MemoryUnknown {
					service.MemoryBytes = &b
				}

				// Why it failed (#887) — asked only of units that actually have, which is what keeps
				// three more property reads affordable: on a healthy machine there are none, and on an
				// unhealthy one there are a handful. Healthy units would answer "success" for all three,
				// which is a row of noise on every unit in the list to say nothing happened.
				if failed {
					if r, err := conn.Result(unit.Path); err == nil && r != "" && r != "success" {
						service.Result = clip(r, maxState)
					}

					// Read as a pair and kept only as a pair. `ExecMainCode` is zero when systemd has no
					// record of a main process ending — a unit that failed before it ever started, most
					// often — and a status of 0 beside that reads as "exited cleanly", which is the
					// opposite of what happened. Neither field is worth having without the other.
					code, codeErr := conn.ExecMainCode(unit.Path)
					status, statusErr := conn.ExecMainStatus(unit.Path)
					if codeErr == nil && statusErr == nil && code != 0 {
						service.ExecMainCode = &code
						service.ExecMainStatus = &status
					}
				}
			}
		}

		services = append(services, service)
	}

	// Failures first, then by name. If the cap below has to cut anything, it must not be the failure —
	// and a stable order means two consecutive pushes can be compared without the diff being noise.
	sort.SliceStable(services, func(i, j int) bool {
		a, b := services[i], services[j]
		if (a.ActiveState == "failed") != (b.ActiveState == "failed") {
			return a.ActiveState == "failed"
		}

		return a.Unit < b.Unit
	})

	if len(services) > maxServices {
		services = services[:maxServices]
	}

	return services, containers, nil
}

// collectContainers reads the container scopes out of a unit list and counts their restarts.
//
// Never nil once asked: an empty list is the answer "this host runs no containers systemd can see" —
// which includes a host on Docker's `cgroupfs` driver, where containers have no scope at all. The receiver
// cannot tell those apart, and the documentation says so rather than this code guessing.
func collectContainers(conn *dbus.Conn, units []dbus.Unit, tracker *ContainerTracker, now time.Time) []Container {
	containers := make([]Container, 0, 8)

	for _, unit := range units {
		id, runtime, ok := containerScope(unit.Name)
		if !ok || unit.Path == "" {
			continue
		}

		c := Container{ID: id, Runtime: runtime, ActiveState: clip(unit.ActiveState, maxState)}

		if at, err := conn.ActiveEnteredAt(unit.Path); err == nil && !at.IsZero() {
			c.StartedAt = &at
			n := tracker.observe(id, at, now)
			c.Restarts = &n
		}

		if b, err := conn.ScopeMemoryCurrent(unit.Path); err == nil && b != dbus.MemoryUnknown {
			c.MemoryBytes = &b
		}

		containers = append(containers, c)
	}

	tracker.forget(now)

	// Most restarts first, so a cap never cuts the container in the loop; then by ID for a stable order.
	sort.SliceStable(containers, func(i, j int) bool {
		a, b := containers[i], containers[j]
		ra, rb := uint64(0), uint64(0)
		if a.Restarts != nil {
			ra = *a.Restarts
		}
		if b.Restarts != nil {
			rb = *b.Restarts
		}
		if ra != rb {
			return ra > rb
		}

		return a.ID < b.ID
	})

	if len(containers) > maxContainers {
		containers = containers[:maxContainers]
	}

	return containers
}

// watchable excludes the units that are bookkeeping rather than something somebody chose to run.
func watchable(unit dbus.Unit) bool {
	switch {
	case unit.Name == "":
		return false
	// A template has no state of its own; its instances do, and they appear separately.
	case strings.Contains(unit.Name, "@."):
		return false
	// `not-found` is a unit somebody referenced and systemd could not load. Reporting it as a service is
	// how a typo in another unit's `Requires=` becomes an alert about a service that does not exist.
	case unit.LoadState == "not-found":
		return false
	}

	// Services and timers are things that were installed to do a job. Mounts, scopes, slices, devices,
	// targets and sockets are the machine's own structure — hundreds of rows describing plumbing.
	return strings.HasSuffix(unit.Name, ".service") || strings.HasSuffix(unit.Name, ".timer")
}

// isEnabled covers the states that mean "this was set up to run".
//
// `static` is deliberately absent: a static unit has no install section and is pulled in by something
// else, so treating it as enabled would sweep in most of the distribution. It still appears here when it
// has failed, via the union above.
func isEnabled(state string) bool {
	switch strings.TrimSpace(state) {
	case "enabled", "enabled-runtime", "generated", "indirect":
		return true
	default:
		return false
	}
}
