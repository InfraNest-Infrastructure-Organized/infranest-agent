package collect

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

/*
Which OOM kills were a cgroup's own limit, and which were this agent (InfraNest #5979).

prod-app emailed "ran out of memory" with 2 GB free: the kill was this agent reaching its own MemoryMax=. What is
worth testing is that each answer is either the kernel's own count or nothing — never a zero made up because
a file was missing, because zero is the claim that sends somebody looking for a memory leak in the machine.
*/

const cgroup2Mountinfo = `25 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
36 25 0:30 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:9 - cgroup2 cgroup2 rw,nsdelegate,memory_recursiveprot
`

const agentCgroup = "0::/system.slice/system-infranest.slice/infranest-agent.service\n"

func events(oom, kills int) string {
	return "low 0\nhigh 0\nmax 41\noom " + strconv.Itoa(oom) + "\noom_kill " + strconv.Itoa(kills) + "\noom_group_kill 0\n"
}

// machine builds a cgroup tree, mountinfo and /proc/self/cgroup in a temporary directory. `files` maps a path
// under the cgroup root to its contents.
func machine(t *testing.T, mountinfo, self string, files map[string]string) oomPaths {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "cgroup")

	write := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range files {
		write(filepath.Join(root, path), body)
	}
	write(filepath.Join(dir, "mountinfo"), strings.ReplaceAll(mountinfo, "/sys/fs/cgroup", root))
	write(filepath.Join(dir, "self"), self)

	return oomPaths{cgroupRoot: root, mountinfo: filepath.Join(dir, "mountinfo"), selfCgroup: filepath.Join(dir, "self")}
}

// The prod-app morning: one kill, and it was the agent's slice reaching the service's limit.
func prodApp() map[string]string {
	return map[string]string{
		"system.slice/memory.events":                                                events(1, 1),
		"system.slice/memory.events.local":                                          events(0, 0),
		"system.slice/system-infranest.slice/memory.events":                         events(1, 1),
		"system.slice/system-infranest.slice/memory.events.local":                   events(0, 0),
		"user.slice/memory.events":                                                  events(0, 0),
		"user.slice/memory.events.local":                                            events(0, 0),
		"init.scope/memory.events":                                                  events(0, 0),
		"cgroup.controllers":                                                        "cpu io memory pids\n",
		"system.slice/system-infranest.slice/infranest-agent.service/memory.events": events(0, 0),
	}
}

func TestTheAgentHittingItsOwnLimitIsALimitKillAndTheAgents(t *testing.T) {
	limit, agent := machine(t, cgroup2Mountinfo, agentCgroup, prodApp()).attribute(1)

	if limit == nil || *limit != 1 {
		t.Fatalf("limit = %v, want 1", limit)
	}
	if agent == nil || *agent != 1 {
		t.Fatalf("agent = %v, want 1: the slice remembers the kill its recreated service cgroup forgot", agent)
	}
}

func TestLimitOOMsAreSummedOverTheTopLevel(t *testing.T) {
	files := prodApp()
	files["user.slice/memory.events"] = events(2, 2) // somebody's build in a user session, under its own limit

	limit, _ := machine(t, cgroup2Mountinfo, agentCgroup, files).attribute(5)

	if limit == nil || *limit != 3 {
		t.Fatalf("limit = %v, want 3", limit)
	}
}

func TestAMachineWideKillLeavesTheLimitCountAlone(t *testing.T) {
	// Four kills since boot, one of them a limit: the other three were the machine, and are left for the
	// receiver to call that.
	limit, _ := machine(t, cgroup2Mountinfo, agentCgroup, prodApp()).attribute(4)

	if limit == nil || *limit != 1 {
		t.Fatalf("limit = %v, want 1", limit)
	}
}

func TestLimitOOMsNeverExceedTheKills(t *testing.T) {
	// `oom` counts the limit being reached, and can run ahead of the kills while a victim is still exiting.
	files := prodApp()
	files["system.slice/memory.events"] = events(3, 1)

	limit, _ := machine(t, cgroup2Mountinfo, agentCgroup, files).attribute(1)

	if limit == nil || *limit != 1 {
		t.Fatalf("limit = %v, want it clamped to the 1 kill", limit)
	}
}

func TestAnAgentOutsideItsSliceSaysNothingAboutItself(t *testing.T) {
	// A unit written before the slice: system.slice holds every other service's kills, and the agent's own
	// cgroup started at zero on its last restart. Either would be a lie.
	limit, agent := machine(t, cgroup2Mountinfo, "0::/system.slice/infranest-agent.service\n", prodApp()).attribute(1)

	if agent != nil {
		t.Fatalf("agent = %d outside its slice", *agent)
	}
	if limit == nil {
		t.Fatal("the machine-wide answer does not depend on where the agent runs")
	}
}

func TestPrintFromAShellSaysNothingAboutTheAgent(t *testing.T) {
	_, agent := machine(t, cgroup2Mountinfo, "0::/user.slice/user-1000.slice/session-3.scope\n", prodApp()).attribute(1)

	if agent != nil {
		t.Fatalf("agent = %d from a login session", *agent)
	}
}

func TestCgroupV1SaysNothing(t *testing.T) {
	const v1 = `25 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
30 25 0:26 / /sys/fs/cgroup ro,nosuid,nodev,noexec shared:8 - tmpfs tmpfs ro,mode=755
40 30 0:35 / /sys/fs/cgroup/memory rw,nosuid,nodev,noexec shared:17 - cgroup cgroup rw,memory
41 30 0:36 / /sys/fs/cgroup/unified rw,nosuid,nodev,noexec shared:10 - cgroup2 cgroup2 rw
`
	limit, agent := machine(t, v1, "12:memory:/system.slice/infranest-agent.service\n0::/system.slice/infranest-agent.service\n", prodApp()).attribute(1)

	if limit != nil || agent != nil {
		t.Fatalf("limit=%v agent=%v on a cgroup v1 machine", limit, agent)
	}
}

func TestLocalEventsSayNothing(t *testing.T) {
	// Mounted with memory_localevents, a slice's memory.events no longer includes its services', and the top
	// level would read zero over every kill beneath it.
	mi := strings.Replace(cgroup2Mountinfo, "rw,nsdelegate", "rw,nsdelegate,memory_localevents", 1)
	limit, agent := machine(t, mi, agentCgroup, prodApp()).attribute(1)

	if limit != nil || agent != nil {
		t.Fatalf("limit=%v agent=%v with local events", limit, agent)
	}
}

func TestTheSandboxsReadOnlyBindIsTheMountThatCounts(t *testing.T) {
	// ProtectControlGroups= stacks a read-only bind of the same hierarchy on top: still cgroup2, still read.
	mi := cgroup2Mountinfo + "120 36 0:30 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup2 rw,nsdelegate\n"
	if limit, _ := machine(t, mi, agentCgroup, prodApp()).attribute(1); limit == nil {
		t.Fatal("the sandbox's bind mount hid the hierarchy")
	}
}

func TestAKernelBeforeHierarchicalEventsSaysNothing(t *testing.T) {
	// Before 5.2 memory.events was the cgroup's own, and there was no memory.events.local beside it.
	files := map[string]string{}
	for path, body := range prodApp() {
		if !strings.HasSuffix(path, ".local") {
			files[path] = body
		}
	}

	if limit, _ := machine(t, cgroup2Mountinfo, agentCgroup, files).attribute(1); limit != nil {
		t.Fatalf("limit = %d from non-hierarchical events", *limit)
	}
}

func TestACgroupNamespaceSaysNothing(t *testing.T) {
	// In a container the top level is the container's, not the machine's.
	limit, agent := machine(t, cgroup2Mountinfo, "0::/\n", prodApp()).attribute(1)

	if limit != nil || agent != nil {
		t.Fatalf("limit=%v agent=%v inside a cgroup namespace", limit, agent)
	}
}

func TestAGarbledEventsFileMakesTheSumUnknown(t *testing.T) {
	// Skipping it would undercount, and an undercount reads as "the machine ran out".
	files := prodApp()
	files["user.slice/memory.events"] = "oom lots\n"

	if limit, _ := machine(t, cgroup2Mountinfo, agentCgroup, files).attribute(1); limit != nil {
		t.Fatalf("limit = %d over an unreadable top-level cgroup", *limit)
	}
}

func TestATopLevelCgroupWithoutTheMemoryControllerAddsNothing(t *testing.T) {
	// No memory.events: nothing beneath it can have a memory limit, so it truthfully contributes zero.
	files := prodApp()
	files["machine.slice/cgroup.procs"] = ""

	limit, _ := machine(t, cgroup2Mountinfo, agentCgroup, files).attribute(1)

	if limit == nil || *limit != 1 {
		t.Fatalf("limit = %v, want 1", limit)
	}
}

func TestNoMemoryControllerAnywhereSaysNothing(t *testing.T) {
	files := map[string]string{"system.slice/cgroup.procs": "", "user.slice/cgroup.procs": ""}

	limit, agent := machine(t, cgroup2Mountinfo, agentCgroup, files).attribute(1)

	if limit != nil || agent != nil {
		t.Fatalf("limit=%v agent=%v with no memory controller", limit, agent)
	}
}

func TestTheMemoryEventsParserNeedsItsKey(t *testing.T) {
	if n, ok := parseMemoryEvent(strings.NewReader(events(2, 1)), "oom_kill"); !ok || n != 1 {
		t.Fatalf("n=%d ok=%v", n, ok)
	}
	// `oom` must not match `oom_kill` or `oom_group_kill`.
	if n, ok := parseMemoryEvent(strings.NewReader("oom_kill 7\noom_group_kill 3\noom 2\n"), "oom"); !ok || n != 2 {
		t.Fatalf("n=%d ok=%v", n, ok)
	}
	if _, ok := parseMemoryEvent(strings.NewReader("low 0\nhigh 0\n"), "oom"); ok {
		t.Fatal("invented a counter the file does not have")
	}
}
