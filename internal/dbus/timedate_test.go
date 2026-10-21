package dbus

import "testing"

// NTPSynchronized (InfraNest #4391) arrives as a variant holding a `b` — four bytes, 0 or 1.
func TestNTPSynchronizedIsReadAsABoolean(t *testing.T) {
	for _, tc := range []struct {
		raw  uint32
		want bool
	}{
		{0, false},
		{1, true},
	} {
		got, err := decodeVariantBool(variantBody("b", 4, rawU32(tc.raw)), "NTPSynchronized")
		if err != nil {
			t.Fatalf("%d: %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("%d decoded to %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// The negative controls. "Synchronised" is the answer that keeps an alert quiet, so anything that is not
// plainly a boolean must be an error rather than a value.
func TestAnythingButABooleanIsRefused(t *testing.T) {
	if _, err := decodeVariantBool(variantBody("s", 4, append(rawU32(1), 'x', 0)), "NTPSynchronized"); err == nil {
		t.Fatal("a string was accepted as a boolean")
	}
	if _, err := decodeVariantBool(variantBody("b", 4, rawU32(2)), "NTPSynchronized"); err == nil {
		t.Fatal("2 was accepted as a boolean")
	}
	if _, err := decodeVariantBool(variantBody("b", 4, nil), "NTPSynchronized"); err == nil {
		t.Fatal("a reply with no value was accepted")
	}
}
