package collect

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"runtime"
	"slices"
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

	// A reboot is needed to finish applying updates. Debian and Ubuntu write a flag file and SUSE's libzypp
	// writes another; everywhere else (RHEL, Alma, Rocky, Fedora, Arch…) nothing does, so the evidence is
	// a newer kernel installed beside the running one (#3835). That comparison is held to the cases it
	// cannot get wrong — see newerKernel — because a wrong "reboot required" costs somebody a maintenance
	// window they did not need. False where no rule could tell, which is "nothing says so", not a check.
	RebootRequired bool `json:"reboot_required"`

	// Which evidence said so: `flag_file` (Debian/Ubuntu's /var/run/reboot-required), `zypper` (SUSE's
	// /run/reboot-needed) or `newer_kernel`. Absent when no reboot is required. Sent so the server page
	// can say *why*, which is what decides whether somebody believes it.
	RebootReason string `json:"reboot_reason,omitempty"`

	// The newest installed kernel, when it is what made a reboot required — "running 5.14.0-427, installed
	// 5.14.0-503" is the sentence that makes the claim checkable.
	NewestKernel string `json:"newest_kernel,omitempty"`

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

	// Whether the kernel's clock is synchronised, and its bound on the error in milliseconds (InfraNest
	// #3832), from adjtimex. A drifting clock breaks TOTP, TLS validation and every log timestamp long
	// before it drifts far enough for the server to refuse the agent's pushes. Nil where it cannot be
	// asked (not Linux), which is not "synchronised".
	TimeSynced  *bool  `json:"time_synced,omitempty"`
	TimeErrorMs *int64 `json:"time_error_ms,omitempty"`

	// How the machine patches and reboots itself, when a tool for it is installed (InfraNest #3825). Nil
	// where neither unattended-upgrades nor dnf-automatic is — which is "a person does it", not "off".
	AutoUpdate *AutoUpdate `json:"auto_update,omitempty"`
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

	s.RebootRequired, s.RebootReason, s.NewestKernel = rebootRequired(s.Kernel)

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

	s.TimeSynced, s.TimeErrorMs = readTimeSync()

	s.AutoUpdate = CollectAutoUpdate()

	if f, err := os.Open(proStatusPath); err == nil {
		if pro, ok := parseProStatus(io.LimitReader(f, maxProStatusBytes)); ok {
			s.Pro = &pro
		}
		_ = f.Close()
	}

	return s
}

// The flag files a distribution leaves when an update needs a reboot. The file's existence *is* the
// signal — its contents, where there are any, are a human-readable note.
var rebootFlags = []struct{ path, reason string }{
	{"/var/run/reboot-required", "flag_file"}, // Debian, Ubuntu (update-notifier)
	{"/run/reboot-needed", "zypper"},          // openSUSE, SLES (libzypp, on installing a kernel or glibc)
}

// Where installed kernels keep their modules. /lib is a link to /usr/lib on a merged-/usr system; the
// second entry is for the ones where it is not.
var modulesRoots = []string{"/lib/modules", "/usr/lib/modules"}

// rebootRequired asks the flag files first, then the kernels on disk.
func rebootRequired(running string) (bool, string, string) {
	for _, flag := range rebootFlags {
		if _, err := os.Stat(flag.path); err == nil {
			return true, flag.reason, ""
		}
	}

	if running == "" {
		return false, "", ""
	}

	for _, root := range modulesRoots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}

		var installed []string
		for _, e := range entries {
			// A kernel install runs depmod, which writes modules.dep. A directory without one is what a
			// removed kernel leaves behind when a DKMS module or a stray file kept it from being deleted.
			if e.IsDir() && fileExists(root+"/"+e.Name()+"/modules.dep") {
				installed = append(installed, e.Name())
			}
		}

		// The root that holds the running kernel is the one that describes this machine; another root, or
		// none, is not evidence either way.
		if !slices.Contains(installed, running) {
			continue
		}

		if newest, ok := newerKernel(running, installed); ok {
			return true, "newer_kernel", clip(newest, maxSystemField)
		}

		return false, "", ""
	}

	return false, "", ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// newerKernel reports the newest installed kernel that is a newer build of the running one.
//
// Three rules keep it from claiming a reboot that is not needed:
//
//   - **The running kernel must itself be installed.** In a container, or on a host booted from a kernel
//     the package manager does not manage (a provider-supplied kernel, a custom build), /lib/modules
//     describes some other kernel, and comparing against it is a guess. No directory for the running
//     kernel, no answer.
//   - **Same flavour only.** A `-lowlatency` kernel installed beside a `-generic` one, or a `+debug` beside
//     the normal one, is a choice, not an update. Two versions are the same flavour when they are
//     identical with their digits removed — "6.8.0-45-generic" and "6.8.0-47-generic" are, "…-generic"
//     and "…-lowlatency" are not, nor is an el9 kernel and a fresh "+debug" one.
//   - **Strictly newer**, compared the way rpm and dpkg compare versions: runs of digits as numbers, runs
//     of letters as text, so 5.14.0-503 is newer than 5.14.0-427 and 6.10 newer than 6.9.
//
// Arch removes the running kernel's modules on upgrade, so the first rule makes it abstain there — a
// missed reboot on Arch rather than a false one anywhere.
func newerKernel(running string, installed []string) (string, bool) {
	found := false
	for _, v := range installed {
		if v == running {
			found = true

			break
		}
	}
	if !found {
		return "", false
	}

	shape := kernelShape(running)
	newest := running

	for _, v := range installed {
		if kernelShape(v) == shape && compareVersions(v, newest) > 0 {
			newest = v
		}
	}

	return newest, newest != running
}

// kernelShape is the version with every run of digits replaced by one '#'.
func kernelShape(v string) string {
	var b strings.Builder
	inDigits := false

	for _, r := range v {
		if r >= '0' && r <= '9' {
			if !inDigits {
				b.WriteByte('#')
			}
			inDigits = true

			continue
		}
		inDigits = false
		b.WriteRune(r)
	}

	return b.String()
}

// compareVersions orders two version strings rpmvercmp-style: split into runs of digits and runs of
// letters (anything else separates), digits compared as numbers, letters as text, a digit run newer than a
// letter run, and the longer version newer when one is a prefix of the other. Returns -1, 0 or 1.
func compareVersions(a, b string) int {
	sa, sb := versionSegments(a), versionSegments(b)

	for i := 0; i < len(sa) && i < len(sb); i++ {
		x, y := sa[i], sb[i]
		xNum, yNum := isDigit(x[0]), isDigit(y[0])

		switch {
		case xNum && !yNum:
			return 1
		case !xNum && yNum:
			return -1
		case xNum:
			x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
			if len(x) != len(y) {
				return sign(len(x) - len(y))
			}
		}

		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}

	return sign(len(sa) - len(sb))
}

func versionSegments(v string) []string {
	var out []string

	for i := 0; i < len(v); {
		c := v[i]
		if !isDigit(c) && !isLetter(c) {
			i++

			continue
		}

		j := i
		for j < len(v) && isDigit(v[j]) == isDigit(c) && (isDigit(v[j]) || isLetter(v[j])) {
			j++
		}
		out = append(out, v[i:j])
		i = j
	}

	return out
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	}

	return 0
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

// The kernel's clock states and the status bit that matter here: TIME_ERROR is "not synchronised", and
// STA_UNSYNC is set by the kernel when no synchronisation daemon has disciplined the clock recently.
const (
	timeError = 5
	staUnsync = 0x0040
)

// timeSyncFrom turns adjtimex's answer into "synchronised?" and the kernel's bound on the error, in ms.
// Split from the syscall so it can be tested anywhere.
func timeSyncFrom(state int, status, maxErrorMicros int64) (*bool, *int64) {
	synced := state != timeError && status&staUnsync == 0
	errMs := maxErrorMicros / 1000

	return &synced, &errMs
}
