package collect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An over-long unreadable path must not reach the payload (#900 follow-up).
//
// The failure it prevents is the worst one this package has: a string over the ingest's limit does not
// cost its own field, it fails validation for the whole push. A rejected push carries no per-reading
// verdict, so the batch never settles, stays at the head of the spool, and is offered again for ever with
// every later reading queued behind it — monitoring stops, on a machine nobody would think to look at.
func TestAnUnreadableDirIsClippedToWhatTheIngestAccepts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is refused nothing, so there is no refusal to record")
	}

	root := t.TempDir()

	// Four levels of long-but-ordinary names: the shape of a cache or container overlay tree, and well
	// past 512 bytes without anything unusual in it.
	deep := root
	for i := 0; i < 4; i++ {
		deep = filepath.Join(deep, strings.Repeat("d", 150))
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Skipf("cannot build the tree: %v", err)
	}
	if err := os.Chmod(deep, 0o000); err != nil {
		t.Skipf("cannot chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(deep, 0o755) })

	scan := ScanUsage(context.Background(), root)

	if len(scan.Unreadable) == 0 {
		t.Skip("no refusal recorded on this filesystem")
	}
	for _, u := range scan.Unreadable {
		if len(u.Path) > maxUsagePath {
			t.Fatalf("a %d-byte path is being sent; the ingest caps it at %d and rejects the whole push",
				len(u.Path), maxUsagePath)
		}
	}
}

// Clipping happens at the end, so the prefix dedup and the ranking still see whole paths.
//
// Truncating at collection would break both on exactly the long paths the clip exists for: a child would
// stop matching its refused ancestor and get reported separately, and `kindFor` would stop recognising a
// directory whose name was cut off.
func TestAChildOfARefusedDirectoryIsStillDedupedWhenPathsAreLong(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is refused nothing")
	}

	s := &UsageScan{}
	long := "/" + strings.Repeat("a", 300) + "/" + strings.Repeat("b", 300)

	s.noteUnreadable(long)
	s.noteUnreadable(long + "/child")

	if len(s.Unreadable) != 1 {
		t.Fatalf("got %d entries, want 1 — the child should be covered by its refused ancestor", len(s.Unreadable))
	}

	s.rankUnreadable()

	if len(s.Unreadable[0].Path) > maxUsagePath {
		t.Fatalf("path is %d bytes after ranking, want <= %d", len(s.Unreadable[0].Path), maxUsagePath)
	}
}
