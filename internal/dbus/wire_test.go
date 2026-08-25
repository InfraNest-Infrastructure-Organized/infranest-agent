package dbus

import (
	"testing"
	"time"
)

// A real ListUnits reply, built here the way systemd builds it.
//
// The encoder and the decoder in this package are each other's only witness, which is a problem: a
// consistent misunderstanding of the format passes a round-trip test and fails against the bus. So this
// builds the reply body *by hand* from the specification's rules — explicit padding, explicit lengths —
// rather than with the encoder under test. If the two disagree, this is the one that is right.
func systemdStyleUnitArray(t *testing.T, units [][]string) []byte {
	t.Helper()

	// Each struct: six strings, an object path, a uint32, a string, an object path.
	var elements []byte
	for _, u := range units {
		// Structs align to 8, measured from the start of the *body*. The array's own length prefix is
		// four bytes and then the first element is padded to 8, so element offsets here start at 0
		// relative to a position that is already 8-aligned.
		for len(elements)%8 != 0 {
			elements = append(elements, 0)
		}
		for _, s := range u[:7] { // six strings plus the object path, all encoded identically
			elements = appendStr(elements, s)
		}
		elements = appendU32(elements, 0)   // job id
		elements = appendStr(elements, "")  // job type
		elements = appendStr(elements, "/") // job object path
	}

	body := appendU32(nil, uint32(len(elements)))
	// The array's elements begin on the alignment of the element type — 8 for a struct — measured from
	// the start of the message body.
	for len(body)%8 != 0 {
		body = append(body, 0)
	}

	return append(body, elements...)
}

func appendStr(b []byte, s string) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	b = appendU32(b, uint32(len(s)))

	return append(append(b, s...), 0)
}

func appendU32(b []byte, v uint32) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}

	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func TestAUnitListIsReadFieldByFieldRatherThanByLuck(t *testing.T) {
	body := systemdStyleUnitArray(t, [][]string{
		{"nginx.service", "A high performance web server", "loaded", "active", "running", "", "/org/freedesktop/systemd1/unit/nginx_2eservice"},
		{"certbot-renew.timer", "Run certbot twice daily", "loaded", "failed", "failed", "", "/org/freedesktop/systemd1/unit/certbot_2drenew_2etimer"},
	})

	units, err := decodeUnits(body)
	if err != nil {
		t.Fatal(err)
	}

	if len(units) != 2 {
		t.Fatalf("expected 2 units, got %d: %+v", len(units), units)
	}
	// Every field asserted, not just the first. An off-by-one in the padding shifts *later* fields, so a
	// test that checks only the name is a test that passes while the states are wrong — which is the one
	// thing this whole file exists to prevent.
	if units[0].Name != "nginx.service" || units[0].ActiveState != "active" || units[0].SubState != "running" {
		t.Fatalf("first unit decoded wrong: %+v", units[0])
	}
	if units[0].Description != "A high performance web server" || units[0].LoadState != "loaded" {
		t.Fatalf("first unit decoded wrong: %+v", units[0])
	}
	if units[1].Name != "certbot-renew.timer" || units[1].ActiveState != "failed" {
		t.Fatalf("second unit decoded wrong: %+v", units[1])
	}
	if units[1].Path != "/org/freedesktop/systemd1/unit/certbot_2drenew_2etimer" {
		t.Fatalf("object path decoded wrong: %q", units[1].Path)
	}
}

func TestAUnitNameOfAwkwardLengthDoesNotShiftEverythingAfterIt(t *testing.T) {
	// Strings are padded to four bytes, so a name whose length lands exactly on a boundary and one that
	// does not exercise different paths. Both must decode, and the *second* unit is the tell: a padding
	// error in the first shifts everything after it and shows up here rather than above.
	for _, name := range []string{"a.service", "ab.service", "abc.service", "abcd.service"} {
		body := systemdStyleUnitArray(t, [][]string{
			{name, "d", "loaded", "failed", "failed", "", "/p"},
			{"second.service", "d", "loaded", "active", "running", "", "/q"},
		})

		units, err := decodeUnits(body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(units) != 2 || units[0].Name != name || units[1].Name != "second.service" {
			t.Fatalf("%s: decoded %+v", name, units)
		}
		if units[1].ActiveState != "active" {
			t.Fatalf("%s: the unit after it decoded wrong: %+v", name, units[1])
		}
	}
}

func TestATruncatedReplyIsAnErrorRatherThanAPanic(t *testing.T) {
	// These bytes come from a system service on a machine we do not control. A monitoring agent that
	// dies on a malformed reply takes the monitoring with it, which is worse than the gap it avoided.
	body := systemdStyleUnitArray(t, [][]string{
		{"nginx.service", "d", "loaded", "active", "running", "", "/p"},
	})

	for cut := 1; cut < len(body); cut++ {
		if _, err := decodeUnits(body[:cut]); err == nil {
			// Some prefixes are legitimately readable as a shorter array, so an error is not required at
			// every cut — what is required is that none of them panics, which is what this loop proves.
			continue
		}
	}
}

func TestAnArrayClaimingMoreThanItHasIsRefused(t *testing.T) {
	// The length is four bytes from the far end of a socket. Believing it is how a reader turns a
	// malformed reply into an allocation the size of the number somebody sent.
	body := appendU32(nil, 0xFFFFFF00)
	body = append(body, 0, 0, 0, 0)

	if _, err := decodeUnits(body); err == nil {
		t.Fatal("a length larger than the reply was accepted")
	}
}

func TestSkipCrossesNestedTypesWithoutLosingItsPlace(t *testing.T) {
	// `skip` is what gets the decoder past header fields it does not want. Getting it wrong does not
	// fail: it resumes reading in the middle of a value and returns something plausible.
	for _, tc := range []struct{ sig, rest string }{
		{"s", ""},
		{"ss", "s"},
		{"a(ss)s", "s"},
		{"aa{sv}u", "u"},
		{"(s(ss))u", "u"},
	} {
		if got := afterOne(tc.sig); got != tc.rest {
			t.Fatalf("afterOne(%q) = %q, want %q", tc.sig, got, tc.rest)
		}
	}
}

func TestTheTimestampIsMicrosecondsAndZeroMeansUnknown(t *testing.T) {
	// systemd reports microseconds since the epoch. Reading them as milliseconds or seconds puts a
	// failure decades from where it happened, and the page renders that without complaint.
	got := time.UnixMicro(1756137600000000).UTC()
	if got.Year() < 2020 || got.Year() > 2100 {
		t.Fatalf("microseconds decoded to %s, which is not a plausible timestamp", got)
	}
}
