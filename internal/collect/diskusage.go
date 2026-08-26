package collect

import (
	"context"
	"errors"
	"fmt"
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

// UsageScan is a bounded walk of one mount. Everything after `Dirs` is the honest half.
type UsageScan struct {
	MountPoint string     `json:"mount_point"`
	Dirs       []DirUsage `json:"dirs"`
	ScannedAt  time.Time  `json:"scanned_at"`
	// True when the walk hit its time budget before finishing. A partial answer presented as complete
	// would have somebody clearing the second-largest directory while the largest was never looked at.
	Partial bool `json:"partial"`

	// Everything the walk did count, whether or not it made the top N. The server subtracts this from
	// what `statfs` reports for the mount, and the difference is the honest size of what we could not
	// see — which needs no privilege to compute and is otherwise invisible.
	//
	// On a stock Docker host the difference is most of the disk: /var/lib/docker is `drwx--x---
	// root:root`, so an unprivileged walk is refused at the door and `du` behaves the same way — it
	// prints 4.0K for a twelve-gigabyte tree, sends the reason to stderr, and exits successfully.
	AccountedBytes int64 `json:"accounted_bytes"`

	// Directories the walk was refused, shallowest first. Not sizes — there is no unprivileged way to
	// learn the size of a directory you cannot traverse, and pretending otherwise is what this whole
	// change exists to stop. Naming them is enough to act on: "15.9 GB unaccounted for, and
	// /var/lib/docker is root-only" tells somebody exactly where to look.
	Unreadable []UnreadableDir `json:"unreadable,omitempty"`
}

// UnreadableDir is a directory the agent could see but not enter.
//
// The metadata is available even when the contents are not: `stat` needs execute permission on the
// *parent*, not on the target. So a `drwx--x---` directory can be named precisely, with its owner and
// mode, which is far more actionable than "3 directories could not be read".
type UnreadableDir struct {
	Path string `json:"path"`
	// Octal, as somebody would read it in `ls -l` — 0710 for /var/lib/docker on a stock install.
	Mode string `json:"mode,omitempty"`
	// True when the directory is owned by uid 0, which is the usual reason and the one that tells an
	// operator this is by design rather than a mistake in their own permissions.
	RootOwned bool `json:"root_owned,omitempty"`
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

	// How many refused directories are worth naming. The point is to name the ones somebody recognises —
	// /var/lib/docker, /root — not to enumerate a tree we were locked out of.
	maxUnreadableDirs = 8
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

	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			scan.noteUnreadable(root)
		}
		scan.Partial = true

		return scan
	}

	// Top-level directories get an equal share of the remaining budget each, rather than one walk racing
	// a single clock from / to /var. One clock is alphabetical amputation: /bin through /usr get counted
	// in full and /var — the directory that is actually filling the disk on nearly every server — is
	// never reached, and the result is labelled merely "partial". A share each means a scan that runs out
	// of time is uniformly thin instead, which is wrong in a way that still points the right way.
	//
	// Recomputed per subtree, so a directory that finishes in milliseconds hands the rest of its share
	// back. In the common case where nothing times out, this is identical to one budget for the walk.
	var subtrees []string

	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())

		if entry.IsDir() {
			if !skipDirs[path] && !isOtherFilesystem(root, path) {
				subtrees = append(subtrees, path)
			}

			continue
		}

		// A regular file directly on the mount — a swapfile or a disk image, which is a genuine answer to
		// "what is filling this" and belongs to the mount point itself.
		if entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				totals[root] += info.Size()
			}
		}
	}

	for i, subtree := range subtrees {
		if deadline.Err() != nil {
			scan.Partial = true

			break
		}

		scan.walk(deadline, usageShare(time.Until(mustDeadline(deadline)), len(subtrees)-i), root, subtree, rootDepth, totals)
	}

	// Asked once at the end rather than only inside the walk. A mount with no subdirectories to walk —
	// or one whose subtrees all finished — never consults the clock otherwise, and would report an
	// already-cancelled scan as complete.
	if deadline.Err() != nil {
		scan.Partial = true
	}

	scan.Dirs = topDirs(totals, usageTopN)

	// Everything counted, not only what made the top N. The server subtracts this from `statfs` to get
	// the size of what we could not see — the one number here that needs no privilege and that nothing
	// else can provide.
	for _, bytes := range totals {
		scan.AccountedBytes += bytes
	}

	return scan
}

// usageShare is how long one top-level directory gets out of what is left.
//
// Its own function because it is the whole of the change and the only part that can be checked without
// racing a clock: an integration test at this scale is a coin toss on a loaded CI runner, where the
// budget expires before the first `ReadDir` returns and *every* strategy scans only the first directory.
//
// `left` is how many directories still have to be walked, including this one, so the arithmetic is
// remaining ÷ remaining-directories rather than a slice decided up front. Recomputing it means a directory
// that finishes in milliseconds hands its unused share to the ones after it.
func usageShare(remaining time.Duration, left int) time.Duration {
	if left <= 0 {
		return 0
	}

	return remaining / time.Duration(left)
}

// mustDeadline is the deadline of a context that was built with one. `WithTimeout` always sets it; the
// fallback exists only so a future caller passing a plain context degrades to "no time left" rather than
// to a zero time in the past, which would make every share negative.
func mustDeadline(ctx context.Context) time.Time {
	if at, ok := ctx.Deadline(); ok {
		return at
	}

	return time.Now()
}

// walk sums one subtree into `totals`, bounded by both the overall deadline and this subtree's share.
func (s *UsageScan) walk(overall context.Context, share time.Duration, root, subtree string, rootDepth int, totals map[string]int64) {
	if share <= 0 {
		s.Partial = true

		return
	}

	deadline, cancel := context.WithTimeout(overall, share)
	defer cancel()

	err := filepath.WalkDir(subtree, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory we cannot read is not a failure of the scan. The agent runs unprivileged on
			// purpose, so some of the filesystem is legitimately closed to it — reporting nothing at all
			// because one directory said no would be the wrong trade.
			//
			// It was, however, being skipped *silently*, and that is the trade nobody made: on a stock
			// Docker host /var/lib/docker is root-only, so the breakdown covered a quarter of the disk
			// and presented it as the whole picture — pointing at a swapfile while twelve gigabytes sat
			// somewhere it never mentioned. Recorded now, so the answer can say what it leaves out.
			if d != nil && d.IsDir() {
				if errors.Is(err, fs.ErrPermission) {
					s.noteUnreadable(path)
				}

				return fs.SkipDir
			}

			return nil
		}

		select {
		case <-deadline.Done():
			s.Partial = true

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
		s.Partial = true
	}
}

// noteUnreadable records a directory the walk was refused, with the metadata that *is* readable.
//
// `stat` needs execute permission on the parent rather than on the target, so a `drwx--x---` directory
// can still be named with its owner and mode even though its contents cannot be listed. That turns
// "3 directories could not be read" into "/var/lib/docker, owned by root, mode 0710", which is the
// difference between a warning and somewhere to look.
//
// Shallowest-first and capped: a refused tree can contain thousands of equally refused children, and a
// list of overlay2 hashes is noise. In practice the walk stops at the first refusal on any branch, so
// this stays short on its own.
func (s *UsageScan) noteUnreadable(path string) {
	if len(s.Unreadable) >= maxUnreadableDirs {
		return
	}

	// Already covered by an ancestor we were refused: report the shallowest, which is the one with a
	// name somebody recognises.
	for _, seen := range s.Unreadable {
		if strings.HasPrefix(path, seen.Path+string(os.PathSeparator)) {
			return
		}
	}

	dir := UnreadableDir{Path: path}

	// Best effort. If even the metadata is unavailable the path alone is still worth reporting — it is
	// the part that tells somebody where to look.
	if info, err := os.Lstat(path); err == nil {
		dir.Mode = fmt.Sprintf("%#o", info.Mode().Perm())
		dir.RootOwned = isRootOwned(info)
	}

	s.Unreadable = append(s.Unreadable, dir)
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
		// A directory path is arbitrary bytes chosen by whoever created it, and nesting has no bound.
		// Clipped to what the ingest accepts rather than left to fail validation for the whole push.
		dirs = append(dirs, DirUsage{
			Path:  clip(path, maxUsagePath),
			Bytes: bytes,
			Kind:  clip(kindFor(path), maxUsageKind),
		})
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
