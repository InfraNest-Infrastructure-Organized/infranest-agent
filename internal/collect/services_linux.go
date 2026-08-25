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
	conn, err := dbus.Dial(busTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	units, err := conn.ListUnits()
	if err != nil {
		return nil, err
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

		if failed && unit.Path != "" {
			// Only for failures, which is what keeps a per-unit property read affordable: this is the
			// number the page turns into "failed 2 days ago", and on a healthy machine it is never asked.
			if at, err := conn.StateChangedAt(unit.Path); err == nil && !at.IsZero() {
				service.StateChangedAt = &at
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

	return services, nil
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
