package collect

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// mountEntry is one line of /proc/self/mounts, before we decide whether it is worth reporting.
type mountEntry struct{ device, point, fstype string }

// MountPoint is where the filesystem is mounted, with the kernel's octal escaping already undone.
func (m mountEntry) MountPoint() string { return m.point }

// pseudoFilesystems are mounted everywhere and are not disk, so reporting them is noise — a `tmpfs` at
// 100% is a perfectly healthy machine and `/run` is full on plenty of fine servers, so an alert on either
// is one nobody can act on. `tmpfs` is excluded knowing the cost: a `/tmp` mounted on tmpfs really can
// fill and really does break things, and that case is not covered here. Reporting every `/run` and
// `/dev/shm` to catch it would trade a rare miss for constant noise, which is the worse of the two. The list is what a stock Linux install mounts, not an attempt at completeness: anything
// unrecognised is reported, because a filesystem we have not heard of is more likely to be real than not.
var pseudoFilesystems = map[string]bool{
	"autofs": true, "bpf": true, "cgroup": true, "cgroup2": true, "configfs": true,
	"debugfs": true, "devpts": true, "devtmpfs": true, "efivarfs": true, "fuse.gvfsd-fuse": true,
	"fusectl": true, "hugetlbfs": true, "mqueue": true, "nsfs": true, "overlay": true,
	"proc": true, "pstore": true, "ramfs": true, "rpc_pipefs": true, "securityfs": true,
	"selinuxfs": true, "squashfs": true, "sysfs": true, "tmpfs": true, "tracefs": true,
}

// parseMounts reads /proc/self/mounts and keeps the filesystems a person could actually fill.
func parseMounts(r io.Reader) ([]mountEntry, error) {
	var out []mountEntry
	seen := map[string]int{}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}

		device, point, fstype := fields[0], unescapeMount(fields[1]), fields[2]
		if pseudoFilesystems[fstype] {
			continue
		}

		// A mount point can appear twice, and the *last* line is the filesystem actually visible there —
		// anything earlier is shadowed by it. Keeping the first would pair one filesystem's device name
		// with another's statfs numbers, which reads as a plausible row and is simply wrong.
		if i, ok := seen[point]; ok {
			out[i] = mountEntry{device: device, point: point, fstype: fstype}
			continue
		}
		seen[point] = len(out)

		out = append(out, mountEntry{device: device, point: point, fstype: fstype})
	}

	return out, scanner.Err()
}

// unescapeMount undoes the octal escaping the kernel applies to spaces, tabs and backslashes in mount
// paths. Without it a mount at "/mnt/my backup" arrives as "/mnt/my\040backup" and never matches
// anything the operator recognises.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}

	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}

	return b.String()
}

// dedupeMounts drops filesystems already reported under another mount point.
//
// A bind mount is the same filesystem seen twice: it has the same device and identical statfs numbers, so
// reporting both shows one disk as two and clutters the disk card with rows nobody can act on separately.
// Containers make this routine — Docker bind-mounts /etc/hosts, /etc/hostname and /etc/resolv.conf from
// the host filesystem, so an unfiltered agent reports the host's disk four times.
//
// Keyed on device *and* the numbers, not device alone: two partitions of one disk are genuinely separate
// filesystems that happen to share a device prefix, and merging those would hide a full one.
func dedupeMounts(mounts []Mount) []Mount {
	seen := map[string]bool{}
	out := make([]Mount, 0, len(mounts))

	for _, m := range mounts {
		key := fmt.Sprintf("%s|%d|%d", m.Device, m.TotalBytes, m.UsedBytes)
		if m.Device != "" && seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}

	return out
}

// parseProcComm pulls the executable name out of /proc/<pid>/stat.
//
// The name is in parentheses and may itself contain spaces or parentheses — `(Web Content)` is a real
// process name — so this cuts on the *last* ')' rather than splitting on whitespace. Doing it the obvious
// way silently mangles exactly the processes people are most likely to be looking for.
func parseProcComm(stat string) (string, error) {
	open := strings.IndexByte(stat, '(')
	closeIdx := strings.LastIndexByte(stat, ')')
	if open < 0 || closeIdx <= open {
		return "", fmt.Errorf("no comm field")
	}

	comm := strings.TrimSpace(stat[open+1 : closeIdx])
	if comm == "" {
		return "", fmt.Errorf("empty comm")
	}

	return comm, nil
}

// parseStatmRSS reads resident set size, in bytes, from /proc/<pid>/statm.
func parseStatmRSS(statm string, pageSize int64) (int64, error) {
	fields := strings.Fields(statm)
	if len(fields) < 2 {
		return 0, fmt.Errorf("expected at least 2 fields")
	}

	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, err
	}

	return pages * pageSize, nil
}

// atoiStrict accepts only an unsigned decimal, so /proc entries like "self" and "net" are rejected
// rather than parsed as zero.
func atoiStrict(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
	}

	return strconv.Atoi(s)
}

// topByMemory keeps the largest few processes and drops the rest.
//
// Sorted by memory rather than CPU because a single reading cannot measure a process's CPU share — that
// needs two samples per process, which means walking all of /proc twice. Memory is a level, readable in
// one pass, and "what is eating this box" is usually answered by it.
func topByMemory(procs []Process, limit int) []Process {
	if limit <= 0 {
		limit = 10
	}

	sort.SliceStable(procs, func(i, j int) bool {
		var a, b int64
		if procs[i].MemoryBytes != nil {
			a = *procs[i].MemoryBytes
		}
		if procs[j].MemoryBytes != nil {
			b = *procs[j].MemoryBytes
		}
		return a > b
	})

	if len(procs) > limit {
		procs = procs[:limit]
	}

	return procs
}

// baseName returns the last path element, handling both separators.
//
// Not path/filepath.Base: that follows the *host* separator, so a Windows path parsed on a Linux build —
// which is what the tests do — would come back unchanged. Splitting on both is what makes this testable
// off-Windows, which is the only place it can be tested at all here.
func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}

	return p
}

// splitNulUTF16 splits a NUL-separated, NUL-NUL-terminated UTF-16 block into strings.
//
// This is the shape several Windows APIs use to return a list — GetLogicalDriveStringsW among them — and
// getting it wrong yields one string containing every drive letter run together.
func splitNulUTF16(buf []uint16) []string {
	var out []string
	start := 0

	for i, c := range buf {
		if c != 0 {
			continue
		}
		if i > start {
			out = append(out, string(utf16.Decode(buf[start:i])))
		}
		start = i + 1
	}

	return out
}
