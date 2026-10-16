package collect

import (
	"strings"
	"testing"
)

/*
Software RAID (InfraNest #3833), from /proc/mdstat in the kernel's documented format: a healthy mirror, a
mirror that has lost a member, a RAID5 rebuilding onto a replacement, an inactive array and a RAID0.
*/
const mdstat = `Personalities : [raid1] [raid6] [raid5] [raid4] [raid0]
md1 : active raid1 sdb2[1] sda2[0]
      1047552 blocks super 1.2 [2/2] [UU]

md0 : active raid1 sdb1[1](F) sda1[0]
      976630464 blocks super 1.2 [2/1] [U_]
      bitmap: 3/8 pages [12KB], 65536KB chunk

md2 : active raid5 sdd1[3] sdc1[2] sdb1[1]
      2929893888 blocks super 1.2 level 5, 512k chunk, algorithm 2 [3/2] [UU_]
      [=>...................]  recovery =  8.5% (124739584/1464946944) finish=133.2min speed=167680K/sec

md127 : inactive sde[0](S)
      976631512 blocks super 1.2

md3 : active raid0 sdf1[1] sdg1[0]
      1953260544 blocks super 1.2 512k chunks

unused devices: <none>
`

func TestArraysAreReadFromMdstat(t *testing.T) {
	arrays := parseMdstat(strings.NewReader(mdstat))
	if len(arrays) != 5 {
		t.Fatalf("got %d arrays", len(arrays))
	}

	by := map[string]RaidArray{}
	for _, a := range arrays {
		by[a.Name] = a
	}

	if a := by["md1"]; a.Degraded() || a.Devices != 2 || a.Active != 2 || a.Level != "raid1" {
		t.Errorf("healthy mirror: %+v", a)
	}
	if a := by["md0"]; !a.Degraded() || a.Failed != 1 {
		t.Errorf("a mirror that lost a disk is degraded: %+v", a)
	}
	if a := by["md2"]; !a.Degraded() || a.Sync != "recovery" || a.SyncPercent == nil || *a.SyncPercent != 8.5 {
		t.Errorf("a rebuild in progress: %+v", a)
	}
	if a := by["md127"]; a.State != "inactive" {
		t.Errorf("inactive: %+v", a)
	}
	if a := by["md3"]; a.Degraded() || a.Devices != 0 {
		t.Errorf("raid0 has nothing to degrade: %+v", a)
	}
}

func TestAMachineWithoutArraysReportsNone(t *testing.T) {
	if a := parseMdstat(strings.NewReader("Personalities : \nunused devices: <none>\n")); len(a) != 0 {
		t.Fatalf("got %v", a)
	}
}
