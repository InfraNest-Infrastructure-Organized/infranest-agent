package collect

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

/*
What is filling this mount (#775).

The walk itself is barely worth testing. What is worth testing is that the answer is **actionable**: that
it names the thing rather than a path, that a deep tree still lands somewhere a human recognises rather
than vanishing at the depth limit, and that a partial scan says so — because a partial answer presented as
complete has somebody clearing the second-largest directory while the largest was never looked at.
*/

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTheBiggestDirectoriesComeFirst(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "small/a.log"), 100)
	write(t, filepath.Join(root, "big/b.log"), 5000)
	write(t, filepath.Join(root, "medium/c.log"), 1000)

	scan := ScanUsage(context.Background(), root)

	if len(scan.Dirs) < 3 {
		t.Fatalf("expected three directories, got %d", len(scan.Dirs))
	}
	if !strings.HasSuffix(scan.Dirs[0].Path, "/big") {
		t.Fatalf("largest first, got %q", scan.Dirs[0].Path)
	}
	if scan.Dirs[0].Bytes != 5000 {
		t.Fatalf("size wrong: %d", scan.Dirs[0].Bytes)
	}
}

func TestADeepTreeRollsUpRatherThanVanishing(t *testing.T) {
	// A file twelve levels down must still be counted. Dropping what is past the depth limit would make a
	// deeply nested directory look empty, which is the worst possible answer: it is exactly the case
	// somebody is trying to find.
	root := t.TempDir()
	deep := filepath.Join(root, "top", "a", "b", "c", "d", "e", "f", "g")
	write(t, filepath.Join(deep, "buried.bin"), 4096)

	scan := ScanUsage(context.Background(), root)

	var total int64
	for _, d := range scan.Dirs {
		total += d.Bytes
	}

	if total != 4096 {
		t.Fatalf("buried bytes were lost: counted %d of 4096", total)
	}
	for _, d := range scan.Dirs {
		if strings.Count(strings.TrimPrefix(d.Path, root), "/") > usageMaxDepth {
			t.Fatalf("reported past the depth limit: %s", d.Path)
		}
	}
}

func TestItNamesTheUsualSuspects(t *testing.T) {
	// "/var/lib/docker is 34 GB" is a fact. "docker images account for 34 GB" is something somebody can
	// act on without first working out what that path is — that difference is the whole feature.
	cases := map[string]string{
		"/var/log/journal":         "journal logs",
		"/var/log/nginx":           "logs",
		"/var/lib/docker/overlay2": "docker images and containers",
		"/var/cache/apt/archives":  "apt package cache",
		"/home/jaspar":             "home directories",
		"/srv/something":           "",
	}

	for path, want := range cases {
		if got := kindFor(path); got != want {
			t.Errorf("kindFor(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestAPrefixMatchDoesNotCatchASimilarlyNamedDirectory(t *testing.T) {
	// `/var/logical` is not `/var/log`. Being wrong here is worse than saying nothing, because somebody
	// would go and delete it.
	if got := kindFor("/var/logical"); got != "" {
		t.Fatalf("matched a similarly named path: %q", got)
	}
}

func TestAnExhaustedBudgetSaysSo(t *testing.T) {
	// A partial answer presented as complete has somebody clearing the second-largest directory while the
	// largest was never looked at.
	root := t.TempDir()
	write(t, filepath.Join(root, "a.log"), 10)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if scan := ScanUsage(ctx, root); !scan.Partial {
		t.Fatal("a cancelled scan reported itself as complete")
	}
}

func TestAnUnreadableDirectoryDoesNotAbandonTheScan(t *testing.T) {
	// The agent runs unprivileged on purpose, so parts of the filesystem are legitimately closed to it.
	// Reporting nothing because one directory said no would be the wrong trade.
	if os.Geteuid() == 0 {
		t.Skip("running as root — a 0000 directory is still readable")
	}

	root := t.TempDir()
	write(t, filepath.Join(root, "visible/a.log"), 2048)

	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	scan := ScanUsage(context.Background(), root)

	var total int64
	for _, d := range scan.Dirs {
		total += d.Bytes
	}
	if total != 2048 {
		t.Fatalf("the readable half was lost: %d", total)
	}
}

func TestTheOrderIsStableBetweenRuns(t *testing.T) {
	// A table that reshuffles between refreshes reads as movement that is not there.
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		write(t, filepath.Join(root, name, "f.bin"), 1000)
	}

	first := ScanUsage(context.Background(), root)
	second := ScanUsage(context.Background(), root)

	for i := range first.Dirs {
		if first.Dirs[i].Path != second.Dirs[i].Path {
			t.Fatalf("order changed between runs at %d: %q vs %q", i, first.Dirs[i].Path, second.Dirs[i].Path)
		}
	}
}

func TestAnUnreadableDirectoryIsNamed(t *testing.T) {
	// The whole point of #900. The old scan skipped a refused directory silently, so a stock Docker host
	// reported a quarter of its disk and presented it as the whole picture — the breakdown pointed at a
	// swapfile while twelve gigabytes sat in root-only /var/lib/docker, which it never mentioned.
	if os.Geteuid() == 0 {
		t.Skip("running as root — a 0000 directory is still readable")
	}

	root := t.TempDir()
	write(t, filepath.Join(root, "visible/a.log"), 2048)

	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	scan := ScanUsage(context.Background(), root)

	if len(scan.Unreadable) != 1 || scan.Unreadable[0].Path != locked {
		t.Fatalf("the refused directory was not named: %+v", scan.Unreadable)
	}
	if scan.Unreadable[0].Mode != "0" {
		t.Fatalf("mode not reported: %q", scan.Unreadable[0].Mode)
	}

	// Refused is not the same claim as ran-out-of-time, and they must not share a flag: one says "there
	// may be more of the same", the other says "there is definitely more, and here is where".
	if scan.Partial {
		t.Fatal("a refused directory was reported as an exhausted budget")
	}
}

func TestOnlyTheShallowestRefusalIsNamed(t *testing.T) {
	// A refused tree contains thousands of equally refused children. /var/lib/docker is the name somebody
	// recognises; ten thousand overlay2 hashes are noise that would bury it.
	if os.Geteuid() == 0 {
		t.Skip("running as root — a 0000 directory is still readable")
	}

	root := t.TempDir()
	outer := filepath.Join(root, "outer")
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outer, 0o755) })

	scan := UsageScan{}
	scan.noteUnreadable(outer)
	scan.noteUnreadable(inner)

	if len(scan.Unreadable) != 1 || scan.Unreadable[0].Path != outer {
		t.Fatalf("a child of a refused directory was reported separately: %+v", scan.Unreadable)
	}
}

func TestTheListOfRefusalsIsCapped(t *testing.T) {
	scan := UsageScan{}
	for i := range maxUnreadableDirs * 3 {
		scan.noteUnreadable(filepath.Join("/x", strconv.Itoa(i)))
	}

	if len(scan.Unreadable) != maxUnreadableDirs {
		t.Fatalf("the cap did not hold: %d", len(scan.Unreadable))
	}
}

func TestEverythingCountedIsReportedNotOnlyTheTopN(t *testing.T) {
	// The server subtracts this from what `statfs` says the mount is using, and the difference is the
	// honest size of what the agent could not see. Summing only the reported directories would make that
	// difference include everything that merely lost the ranking, and the unaccounted figure would be
	// nonsense on any machine with more than usageTopN directories.
	root := t.TempDir()
	for i := range usageTopN + 5 {
		write(t, filepath.Join(root, strconv.Itoa(i), "f.bin"), 1000)
	}

	scan := ScanUsage(context.Background(), root)

	if len(scan.Dirs) != usageTopN {
		t.Fatalf("expected the ranking to be truncated, got %d", len(scan.Dirs))
	}
	if want := int64((usageTopN + 5) * 1000); scan.AccountedBytes != want {
		t.Fatalf("accounted %d, want %d", scan.AccountedBytes, want)
	}
}

func TestARegularFileOnTheMountItselfIsCounted(t *testing.T) {
	// A swapfile or a disk image sitting directly on the mount is a genuine answer to "what is filling
	// this", and it has no directory to be attributed to but the mount point.
	root := t.TempDir()
	write(t, filepath.Join(root, "swapfile"), 4096)

	scan := ScanUsage(context.Background(), root)

	if scan.AccountedBytes != 4096 {
		t.Fatalf("a file on the mount root was not counted: %d", scan.AccountedBytes)
	}
}

func TestTheBudgetIsSharedBetweenTopLevelDirectoriesNotRacedAsOneClock(t *testing.T) {
	// One clock is alphabetical amputation: /bin through /usr get counted in full and /var — the directory
	// actually filling the disk on nearly every server — is never reached, and the result is labelled
	// merely "partial". A share each makes a truncated answer uniformly thin instead.
	//
	// Asserted on the arithmetic rather than by racing a real walk. The racing version failed on CI and
	// passed locally, for a reason that is not a bug: at the budgets that make a walk truncate, the clock
	// expires before the first ReadDir returns, and *every* strategy — shared or not — scans only the
	// first directory. There is no wall-clock scale at which the difference is reliably observable.
	if got := usageShare(6*time.Second, 6); got != time.Second {
		t.Fatalf("six directories should get a second each, got %s", got)
	}

	// The last one gets everything still unspent, not a sixth of the original budget: five directories
	// that finished quickly hand their slots back rather than leaving the sixth with a token share.
	if got := usageShare(5*time.Second, 1); got != 5*time.Second {
		t.Fatalf("the last directory should get what is left, got %s", got)
	}

	// Nothing left to walk. Dividing by zero here would panic on a mount whose entries were all skipped.
	if got := usageShare(time.Second, 0); got != 0 {
		t.Fatalf("no directories left should get no time, got %s", got)
	}
}

func TestEveryTopLevelDirectoryIsReachedWhenThereIsTimeForAll(t *testing.T) {
	// The other half of the split budget: with time to spare, sharing it must not leave anyone out. A
	// share that came back zero, or a loop that stopped at the first subtree, would look exactly like a
	// scan that ran out of time — and would be silently wrong on every machine rather than only on busy
	// ones.
	root := t.TempDir()
	names := []string{"a", "b", "c", "d", "e", "f"}
	for _, name := range names {
		write(t, filepath.Join(root, name, "f.bin"), 1000)
	}

	scan := ScanUsage(context.Background(), root)

	if scan.Partial {
		t.Fatal("a walk of six small directories should not run out of budget")
	}
	if len(scan.Dirs) != len(names) {
		t.Fatalf("not every directory was reached: %+v", scan.Dirs)
	}
}

func TestARefusedMountRootIsNamedToo(t *testing.T) {
	// The early return when the mount root itself cannot be listed. Rarer than a refused subdirectory,
	// but the same claim, and returning an empty breakdown with no explanation is exactly the silence
	// this change exists to remove.
	if os.Geteuid() == 0 {
		t.Skip("running as root — a 0000 directory is still readable")
	}

	parent := t.TempDir()
	root := filepath.Join(parent, "locked")
	if err := os.MkdirAll(root, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	scan := ScanUsage(context.Background(), root)

	if len(scan.Unreadable) != 1 || scan.Unreadable[0].Path != root {
		t.Fatalf("the refused mount root was not named: %+v", scan.Unreadable)
	}
}
