// Package dbus speaks just enough of the D-Bus wire protocol to ask systemd what is running.
//
// Hand-written rather than imported because this agent has no third-party dependencies and CI fails the
// build if `go.sum` stops being empty. That is a real cost — the encoding below is the fiddly part of
// D-Bus, and alignment padding is exactly the sort of thing that is subtly wrong and still passes on the
// machine you wrote it on. It is paid once, and it buys a binary whose entire dependency tree is the
// standard library.
//
// Scope is deliberately tiny: enough to make a method call and read the reply. No signals, no
// introspection, no server side, no file descriptor passing. Anything this package cannot express is a
// thing the agent has no business doing.
package dbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
)

// D-Bus values are aligned to their own size, and the padding is part of the format rather than an
// optimisation: a reader that gets it wrong does not fail, it reads the next field from the wrong offset
// and returns plausible nonsense. Every alignment in this file is from the specification.
func align(offset, boundary int) int {
	if rem := offset % boundary; rem != 0 {
		return offset + boundary - rem
	}

	return offset
}

// alignmentOf is the boundary a value of this type starts on.
//
// Signature characters, from the specification: y byte, b boolean, n/q int16/uint16, i/u int32/uint32,
// x/t int64/uint64, d double, s string, o object path, g signature, a array, ( struct, v variant.
func alignmentOf(sig byte) int {
	switch sig {
	case 'y', 'g', 'v':
		return 1
	case 'n', 'q':
		return 2
	case 'b', 'i', 'u', 's', 'o', 'a':
		return 4
	case 'x', 't', 'd', '(', '{':
		return 8
	default:
		return 1
	}
}

// encoder writes little-endian D-Bus values, tracking its own offset so it can pad correctly.
//
// Little-endian throughout, declared in the message header. The protocol allows either and requires the
// reader to honour what the sender chose; we are only ever the sender here, and every machine this runs
// on is little-endian anyway.
type encoder struct {
	buf []byte
}

func (e *encoder) pad(boundary int) {
	for len(e.buf) < align(len(e.buf), boundary) {
		e.buf = append(e.buf, 0)
	}
}

func (e *encoder) byte(v byte) { e.buf = append(e.buf, v) }

func (e *encoder) uint32(v uint32) {
	e.pad(4)
	e.buf = binary.LittleEndian.AppendUint32(e.buf, v)
}

// str writes a string or object path: a length, the bytes, and a NUL the length does not count.
func (e *encoder) str(v string) {
	e.uint32(uint32(len(v)))
	e.buf = append(e.buf, v...)
	e.buf = append(e.buf, 0)
}

// signature writes a signature: a *byte* length rather than a uint32, and no alignment. The one type
// whose header is not four bytes, which is why it has its own method rather than reusing str.
func (e *encoder) signature(v string) {
	e.buf = append(e.buf, byte(len(v)))
	e.buf = append(e.buf, v...)
	e.buf = append(e.buf, 0)
}

// decoder reads little-endian D-Bus values out of a reply body.
//
// Every read is bounds-checked and records the first failure rather than panicking: this parses bytes
// from a system service on a machine we do not control, and a monitoring agent that dies on a malformed
// reply takes the monitoring with it — which is worse than the gap it was avoiding.
type decoder struct {
	buf []byte
	pos int
	err error
}

func (d *decoder) fail(format string, args ...any) {
	if d.err == nil {
		d.err = fmt.Errorf(format, args...)
	}
}

func (d *decoder) need(n int) bool {
	if d.err != nil {
		return false
	}
	if d.pos+n > len(d.buf) {
		d.fail("reply ended early: wanted %d bytes at offset %d of %d", n, d.pos, len(d.buf))

		return false
	}

	return true
}

func (d *decoder) pad(boundary int) {
	next := align(d.pos, boundary)
	if next > len(d.buf) {
		d.fail("reply ended inside padding at offset %d", d.pos)

		return
	}
	d.pos = next
}

func (d *decoder) byte() byte {
	if !d.need(1) {
		return 0
	}
	v := d.buf[d.pos]
	d.pos++

	return v
}

func (d *decoder) uint32() uint32 {
	d.pad(4)
	if !d.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(d.buf[d.pos:])
	d.pos += 4

	return v
}

func (d *decoder) uint64() uint64 {
	d.pad(8)
	if !d.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(d.buf[d.pos:])
	d.pos += 8

	return v
}

func (d *decoder) str() string {
	n := int(d.uint32())
	// A length is four bytes from a remote peer, so it can claim four gigabytes. Checked against what is
	// actually here before it becomes a slice bound.
	if !d.need(n + 1) {
		return ""
	}
	v := string(d.buf[d.pos : d.pos+n])
	d.pos += n + 1 // the NUL the length does not count

	return v
}

func (d *decoder) signature() string {
	n := int(d.byte())
	if !d.need(n + 1) {
		return ""
	}
	v := string(d.buf[d.pos : d.pos+n])
	d.pos += n + 1

	return v
}

// skip advances past one value of the given signature without interpreting it.
//
// Needed because a reply carries fields we do not want — the job id and job path on every unit, the
// header fields of a message we already understand — and the only way past a value is to know its shape.
// Returns the remaining signature after the value it consumed.
func (d *decoder) skip(sig string) string {
	if d.err != nil || sig == "" {
		return ""
	}

	switch sig[0] {
	case 'y':
		d.byte()
	case 'b', 'i', 'u':
		d.uint32()
	case 'x', 't', 'd':
		d.uint64()
	case 's', 'o':
		d.str()
	case 'g':
		d.signature()
	case 'v':
		inner := d.signature()
		d.skip(inner)
	case 'a':
		rest := sig[1:]
		n := int(d.uint32())
		if n < 0 || n > len(d.buf) {
			d.fail("array claims %d bytes, which the reply does not have", n)

			return ""
		}
		d.pad(alignmentOf(rest[0]))
		end := d.pos + n
		for d.pos < end && d.err == nil {
			d.skip(rest)
		}
		d.pos = end

		return afterOne(rest)
	case '(':
		d.pad(8)
		rest := sig[1:]
		for rest != "" && rest[0] != ')' && d.err == nil {
			before := rest
			rest = d.skip(rest)
			if rest == before {
				d.fail("cannot advance past signature %q", rest)

				return ""
			}
		}

		return strings.TrimPrefix(rest, ")")
	default:
		d.fail("unsupported type %q in signature", sig[0])

		return ""
	}

	return sig[1:]
}

// afterOne returns the signature remaining after one complete type at the front of sig.
//
// A single type is not always a single character: `aa{sv}` is one type four characters long, and
// consuming it as one is the difference between reading the next field and reading the middle of this one.
func afterOne(sig string) string {
	if sig == "" {
		return ""
	}

	switch sig[0] {
	case 'a':
		return afterOne(sig[1:])
	case '(', '{':
		closing := byte(')')
		if sig[0] == '{' {
			closing = '}'
		}
		depth := 0
		for i := 0; i < len(sig); i++ {
			switch sig[i] {
			case '(', '{':
				depth++
			case ')', '}':
				depth--
				if depth == 0 && sig[i] == closing {
					return sig[i+1:]
				}
			}
		}

		return ""
	default:
		return sig[1:]
	}
}

var errTooLarge = errors.New("message is larger than this client will read")

// maxMessage bounds what we will accept in one reply.
//
// The length arrives as a uint32 from the bus, so an unbounded reader would allocate whatever it is told
// to. Two hundred units with descriptions is comfortably under a megabyte; sixteen is room for a fleet
// nobody is running, and still small enough that the agent's 64MB memory limit is never the thing that
// notices.
const maxMessage = 16 << 20

func checkedLen(v uint32) (int, error) {
	if v > maxMessage || v > math.MaxInt32 {
		return 0, errTooLarge
	}

	return int(v), nil
}
