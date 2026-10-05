package collect

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
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

	// How many processes the kernel has killed for lack of memory since this boot (#2810), from
	// /proc/vmstat's `oom_kill`. A counter rather than a level, and that is the whole point: memory_percent
	// cannot see an OOM kill, because the kill is what frees the memory — the next reading looks healthy.
	// The counter is the one place the event survives until somebody asks. Nil where the kernel does not
	// keep it (before 4.13, or no /proc), which is not zero: zero is a claim that nothing was killed.
	OOMKills *uint64 `json:"oom_kills,omitempty"`

	// The kernel's random identifier for this boot. Sent beside OOMKills so the receiver can tell a
	// counter that reset because the machine rebooted from one that did not move — comparing the numbers
	// alone cannot, since a reboot followed by as many kills as before reads as "no change".
	BootID string `json:"boot_id,omitempty"`

	// How many CPUs this machine can schedule on (#2886). The provider's plan says what was bought; this
	// says what the kernel was given, and after a resize the two disagree until the provider's next sync —
	// or for ever, on a server no provider describes. Go's own count, which follows CPU affinity and not a
	// cgroup quota: the right answer for an agent running on the host, an overstatement inside a container
	// that has been given a slice of it.
	CPUs int `json:"cpus,omitempty"`

	// The Ubuntu Pro client's own account of this machine (#2811). Nil where there is no Pro client, or its
	// cache could not be read — which is not "detached": a Debian box and an unreadable file both say
	// nothing, and only a cache that says `attached: false` is a claim worth alerting on.
	Pro *ProStatus `json:"pro,omitempty"`
}

// ProStatus is the part of `pro status` that decides whether a machine is still being patched.
//
// When a subscription lapses or a machine is detached, Livepatch stops applying kernel fixes and the ESM
// archives stop serving security updates — and nothing on the machine says so. Everything else in the
// client's cache (the account name, the contract id, the machine id) is left where it is: none of it is
// needed to answer the question, and all of it identifies somebody.
type ProStatus struct {
	Attached bool `json:"attached"`

	// The client's own word for each service's state — `enabled`, `disabled`, `warning`, `n/a` — sent
	// verbatim, the same rule as a systemd unit's state. Absent when the cache does not list the service,
	// which an unattached machine's cache does not.
	Livepatch string `json:"livepatch,omitempty"`
	ESMInfra  string `json:"esm_infra,omitempty"`

	// When the subscription ends, RFC3339 in UTC. Sent because the cache is only as fresh as the client's
	// last refresh: a contract that lapsed since then still reads `attached: true`, and the expiry date is
	// what lets the receiver see through that. Absent on an unattached machine.
	Expires string `json:"expires,omitempty"`
}

// CollectSystem reads what the machine says about itself. Every field is independent: one unreadable file
// costs that field and nothing else, the same rule the metric collectors follow.
func CollectSystem() System {
	var s System

	if v, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		s.Kernel = clip(strings.TrimSpace(string(v)), maxSystemField)
	}

	if f, err := os.Open("/etc/os-release"); err == nil {
		s.OS = clip(parseOSRelease(f), maxSystemField)
		_ = f.Close()
	}

	// Debian and Ubuntu. The file's existence *is* the signal — its contents are a human-readable note.
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		s.RebootRequired = true
	}

	if f, err := os.Open("/proc/vmstat"); err == nil {
		if n, ok := parseOOMKills(f); ok {
			s.OOMKills = &n
		}
		_ = f.Close()
	}

	if v, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		s.BootID = clip(strings.TrimSpace(string(v)), maxBootID)
	}

	if pending, security, ok := readUpdateStamp(); ok {
		s.PendingUpdates = &pending
		s.SecurityUpdates = &security
	}

	s.CPUs = runtime.NumCPU()

	if f, err := os.Open(proStatusPath); err == nil {
		if pro, ok := parseProStatus(io.LimitReader(f, maxProStatusBytes)); ok {
			s.Pro = &pro
		}
		_ = f.Close()
	}

	return s
}

// The Ubuntu Pro client's status cache. World-readable by design — it is what lets `pro status` answer an
// unprivileged user without asking Canonical — so reading it needs nothing the agent does not already have,
// and is the only way to ask, since running `pro status` is a subprocess.
const proStatusPath = "/var/lib/ubuntu-advantage/status.json"

// A real cache is a few tens of kilobytes. The cap is what keeps a corrupted or replaced file from being
// read into memory whole, under a unit that is allowed 64 MB.
const maxProStatusBytes = 1 << 20

// parseProStatus pulls the attached flag, two service states and the expiry out of the client's cache.
//
// Split from the file read so it can be tested against captured caches on any machine. A cache without an
// `attached` key is not one this parser understands, and yields nothing rather than a guessed false — a
// false is the claim that raises an alert.
func parseProStatus(r io.Reader) (ProStatus, bool) {
	var raw struct {
		Attached *bool           `json:"attached"`
		Expires  json.RawMessage `json:"expires"`
		Services []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"services"`
	}

	if err := json.NewDecoder(r).Decode(&raw); err != nil || raw.Attached == nil {
		return ProStatus{}, false
	}

	pro := ProStatus{Attached: *raw.Attached}

	for _, svc := range raw.Services {
		switch svc.Name {
		case "livepatch":
			pro.Livepatch = clip(svc.Status, maxState)
		case "esm-infra":
			pro.ESMInfra = clip(svc.Status, maxState)
		}
	}

	// The client writes `null` or a non-date placeholder when there is no contract; only a real timestamp
	// is passed on, and only for an attached machine, where it means something.
	var expires string
	if pro.Attached && json.Unmarshal(raw.Expires, &expires) == nil {
		if t, err := time.Parse(time.RFC3339, expires); err == nil {
			pro.Expires = t.UTC().Format(time.RFC3339)
		}
	}

	return pro, true
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

// parseOOMKills pulls the `oom_kill` counter out of /proc/vmstat.
//
// The file is `name value` per line, a hundred-odd of them. Only the one line matters; the rest are the
// kernel's own bookkeeping. Absent on kernels older than 4.13, where the answer is "cannot tell" rather
// than zero.
func parseOOMKills(r io.Reader) (uint64, bool) {
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		name, value, found := strings.Cut(scanner.Text(), " ")
		if !found || name != "oom_kill" {
			continue
		}

		n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, false
		}

		return n, true
	}

	return 0, false
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
