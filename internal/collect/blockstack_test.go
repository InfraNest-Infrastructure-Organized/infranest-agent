package collect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

/*
Which disks a filesystem is stored on (InfraNest #4922).

A mount's device names a disk only in the simplest case. Ubuntu's installer puts / on
/dev/mapper/ubuntu--vg-ubuntu--lv, some cloud images mount /dev/root, and a mirror is one md device over two
disks — none of which a receiver can join to a row of `disks[]` by its name. The kernel knows, and says so in
sysfs; these tests build that tree as the kernel lays it out and check the walk down it.
*/

// fakeSys lays out a sysfs: two NVMe disks with two partitions each, an md mirror over the second
// partitions, LVM on the mirror, a loop device, a two-device cycle that a broken sysfs could present, and
// a volume over more disks than the cap.
func fakeSys(t *testing.T) string {
	t.Helper()
	sys := t.TempDir()

	dir := func(p string) string {
		full := filepath.Join(sys, p)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		return full
	}
	file := func(p string) {
		if err := os.WriteFile(filepath.Join(sys, p), []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(from, to string) {
		dir(filepath.Dir(from))
		if err := os.Symlink(filepath.Join(sys, to), filepath.Join(sys, from)); err != nil {
			t.Fatal(err)
		}
	}

	disk := func(name, path string) {
		dir(path + "/device")
		link("block/"+name, path)
	}

	for _, n := range []string{"nvme0n1", "nvme1n1"} {
		base := "devices/pci0000:00/nvme/" + n
		disk(n, base)
		for _, p := range []string{"p1", "p2"} {
			dir(base + "/" + n + p)
			file(base + "/" + n + p + "/partition")
		}
	}
	link("dev/block/259:0", "devices/pci0000:00/nvme/nvme0n1")
	link("class/block/nvme1n1p1", "devices/pci0000:00/nvme/nvme1n1/nvme1n1p1")
	link("class/block/dm-0", "devices/virtual/block/dm-0")
	link("dev/block/259:1", "devices/pci0000:00/nvme/nvme0n1/nvme0n1p1")

	dir("devices/virtual/block/md127")
	link("devices/virtual/block/md127/slaves/nvme0n1p2", "devices/pci0000:00/nvme/nvme0n1/nvme0n1p2")
	link("devices/virtual/block/md127/slaves/nvme1n1p2", "devices/pci0000:00/nvme/nvme1n1/nvme1n1p2")
	link("dev/block/9:127", "devices/virtual/block/md127")

	dir("devices/virtual/block/dm-0")
	link("devices/virtual/block/dm-0/slaves/md127", "devices/virtual/block/md127")
	link("dev/block/253:0", "devices/virtual/block/dm-0")

	dir("devices/virtual/block/loop0")
	link("block/loop0", "devices/virtual/block/loop0")
	link("dev/block/7:0", "devices/virtual/block/loop0")

	dir("devices/virtual/block/dm-1")
	dir("devices/virtual/block/dm-2")
	link("devices/virtual/block/dm-1/slaves/dm-2", "devices/virtual/block/dm-2")
	link("devices/virtual/block/dm-2/slaves/dm-1", "devices/virtual/block/dm-1")
	link("dev/block/253:1", "devices/virtual/block/dm-1")

	dir("devices/virtual/block/dm-9")
	for i := 0; i < maxMountDisks+4; i++ {
		name := fmt.Sprintf("sd%c", 'a'+i)
		disk(name, "devices/pci0000:00/scsi/"+name)
		link("devices/virtual/block/dm-9/slaves/"+name, "devices/pci0000:00/scsi/"+name)
	}
	link("dev/block/253:9", "devices/virtual/block/dm-9")

	return sys
}

func TestAFilesystemIsTracedDownToTheWholeDisksItIsStoredOn(t *testing.T) {
	sys := fakeSys(t)

	cases := []struct {
		name         string
		major, minor uint64
		want         []string
	}{
		{"a partition is on its disk", 259, 1, []string{"nvme0n1"}},
		{"an unpartitioned disk is itself", 259, 0, []string{"nvme0n1"}},
		{"a mirror is on both disks", 9, 127, []string{"nvme0n1", "nvme1n1"}},
		{"LVM on a mirror is on both disks", 253, 0, []string{"nvme0n1", "nvme1n1"}},
		{"a loop device is on no disk", 7, 0, nil},
		{"a device sysfs does not know is on no disk", 8, 99, nil},
		{"a cycle ends", 253, 1, nil},
	}
	for _, c := range cases {
		if got := wholeDisks(sys, c.major, c.minor); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAnAnonymousFilesystemIsTracedFromTheDeviceItWasMountedFrom(t *testing.T) {
	// btrfs numbers its subvolumes with major 0, so the walk starts from the name in the mount table.
	sys := fakeSys(t)

	if got := wholeDisksNamed(sys, "nvme1n1p1"); !reflect.DeepEqual(got, []string{"nvme1n1"}) {
		t.Fatalf("got %v", got)
	}
	if got := wholeDisksNamed(sys, "dm-0"); !reflect.DeepEqual(got, []string{"nvme0n1", "nvme1n1"}) {
		t.Fatalf("got %v", got)
	}
	for _, name := range []string{"", ".", "..", "../../etc", "nope"} {
		if got := wholeDisksNamed(sys, name); got != nil {
			t.Errorf("%q: got %v", name, got)
		}
	}
}

func TestTheDisksOfOneMountAreCapped(t *testing.T) {
	if got := wholeDisks(fakeSys(t), 253, 9); len(got) != maxMountDisks {
		t.Fatalf("got %d: %v", len(got), got)
	}
}

func TestDevNumbersSplitsTheKernelsEncoding(t *testing.T) {
	// glibc's makedev, written out, so the test does not borrow the code it checks.
	makedev := func(major, minor uint64) uint64 {
		return (minor & 0xff) | ((major & 0xfff) << 8) | ((minor &^ 0xff) << 12) | ((major &^ 0xfff) << 32)
	}

	for _, c := range [][2]uint64{{8, 0}, {259, 1}, {253, 0}, {9, 127}, {259, 1048575}, {4095, 255}, {70000, 300}} {
		major, minor := devNumbers(makedev(c[0], c[1]))
		if major != c[0] || minor != c[1] {
			t.Errorf("%d:%d came back as %d:%d", c[0], c[1], major, minor)
		}
	}
}

// PAYLOAD.md states what contract 20 adds to a sample at its caps; this is that figure, measured. The rows
// ride on every sample, so a cap that grew unnoticed would grow every batch sixty times over.
func TestWhatContract20AddsToASampleAtItsCaps(t *testing.T) {
	name := strings.Repeat("n", maxDiskName)
	busy := 100.0

	without := Sample{Mounts: make([]Mount, 64)}
	with := Sample{Mounts: make([]Mount, 64)}
	for i := range with.Mounts {
		for j := 0; j < maxMountDisks; j++ {
			with.Mounts[i].Disks = append(with.Mounts[i].Disks, name)
		}
	}
	for i := 0; i < maxDisks; i++ {
		with.Disks = append(with.Disks, Disk{Name: name, ReadBps: 9_999_999_999, WriteBps: 9_999_999_999, BusyPercent: &busy})
	}

	a, _ := json.Marshal(without)
	b, _ := json.Marshal(with)
	added := len(b) - len(a)
	if added > 22_026 {
		t.Fatalf("contract 20 adds %d bytes to a sample at its caps; PAYLOAD.md says 22,026", added)
	}
	t.Logf("contract 20 adds %d bytes per sample at its caps", added)
}
