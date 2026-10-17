//go:build linux

package collect

import (
	"io"
	"os"
	"time"
)

// readTraffic reads the counters once: physical interfaces from /proc/net/dev, whole disks from
// /proc/diskstats. A source that cannot be opened is nil and abstains.
func readTraffic() trafficReading {
	cur := trafficReading{at: time.Now()}

	if f, err := os.Open("/proc/net/dev"); err == nil {
		cur.net = onlyPhysical(parseNetDev(io.LimitReader(f, 1<<20)), func(name string) bool {
			return fileExists("/sys/class/net/" + name + "/device")
		}, true)
		_ = f.Close()
	}

	if f, err := os.Open("/proc/diskstats"); err == nil {
		// Whole disks only: a partition, an LVM or md volume and a loop device are all views of a disk
		// already counted. /sys/block lists whole disks, and only real ones have a device link.
		cur.disk = onlyPhysical(parseDiskstats(io.LimitReader(f, 1<<20)), func(name string) bool {
			return fileExists("/sys/block/" + name + "/device")
		}, false)
		_ = f.Close()
	}

	return cur
}
