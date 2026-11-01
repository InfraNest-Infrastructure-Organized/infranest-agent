package journal

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The fixtures are written by systemd's own writer (testdata/generate.sh), in both layouts it uses: compact (systemd
// 252 and later, the default) and regular.
var layouts = []string{"compact", "regular"}

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	fh, err := os.Open(filepath.Join("testdata", name+".journal.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func openFixture(t testing.TB, name string) []*openFile {
	t.Helper()
	b := fixture(t, name)
	f, err := Open(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}

	return []*openFile{{File: f, closer: nopCloser{}}}
}

func at(hour, minute int) time.Time {
	return time.Date(2026, 11, 1, hour, minute, 0, 0, time.UTC)
}

func TestTheHeaderSaysWhatTheFileHolds(t *testing.T) {
	for _, layout := range layouts {
		f := openFixture(t, layout)[0]
		if f.compact != (layout == "compact") {
			t.Errorf("%s: compact = %v", layout, f.compact)
		}
		if !f.Tail().Equal(at(10, 31)) {
			t.Errorf("%s: the newest entry is at %s, want 10:31", layout, f.Tail())
		}
		n := 0
		f.eachEntry(func(int64) bool { n++; return true })
		if n != 19 {
			t.Errorf("%s: %d entries, want 19", layout, n)
		}
	}
}

func TestAFailedUnitsLastLinesAreReadMaskedAndInOrder(t *testing.T) {
	want := []string{
		"starting nginx",
		"upstream failed: postgres://shop:[masked]@db.internal/shop",
		"auth token=[masked] rejected",
		"emerg: bind() to 0.0.0.0:80 failed",
		"(98: Address already in use)",
		"nginx.service: Failed with result 'exit-code'.",
	}
	for _, layout := range layouts {
		s := lastLines(openFixture(t, layout), "nginx.service", at(9, 0), at(11, 0))
		var got []string
		for _, l := range s.Lines {
			got = append(got, l.Message)
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: lines\n%s\nwant\n%s", layout, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		// The debug line is left out; the long one is stored compressed and is counted, never guessed at.
		if s.Compressed != 1 {
			t.Errorf("%s: compressed = %d, want 1", layout, s.Compressed)
		}
		if len(s.Lines) > 0 && (s.Lines[0].Priority != 6 || !s.Lines[0].At.Equal(at(10, 5))) {
			t.Errorf("%s: first line %+v", layout, s.Lines[0])
		}
		// The multi-line message keeps its time and priority on both of its lines.
		if len(s.Lines) > 4 && (s.Lines[3].Priority != 2 || s.Lines[4].Priority != 2 || !s.Lines[4].At.Equal(at(10, 23))) {
			t.Errorf("%s: the split message lost its origin: %+v %+v", layout, s.Lines[3], s.Lines[4])
		}
	}
}

func TestLastLinesStopAtTheMomentAsked(t *testing.T) {
	for _, layout := range layouts {
		s := lastLines(openFixture(t, layout), "nginx.service", at(10, 0), at(10, 21))
		if len(s.Lines) != 3 || s.Lines[2].Message != "auth token=[masked] rejected" {
			t.Errorf("%s: %+v", layout, s.Lines)
		}
		if s := lastLines(openFixture(t, layout), "nginx.service", at(10, 21), at(11, 0)); len(s.Lines) != 4 {
			t.Errorf("%s: since 10:21, %d lines, want 4", layout, len(s.Lines))
		}
		if s := lastLines(openFixture(t, layout), "postgresql.service", at(9, 0), at(11, 0)); len(s.Lines) != 0 {
			t.Errorf("%s: a unit that wrote nothing has lines: %+v", layout, s.Lines)
		}
	}
}

func TestAnHoursErrorsAndWarningsAreCountedPerUnit(t *testing.T) {
	for _, layout := range layouts {
		h := countHour(openFixture(t, layout), at(9, 0))
		if len(h.Units) != 1 || h.Units[0] != (UnitCount{Unit: "app.service", Errors: 5, Warnings: 3}) || h.Partial {
			t.Errorf("%s: 09:00 %+v", layout, h)
		}
		// systemd's own "Failed with result" line counts against the unit it is about, not init.scope.
		h = countHour(openFixture(t, layout), at(10, 0))
		want := []UnitCount{{Unit: "nginx.service", Errors: 3, Warnings: 2}, {Unit: "app.service", Errors: 2}}
		if len(h.Units) != 2 || h.Units[0] != want[0] || h.Units[1] != want[1] {
			t.Errorf("%s: 10:00 %+v", layout, h.Units)
		}
		if h := countHour(openFixture(t, layout), at(11, 0)); len(h.Units) != 0 || h.Units == nil {
			t.Errorf("%s: a quiet hour is an empty list, got %+v", layout, h.Units)
		}
	}
}

func TestADamagedFileCostsEntriesNeverAPanic(t *testing.T) {
	for _, layout := range layouts {
		good := fixture(t, layout)
		// Cut anywhere, and with bytes overwritten anywhere: the reader skips what it cannot read.
		// The objects sit after the hash tables, in the file's last few kilobytes: that is where the cuts and the
		// damage are concentrated, with a sparser sweep over the rest.
		objects := len(good) - 16<<10
		var points []int
		for p := 0; p < objects; p += objects / 64 {
			points = append(points, p)
		}
		for p := objects; p < len(good); p += 61 {
			points = append(points, p)
		}
		for _, cut := range points {
			b := good[:cut]
			if f, err := Open(bytes.NewReader(b), int64(len(b))); err == nil {
				lastLines([]*openFile{{File: f, closer: nopCloser{}}}, "nginx.service", at(9, 0), at(11, 0))
			}
		}
		for _, pos := range points {
			b := bytes.Clone(good)
			for i := pos; i < pos+8 && i < len(b); i++ {
				b[i] ^= 0xff
			}
			if f, err := Open(bytes.NewReader(b), int64(len(b))); err == nil {
				countHour([]*openFile{{File: f, closer: nopCloser{}}}, at(10, 0))
			}
		}
	}
}

func TestAFileThisAgentCannotReadIsRefusedAtTheHeader(t *testing.T) {
	b := fixture(t, "compact")
	if _, err := Open(bytes.NewReader([]byte("not a journal")), 13); err != ErrFormat {
		t.Errorf("garbage: %v", err)
	}
	newer := bytes.Clone(b)
	newer[12] |= 1 << 6 // an incompatible flag from a systemd newer than this reader
	if _, err := Open(bytes.NewReader(newer), int64(len(newer))); err != ErrFormat {
		t.Errorf("a newer format: %v", err)
	}
}

func TestCheckSaysWhyTheJournalIsNotRead(t *testing.T) {
	dir := t.TempDir()
	was := Dirs
	Dirs = []string{dir}
	t.Cleanup(func() { Dirs = was })

	if s := Check(); s.Readable || s.Reason != ReasonNoJournal {
		t.Errorf("no files: %+v", s)
	}

	machine := filepath.Join(dir, "0123456789abcdef0123456789abcdef")
	if err := os.MkdirAll(machine, 0o755); err != nil {
		t.Fatal(err)
	}
	// A user's own journal is not the system's, and is never read.
	if err := os.WriteFile(filepath.Join(machine, "user-1000.journal"), fixture(t, "compact"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := Check(); s.Reason != ReasonNoJournal {
		t.Errorf("only a user journal: %+v", s)
	}

	if err := os.WriteFile(filepath.Join(machine, "system.journal"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := Check(); s.Readable || s.Reason != ReasonUnknownFormat {
		t.Errorf("a broken file: %+v", s)
	}

	if err := os.WriteFile(filepath.Join(machine, "system.journal"), fixture(t, "compact"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := Check(); !s.Readable || s.Reason != ReasonOK {
		t.Errorf("a readable file: %+v", s)
	}
	if s := LastLines("nginx.service", at(9, 0), at(11, 0)); len(s.Lines) != 6 {
		t.Errorf("read from the directory: %d lines", len(s.Lines))
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	if err := os.Chmod(filepath.Join(machine, "system.journal"), 0o000); err != nil {
		t.Fatal(err)
	}
	if s := Check(); s.Readable || s.Reason != ReasonNoPermission {
		t.Errorf("not in the group: %+v", s)
	}
}

func TestLinesAreCleanedAndClippedOnARuneBoundary(t *testing.T) {
	if got := clip(strings.Repeat("é", 200), 301); len(got) != 300 {
		t.Errorf("clip cut inside a rune: %d bytes", len(got))
	}
	if got := clean("a\x1b[31mred\tb"); got != "a[31mred b" {
		t.Errorf("clean: %q", got)
	}
}

func FuzzOpen(f *testing.F) {
	for _, layout := range layouts {
		f.Add(fixture(f, layout))
	}
	// Auth and authpriv lines, so the failed-login classifier is fuzzed along with the reader (InfraNest #5868).
	f.Add(fixture(f, "authfail"))
	f.Fuzz(func(t *testing.T, b []byte) {
		file, err := Open(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return
		}
		files := []*openFile{{File: file, closer: nopCloser{}}}
		lastLines(files, "nginx.service", time.Time{}, at(23, 0))
		countHour(files, at(10, 0))
		countHour(files, at(14, 0))
	})
}

// synthetic writes a minimal journal file in the regular layout: one unit, n entries from `from`, and a
// single entry array holding them all, 400 ms apart — larger than one chunk of items, which systemd's own small fixtures never are.
func synthetic(n int, from time.Time) []byte {
	le := func(b []byte, off int, v uint64) {
		for i := 0; i < 8; i++ {
			b[off+i] = byte(v >> (8 * i))
		}
	}
	align := func(n int) int { return (n + 7) &^ 7 }
	buf := make([]byte, 272)
	copy(buf, signature)
	le(buf, 88, 272)
	data := func(payload string) int {
		off := len(buf)
		obj := make([]byte, align(64+len(payload)))
		obj[0] = objectData
		le(obj, 8, uint64(64+len(payload)))
		copy(obj[64:], payload)
		buf = append(buf, obj...)

		return off
	}
	fields := []int{data("_SYSTEMD_UNIT=big.service"), data("PRIORITY=3"), data("MESSAGE=it broke")}
	var entries []int
	for i := 0; i < n; i++ {
		off := len(buf)
		obj := make([]byte, 64+16*len(fields))
		obj[0] = objectEntry
		le(obj, 8, uint64(len(obj)))
		le(obj, 24, uint64(from.Add(time.Duration(i)*400*time.Millisecond).UnixMicro()))
		for j, d := range fields {
			le(obj, 64+16*j, uint64(d))
		}
		buf = append(buf, obj...)
		entries = append(entries, off)
	}
	array := len(buf)
	obj := make([]byte, 24+8*n)
	obj[0] = objectEntryArray
	le(obj, 8, uint64(len(obj)))
	for i, e := range entries {
		le(obj, 24+8*i, uint64(e))
	}
	buf = append(buf, obj...)
	le(buf, 176, uint64(array))
	le(buf, 192, uint64(from.Add(time.Duration(n-1)*400*time.Millisecond).UnixMicro()))

	return buf
}

func TestALargeEntryArrayIsReadInChunksNewestFirst(t *testing.T) {
	n := itemsChunk*2 + 100
	b := synthetic(n, at(10, 0))
	open := func() []*openFile {
		f, err := Open(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}

		return []*openFile{{File: f, closer: nopCloser{}}}
	}
	if h := countHour(open(), at(10, 0)); len(h.Units) != 1 || h.Units[0].Errors != n {
		t.Fatalf("counted %+v, want %d errors", h.Units, n)
	}
	s := lastLines(open(), "big.service", at(9, 0), at(12, 0))
	if len(s.Lines) != MaxLines || !s.Lines[MaxLines-1].At.Equal(at(10, 0).Add(time.Duration(n-1)*400*time.Millisecond)) {
		t.Fatalf("want the newest %d lines, got %d ending %v", MaxLines, len(s.Lines), s.Lines[len(s.Lines)-1].At)
	}
}
