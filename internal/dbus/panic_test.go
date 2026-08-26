package dbus

import "testing"

// A malformed signature must be an error, never a crash.
//
// These strings are not hypothetical: header field signatures are read off the wire, so the shape of
// what `skip` is asked to walk is chosen by whatever is on the other end of the socket. `skip("a")` —
// an array of nothing, which is not a valid type — indexed past the end of the string and panicked,
// taking the whole agent down with it. The decoder's own comment promises the opposite, and a
// monitoring agent that dies on a malformed reply takes the monitoring with it.
func TestAMalformedSignatureIsAnErrorRatherThanACrash(t *testing.T) {
	for _, sig := range []string{"a", "aa", "aaa", "(", "a(", "{", "v", "a{", "(a"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("skip(%q) panicked: %v", sig, r)
				}
			}()

			d := &decoder{buf: []byte{0, 0, 0, 0, 0, 0, 0, 0}}
			d.skip(sig)
		}()
	}
}

// The same bytes through the path that actually reads them: a reply header whose field signature is
// malformed. This is the reachable version of the case above.
func TestAMalformedHeaderFieldDoesNotCrashTheReader(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("parseFields panicked: %v", r)
		}
	}()

	// field code 9 (unknown), signature "a", then nothing that follows it.
	parseFields([]byte{9, 1, 'a', 0, 0, 0, 0, 0})
}
