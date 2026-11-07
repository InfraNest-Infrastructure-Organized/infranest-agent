package collect

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

/*
Which OOM kills were the machine running out, and which were a cgroup hitting its own limit (InfraNest #5979).

/proc/vmstat's `oom_kill` counts every kill: the whole machine out of memory, and a service that reached the
MemoryMax= somebody gave it. Those are different sentences — "the server ran out of memory" and "this service
hit the limit it was given" — and the first was being said of the second, on a machine with 2 GB free, because
the service that hit its limit was this agent.

cgroup v2's memory.events tells them apart, and is world-readable:

  - `oom` rises when a cgroup's usage reached its limit and the OOM killer was invoked for it. A machine-wide
    OOM raises it nowhere: only the charge path of a limited cgroup does.
  - `oom_kill` counts processes in the cgroup killed by *any* OOM, the machine-wide kind included.

Both are hierarchical (since 5.2; `memory.events.local` arrived with that change and is how its absence is
recognised): an event in a child is added to every ancestor at the moment it happens, and stays there when the
child is removed. So the sum of `oom` over the top-level cgroups is every limit OOM on the machine, and a
service whose cgroup is recreated on every restart does not take its history with it. The root cgroup has no
memory.events, and needs none: a limit can only be set below it.

`oom` counts limit OOMs, not kills. Nearly always one is one; it can run ahead when a second allocation reaches
the limit while the first victim is still exiting. So the sum is clamped to `oom_kill`, and a receiver
comparing two readings clamps the rise the same way.
*/

// The service's slice (packaging/infranest-agent.service): the agent's own cgroup is removed and recreated on
// every restart, and its counters with it, so the kill that caused a Restart= is gone by the time the restarted
// agent could read it. A slice stays for the boot, and its hierarchical `oom_kill` is the agent's kills alone.
const agentSlice = "system-infranest.slice"

// Where the three answers are read from. A value so the tests can point it at a directory they built.
type oomPaths struct {
	cgroupRoot string // the cgroup v2 hierarchy
	mountinfo  string // how it is mounted
	selfCgroup string // where this process sits in it
}

var hostOOMPaths = oomPaths{
	cgroupRoot: "/sys/fs/cgroup",
	mountinfo:  "/proc/self/mountinfo",
	selfCgroup: "/proc/self/cgroup",
}

// attribute answers `oom_kills_limit` and `oom_kills_agent` beside a total of `total`. Either is nil when it
// cannot be told, which is never zero: zero is the claim that none of the kills were of that kind.
func (p oomPaths) attribute(total uint64) (limit, agent *uint64) {
	if !p.hierarchicalCgroup2() {
		return nil, nil
	}

	self, ok := p.ownCgroup()
	// "/" is not a place systemd puts a service: it is a cgroup namespace, which shows this process a subtree as
	// though it were the whole machine. The top-level cgroups it can see are then not the machine's.
	if !ok || self == "/" {
		return nil, nil
	}

	if n, ok := p.limitOOMs(); ok {
		n = min(n, total)
		limit = &n
	}

	if n, ok := p.agentOOMKills(self); ok {
		agent = &n
	}

	return limit, agent
}

// hierarchicalCgroup2 reports whether cgroupRoot is a cgroup v2 hierarchy whose memory.events count their
// descendants. A cgroup v1 or hybrid machine has no such file at the root of /sys/fs/cgroup, and one mounted
// with `memory_localevents` keeps each cgroup's events to itself — the sum of the top level would then miss
// every service's.
func (p oomPaths) hierarchicalCgroup2() bool {
	f, err := os.Open(p.mountinfo)
	if err != nil {
		return false
	}
	defer f.Close()

	return parseCgroup2Mount(io.LimitReader(f, 4<<20), p.cgroupRoot)
}

// parseCgroup2Mount reads /proc/self/mountinfo for the mount at `point`. The last entry wins: a sandbox's
// read-only bind (ProtectControlGroups=) is stacked on top of the host's, and it is the one this process sees.
//
//	36 25 0:30 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime shared:9 - cgroup2 cgroup2 rw,nsdelegate
func parseCgroup2Mount(r io.Reader, point string) bool {
	found, ok := false, false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), 64<<10)

	for scanner.Scan() {
		before, after, sep := strings.Cut(scanner.Text(), " - ")
		fields := strings.Fields(before)
		if !sep || len(fields) < 5 || fields[4] != point {
			continue
		}

		tail := strings.Fields(after)
		found = true
		ok = len(tail) >= 1 && tail[0] == "cgroup2"
		if ok && len(tail) >= 3 {
			for _, opt := range strings.Split(tail[2], ",") {
				if opt == "memory_localevents" {
					ok = false
				}
			}
		}
	}

	return found && ok
}

// ownCgroup is this process's path in the v2 hierarchy, from its "0::" line.
func (p oomPaths) ownCgroup() (string, bool) {
	body, err := os.ReadFile(p.selfCgroup)
	if err != nil {
		return "", false
	}

	for _, line := range strings.Split(string(body), "\n") {
		if path, found := strings.CutPrefix(line, "0::"); found && strings.HasPrefix(path, "/") {
			return path, true
		}
	}

	return "", false
}

// limitOOMs sums `oom` over the top-level cgroups.
//
// A top-level cgroup with no memory.events has no memory controller, so nothing beneath it can have a memory
// limit to hit: it adds nothing, truthfully. One whose file is there and cannot be read or parsed makes the whole
// answer unknown — a partial sum would undercount, and an undercount reads as "the machine ran out". And with no
// memory.events.local anywhere the kernel predates hierarchical events, so the top level says nothing about the
// services under it.
//
// The sum falls if a top-level cgroup that saw limit OOMs is removed (a transient machine.slice, a container
// runtime's own tree). Within a boot a receiver treats a fall as a new baseline, never as a rise.
func (p oomPaths) limitOOMs() (uint64, bool) {
	entries, err := os.ReadDir(p.cgroupRoot)
	if err != nil {
		return 0, false
	}

	var sum uint64
	seen, hierarchical := false, false

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		dir := filepath.Join(p.cgroupRoot, e.Name())
		n, err := readMemoryEvent(filepath.Join(dir, "memory.events"), "oom")
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, false
		}

		seen = true
		sum += n
		if _, err := os.Stat(filepath.Join(dir, "memory.events.local")); err == nil {
			hierarchical = true
		}
	}

	return sum, seen && hierarchical
}

// agentOOMKills is `oom_kill` of the agent's slice: how many times a process of this unit was killed for memory
// since the slice was started, which is the boot. Only when this process runs in that slice — anywhere else
// (an older unit in system.slice, `infranest-agent print` from a shell, OpenRC) the parent holds other things,
// and the agent's own cgroup forgets its kills on each restart, so there is nothing honest to say.
func (p oomPaths) agentOOMKills(self string) (uint64, bool) {
	parent := path.Dir(self)
	if !strings.HasSuffix(self, ".service") || path.Base(parent) != agentSlice {
		return 0, false
	}

	n, err := readMemoryEvent(filepath.Join(p.cgroupRoot, parent, "memory.events"), "oom_kill")
	if err != nil {
		return 0, false
	}

	return n, true
}

// readMemoryEvent reads one counter from a memory.events file. A file without the key is an error, not zero.
func readMemoryEvent(file, key string) (uint64, error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n, ok := parseMemoryEvent(io.LimitReader(f, 64<<10), key)
	if !ok {
		return 0, errors.New("memory.events: no " + key)
	}

	return n, nil
}

// parseMemoryEvent pulls `key` out of a memory.events body — `low 0\nhigh 12\nmax 3\noom 1\noom_kill 1\n…`.
func parseMemoryEvent(r io.Reader, key string) (uint64, bool) {
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		name, value, found := strings.Cut(scanner.Text(), " ")
		if !found || name != key {
			continue
		}

		n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)

		return n, err == nil
	}

	return 0, false
}
