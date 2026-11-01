package dbus

import "testing"

// FragmentPath (InfraNest #5789) is systemd's answer to "which file was this unit loaded from", a string on the
// unit interface. A unit with no file of its own answers with an empty string, which must read as empty rather
// than as an error or a path.
func TestAUnitFileIsReadAsAStringAndAnEmptyAnswerStaysEmpty(t *testing.T) {
	for _, want := range []string{"/lib/systemd/system/ssh.service", ""} {
		got, err := decodeVariantString(variantBody("s", 4, rawStr(want)), "FragmentPath")
		if err != nil {
			t.Fatalf("FragmentPath %q: %v", want, err)
		}
		if got != want {
			t.Fatalf("FragmentPath decoded to %q, want %q", got, want)
		}
	}

	// An object path is a different type on the wire with the same layout; it must be refused, not reinterpreted.
	if _, err := decodeVariantString(variantBody("o", 4, rawStr("/org/freedesktop/systemd1")), "FragmentPath"); err == nil {
		t.Fatal("an object path was accepted as FragmentPath")
	}
}
