package collect

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

/*
Throughput from the agent (InfraNest #3831).

Until now only servers polled from a provider's metrics API had network and disk throughput; the agent
sits on the machine and can read the kernel's own counters for free. What is worth testing is what must
not be counted: twice (a bridge beside its NIC, an LVM volume beside its disk), across a reset, or on the
first sample, which has nothing to measure against.
*/

// Captured from a Docker host: a NIC, loopback, the docker bridge and one container's veth.
const netDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 4812344   41230    0    0    0     0          0         0  4812344   41230    0    0    0     0       0          0
  eth0: 98765432  123456    0    0    0     0          0         0 12345678   65432    0    0    0     0       0          0
docker0: 5000000    6000    0    0    0     0          0         0  7000000    8000    0    0    0     0       0          0
veth1a2b: 7000000    8000    0    0    0     0          0         0  5000000    6000    0    0    0     0       0          0
`

// A disk, its two partitions, an LVM volume on the second, and a loop device.
const diskstats = `   8       0 sda 20000 0 400000 0 10000 0 200000 0 0 0 0 0 0 0 0 0 0
   8       1 sda1 100 0 2000 0 50 0 1000 0 0 0 0 0 0 0 0 0 0
   8       2 sda2 19900 0 398000 0 9950 0 199000 0 0 0 0 0 0 0 0 0 0
 253       0 dm-0 19800 0 396000 0 9900 0 198000 0 0 0 0 0 0 0 0 0 0
   7       0 loop0 10 0 80 0 0 0 0 0 0 0 0 0 0 0 0 0 0
`

func TestTheCountersAreReadFromProc(t *testing.T) {
	net := parseNetDev(strings.NewReader(netDev))
	if net["eth0"] != [2]uint64{98765432, 12345678} || len(net) != 4 {
		t.Fatalf("got %v", net)
	}

	disk := parseDiskstats(strings.NewReader(diskstats))
	if got := disk["sda"]; got.read != 400000*512 || got.written != 200000*512 {
		t.Fatalf("sectors are 512 bytes whatever the device: got %v", disk["sda"])
	}
}

func TestOnlyTheDevicesTrafficReallyCrossesAreCounted(t *testing.T) {
	physical := func(name string) bool { return name == "eth0" || name == "sda" }

	net := onlyPhysical(parseNetDev(strings.NewReader(netDev)), physical, true)
	if len(net) != 1 || net["eth0"] == ([2]uint64{}) {
		t.Fatalf("the bridge and veth would count container traffic twice: %v", net)
	}

	disk := onlyPhysical(parseDiskstats(strings.NewReader(diskstats)), physical, false)
	if len(disk) != 1 || disk["sda"] == (diskCounters{}) {
		t.Fatalf("partitions, LVM and loop are views of a disk already counted: %v", disk)
	}
}

func TestWithoutADeviceLinkEverythingButLoopbackIsCounted(t *testing.T) {
	net := onlyPhysical(parseNetDev(strings.NewReader(netDev)), func(string) bool { return false }, true)
	if _, ok := net["lo"]; ok || len(net) != 3 {
		t.Fatalf("got %v", net)
	}
	// Disks have no such fallback: guessing which block device is real double-counts.
	if disk := onlyPhysical(parseDiskstats(strings.NewReader(diskstats)), func(string) bool { return false }, false); len(disk) != 0 {
		t.Fatalf("got %v", disk)
	}
}

func TestARateIsBytesOverTheTimeBetweenSamples(t *testing.T) {
	tr := NewTrafficTracker()
	t0 := time.Now()

	if r := tr.observe(trafficReading{at: t0, net: map[string][2]uint64{"eth0": {1000, 0}}, disk: map[string]diskCounters{}}); r.in != nil {
		t.Fatal("the first sample has nothing to measure against")
	}

	r := tr.observe(trafficReading{
		at:   t0.Add(60 * time.Second),
		net:  map[string][2]uint64{"eth0": {61000, 6000}},
		disk: map[string]diskCounters{},
	})
	in, out, read := r.in, r.out, r.read
	if in == nil || *in != 1000 || *out != 100 || read == nil || *read != 0 {
		t.Fatalf("got in=%v out=%v read=%v", in, out, read)
	}
}

func TestADeviceThatResetOrIsNewIsLeftOutRatherThanWrapped(t *testing.T) {
	tr := NewTrafficTracker()
	t0 := time.Now()
	tr.observe(trafficReading{at: t0, net: map[string][2]uint64{"eth0": {5000, 5000}, "eth1": {100, 100}}})

	in := tr.observe(trafficReading{at: t0.Add(10 * time.Second), net: map[string][2]uint64{
		"eth0": {10, 10},        // reset: the driver was reloaded
		"eth1": {1100, 100},     // +1000
		"eth2": {999999, 99999}, // new: no earlier reading
	}}).in
	if in == nil || *in != 100 {
		t.Fatalf("got %v", in)
	}
}

func TestAnUnreadableSourceAbstainsRatherThanReadingZero(t *testing.T) {
	tr := NewTrafficTracker()
	t0 := time.Now()
	tr.observe(trafficReading{at: t0, net: map[string][2]uint64{"eth0": {0, 0}}})

	r := tr.observe(trafficReading{at: t0.Add(time.Minute), net: map[string][2]uint64{"eth0": {0, 0}}})
	if r.read != nil || r.write != nil || r.disks != nil {
		t.Fatal("no diskstats is not zero disk traffic")
	}
}

/*
Disk activity per disk (InfraNest #4922).

The totals above answer "how much did this machine read". They cannot say which disk: a data volume
saturated by a backup while the root disk idles reads the same as two half-busy disks. What must hold is
that each row is a whole disk (the same rule the totals use, so the rows add up to them), that the busy
share is io_ticks over the interval and never more than all of it, and that a line too old to carry
io_ticks abstains rather than reading as idle.
*/

// A server as an installer leaves it: two NVMe disks, each partitioned, an md mirror across their second
// partitions, LVM on the mirror, a snap's loop device and a zram swap. Fields after the name: reads, merged,
// sectors read, ms reading, writes, merged, sectors written, ms writing, in flight, io_ticks, weighted.
const diskstatsStack = `259       0 nvme0n1 1000 0 20000 0 500 0 10000 0 0 1000 0
 259       1 nvme0n1p1 10 0 200 0 5 0 100 0 0 10 0
 259       2 nvme0n1p2 990 0 19800 0 495 0 9900 0 0 990 0
 259       3 nvme1n1 1000 0 20000 0 500 0 10000 0 0 2000 0
 259       4 nvme1n1p1 10 0 200 0 5 0 100 0 0 10 0
 259       5 nvme1n1p2 990 0 19800 0 495 0 9900 0 0 1990 0
   9     127 md127 1980 0 39600 0 990 0 19800 0 0 2980 0
 253       0 dm-0 1980 0 39600 0 990 0 19800 0 0 2980 0
   7       0 loop0 10 0 80 0 0 0 0 0 0 5 0
 252       0 zram0 50 0 400 0 50 0 400 0 0 0 0
`

// The same machine a minute later: nvme0n1 read 60 MB and was busy 30 s of it; nvme1n1 wrote 6 MB and was
// busy the whole minute; every view stacked on them moved too, and must not appear.
const diskstatsStackLater = `259       0 nvme0n1 5000 0 137188 0 500 0 10000 0 0 31000 0
 259       1 nvme0n1p1 10 0 200 0 5 0 100 0 0 10 0
 259       2 nvme0n1p2 4990 0 136988 0 495 0 9900 0 0 30990 0
 259       3 nvme1n1 1000 0 20000 0 1500 0 21719 0 0 62000 0
 259       4 nvme1n1p1 10 0 200 0 5 0 100 0 0 10 0
 259       5 nvme1n1p2 990 0 19800 0 1495 0 21619 0 0 61990 0
   9     127 md127 5980 0 156788 0 1990 0 31519 0 0 92980 0
 253       0 dm-0 5980 0 156788 0 1990 0 31519 0 0 92980 0
   7       0 loop0 900 0 99999 0 0 0 0 0 0 60000 0
 252       0 zram0 9999 0 99999 0 9999 0 99999 0 0 0 0
`

func wholeDisk(name string) bool { return name == "nvme0n1" || name == "nvme1n1" }

func TestEachWholeDiskGetsItsOwnRowAndNothingStackedOnItDoes(t *testing.T) {
	tr := NewTrafficTracker()
	t0 := time.Now()

	first := tr.observe(trafficReading{at: t0, disk: onlyPhysical(parseDiskstats(strings.NewReader(diskstatsStack)), wholeDisk, false)})
	if first.disks != nil {
		t.Fatal("the first sample has nothing to measure against")
	}

	r := tr.observe(trafficReading{at: t0.Add(time.Minute), disk: onlyPhysical(parseDiskstats(strings.NewReader(diskstatsStackLater)), wholeDisk, false)})
	if len(r.disks) != 2 {
		t.Fatalf("partitions, md, LVM, loop and zram are views of a disk already counted: %+v", r.disks)
	}

	// Busiest first: nvme1n1 was never idle.
	one, zero := r.disks[0], r.disks[1]
	if one.Name != "nvme1n1" || zero.Name != "nvme0n1" {
		t.Fatalf("busiest first: got %s, %s", one.Name, zero.Name)
	}
	if one.BusyPercent == nil || *one.BusyPercent != 100 || one.WriteBps != (21719-10000)*512/60 || one.ReadBps != 0 {
		t.Fatalf("nvme1n1: %+v busy=%v", one, deref64(one.BusyPercent))
	}
	if zero.BusyPercent == nil || *zero.BusyPercent != 50 || zero.ReadBps != (137188-20000)*512/60 || zero.WriteBps != 0 {
		t.Fatalf("nvme0n1: %+v busy=%v", zero, deref64(zero.BusyPercent))
	}

	// The rows are the totals, broken down: nothing counted that the totals leave out, nor the reverse.
	if *r.read != zero.ReadBps+one.ReadBps || *r.write != zero.WriteBps+one.WriteBps {
		t.Fatalf("totals %d/%d disagree with the rows %+v", *r.read, *r.write, r.disks)
	}
}

func TestBusyIsNeverMoreThanTheWholeInterval(t *testing.T) {
	tr := NewTrafficTracker()
	t0 := time.Now()
	tr.observe(trafficReading{at: t0, disk: map[string]diskCounters{"sda": {busyMs: 0, busyKnown: true}}})

	// 61 s of ticks in a 60 s interval: the kernel's clock and ours disagree at the edges.
	r := tr.observe(trafficReading{at: t0.Add(time.Minute), disk: map[string]diskCounters{"sda": {busyMs: 61000, busyKnown: true}}})
	if len(r.disks) != 1 || r.disks[0].BusyPercent == nil || *r.disks[0].BusyPercent != 100 {
		t.Fatalf("got %+v", r.disks)
	}
}

func TestALineWithoutIoTicksAbstainsFromBusyRatherThanReadingIdle(t *testing.T) {
	// Ten fields: enough for bytes, not for io_ticks.
	short := parseDiskstats(strings.NewReader("   8       0 sda 100 0 2000 0 50 0 1000\n"))
	if d := short["sda"]; d.busyKnown || d.read != 2000*512 {
		t.Fatalf("got %+v", d)
	}

	tr := NewTrafficTracker()
	t0 := time.Now()
	tr.observe(trafficReading{at: t0, disk: short})
	r := tr.observe(trafficReading{at: t0.Add(time.Minute), disk: short})
	if len(r.disks) != 1 || r.disks[0].BusyPercent != nil {
		t.Fatalf("no io_ticks is not 0%% busy: %+v", r.disks)
	}

	b, _ := json.Marshal(r.disks[0])
	if strings.Contains(string(b), "busy_percent") {
		t.Fatalf("an unknown busy share must be absent on the wire: %s", b)
	}
}

func TestADiskThatResetOrAppearedIsLeftOutOfThisSample(t *testing.T) {
	tr := NewTrafficTracker()
	t0 := time.Now()
	tr.observe(trafficReading{at: t0, disk: map[string]diskCounters{
		"sda": {read: 5000, written: 5000, busyMs: 100, busyKnown: true},
		"sdb": {read: 100, written: 100, busyKnown: true},
	}})

	r := tr.observe(trafficReading{at: t0.Add(10 * time.Second), disk: map[string]diskCounters{
		"sda": {read: 10, written: 10, busyKnown: true},     // reset
		"sdb": {read: 1100, written: 100, busyKnown: true},  // +1000
		"sdc": {read: 9999999, written: 1, busyKnown: true}, // new
	}})
	if len(r.disks) != 1 || r.disks[0].Name != "sdb" || r.disks[0].ReadBps != 100 {
		t.Fatalf("got %+v", r.disks)
	}
}

func TestNoWholeDiskMeansTheFieldIsAbsent(t *testing.T) {
	tr := NewTrafficTracker()
	t0 := time.Now()
	none := onlyPhysical(parseDiskstats(strings.NewReader(diskstatsStack)), func(string) bool { return false }, false)
	tr.observe(trafficReading{at: t0, disk: none})
	r := tr.observe(trafficReading{at: t0.Add(time.Minute), disk: none})
	if r.disks != nil {
		t.Fatalf("got %+v", r.disks)
	}

	b, _ := json.Marshal(Sample{Disks: r.disks})
	if strings.Contains(string(b), `"disks"`) {
		t.Fatalf("absent, not []: %s", b)
	}
}

func TestTheRowsAreCappedKeepingTheBusiest(t *testing.T) {
	prev, cur := map[string]diskCounters{}, map[string]diskCounters{}
	for i := 0; i < maxDisks+10; i++ {
		name := fmt.Sprintf("sd%03d", i)
		prev[name] = diskCounters{busyKnown: true}
		cur[name] = diskCounters{busyMs: uint64(i) * 100, busyKnown: true}
	}

	tr := NewTrafficTracker()
	t0 := time.Now()
	tr.observe(trafficReading{at: t0, disk: prev})
	r := tr.observe(trafficReading{at: t0.Add(time.Minute), disk: cur})
	if len(r.disks) != maxDisks {
		t.Fatalf("got %d rows", len(r.disks))
	}
	if r.disks[0].Name != fmt.Sprintf("sd%03d", maxDisks+9) || r.disks[maxDisks-1].Name != "sd010" {
		t.Fatalf("the busiest are kept: first %s, last %s", r.disks[0].Name, r.disks[maxDisks-1].Name)
	}
}

func deref64(p *float64) any {
	if p == nil {
		return nil
	}

	return *p
}
