// Package journal reads systemd's journal files directly (InfraNest #5848), for two things only: a failed unit's
// last lines, and how many errors and warnings each unit logged in an hour.
//
// It is a reader of the on-disk format (https://systemd.io/JOURNAL_FILE_FORMAT/), not a client of journald: the agent
// starts no process, so `journalctl` is out by construction, and linking libsystemd would end the static,
// dependency-free build. What it reads is deliberately small — the entry arrays, each entry's own items, and a data
// object's payload — and it never trusts an offset: every one is checked against the file before it is read, an object
// of the wrong type ends the walk, and a field it cannot read is skipped and counted, never guessed.
//
// What it does not do, on purpose:
//
//   - **No hash tables.** Finding entries by field would need the hash function the file was written with (Jenkins
//     lookup3, or SipHash keyed by the file id). Walking the entries backwards from the newest needs neither, and an
//     hour of entries is what both questions ask about anyway.
//   - **No decompression.** journald stores a field longer than its threshold (512 bytes by default) compressed with
//     XZ, LZ4 or Zstandard. Decompressing them would mean three decoders in the one binary somebody installs on every
//     machine; the field is skipped and the line says so.
//   - **No writing.** The file is opened read-only, and only when the operating system lets the agent's user read it.
package journal

import (
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"time"
)

// The format's constants, from systemd's src/libsystemd/sd-journal/journal-def.h.
const (
	signature = "LPKSHHRH"

	objectData       = 1
	objectEntry      = 3
	objectEntryArray = 6

	// incompatible_flags: what a reader must understand to read the file at all.
	incompatibleXZ        = 1 << 0
	incompatibleLZ4       = 1 << 1
	incompatibleKeyedHash = 1 << 2
	incompatibleZSTD      = 1 << 3
	incompatibleCompact   = 1 << 4
	incompatibleKnown     = incompatibleXZ | incompatibleLZ4 | incompatibleKeyedHash | incompatibleZSTD | incompatibleCompact

	// An object's own flags: its payload is compressed.
	objectCompressed = 1<<0 | 1<<1 | 1<<2

	objectHeaderSize = 16
	// The header fields this reader uses end at tail_entry_realtime (offset 192 + 8).
	minHeaderSize = 200
)

// Bounds on what one file may make this reader do. A journal file is written by journald and readable only by the
// system-journal group, so it is not hostile — but it can be torn by a crash, and a torn file must cost a skipped
// entry, never a loop or an allocation the size of a corrupt length field.
const (
	maxEntryArrays = 4096
	maxItems       = 512
	maxFieldName   = 64
	maxValue       = 256
	// MaxMessage is how much of one message is read.
	MaxMessage = 2048
)

// ErrFormat means the file is not a journal file this reader understands: a different signature, a header too short
// for the fields it needs, or a format feature newer than this agent.
var ErrFormat = errors.New("not a journal file this agent can read")

// Entry is one journal entry, as much of it as this reader looks at.
type Entry struct {
	At time.Time
	// The unit the entry is about: `UNIT=` when systemd itself wrote it about a unit (its `_SYSTEMD_UNIT` is then
	// init.scope), else the unit the writing process belongs to. Empty for the kernel and anything outside a unit.
	Unit string
	// 0 (emerg) to 7 (debug); -1 when the entry has none.
	Priority int
	// syslog's facility (`SYSLOG_FACILITY`), -1 when the entry has none: 4 is auth, 10 authpriv.
	Facility int
	// `SYSLOG_IDENTIFIER`: the program that wrote it, as it named itself.
	Identifier string
	// `_TRANSPORT`: how journald received it — `audit` for the kernel's audit records.
	Transport string
	// Where the message's payload is, so it is read only for the entries a caller keeps.
	message, messageLen int64
	// The entry had a field this reader could not read because it is compressed — its message, when no message was
	// found beside it.
	Compressed bool
}

// HasMessage is whether the entry's message can be read.
func (e Entry) HasMessage() bool { return e.message > 0 }

// File is one open journal file.
type File struct {
	r       *blockReader
	size    int64
	compact bool

	headerSize      int64
	entryArray      int64
	tailRealtime    uint64
	fields          map[int64]field
	EntriesScanned  int
	CompressedItems int
}

type fieldKind uint8

const (
	kindOther fieldKind = iota
	kindUnit            // _SYSTEMD_UNIT
	kindAbout           // UNIT, written by systemd about a unit
	kindPriority
	kindMessage
	kindCompressed
	kindFacility   // SYSLOG_FACILITY
	kindIdentifier // SYSLOG_IDENTIFIER
	kindTransport  // _TRANSPORT
)

type field struct {
	kind  fieldKind
	value string
	// For a message: where its value starts, and how long it is.
	at, length int64
}

// Open reads a journal file's header. `size` is the file's size on disk, which bounds every read — never the size the
// header claims.
func Open(r io.ReaderAt, size int64) (*File, error) {
	f := &File{r: newBlockReader(r, size), size: size, fields: map[int64]field{}}
	h, err := f.r.read(0, minHeaderSize)
	if err != nil {
		return nil, ErrFormat
	}
	if string(h[:8]) != signature {
		return nil, ErrFormat
	}
	incompatible := binary.LittleEndian.Uint32(h[12:])
	if incompatible&^incompatibleKnown != 0 {
		return nil, ErrFormat
	}
	f.compact = incompatible&incompatibleCompact != 0
	f.headerSize = int64(binary.LittleEndian.Uint64(h[88:]))
	if f.headerSize < minHeaderSize || f.headerSize > size {
		return nil, ErrFormat
	}
	f.entryArray = int64(binary.LittleEndian.Uint64(h[176:]))
	f.tailRealtime = binary.LittleEndian.Uint64(h[192:])

	return f, nil
}

// Tail is when the newest entry in the file was written; zero when the file is empty.
func (f *File) Tail() time.Time {
	if f.tailRealtime == 0 {
		return time.Time{}
	}

	return time.UnixMicro(int64(f.tailRealtime)).UTC()
}

// object reads an object's header at `off` and checks it: aligned, past the file header, of the wanted type, and
// wholly inside the file. Returns the object's size.
func (f *File) object(off int64, want uint8) (size int64, flags uint8, ok bool) {
	if off < f.headerSize || off%8 != 0 || off+objectHeaderSize > f.size {
		return 0, 0, false
	}
	b, err := f.r.read(off, objectHeaderSize)
	if err != nil || b[0] != want {
		return 0, 0, false
	}
	size = int64(binary.LittleEndian.Uint64(b[8:]))
	if size < objectHeaderSize || size > f.size-off {
		return 0, 0, false
	}

	return size, b[1], true
}

type entryArray struct{ off, n int64 }

// arrays lists the file's entry arrays, oldest first: the chain the header points to, each array holding the offsets
// of the entries after the previous one's.
func (f *File) arrays() []entryArray {
	var out []entryArray
	off := f.entryArray
	for i := 0; off != 0 && i < maxEntryArrays; i++ {
		size, _, ok := f.object(off, objectEntryArray)
		if !ok || size < 24 {
			break
		}
		b, err := f.r.read(off+16, 8)
		if err != nil {
			break
		}
		out = append(out, entryArray{off, (size - 24) / f.itemSize()})
		next := int64(binary.LittleEndian.Uint64(b))
		// The chain only ever moves forward through the file; anything else is a torn file, not a longer chain.
		if next <= off {
			break
		}
		off = next
	}

	return out
}

// itemSize is an entry array's item: an offset, 32 bits wide in the compact layout.
func (f *File) itemSize() int64 {
	if f.compact {
		return 4
	}

	return 8
}

// itemsChunk is how many of an array's items are read at a time. An array doubles each time journald adds one, so
// the newest in a full file holds hundreds of thousands — read whole, that would be megabytes for the few hundred
// entries an hour usually needs.
const itemsChunk = 4096

// eachEntry calls fn with each entry offset, newest first, until it returns false.
func (f *File) eachEntry(fn func(int64) bool) {
	arrays := f.arrays()
	item := f.itemSize()
	for ai := len(arrays) - 1; ai >= 0; ai-- {
		a := arrays[ai]
		for hi := a.n; hi > 0; {
			lo := max(0, hi-itemsChunk)
			b, err := f.r.read(a.off+24+lo*item, (hi-lo)*item)
			if err != nil {
				return
			}
			for i := hi - 1; i >= lo; i-- {
				var e int64
				if f.compact {
					e = int64(binary.LittleEndian.Uint32(b[(i-lo)*4:]))
				} else {
					e = int64(binary.LittleEndian.Uint64(b[(i-lo)*8:]))
				}
				// An unused slot at the end of the newest array.
				if e != 0 && !fn(e) {
					return
				}
			}
			hi = lo
		}
	}
}

// Backward calls fn with each entry, newest first, until fn returns false, an entry is older than `since`, or `limit`
// entries have been read. Entries it cannot read are skipped.
func (f *File) Backward(since time.Time, limit int, fn func(Entry) bool) {
	f.eachEntry(func(off int64) bool {
		if f.EntriesScanned >= limit {
			return false
		}
		e, ok := f.entry(off)
		if !ok {
			return true
		}
		f.EntriesScanned++
		if e.At.Before(since) {
			return false
		}

		return fn(e)
	})
}

// entry reads one entry object and the fields this reader looks at.
func (f *File) entry(off int64) (Entry, bool) {
	size, _, ok := f.object(off, objectEntry)
	if !ok || size < 64 {
		return Entry{}, false
	}
	b, err := f.r.read(off, 64)
	if err != nil {
		return Entry{}, false
	}
	e := Entry{At: time.UnixMicro(int64(binary.LittleEndian.Uint64(b[24:]))).UTC(), Priority: -1, Facility: -1}

	item := int64(16)
	if f.compact {
		item = 4
	}
	n := (size - 64) / item
	if n > maxItems {
		n = maxItems
	}
	items, err := f.r.read(off+64, n*item)
	if err != nil {
		return Entry{}, false
	}
	var unit, about string
	compressed := false
	for i := int64(0); i < n; i++ {
		var d int64
		if f.compact {
			d = int64(binary.LittleEndian.Uint32(items[i*4:]))
		} else {
			d = int64(binary.LittleEndian.Uint64(items[i*16:]))
		}
		fd := f.data(d)
		switch fd.kind {
		case kindUnit:
			unit = fd.value
		case kindAbout:
			about = fd.value
		case kindPriority:
			if p, err := strconv.Atoi(fd.value); err == nil && p >= 0 && p <= 7 {
				e.Priority = p
			}
		case kindFacility:
			if v, err := strconv.Atoi(fd.value); err == nil && v >= 0 && v <= 23 {
				e.Facility = v
			}
		case kindIdentifier:
			e.Identifier = fd.value
		case kindTransport:
			e.Transport = fd.value
		case kindMessage:
			e.message, e.messageLen = fd.at, fd.length
		case kindCompressed:
			compressed = true
			f.CompressedItems++
		}
	}
	e.Unit = unit
	if about != "" && (unit == "" || unit == "init.scope") {
		e.Unit = about
	}
	e.Compressed = compressed && e.message == 0

	return e, true
}

// data classifies one data object by its field name, remembering the answer: most fields an entry carries are shared
// with thousands of others (`_SYSTEMD_UNIT=nginx.service` is one object however often it logs).
func (f *File) data(off int64) field {
	if fd, ok := f.fields[off]; ok {
		return fd
	}
	fd := f.readData(off)
	// Bounded: a file is read for one question and then closed, but an hour of a busy machine is a lot of entries.
	if len(f.fields) >= 16384 {
		clear(f.fields)
	}
	f.fields[off] = fd

	return fd
}

func (f *File) readData(off int64) field {
	size, flags, ok := f.object(off, objectData)
	payloadAt := int64(64)
	if f.compact {
		payloadAt = 72
	}
	if !ok || size <= payloadAt {
		return field{kind: kindOther}
	}
	if flags&objectCompressed != 0 {
		return field{kind: kindCompressed}
	}
	length := size - payloadAt
	head, err := f.r.read(off+payloadAt, min(length, maxFieldName+1))
	if err != nil {
		return field{kind: kindOther}
	}
	eq := -1
	for i, c := range head {
		if c == '=' {
			eq = i

			break
		}
	}
	if eq <= 0 {
		return field{kind: kindOther}
	}
	name := string(head[:eq])
	valueAt, valueLen := off+payloadAt+int64(eq)+1, length-int64(eq)-1
	var kind fieldKind
	switch name {
	case "_SYSTEMD_UNIT":
		kind = kindUnit
	case "UNIT":
		kind = kindAbout
	case "PRIORITY":
		kind = kindPriority
	case "SYSLOG_FACILITY":
		kind = kindFacility
	case "SYSLOG_IDENTIFIER":
		kind = kindIdentifier
	case "_TRANSPORT":
		kind = kindTransport
	case "MESSAGE":
		return field{kind: kindMessage, at: valueAt, length: valueLen}
	default:
		return field{kind: kindOther}
	}
	if valueLen > maxValue {
		return field{kind: kindOther}
	}
	v, err := f.r.read(valueAt, valueLen)
	if err != nil {
		return field{kind: kindOther}
	}

	return field{kind: kind, value: string(v)}
}

// Message reads an entry's message, up to MaxMessage bytes, and whether it was cut there.
func (f *File) Message(e Entry) (string, bool) {
	if e.message == 0 {
		return "", false
	}
	n := min(e.messageLen, MaxMessage)
	b, err := f.r.read(e.message, n)
	if err != nil {
		return "", false
	}

	return string(b), e.messageLen > n
}

// blockReader reads through a small cache of 64 KiB blocks. Entries and the objects they point to are written next to
// each other, so walking an hour of entries touches the same few blocks over and over; read one field at a time
// straight from the file, that walk was a system call per field.
type blockReader struct {
	r     io.ReaderAt
	size  int64
	order []int64
	cache map[int64][]byte
}

const (
	blockSize = 64 << 10
	maxBlocks = 32
)

func newBlockReader(r io.ReaderAt, size int64) *blockReader {
	return &blockReader{r: r, size: size, cache: map[int64][]byte{}}
}

var errOutside = errors.New("outside the file")

// read returns n bytes at off. The slice may share a cached block, so it is never written to.
func (b *blockReader) read(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off+n > b.size || n > 1<<20 {
		return nil, errOutside
	}
	first, last := off/blockSize, (off+n-1)/blockSize
	if n == 0 {
		return []byte{}, nil
	}
	if first == last {
		blk, err := b.block(first)
		if err != nil {
			return nil, err
		}
		start := off - first*blockSize
		if start+n > int64(len(blk)) {
			return nil, errOutside
		}

		return blk[start : start+n], nil
	}
	out := make([]byte, 0, n)
	for i := first; i <= last; i++ {
		blk, err := b.block(i)
		if err != nil {
			return nil, err
		}
		lo, hi := int64(0), int64(len(blk))
		if i == first {
			lo = off - first*blockSize
		}
		if i == last {
			hi = off + n - last*blockSize
		}
		if lo > hi || hi > int64(len(blk)) {
			return nil, errOutside
		}
		out = append(out, blk[lo:hi]...)
	}

	return out, nil
}

func (b *blockReader) block(i int64) ([]byte, error) {
	if blk, ok := b.cache[i]; ok {
		return blk, nil
	}
	n := min(int64(blockSize), b.size-i*blockSize)
	blk := make([]byte, n)
	if _, err := b.r.ReadAt(blk, i*blockSize); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(b.order) >= maxBlocks {
		delete(b.cache, b.order[0])
		b.order = b.order[1:]
	}
	b.cache[i] = blk
	b.order = append(b.order, i)

	return blk, nil
}
