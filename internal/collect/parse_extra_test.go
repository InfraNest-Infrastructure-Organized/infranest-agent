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
