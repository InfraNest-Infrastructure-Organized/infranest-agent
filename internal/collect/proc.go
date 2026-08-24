package collect

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The parsers live apart from the file reading on purpose: they take an io.Reader, so they are testable
// against captured /proc output on any machine. Splitting them is also what keeps the platform-specific
// file left in linux.go down to a handful of lines.

// cpuTimes is one row of /proc/stat's aggregate `cpu` line, in USER_HZ ticks.
type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

// busy is the time this machine spent doing work *it* asked for.
//
// Deliberately not `total - idle`. That includes iowait (waiting on a disk, not using a CPU) and steal —
// CPU the hypervisor handed to another tenant. On a shared vCPU steal is routinely most of the apparent
// load, and reporting it as ours produced alerts nobody could act on, because nothing running on this
// machine caused it and nothing running on this machine can fix it.
func (c cpuTimes) busy() uint64 { return c.user + c.nice + c.system + c.irq + c.softirq }

// total is every tick accounted for, including the ones we do not count as ours.
func (c cpuTimes) total() uint64 {
	return c.busy() + c.idle + c.iowait + c.steal
}

// parseStat reads the aggregate `cpu` line from /proc/stat.
func parseStat(r io.Reader) (cpuTimes, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || fields[0] != "cpu" {
			continue
		}

		// user nice system idle iowait irq softirq steal — steal is field 8 and may be absent on older
		// kernels, which is why the loop below tolerates a short line.
		var v [8]uint64
		for i := 0; i < 8 && i+1 < len(fields); i++ {
			n, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return cpuTimes{}, fmt.Errorf("cpu field %d: %w", i, err)
			}
			v[i] = n
		}

		return cpuTimes{
			user: v[0], nice: v[1], system: v[2], idle: v[3],
			iowait: v[4], irq: v[5], softirq: v[6], steal: v[7],
		}, nil
	}

	if err := scanner.Err(); err != nil {
		return cpuTimes{}, err
	}

	return cpuTimes{}, fmt.Errorf("no aggregate cpu line")
}

// cpuPercent turns two readings into the share of wall time this machine spent busy.
func cpuPercent(before, after cpuTimes) (float64, error) {
	total := after.total() - before.total()
	if after.total() < before.total() {
		// Counters only go up. A decrease means the machine rebooted between samples, or /proc lied.
		return 0, fmt.Errorf("cpu counters went backwards")
	}
	if total == 0 {
		return 0, fmt.Errorf("no time elapsed between cpu samples")
	}

	busy := float64(after.busy()-before.busy()) / float64(total) * 100
	return clampPercent(busy), nil
}

// parseMeminfo reads /proc/meminfo, returning bytes.
//
// "Used" is total minus MemAvailable, not total minus MemFree. MemFree counts the page cache as used,
// which makes a healthy Linux box look permanently full — the number people mean by "memory used" is
// what cannot be reclaimed, and the kernel publishes exactly that.
func parseMeminfo(r io.Reader) (used, total, swapUsed int64, err error) {
	vals := map[string]int64{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		n, convErr := strconv.ParseInt(fields[0], 10, 64)
		if convErr != nil {
			continue
		}
		// Everything in /proc/meminfo we care about is in kB.
		vals[key] = n * 1024
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, 0, err
	}

	total, ok := vals["MemTotal"]
	if !ok || total <= 0 {
		return 0, 0, 0, fmt.Errorf("MemTotal missing")
	}

	available, ok := vals["MemAvailable"]
	if !ok {
		// Kernels before 3.14 have no MemAvailable. Free plus the reclaimable caches is the same idea,
		// approximately, and approximately right beats absent.
		available = vals["MemFree"] + vals["Buffers"] + vals["Cached"]
	}

	used = total - available
	if used < 0 {
		used = 0
	}

	swapUsed = vals["SwapTotal"] - vals["SwapFree"]
	if swapUsed < 0 {
		swapUsed = 0
	}

	return used, total, swapUsed, nil
}

// parseLoadavg reads the 1, 5 and 15 minute load averages from /proc/loadavg.
func parseLoadavg(r io.Reader) (one, five, fifteen float64, err error) {
	data, err := io.ReadAll(io.LimitReader(r, 256))
	if err != nil {
		return 0, 0, 0, err
	}

	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("expected at least 3 fields, got %d", len(fields))
	}

	out := [3]float64{}
	for i := range out {
		if out[i], err = strconv.ParseFloat(fields[i], 64); err != nil {
			return 0, 0, 0, fmt.Errorf("load field %d: %w", i, err)
		}
	}

	return out[0], out[1], out[2], nil
}

// parseUptime reads whole seconds since boot from /proc/uptime.
func parseUptime(r io.Reader) (int64, error) {
	data, err := io.ReadAll(io.LimitReader(r, 128))
	if err != nil {
		return 0, err
	}

	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty")
	}

	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	if seconds < 0 {
		return 0, fmt.Errorf("negative uptime")
	}

	return int64(seconds), nil
}

func clampPercent(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		// Rounding across a sampling boundary can land a hair over. The server rejects anything above
		// 100 outright, so a reading that would cost the whole push is worth clamping here.
		return 100
	default:
		return v
	}
}
