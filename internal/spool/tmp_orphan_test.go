package spool

import (
	"os"
	"path/filepath"
	"testing"
)

// A half-written reading from a previous run must not accumulate.
//
// `<seq>.json.tmp` is a write that never committed. Nothing else collects them: `names()` filters to
// `.json`, so `trim()` neither counts nor deletes one and the spool's bound does not apply — they simply
// pile up. The case that matters is the one no error path sees: a process killed mid-write leaves the file
// behind with nobody left to clean it up.
func TestAHalfWrittenReadingIsSweptOnStartup(t *testing.T) {
	dir := t.TempDir()

	orphan := filepath.Join(dir, "00000000000000000007.json.tmp")
	if err := os.WriteFile(orphan, []byte(`{"cpu_per`), 0o600); err != nil {
		t.Fatalf("cannot plant the orphan: %v", err)
	}
	// A committed reading beside it, which must survive.
	keep := filepath.Join(dir, "00000000000000000008.json")
	if err := os.WriteFile(keep, []byte(`{"cpu_percent":1}`), 0o600); err != nil {
		t.Fatalf("cannot plant the reading: %v", err)
	}

	if _, err := New(dir); err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("the half-written reading survived startup; nothing else will ever remove it")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("a committed reading was swept away: %v", err)
	}
}
