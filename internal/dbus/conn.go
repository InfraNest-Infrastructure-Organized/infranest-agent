package dbus

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// SystemBusPath is the socket every systemd installation puts the system bus on.
//
// Hardcoded rather than read from DBUS_SYSTEM_BUS_ADDRESS. The environment variable is the documented way
// to point a client somewhere else, and that is exactly why this does not honour it: an agent that can be
// aimed at an arbitrary socket by an environment variable has a lever it does not need. There is one bus
// worth asking, and it is always here.
const SystemBusPath = "/run/dbus/system_bus_socket"

// Conn is a connection to the bus, after authentication.
//
// Not safe for concurrent use, and not meant to be: the collector opens one, asks its questions in order
// and closes it. Holding it open between collection cycles would be a file descriptor kept for the life
// of the process to save a handshake that costs a millisecond.
type Conn struct {
	rw     io.ReadWriter
	closer io.Closer
	in     *bufio.Reader
	serial uint32
}

// Dial connects to the system bus and completes the handshake.
//
// Every failure here is ordinary rather than exceptional — no D-Bus on the machine, a container without
// the socket mounted, a sandbox that will not allow AF_UNIX — and each is reported as a collector error
// rather than raised, so the agent keeps sending everything else.
func Dial(timeout time.Duration) (*Conn, error) {
	sock, err := net.DialTimeout("unix", SystemBusPath, timeout)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the system bus at %s: %w", SystemBusPath, err)
	}

	// One deadline for the whole exchange. A bus that accepts the connection and then says nothing would
	// otherwise hold the collection cycle open for ever, and this runs on a schedule.
	_ = sock.SetDeadline(time.Now().Add(timeout))

	conn := newConn(sock, sock)
	if err := conn.handshake(); err != nil {
		_ = sock.Close()

		return nil, err
	}

	return conn, nil
}

// newConn wraps an already-connected transport. Split out so the protocol can be tested over an in-memory
// pipe: the encoding is the part worth testing, and it has nothing to do with sockets.
func newConn(rw io.ReadWriter, closer io.Closer) *Conn {
	return &Conn{rw: rw, closer: closer, in: bufio.NewReader(rw)}
}

func (c *Conn) Close() error {
	if c.closer == nil {
		return nil
	}

	return c.closer.Close()
}

/*
handshake performs SASL EXTERNAL and says Hello.

EXTERNAL means "the kernel already knows who I am": the bus reads our uid from the socket's peer
credentials, so there is no password and nothing to configure. It is also why this needs no privilege —
the bus is world-connectable by design, and what an unprivileged caller may then *do* is decided by
polkit, not by the handshake.

The wire format here is line-based text, unlike everything after it: a leading NUL byte, then commands
terminated by CRLF, until BEGIN switches the connection to the binary protocol.
*/
func (c *Conn) handshake() error {
	// The leading NUL is not a command and not optional. It exists so the first byte of a connection is
	// never a valid message, which is how the bus tells a client from something else entirely.
	if _, err := c.rw.Write([]byte{0}); err != nil {
		return fmt.Errorf("cannot start the bus handshake: %w", err)
	}

	// The uid, hex-encoded, is the whole credential. The bus checks it against what the kernel reports
	// and ignores it if they disagree, so lying here achieves nothing.
	uid := fmt.Sprintf("%d", os.Getuid())
	if err := c.line("AUTH EXTERNAL %s", hexOf(uid)); err != nil {
		return err
	}

	reply, err := c.readLine()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(reply, "OK ") {
		// REJECTED is the ordinary answer when the bus does not accept EXTERNAL — a configuration this
		// agent cannot do anything about, so it is reported rather than worked around.
		return fmt.Errorf("the system bus refused the connection: %s", strings.TrimSpace(reply))
	}

	if err := c.line("BEGIN"); err != nil {
		return err
	}

	// Hello is mandatory: the bus assigns this connection a name and will not route anything until it has.
	if _, _, err := c.Call(
		"org.freedesktop.DBus",
		"/org/freedesktop/DBus",
		"org.freedesktop.DBus",
		"Hello",
		"",
	); err != nil {
		return fmt.Errorf("the bus would not complete Hello: %w", err)
	}

	return nil
}

func (c *Conn) line(format string, args ...any) error {
	_, err := fmt.Fprintf(c.rw, format+"\r\n", args...)
	if err != nil {
		return fmt.Errorf("cannot write to the system bus: %w", err)
	}

	return nil
}

func (c *Conn) readLine() (string, error) {
	line, err := c.in.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("the system bus closed during the handshake: %w", err)
	}

	return line, nil
}

func hexOf(s string) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(s)*2)
	for i := 0; i < len(s); i++ {
		out = append(out, digits[s[i]>>4], digits[s[i]&0x0f])
	}

	return string(out)
}

// Message header field codes, from the specification.
const (
	fieldPath        = 1
	fieldInterface   = 2
	fieldMember      = 3
	fieldErrorName   = 4
	fieldReplySerial = 5
	fieldDestination = 6
	fieldSignature   = 8
)

const (
	msgMethodCall   = 1
	msgMethodReturn = 2
	msgError        = 3
)

// Call makes a method call and returns the reply body together with its signature.
//
// `args` covers only the shapes this agent sends, which is strings — `GetUnitFileState` takes one and
// `Properties.Get` takes two. Anything else is deliberately not expressible: a client that can only ask
// for strings cannot be talked into sending a unit-control payload by mistake.
func (c *Conn) Call(destination, path, iface, member string, signature string, args ...string) ([]byte, string, error) {
	body := &encoder{}
	for _, arg := range args {
		body.str(arg)
	}

	c.serial++

	msg := &encoder{}
	msg.byte('l') // little-endian
	msg.byte(msgMethodCall)
	msg.byte(0) // flags: none. Notably not NO_REPLY_EXPECTED — we always want the answer.
	msg.byte(1) // protocol version
	msg.uint32(uint32(len(body.buf)))
	msg.uint32(c.serial)

	fields := &encoder{}
	appendField(fields, fieldPath, 'o', path)
	appendField(fields, fieldDestination, 's', destination)
	appendField(fields, fieldInterface, 's', iface)
	appendField(fields, fieldMember, 's', member)
	if signature != "" {
		appendField(fields, fieldSignature, 'g', signature)
	}

	msg.uint32(uint32(len(fields.buf)))
	msg.buf = append(msg.buf, fields.buf...)
	// The body starts on an eight-byte boundary measured from the start of the message, whatever the
	// header length happened to be. Forgetting this is the classic way to write a D-Bus encoder that
	// works for one method and not the next.
	msg.pad(8)
	msg.buf = append(msg.buf, body.buf...)

	if _, err := c.rw.Write(msg.buf); err != nil {
		return nil, "", fmt.Errorf("cannot send %s to the system bus: %w", member, err)
	}

	return c.readReply(c.serial, member)
}

func appendField(e *encoder, code byte, typ byte, value string) {
	// Each field is a struct of (byte, variant), and a struct starts on an eight-byte boundary.
	e.pad(8)
	e.byte(code)
	e.signature(string(typ))
	if typ == 'g' {
		e.signature(value)
	} else {
		e.str(value)
	}
}

// readReply reads messages until the one answering our serial arrives.
//
// A loop rather than a single read because the bus may deliver signals — NameAcquired arrives
// unprompted right after Hello — and a client that assumed the next message was its own reply would
// parse a signal as an answer and return whatever that happened to decode to.
func (c *Conn) readReply(serial uint32, member string) ([]byte, string, error) {
	for attempts := 0; attempts < 16; attempts++ {
		head := make([]byte, 16)
		if _, err := io.ReadFull(c.in, head); err != nil {
			return nil, "", fmt.Errorf("no reply to %s from the system bus: %w", member, err)
		}

		d := &decoder{buf: head}
		endian := d.byte()
		if endian != 'l' {
			// Big-endian is legal and systemd never sends it. Refusing is honest: the decoder below
			// reads little-endian, and pretending otherwise would return numbers with their bytes
			// reversed rather than an error anyone could act on.
			return nil, "", errors.New("the system bus replied big-endian, which this client does not read")
		}
		msgType := d.byte()
		d.byte() // flags
		d.byte() // protocol version
		bodyLen, err := checkedLen(d.uint32())
		if err != nil {
			return nil, "", err
		}
		replySerial := d.uint32()
		_ = replySerial
		fieldsLen, err := checkedLen(d.uint32())
		if err != nil {
			return nil, "", err
		}

		// The fields array, then padding to the body's eight-byte boundary. Both are read whatever this
		// message turns out to be, because skipping a message means consuming all of it.
		fields := make([]byte, fieldsLen)
		if _, err := io.ReadFull(c.in, fields); err != nil {
			return nil, "", fmt.Errorf("truncated reply header for %s: %w", member, err)
		}
		if padding := align(16+fieldsLen, 8) - (16 + fieldsLen); padding > 0 {
			if _, err := io.ReadFull(c.in, make([]byte, padding)); err != nil {
				return nil, "", fmt.Errorf("truncated reply padding for %s: %w", member, err)
			}
		}

		body := make([]byte, bodyLen)
		if _, err := io.ReadFull(c.in, body); err != nil {
			return nil, "", fmt.Errorf("truncated reply body for %s: %w", member, err)
		}

		gotSerial, signature, errName := parseFields(fields)

		switch {
		case msgType == msgError && gotSerial == serial:
			// The bus names what went wrong; the body's first string explains it. Both are worth keeping:
			// the name is stable enough to branch on and the text is what a person needs.
			detail := ""
			if strings.HasPrefix(signature, "s") {
				detail = (&decoder{buf: body}).str()
			}

			return nil, "", fmt.Errorf("%s: %s", errName, detail)
		case msgType == msgMethodReturn && gotSerial == serial:
			return body, signature, nil
		}
		// Anything else is somebody else's message. Read the next one.
	}

	return nil, "", fmt.Errorf("the system bus sent nothing answering %s", member)
}

// parseFields reads the header field array for the three things a reply tells us.
func parseFields(buf []byte) (replySerial uint32, signature string, errName string) {
	d := &decoder{buf: buf}

	for d.pos < len(d.buf) && d.err == nil {
		d.pad(8)
		if d.pos >= len(d.buf) {
			break
		}
		code := d.byte()
		typ := d.signature()

		switch code {
		case fieldReplySerial:
			replySerial = d.uint32()
		case fieldSignature:
			signature = d.signature()
		case fieldErrorName:
			errName = d.str()
		default:
			d.skip(typ)
		}
	}

	if errName == "" {
		errName = "the system bus returned an error"
	}

	return replySerial, signature, errName
}
