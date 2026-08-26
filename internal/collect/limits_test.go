package collect

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The failure this prevents is a permanent one, so it is worth a test that states it.
//
// A string over the server's limit does not cost that field: it fails validation for the entire push,
// and a validation failure carries no per-reading verdict — so the batch never settles, stays at the head
// of the spool, and is offered again for ever with every later reading queued behind it. One long command
// line would silently end monitoring on that machine.
func TestNothingIsSentLongerThanTheIngestAccepts(t *testing.T) {
	long := strings.Repeat("x", 4096)

	for _, tc := range []struct {
		name  string
		got   string
		limit int
	}{
		{"command", clip(long, maxCommand), maxCommand},
		{"mount point", clip(long, maxMountPoint), maxMountPoint},
		{"unit", clip(long, maxUnit), maxUnit},
		{"description", clip(long, maxDescription), maxDescription},
		{"state", clip(long, maxState), maxState},
		{"usage path", clip(long, maxUsagePath), maxUsagePath},
		{"kernel", clip(long, maxSystemField), maxSystemField},
		{"failure reason", clip(long, maxFailReason), maxFailReason},
	} {
		if len(tc.got) > tc.limit {
			t.Fatalf("%s: %d bytes, limit is %d", tc.name, len(tc.got), tc.limit)
		}
	}
}

func TestClippingNeverSplitsACharacter(t *testing.T) {
	// Mount points and command lines are arbitrary bytes chosen by somebody else, and a path with an
	// emoji in it is unusual rather than invalid. Cutting mid-character produces invalid UTF-8, which the
	// JSON encoder turns into a replacement character and the server then stores.
	s := strings.Repeat("é", 200) // two bytes each, so most limits land mid-character

	for n := 1; n < 400; n++ {
		got := clip(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("clip(…, %d) produced invalid UTF-8", n)
		}
		if len(got) > n {
			t.Fatalf("clip(…, %d) returned %d bytes", n, len(got))
		}
	}
}

func TestAShortStringIsUntouched(t *testing.T) {
	if got := clip("nginx.service", maxUnit); got != "nginx.service" {
		t.Fatalf("clip shortened a string that fitted: %q", got)
	}
}
