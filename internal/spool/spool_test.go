package spool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newSpool(t *testing.T) *Spool {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "spool"))
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func TestReadingsComeBackOldestFirst(t *testing.T) {
	// The server refuses a backfill that reaches at or before what it already holds, so a batch delivered
	// out of order would have its own earlier readings rejected as `already_covered` — the spool would
	// drain while silently losing most of what it held.
	s := newSpool(t)
	for i := int64(1); i <= 3; i++ {
		if err := s.Add(i, map[string]int64{"n": i}); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := s.Take(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries", len(entries))
	}
	for i, e := range entries {
		var got map[string]int64
		_ = json.Unmarshal(e.Data, &got)
		if got["n"] != int64(i+1) {
			t.Fatalf("entry %d is %v — out of order", i, got)
		}
	}
}

func TestTakeDoesNotDelete(t *testing.T) {
	// A reading is delivered when the server says it stored it, never when the request merely finished.
	// A 502 from something in front of the API completes too.
	s := newSpool(t)
	_ = s.Add(1, map[string]int{"n": 1})

	if _, err := s.Take(10); err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 {
		t.Fatal("Take removed a reading that was never acknowledged")
	}
}

func TestDropRemovesOnlyWhatWasAccepted(t *testing.T) {
	s := newSpool(t)
	for i := int64(1); i <= 3; i++ {
		_ = s.Add(i, map[string]int64{"n": i})
	}

	entries, _ := s.Take(2)
	s.Drop(entries)

	if s.Len() != 1 {
		t.Fatalf("expected 1 reading left, got %d", s.Len())
	}
}

func TestTheOldestIsEvictedWhenFull(t *testing.T) {
	// When the cap and a new reading collide, the recent past is what somebody is about to look at — and
	// the oldest reading is the one the server would refuse as `too_old` anyway.
	s := newSpool(t)
	s.Max = 3

	for i := int64(1); i <= 5; i++ {
		if err := s.Add(i, map[string]int64{"n": i}); err != nil {
			t.Fatal(err)
		}
	}

	entries, _ := s.Take(10)
	if len(entries) != 3 {
		t.Fatalf("expected the spool to hold 3, got %d", len(entries))
	}

	var first map[string]int64
	_ = json.Unmarshal(entries[0].Data, &first)
	if first["n"] != 3 {
		t.Fatalf("expected the oldest survivor to be 3, got %v", first)
	}
}

func TestADamagedFileDoesNotBlockTheOnesBehindIt(t *testing.T) {
	// Unreadable on disk is unreadable for ever. Keeping it would block the head of the spool and stop
	// every later reading from ever being delivered — one corrupt file turning into total silence.
	s := newSpool(t)
	_ = s.Add(1, map[string]int{"n": 1})
	_ = s.Add(2, map[string]int{"n": 2})

	entries, _ := s.Take(10)
	if err := os.WriteFile(filepath.Join(s.dir, entries[0].Name), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.Take(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the good reading to survive, got %d entries", len(got))
	}
}

func TestAPartialWriteIsNeverVisible(t *testing.T) {
	// Written to a temp name and renamed, so a reader never catches half a file — which would parse as
	// damaged and cost a good reading for a reason that has nothing to do with the machine.
	s := newSpool(t)
	_ = s.Add(1, map[string]int{"n": 1})

	names, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if filepath.Ext(n.Name()) == ".tmp" {
			t.Fatalf("a temp file was left behind: %s", n.Name())
		}
	}
}
