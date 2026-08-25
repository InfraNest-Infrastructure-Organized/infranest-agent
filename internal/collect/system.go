package collect

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"
)

// System is what the server page's System card shows: the machine's own description of itself.
//
// All of it read from files. There is deliberately no `uname -r`, no `apt list --upgradable`, no
// `needs-restarting` — the agent executes nothing, and CI proves it by inspecting the dependency graph.
// That constraint is what shapes this file: everything here is a read of something the distribution
// already wrote down.
type System struct {
	// "6.8.0-45-generic". The kernel currently running, which is not necessarily the newest installed —
	// see RebootRequired, which is the whole reason that distinction matters.
	Kernel string `json:"kernel,omitempty"`

	// The distribution's own name for itself, from /etc/os-release.
	OS string `json:"os,omitempty"`

	// Packages with an update waiting, and how many of those are security updates. Null rather than zero
	// when we could not tell: zero is a claim that everything is current, and it is the claim somebody
	// would act on by not patching.
	PendingUpdates  *int `json:"pending_updates,omitempty"`
	SecurityUpdates *int `json:"security_updates,omitempty"`

	// The distribution says a reboot is needed. Debian and Ubuntu write a flag file; on distributions
	// that do not, this stays false rather than guessing from kernel versions — a wrong "reboot required"
	// costs somebody a maintenance window they did not need.
	RebootRequired bool `json:"reboot_required"`
}

// CollectSystem reads what the machine says about itself. Every field is independent: one unreadable file
// costs that field and nothing else, the same rule the metric collectors follow.
func CollectSystem() System {
	var s System

	if v, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		s.Kernel = strings.TrimSpace(string(v))
	}

	if f, err := os.Open("/etc/os-release"); err == nil {
		s.OS = parseOSRelease(f)
		_ = f.Close()
	}

	// Debian and Ubuntu. The file's existence *is* the signal — its contents are a human-readable note.
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		s.RebootRequired = true
	}

	if pending, security, ok := readUpdateStamp(); ok {
		s.PendingUpdates = &pending
		s.SecurityUpdates = &security
	}

	return s
}

// parseOSRelease pulls PRETTY_NAME out of /etc/os-release.
//
// The file is shell-ish `KEY="value"` and every distribution writes it. Quotes are stripped because they
// are part of the format rather than the name — "Ubuntu 24.04.1 LTS" should not arrive with them attached.
func parseOSRelease(r io.Reader) string {
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), "=")
		if !found || strings.TrimSpace(key) != "PRETTY_NAME" {
			continue
		}

		return strings.Trim(strings.TrimSpace(value), `"'`)
	}

	return ""
}

// readUpdateStamp reads the counts `update-notifier` leaves behind.
//
// Debian and Ubuntu run their own periodic check and write the answer to
// `/var/lib/update-notifier/updates-available`, in the form:
//
//	3 updates can be applied immediately.
//	1 of these updates is a standard security update.
//
// Reading a file the distribution already maintains is the only option that respects "executes nothing" —
// the alternative is `apt list --upgradable`, which is a subprocess and also several seconds of work that
// hits the package database on somebody's production server.
//
// The honest cost, recorded because it will come up: on a machine where update-notifier is absent or its
// timer is disabled, this returns nothing at all. Nothing is the right answer there — better an absent
// figure than a stale or invented one, since "0 updates" is what somebody would act on by not patching.
func readUpdateStamp() (pending, security int, ok bool) {
	body, err := os.ReadFile("/var/lib/update-notifier/updates-available")
	if err != nil {
		return 0, 0, false
	}

	return parseUpdateStamp(string(body))
}

// parseUpdateStamp is split from the file read so it can be tested against captured output on any
// machine — the same reason the /proc parsers take an io.Reader.
func parseUpdateStamp(body string) (pending, security int, ok bool) {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		n, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}

		lower := strings.ToLower(line)

		switch {
		case strings.Contains(lower, "security"):
			security, ok = n, true
		case strings.Contains(lower, "update"):
			pending, ok = n, true
		}
	}

	return pending, security, ok
}
