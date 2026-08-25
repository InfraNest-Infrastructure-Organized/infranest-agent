package collect

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DirUsage is one directory and how much it holds, for "what is filling this mount" (#775).
type DirUsage struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	// A name for what this is, when we recognise it — "journal logs", "docker images". The whole value of
	// the answer is in this field: "/var/lib/docker is 34 GB" is a fact, and "docker images account for
	// 34 GB" is something somebody can act on without first working out what that path is.
	Kind string `json:"kind,omitempty"`
}

// UsageScan is a bounded walk of one mount. `Partial` is the honest half.
type UsageScan struct {
	MountPoint string     `json:"mount_point"`
	Dirs       []DirUsage `json:"dirs"`
	ScannedAt  time.Time  `json:"scanned_at"`
	// True when the walk hit its time budget or depth limit before finishing. A partial answer presented
	// as complete would have somebody clearing the second-largest directory while the largest was never
	// looked at.
	Partial bool `json:"partial"`
}

// Bounds on the walk. A monitoring agent must not become the reason a server falls over, and walking a
// filesystem is the most expensive thing this program ever does — so it is the one that needs a leash.
const (
	// Deep enough for /var/lib/docker and /var/log/journal, shallow enough that a source tree with
	// thousands of nested directories does not turn into millions of stat calls.
	usageMaxDepth = 4

	// The whole scan, not per directory. Held to a few seconds because this runs on somebody's production
	// server and the disk it is measuring is a disk something else is trying to use.
	usageBudget = 15 * time.Second

	// How many directories are worth reporting. Past the top few the answer stops being actionable — the
	// tail of a filesystem is always thousands of small things.
	usageTopN = 8
)

// skipDirs never contain anything worth reporting and can be enormous or infinite.
//
// The pseudo-filesystems are the important ones: /proc contains a directory per process and walking it is
// both meaningless and slow, and /sys can recurse effectively forever through symlinked device trees.
var skipDirs = map[string]bool{
	"/proc": true, "/sys": true, "/dev": true, "/run": true,
}

// knownKinds names the usual suspects.
//
// "What is filling this disk" is nearly always one of about six things, and naming it is the difference
// between a graph and an action. Matched on a path prefix rather than guessed from size, because being
// wrong here would be worse than saying nothing — somebody would go and delete it.
var knownKinds = []struct {
	prefix string
	kind   string
}{
	{"/var/log/journal", "journal logs"},
	{"/var/log", "logs"},
	{"/var/lib/docker", "docker images and containers"},
	{"/var/lib/containers", "container images"},
	{"/var/cache/apt", "apt package cache"},
	{"/var/cache/yum", "yum package cache"},
	{"/var/cache/dnf", "dnf package cache"},
	{"/var/lib/postgresql", "postgres data"},
	{"/var/lib/mysql", "mysql data"},
	{"/var/backups", "backups"},
	{"/home", "home directories"},
	{"/tmp", "temporary files"},
}

func kindFor(path string) string {
	for _, k := range knownKinds {
		if path == k.prefix || strings.HasPrefix(path, k.prefix+"/") {
			return k.kind
		}
	}

	return ""
}

// ScanUsage walks one mount and reports the largest directories under it.
//
// Deliberately **not** part of a collection cycle. This is minutes of work in the worst case where a
// sample is milliseconds, and running it every twenty seconds would make the agent the heaviest thing on
// the machine. The caller schedules it — hourly at most — and sends the result separately.
//
// Sizes are apparent file sizes summed per directory, not `du`'s block accounting. The difference matters
// for sparse files and small-file overhead, and the question being answered is "which of these is worth
// looking at", where a few percent does not change the answer.
func ScanUsage(ctx context.Context, mountPoint string) UsageScan {
	scan := UsageScan{MountPoint: mountPoint, ScannedAt: time.Now().UTC()}

	deadline, cancel := context.WithTimeout(ctx, usageBudget)
	defer cancel()

	// Bytes directly under each directory at or above the reporting depth, keyed by path. Children roll
	// up into the ancestor at `usageMaxDepth`, so a deep tree is still attributed to something nameable
	// rather than vanishing because nobody walked far enough.
	totals := map[string]int64{}

	root := filepath.Clean(mountPoint)
	rootDepth := strings.Count(root, string(os.PathSeparator))

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory we cannot read is not a failure of the scan. The agent runs unprivileged on
			// purpose, so some of the filesystem is legitimately closed to it — reporting nothing at all
			// because one directory said no would be the wrong trade.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}

			return nil
		}

		select {
		case <-deadline.Done():
			scan.Partial = true

			return filepath.SkipAll
		default:
		}

		if d.IsDir() {
			if skipDirs[path] {
				return fs.SkipDir
			}
			// Another filesystem mounted underneath this one. Its usage belongs to *it*, and counting it
			// here would attribute a full /var/lib/docker to whatever happens to be mounted above it.
			if path != root && isOtherFilesystem(root, path) {
				return fs.SkipDir
			}

			return nil
		}

		if !d.Type().IsRegular() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		totals[bucketFor(path, rootDepth)] += info.Size()

		return nil
	})

	if err != nil {
		scan.Partial = true
	}

	scan.Dirs = topDirs(totals, usageTopN)

	return scan
}

// bucketFor is the directory a file's bytes are attributed to: its own, or the ancestor at the depth
// limit. Rolling up rather than dropping means a file twelve levels down still lands somewhere a human
// recognises instead of being silently uncounted.
func bucketFor(path string, rootDepth int) string {
	dir := filepath.Dir(path)

	for strings.Count(dir, string(os.PathSeparator))-rootDepth > usageMaxDepth {
		dir = filepath.Dir(dir)
	}

	return dir
}

// topDirs picks the largest N, biggest first, and names the ones we recognise.
func topDirs(totals map[string]int64, n int) []DirUsage {
	dirs := make([]DirUsage, 0, len(totals))
	for path, bytes := range totals {
		dirs = append(dirs, DirUsage{Path: path, Bytes: bytes, Kind: kindFor(path)})
	}

	sort.Slice(dirs, func(i, j int) bool {
		if dirs[i].Bytes != dirs[j].Bytes {
			return dirs[i].Bytes > dirs[j].Bytes
		}

		// Ties broken by path so two runs on an unchanged filesystem produce the same list. A table that
		// reshuffles between refreshes reads as movement that is not there.
		return dirs[i].Path < dirs[j].Path
	})

	if len(dirs) > n {
		dirs = dirs[:n]
	}

	return dirs
}
