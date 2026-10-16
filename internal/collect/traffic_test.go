package collect

import (
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
	if disk["sda"] != [2]uint64{400000 * 512, 200000 * 512} {
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
	if len(disk) != 1 || disk["sda"] == ([2]uint64{}) {
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

	if in, _, _, _ := tr.observe(trafficReading{at: t0, net: map[string][2]uint64{"eth0": {1000, 0}}, disk: map[string][2]uint64{}}); in != nil {
		t.Fatal("the first sample has nothing to measure against")
	}

	in, out, read, _ := tr.observe(trafficReading{
		at:   t0.Add(60 * time.Second),
		net:  map[string][2]uint64{"eth0": {61000, 6000}},
		disk: map[string][2]uint64{},
	})
	if in == nil || *in != 1000 || *out != 100 || read == nil || *read != 0 {
		t.Fatalf("got in=%v out=%v read=%v", in, out, read)
	}
}

func TestADeviceThatResetOrIsNewIsLeftOutRatherThanWrapped(t *testing.T) {
	tr := NewTrafficTracker()
	t0 := time.Now()
	tr.observe(trafficReading{at: t0, net: map[string][2]uint64{"eth0": {5000, 5000}, "eth1": {100, 100}}})

	in, _, _, _ := tr.observe(trafficReading{at: t0.Add(10 * time.Second), net: map[string][2]uint64{
		"eth0": {10, 10},        // reset: the driver was reloaded
		"eth1": {1100, 100},     // +1000
		"eth2": {999999, 99999}, // new: no earlier reading
	}})
	if in == nil || *in != 100 {
		t.Fatalf("got %v", in)
	}
}

func TestAnUnreadableSourceAbstainsRatherThanReadingZero(t *testing.T) {
	tr := NewTrafficTracker()
	t0 := time.Now()
	tr.observe(trafficReading{at: t0, net: map[string][2]uint64{"eth0": {0, 0}}})

	_, _, read, write := tr.observe(trafficReading{at: t0.Add(time.Minute), net: map[string][2]uint64{"eth0": {0, 0}}})
	if read != nil || write != nil {
		t.Fatal("no diskstats is not zero disk traffic")
	}
}
