package collect

import (
	"bufio"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TrafficTracker turns two readings of the kernel's byte counters into rates (InfraNest #3831).
//
// The counters only ever grow, so a rate needs the previous reading — which is why this lives for the
// life of the process, like ContainerTracker. The rate is over the whole interval between samples rather
// than a short burst sampled now: "how much went through this minute" is what a throughput alert means,
// and a 300 ms window would read one large request as a saturated link.
type TrafficTracker struct {
	mu   sync.Mutex
	prev *trafficReading
}

// NewTrafficTracker returns a tracker with no previous reading, so the first sample sends no rates.
func NewTrafficTracker() *TrafficTracker { return &TrafficTracker{} }

// trafficReading is one read of the counters: bytes in/out per interface, and per disk what /proc/diskstats
// says about it. A nil map is a source that could not be read, which abstains rather than reading as zero.
type trafficReading struct {
	at   time.Time
	net  map[string][2]uint64
	disk map[string]diskCounters
}

// diskCounters is one whole disk's line of /proc/diskstats: bytes read and written, and the milliseconds
// it has spent with at least one request in flight (`io_ticks`). busyKnown is false on a line too short
// to carry io_ticks, which then abstains from the busy share rather than reading as idle.
type diskCounters struct {
	read, written uint64
	busyMs        uint64
	busyKnown     bool
}

// Disk is one whole disk's activity since the previous sample (InfraNest #4922).
//
// The totals beside it (`disk_read_bps` / `disk_write_bps`) sum every disk, so "the data volume is
// saturated by the backup while the root disk is idle" could not be said. This says it, one row per disk,
// named as the kernel names it — the same names `mounts[].disks` uses, which is how a receiver puts a
// mount and its disk's activity in one sentence.
type Disk struct {
	Name     string `json:"name"`
	ReadBps  int64  `json:"read_bps"`
	WriteBps int64  `json:"write_bps"`

	// The share of the interval the disk had at least one request in flight, 0–100. On a spinning disk
	// 100 is saturated; an SSD or NVMe disk serves many requests at once, so there it means "never idle"
	// and not "can take no more". Nil where the kernel's line carries no io_ticks.
	BusyPercent *float64 `json:"busy_percent,omitempty"`
}

// trafficRates is what one observation yields. Nil fields and a nil disk list are "nothing to measure
// against" — the first sample, or a source that could not be read.
type trafficRates struct {
	in, out, read, write *int64
	disks                []Disk
}

// observe stores this reading and returns the rates since the previous one, nil where there is none.
//
// Per device, so one that appeared, vanished or reset (its counter went backwards — a driver reload, a
// reboot the process somehow survived) is left out of the sum rather than wrapping into an absurd figure.
func (t *TrafficTracker) observe(cur trafficReading) trafficRates {
	t.mu.Lock()
	defer t.mu.Unlock()

	prev := t.prev
	t.prev = &cur

	if prev == nil {
		return trafficRates{}
	}

	secs := cur.at.Sub(prev.at).Seconds()
	if secs < 1 {
		return trafficRates{}
	}

	var r trafficRates
	r.in, r.out = rates(prev.net, cur.net, secs)
	r.read, r.write = rates(diskBytes(prev.disk), diskBytes(cur.disk), secs)
	r.disks = perDisk(prev.disk, cur.disk, secs)

	return r
}

// diskBytes is the read/written pair per disk that the totals are summed from, so the totals stay exactly
// what they were before the per-disk rows existed.
func diskBytes(disks map[string]diskCounters) map[string][2]uint64 {
	if disks == nil {
		return nil
	}

	out := make(map[string][2]uint64, len(disks))
	for name, d := range disks {
		out[name] = [2]uint64{d.read, d.written}
	}

	return out
}

// maxDisks bounds the per-disk rows. Each is about a hundred bytes and rides on every sample, so a storage
// server with sixty drives would otherwise put six kilobytes on each of a backfill's sixty samples. The
// busiest are kept, because those are the rows anyone is looking for.
const maxDisks = 32

// maxDiskName is the kernel's own bound on a block device name (DISK_NAME_LEN).
const maxDiskName = 32

// perDisk turns two readings into one row per disk present in both, busiest first.
//
// A disk that is new, has gone, or whose counters went backwards is left out of this sample — the same
// rule the totals follow — rather than reported with a rate computed against nothing. An empty result is
// nil, so the field is absent where there is no whole disk to report.
func perDisk(prev, cur map[string]diskCounters, secs float64) []Disk {
	if prev == nil || cur == nil {
		return nil
	}

	var out []Disk
	for name, now := range cur {
		was, ok := prev[name]
		if !ok || now.read < was.read || now.written < was.written {
			continue
		}

		d := Disk{
			Name:     clip(name, maxDiskName),
			ReadBps:  int64(float64(now.read-was.read) / secs),
			WriteBps: int64(float64(now.written-was.written) / secs),
		}

		if now.busyKnown && was.busyKnown && now.busyMs >= was.busyMs {
			// io_ticks is wall-clock milliseconds with a request in flight, so it cannot honestly exceed
			// the interval; clamped anyway, because the interval is measured here and the ticks in the
			// kernel, and the two clocks disagree by a tick or two at the edges.
			pct := float64(now.busyMs-was.busyMs) / (secs * 1000) * 100
			pct = math.Round(math.Min(math.Max(pct, 0), 100)*10) / 10
			d.BusyPercent = &pct
		}

		out = append(out, d)
	}

	sort.Slice(out, func(i, j int) bool {
		bi, bj := busy(out[i]), busy(out[j])
		if bi != bj {
			return bi > bj
		}
		ti, tj := out[i].ReadBps+out[i].WriteBps, out[j].ReadBps+out[j].WriteBps
		if ti != tj {
			return ti > tj
		}
		return out[i].Name < out[j].Name
	})

	if len(out) > maxDisks {
		out = out[:maxDisks]
	}

	return out
}

// busy orders a disk with no busy share after every disk that has one.
func busy(d Disk) float64 {
	if d.BusyPercent == nil {
		return -1
	}
	return *d.BusyPercent
}

func rates(prev, cur map[string][2]uint64, secs float64) (*int64, *int64) {
	if prev == nil || cur == nil {
		return nil, nil
	}

	var a, b uint64
	for name, now := range cur {
		was, ok := prev[name]
		if !ok || now[0] < was[0] || now[1] < was[1] {
			continue
		}
		a += now[0] - was[0]
		b += now[1] - was[1]
	}

	ra, rb := int64(float64(a)/secs), int64(float64(b)/secs)

	return &ra, &rb
}

// parseNetDev reads /proc/net/dev into receive and transmit bytes per interface.
//
//	Inter-|   Receive                                                |  Transmit
//	 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets …
//	  eth0: 1234567    8910    0    0    0     0          0         0  7654321    5432 …
func parseNetDev(r io.Reader) map[string][2]uint64 {
	out := map[string][2]uint64{}
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		name, rest, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(f[0], 10, 64)
		tx, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out[strings.TrimSpace(name)] = [2]uint64{rx, tx}
	}

	return out
}

// parseDiskstats reads /proc/diskstats into bytes read and written per block device, and the milliseconds
// each has spent doing I/O. The kernel counts in 512-byte sectors whatever the device's real sector size.
//
//	major minor name  reads merged sectors ms  writes merged sectors ms  in-flight io_ticks weighted …
//	8     0     sda   12345 0      678900  0   2345   0      456700  0   0         0        0
func parseDiskstats(r io.Reader) map[string]diskCounters {
	out := map[string]diskCounters{}
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		f := strings.Fields(scanner.Text())
		if len(f) < 10 {
			continue
		}
		read, err1 := strconv.ParseUint(f[5], 10, 64)
		written, err2 := strconv.ParseUint(f[9], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		d := diskCounters{read: read * 512, written: written * 512}
		if len(f) >= 13 {
			if ticks, err := strconv.ParseUint(f[12], 10, 64); err == nil {
				d.busyMs, d.busyKnown = ticks, true
			}
		}
		out[f[2]] = d
	}

	return out
}

// onlyPhysical keeps the entries backed by a device, falling back to everything but loopback when none
// is (a container's interfaces, a VM type whose NIC has no device link): some figure is better than none.
//
// Counting a bridge or a veth pair as well as the NIC would count one container's traffic twice, and
// counting an LVM volume as well as its disk would do the same for every write.
func onlyPhysical[V any](all map[string]V, isPhysical func(string) bool, fallback bool) map[string]V {
	if all == nil {
		return nil
	}

	out := map[string]V{}
	for name, v := range all {
		if isPhysical(name) {
			out[name] = v
		}
	}

	if len(out) == 0 && fallback {
		for name, v := range all {
			if name != "lo" {
				out[name] = v
			}
		}
	}

	return out
}
