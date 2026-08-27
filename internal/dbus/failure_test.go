package dbus

import "testing"

// A `Properties.Get` reply body, built the way the bus builds one: a signature, then the value at its
// own alignment. Built by hand rather than with the encoder, for the reason given in wire_test.go — an
// encoder and decoder that share a misunderstanding agree with each other and disagree with systemd.
// The value's alignment is measured from the start of the *body*, not from the start of the value — so
// the signature has to be laid down first and the padding computed after it. Getting that wrong is how a
// hand-built fixture disagrees with the bus while looking correct.
func variantBody(sig string, align int, value []byte) []byte {
	b := []byte{byte(len(sig))}
	b = append(b, sig...)
	b = append(b, 0)
	for len(b)%align != 0 {
		b = append(b, 0)
	}

	return append(b, value...)
}

// A four-byte little-endian value with no padding of its own — variantBody has already aligned the body.
func rawU32(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

// A D-Bus string: a four-byte length, the bytes, and the NUL the length does not count.
func rawStr(s string) []byte {
	return append(append(rawU32(uint32(len(s))), s...), 0)
}

func TestAFailureReasonIsReadAsAStringOrRefused(t *testing.T) {
	body := variantBody("s", 4, rawStr("oom-kill"))
	got, err := decodeVariantString(body, "Result")
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got != "oom-kill" {
		t.Fatalf("Result decoded to %q, want %q", got, "oom-kill")
	}

	// The negative control. A property answering with a different type must be an error — decoding it
	// anyway reads a length off a number and returns whatever happens to follow, which is a value the
	// page will render as a failure reason.
	wrong := variantBody("t", 8, make([]byte, 8))
	if _, err := decodeVariantString(wrong, "Result"); err == nil {
		t.Fatal("a uint64 was accepted as a Result string; a wrong type must be refused, not reinterpreted")
	}
}

// The bytes for -1 and for 4294967295 are the same four bytes, so the conversion is the only thing
// separating "killed by signal 1" from a four-billion exit status.
func TestASignedExitStatusSurvivesTheDecode(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int32
	}{
		{"a clean exit", 0},
		{"an ordinary failure", 1},
		{"the highest byte status", 255},
		{"a negative value", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := variantBody("i", 4, rawU32(uint32(tc.want)))
			got, err := decodeVariantInt32(body, "ExecMainStatus")
			if err != nil {
				t.Fatalf("ExecMainStatus: %v", err)
			}
			if got != tc.want {
				t.Fatalf("decoded %d, want %d", got, tc.want)
			}
		})
	}

	// The negative control, same as above: `u` and `i` are laid out identically, so nothing but the
	// signature check distinguishes them and a missing check would be invisible.
	if _, err := decodeVariantInt32(variantBody("u", 4, rawU32(7)), "ExecMainStatus"); err == nil {
		t.Fatal("a uint32 was accepted as an int32; the signature check is what makes the sign meaningful")
	}
}
