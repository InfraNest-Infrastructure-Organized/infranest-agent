package collect

import (
	"bufio"
	"io"
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

// trafficReading is one read of the counters: bytes in/out per interface, bytes read/written per disk.
// A nil map is a source that could not be read, which abstains rather than reading as zero.
type trafficReading struct {
	at   time.Time
	net  map[string][2]uint64
	disk map[string][2]uint64
}

// observe stores this reading and returns the rates since the previous one, nil where there is none.
//
// Per device, so one that appeared, vanished or reset (its counter went backwards — a driver reload, a
// reboot the process somehow survived) is left out of the sum rather than wrapping into an absurd figure.
func (t *TrafficTracker) observe(cur trafficReading) (in, out, read, write *int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	prev := t.prev
	t.prev = &cur

	if prev == nil {
		return nil, nil, nil, nil
	}

	secs := cur.at.Sub(prev.at).Seconds()
	if secs < 1 {
		return nil, nil, nil, nil
	}

	in, out = rates(prev.net, cur.net, secs)
	read, write = rates(prev.disk, cur.disk, secs)

	return in, out, read, write
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

// parseDiskstats reads /proc/diskstats into bytes read and written per block device. The kernel counts
// in 512-byte sectors whatever the device's real sector size.
//
//	8       0 sda 12345 0 678900 0 2345 0 456700 0 0 0 0 …
func parseDiskstats(r io.Reader) map[string][2]uint64 {
	out := map[string][2]uint64{}
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
		out[f[2]] = [2]uint64{read * 512, written * 512}
	}

	return out
}

// onlyPhysical keeps the entries backed by a device, falling back to everything but loopback when none
// is (a container's interfaces, a VM type whose NIC has no device link): some figure is better than none.
//
// Counting a bridge or a veth pair as well as the NIC would count one container's traffic twice, and
// counting an LVM volume as well as its disk would do the same for every write.
func onlyPhysical(all map[string][2]uint64, isPhysical func(string) bool, fallback bool) map[string][2]uint64 {
	if all == nil {
		return nil
	}

	out := map[string][2]uint64{}
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
