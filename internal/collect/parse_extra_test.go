package collect

import (
	"strings"
	"testing"
)

const sampleMounts = `sysfs /sys sysfs rw,nosuid 0 0
proc /proc proc rw,nosuid 0 0
/dev/sda1 / ext4 rw,relatime 0 0
tmpfs /run tmpfs rw,nosuid 0 0
/dev/sda2 /var ext4 rw,relatime 0 0
/dev/sda2 /var btrfs rw,relatime 0 0
/dev/sdb1 /mnt/my\040backup ext4 rw 0 0
cgroup2 /sys/fs/cgroup cgroup2 rw 0 0
`

func TestParseMountsKeepsOnlyFilesystemsSomeoneCouldFill(t *testing.T) {
	got, err := parseMounts(strings.NewReader(sampleMounts))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var points []string
	for _, m := range got {
		points = append(points, m.MountPoint())
	}

	want := []string{"/", "/var", "/mnt/my backup"}
	if len(points) != len(want) {
		t.Fatalf("got %v, want %v", points, want)
	}
	for i := range want {
		if points[i] != want[i] {
			t.Errorf("at %d: got %q, want %q", i, points[i], want[i])
		}
	}
}

func TestParseMountsDropsTmpfsAndFriends(t *testing.T) {
	// A tmpfs at 100% is a healthy machine. Alerting on it is noise nobody can act on, and /run is full
	// on plenty of perfectly fine servers.
	got, _ := parseMounts(strings.NewReader(sampleMounts))
	for _, m := range got {
		if m.MountPoint() == "/run" || strings.HasPrefix(m.MountPoint(), "/sys") || m.MountPoint() == "/proc" {
			t.Errorf("pseudo filesystem %q should not be reported", m.MountPoint())
		}
	}
}

func TestParseMountsReportsEachMountPointOnce(t *testing.T) {
	// /var appears twice above — a subvolume remount. Reporting both would make one full disk look like
	// two, and would double-count it in "the fullest mount".
	got, _ := parseMounts(strings.NewReader(sampleMounts))
	seen := map[string]int{}
	for _, m := range got {
		seen[m.MountPoint()]++
	}
	if seen["/var"] != 1 {
		t.Errorf("/var reported %d times", seen["/var"])
	}
}

func TestUnescapeMountHandlesSpaces(t *testing.T) {
	// Without this a mount at "/mnt/my backup" arrives as "/mnt/my\040backup" and matches nothing the
	// operator recognises.
	if got := unescapeMount(`/mnt/my\040backup`); got != "/mnt/my backup" {
		t.Errorf("got %q", got)
	}
	if got := unescapeMount("/plain/path"); got != "/plain/path" {
		t.Errorf("unescaping mangled a plain path: %q", got)
	}
}

func TestParseProcCommSurvivesNamesWithSpacesAndBrackets(t *testing.T) {
	// "(Web Content)" is a real Firefox process. Splitting on whitespace mangles exactly the processes
	// someone is most likely to be hunting for.
	cases := map[string]string{
		`1234 (nginx) S 1 1234 1234 0 -1 4194560 1 0`:            "nginx",
		`4321 (Web Content) S 1 4321 4321 0 -1 4194560 1 0`:      "Web Content",
		`999 (weird):) name) S 1 999 999 0 -1 4194560 1 0`:       "weird):) name",
		`5555 (postgres: writer) S 1 5555 5555 0 -1 4194560 1 0`: "postgres: writer",
	}

	for stat, want := range cases {
		got, err := parseProcComm(stat)
		if err != nil {
			t.Errorf("%q: unexpected error %v", stat, err)
			continue
		}
		if got != want {
			t.Errorf("%q: got %q, want %q", stat, got, want)
		}
	}
}

func TestParseProcCommRejectsRubbish(t *testing.T) {
	for _, s := range []string{"", "1234 no brackets here", "1234 )backwards( S"} {
		if _, err := parseProcComm(s); err == nil {
			t.Errorf("%q was accepted", s)
		}
	}
}

func TestParseStatmRSSIsInBytes(t *testing.T) {
	got, err := parseStatmRSS("1000 250 100 10 0 200 0", 4096)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 250*4096 {
		t.Errorf("got %d, want %d", got, 250*4096)
	}
}

func TestAtoiStrictRejectsProcEntriesThatAreNotProcesses(t *testing.T) {
	for _, s := range []string{"self", "net", "thread-self", "", "12a"} {
		if _, err := atoiStrict(s); err == nil {
			t.Errorf("%q was accepted as a pid", s)
		}
	}
	if n, err := atoiStrict("1234"); err != nil || n != 1234 {
		t.Errorf("got %d, %v", n, err)
	}
}

func TestTopByMemoryKeepsTheLargest(t *testing.T) {
	procs := []Process{
		{Command: "small", MemoryBytes: i64(10)},
		{Command: "huge", MemoryBytes: i64(1000)},
		{Command: "none"},
		{Command: "medium", MemoryBytes: i64(500)},
	}

	got := topByMemory(procs, 2)
	if len(got) != 2 || got[0].Command != "huge" || got[1].Command != "medium" {
		t.Errorf("got %+v", got)
	}
}

func TestDedupeMountsCollapsesBindMountsOfTheSameFilesystem(t *testing.T) {
	// Exactly what a container produces: Docker bind-mounts /etc/hosts, /etc/hostname and
	// /etc/resolv.conf from the host, and each reports the host disk's statfs verbatim.
	got := dedupeMounts([]Mount{
		{MountPoint: "/", Device: "/dev/vdb1", TotalBytes: 100, UsedBytes: 40},
		{MountPoint: "/etc/hosts", Device: "/dev/vdb1", TotalBytes: 100, UsedBytes: 40},
		{MountPoint: "/etc/hostname", Device: "/dev/vdb1", TotalBytes: 100, UsedBytes: 40},
		{MountPoint: "/var", Device: "/dev/vdb2", TotalBytes: 200, UsedBytes: 10},
	})

	if len(got) != 2 {
		t.Fatalf("got %d mounts, want 2: %+v", len(got), got)
	}
	if got[0].MountPoint != "/" || got[1].MountPoint != "/var" {
		t.Errorf("kept the wrong ones: %+v", got)
	}
}

func TestDedupeMountsKeepsSeparatePartitionsOfOneDisk(t *testing.T) {
	// Two partitions are genuinely separate filesystems. Merging them on device alone would hide a full
	// one behind an empty one.
	got := dedupeMounts([]Mount{
		{MountPoint: "/", Device: "/dev/sda1", TotalBytes: 100, UsedBytes: 95},
		{MountPoint: "/var", Device: "/dev/sda2", TotalBytes: 100, UsedBytes: 5},
	})

	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}

func TestDedupeMountsKeepsDevicelessFilesystems(t *testing.T) {
	// A network or fuse mount may report an empty device. Those must not all collapse into one row.
	got := dedupeMounts([]Mount{
		{MountPoint: "/a", TotalBytes: 100, UsedBytes: 1},
		{MountPoint: "/b", TotalBytes: 100, UsedBytes: 1},
	})

	if len(got) != 2 {
		t.Errorf("got %d, want 2 — deviceless mounts were collapsed", len(got))
	}
}

func TestParseMountsKeepsTheVisibleFilesystemWhenOneIsMountedOverAnother(t *testing.T) {
	// Mounting over an existing mount point hides what was there. Keeping the first line pairs one
	// filesystem's device name with another's statfs numbers — a row that reads as plausible and is wrong.
	got, err := parseMounts(strings.NewReader(
		"/dev/sda2 /var ext4 rw 0 0\n/dev/sdc1 /var xfs rw 0 0\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].device != "/dev/sdc1" {
		t.Errorf("device = %q, want /dev/sdc1 — the shadowed mount was kept", got[0].device)
	}
}

func TestBaseNameHandlesBothSeparators(t *testing.T) {
	// filepath.Base follows the host separator, so a Windows path parsed on a Linux build comes back
	// unchanged — which is exactly the case these tests are for, since Windows code cannot be run here.
	cases := map[string]string{
		`C:\Program Files\nginx\nginx.exe`: "nginx.exe",
		`C:\Windows\System32\svchost.exe`:  "svchost.exe",
		"/usr/bin/node":                    "node",
		"nginx":                            "nginx",
		"":                                 "",
	}

	for in, want := range cases {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitNulUTF16SeparatesTheDriveList(t *testing.T) {
	// GetLogicalDriveStringsW returns "C:\<NUL>D:\<NUL><NUL>". Reading it as one string yields a single
	// entry containing every drive letter run together, which then matches no drive at all.
	buf := []uint16{}
	for _, s := range []string{`C:\`, `D:\`} {
		for _, r := range s {
			buf = append(buf, uint16(r))
		}
		buf = append(buf, 0)
	}
	buf = append(buf, 0)

	got := splitNulUTF16(buf)
	if len(got) != 2 || got[0] != `C:\` || got[1] != `D:\` {
		t.Errorf("got %q, want [C:\\ D:\\]", got)
	}
}

func TestSplitNulUTF16HandlesAnEmptyBlock(t *testing.T) {
	if got := splitNulUTF16([]uint16{0, 0}); len(got) != 0 {
		t.Errorf("got %q, want none", got)
	}
}

// A process whose name contains a parenthesis puts every field after the comm out by one.
//
// Not a contrivance: `(sd-pam)` is on every systemd machine, and a program launched from a path with
// brackets in it lands here too. Counting fields from the start of the line is the obvious way to parse
// this file and it is wrong on exactly the machines least like a test one.
func TestProcTimesAreReadAfterTheCommNotFromTheStart(t *testing.T) {
	// pid 1681, comm "ruby (2)", state S, then the documented fields. utime=fields[11]=120,
	// stime=fields[12]=30, starttime=fields[19]=99000.
	line := "1681 (ruby (2)) S 1 1681 1681 0 -1 4194304 100 200 0 0 120 30 0 0 20 0 5 0 99000 1234 5678"

	got, err := parseProcTimes(line)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if got.CPUTicks != 150 {
		t.Fatalf("CPUTicks = %d, want 150 (utime 120 + stime 30)", got.CPUTicks)
	}
	if got.StartTicks != 99000 {
		t.Fatalf("StartTicks = %d, want 99000", got.StartTicks)
	}
}

func TestBootTimeIsReadFromProcStat(t *testing.T) {
	// A process's start time is recorded relative to boot, so without this the page can say a process has
	// been running three days but not since when — and "since when" is the half that lines up with
	// everything else on screen.
	stat := "cpu  1 2 3\nintr 99\nbtime 1756000000\nprocesses 1234\n"

	got, err := parseBootTime(stat)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if got != 1756000000 {
		t.Fatalf("btime = %d", got)
	}
}

func TestAMissingBootTimeIsAnErrorNotAZero(t *testing.T) {
	// Zero would put every process's start at January 1970 — a date, which is believed in a way a dash
	// is not.
	if _, err := parseBootTime("cpu 1 2 3\nprocesses 4\n"); err == nil {
		t.Fatal("a /proc/stat with no btime was accepted")
	}
}

func TestATruncatedStatLineIsRefused(t *testing.T) {
	if _, err := parseProcTimes("1 (init) S 1 1 1"); err == nil {
		t.Fatal("a stat line with too few fields was accepted")
	}
}
