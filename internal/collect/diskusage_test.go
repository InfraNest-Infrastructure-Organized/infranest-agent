package collect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
