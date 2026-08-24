//go:build linux

package collect

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

// Options is what the operator chose. Everything here narrows what is collected; there is deliberately no
// option that widens what the agent is allowed to do.
type Options struct {
	// Processes turns the process list on. Off by default: command lines routinely carry credentials.
	Processes bool
	// ProcessArgs ships full command lines rather than the executable name. Off by default, and the
	// documentation says plainly what turning it on means.
	ProcessArgs bool
	// MaxProcesses bounds the list. The server caps it too; sending more just wastes both ends.
	MaxProcesses int
	// CPUInterval is how long to wait between the two /proc/stat readings a percentage needs.
	CPUInterval time.Duration
}

// Collect takes one reading.
//
// Every collector is independent and its failure is recorded rather than raised: an agent that gives up
// because it could not stat one mount takes the alerting down with it, which is worse than the gap it was
// avoiding. The only error returned is one that makes the whole sample meaningless.
func Collect(opts Options) (Sample, error) {
	s := Sample{CollectedAt: time.Now().UTC()}

	if pct, err := collectCPU(opts.CPUInterval); err != nil {
		s.fail("cpu", err)
	} else {
		s.CPUPercent = f64(pct)
	}

	if used, total, swap, err := readMeminfo(); err != nil {
		s.fail("memory", err)
	} else {
		s.MemoryUsed, s.MemoryTotal, s.SwapUsed = i64(used), i64(total), i64(swap)
		s.MemoryPercent = f64(clampPercent(float64(used) / float64(total) * 100))
	}

	if one, five, fifteen, err := readLoadavg(); err != nil {
		s.fail("load", err)
	} else {
		s.Load1, s.Load5, s.Load15 = f64(one), f64(five), f64(fifteen)
	}

	if up, err := readUptime(); err != nil {
		s.fail("uptime", err)
	} else {
		s.Uptime = i64(up)
	}

	if mounts, err := readMounts(); err != nil {
		s.fail("mounts", err)
	} else {
		s.Mounts = mounts
	}

	if opts.Processes {
		if procs, err := readProcesses(opts); err != nil {
			s.fail("processes", err)
		} else {
			s.Processes = procs
		}
	}

	return s, nil
}

func collectCPU(interval time.Duration) (float64, error) {
	if interval <= 0 {
		interval = 300 * time.Millisecond
	}

	before, err := readStat()
	if err != nil {
		return 0, err
	}

	time.Sleep(interval)

	after, err := readStat()
	if err != nil {
		return 0, err
	}

	return cpuPercent(before, after)
}

func readStat() (cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()

	return parseStat(f)
}

func readMeminfo() (used, total, swap int64, err error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()

	return parseMeminfo(f)
}

func readLoadavg() (one, five, fifteen float64, err error) {
	f, err := os.Open("/proc/loadavg")
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()

	return parseLoadavg(f)
}

func readUptime() (int64, error) {
	f, err := os.Open("/proc/uptime")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	return parseUptime(f)
}

// readMounts reports the real filesystems, skipping the ones nobody can fill.
//
// Each mount is measured in its own goroutine behind a deadline, which is not defensive programming for
// its own sake: `statfs` on a hard-mounted NFS or CIFS share whose server has gone away does not return an
// error, it blocks in uninterruptible sleep, forever. Measured inline, one dead share would hang the whole
// collection — no CPU, no memory, no other mount — and every metric on the machine would go dark because
// of a filesystem nobody was even alerting on. The agent going quiet then looks exactly like the server
// dying, which is the alert this product sells.
//
// A goroutine stuck in a syscall cannot be cancelled, so the ones that never return are abandoned rather
// than waited for. That leaks a goroutine per dead mount per cycle, which is why `mountsSeen` remembers
// them: a share that timed out once is skipped from then on, so the leak is bounded by the number of
// broken mounts rather than growing with every sample.
func readMounts() ([]Mount, error) {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	entries, err := parseMounts(f)
	if err != nil {
		return nil, err
	}

	live := make([]mountEntry, 0, len(entries))
	for _, e := range entries {
		if unresponsiveMounts.blocked(e.point) {
			continue
		}
		live = append(live, e)
	}

	out, unanswered := probeMounts(live, statMount, mountTimeout)
	for _, point := range unanswered {
		// Remember it, so the next cycle does not start another goroutine against the same dead share.
		// A goroutine blocked in a syscall cannot be cancelled, so this is what bounds the leak to the
		// number of broken mounts rather than letting it grow with every sample.
		unresponsiveMounts.block(point)
	}

	return dedupeMounts(out), nil
}

// statMount measures one filesystem. Returns false for anything that is not a fillable disk.
//
// os.Stat is inside here with the statfs on purpose: on a hard-mounted share whose server has gone away,
// os.Stat blocks in uninterruptible sleep exactly as statfs does, so leaving it outside the timeout would
// reintroduce the hang this whole structure exists to prevent.
func statMount(e mountEntry) (Mount, bool) {
	// A bind-mounted *file* is not a filesystem anyone can fill — it reports the statfs of whatever holds
	// it. Docker mounts /etc/hosts, /etc/hostname and /etc/resolv.conf this way, so without this check a
	// container reports the host's disk once per file.
	if info, err := os.Stat(e.point); err != nil || !info.IsDir() {
		return Mount{}, false
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(e.point, &st); err != nil {
		return Mount{}, false
	}

	blockSize := int64(st.Bsize)
	total := int64(st.Blocks) * blockSize
	if total <= 0 {
		return Mount{}, false
	}

	// Free-for-unprivileged, not total-free: the blocks reserved for root are not space anything else can
	// use, so counting them as free understates how full the disk is to everyone who is not root — which
	// is everyone who will hit the wall first.
	avail := int64(st.Bavail) * blockSize

	return Mount{
		MountPoint: e.point,
		Device:     e.device,
		TotalBytes: total,
		UsedBytes:  total - avail,
	}, true
}

// readProcesses walks /proc for the busiest few.
//
// It reads only what it needs: the executable name from /proc/<pid>/stat, and resident memory from
// /proc/<pid>/statm. Command-line arguments are read only when the operator asked for them.
func readProcesses(opts Options) ([]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	pageSize := int64(os.Getpagesize())
	procs := make([]Process, 0, 64)

	for _, entry := range entries {
		pid, err := atoiStrict(entry.Name())
		if err != nil {
			continue // not a process directory
		}

		statBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue // exited between the listing and the read; entirely normal
		}

		comm, err := parseProcComm(string(statBytes))
		if err != nil {
			continue
		}

		p := Process{Command: comm, PID: pid}

		if rss, err := readRSS(pid, pageSize); err == nil {
			p.MemoryBytes = i64(rss)
		}

		if opts.ProcessArgs {
			if args, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
				if full := strings.TrimSpace(strings.ReplaceAll(string(args), "\x00", " ")); full != "" {
					p.Command = full
				}
			}
		}

		procs = append(procs, p)
	}

	return topByMemory(procs, opts.MaxProcesses), nil
}

func readRSS(pid int, pageSize int64) (int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0, err
	}

	return parseStatmRSS(string(data), pageSize)
}
