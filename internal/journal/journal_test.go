package journal

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
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

// FuzzOpen's seeds are the fixtures without their hash tables (shrink). The files journald writes are 3.7 MB, almost
// all of it preallocated hash tables this reader never reads, and Go's fuzzer passes every input between its processes
// as Go source: seeds that size parse for seconds per call, and a run of minutes executed two inputs (InfraNest #5868).
func FuzzOpen(f *testing.F) {
	for _, name := range append(append([]string{}, layouts...), "authfail") {
		f.Add(shrink(f, fixture(f, name)))
	}
	f.Add(synthetic(3, at(10, 0)))
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

// shrink drops a journal file's hash tables and tags, moving every other object up and rewriting the offsets this
// reader follows: the header's entry array, each entry array's next array and items, and each entry's items. What is
// left reads exactly as the original does (TestAShrunkFileReadsAsTheOriginal) at a hundredth of the size.
func shrink(t testing.TB, b []byte) []byte {
	t.Helper()
	u64 := func(off int64) int64 { return int64(binary.LittleEndian.Uint64(b[off:])) }
	f, err := Open(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	type obj struct{ off, size int64 }
	var kept []obj
	moved := map[int64]int64{0: 0}
	at := f.headerSize
	for off := f.headerSize; off+objectHeaderSize <= int64(len(b)); {
		size := u64(off + 8)
		if size < objectHeaderSize {
			break
		}
		if typ := b[off]; typ == objectData || typ == objectEntry || typ == objectEntryArray {
			moved[off] = at
			kept = append(kept, obj{off, size})
			at += (size + 7) &^ 7
		}
		off += (size + 7) &^ 7
	}
	out := make([]byte, at)
	copy(out, b[:f.headerSize])
	put := func(dst int64, off int64, wide bool) {
		if wide {
			binary.LittleEndian.PutUint64(out[dst:], uint64(moved[off]))
		} else {
			binary.LittleEndian.PutUint32(out[dst:], uint32(moved[off]))
		}
	}
	put(176, u64(176), true)
	for _, o := range kept {
		dst := moved[o.off]
		copy(out[dst:], b[o.off:o.off+o.size])
		switch b[o.off] {
		case objectEntryArray:
			put(dst+16, u64(o.off+16), true)
			for i := int64(24); i+f.itemSize() <= o.size; i += f.itemSize() {
				if f.compact {
					put(dst+i, int64(binary.LittleEndian.Uint32(b[o.off+i:])), false)
				} else {
					put(dst+i, u64(o.off+i), true)
				}
			}
		case objectEntry:
			step := int64(16)
			if f.compact {
				step = 4
			}
			for i := int64(64); i+step <= o.size; i += step {
				if f.compact {
					put(dst+i, int64(binary.LittleEndian.Uint32(b[o.off+i:])), false)
				} else {
					put(dst+i, u64(o.off+i), true)
				}
			}
		}
	}

	return out
}

func TestAShrunkFileReadsAsTheOriginal(t *testing.T) {
	for _, name := range append(append([]string{}, layouts...), "authfail") {
		b := fixture(t, name)
		small := shrink(t, b)
		if len(small)*20 > len(b) {
			t.Errorf("%s: shrunk to %d of %d bytes", name, len(small), len(b))
		}
		read := func(b []byte) string {
			file, err := Open(bytes.NewReader(b), int64(len(b)))
			if err != nil {
				t.Fatal(err)
			}
			files := []*openFile{{File: file, closer: nopCloser{}}}
			var sb strings.Builder
			for h := 9; h <= 15; h++ {
				fmt.Fprintf(&sb, "%+v\n", countHour(files, at(h, 0)))
			}
			fmt.Fprintf(&sb, "%+v\n", lastLines(files, "nginx.service", time.Time{}, at(23, 0)))

			return sb.String()
		}
		if a, c := read(b), read(small); a != c {
			t.Errorf("%s reads differently once shrunk:\n%s\nvs\n%s", name, a, c)
		}
	}
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
