package collect

import (
	"strings"
	"testing"
)

/*
The System card's fields (#767).

All of it read from files the distribution already wrote, because the agent executes nothing. What is
worth testing is the *absent* cases: every field here is one somebody would act on, and "0 updates
pending" is a claim that gets acted on by not patching.
*/

func TestThePrettyNameIsTheOsName(t *testing.T) {
	const osRelease = `PRETTY_NAME="Ubuntu 24.04.1 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
`
	// Quotes are part of the format, not the name.
	if got := parseOSRelease(strings.NewReader(osRelease)); got != "Ubuntu 24.04.1 LTS" {
		t.Fatalf("got %q", got)
	}
}

func TestAFileWithoutAPrettyNameYieldsNothing(t *testing.T) {
	if got := parseOSRelease(strings.NewReader("NAME=\"Something\"\n")); got != "" {
		t.Fatalf("invented an OS name: %q", got)
	}
}

func TestTheUpdateCountsAreReadFromTheStamp(t *testing.T) {
	// Captured from a real Ubuntu box, which is the only thing worth parsing against.
	const stamp = `
3 updates can be applied immediately.
1 of these updates is a standard security update.
To see these additional updates run: apt list --upgradable
`
	pending, security, ok := parseUpdateStamp(stamp)

	if !ok || pending != 3 || security != 1 {
		t.Fatalf("pending=%d security=%d ok=%v", pending, security, ok)
	}
}

func TestAMachineWithNothingPendingSaysZeroRatherThanNothing(t *testing.T) {
	// The distinction that matters: a stamp saying zero is a *measurement* of zero, and it should be
	// reported as such. It is the absent stamp — the next test — that must not become a zero.
	pending, security, ok := parseUpdateStamp("0 updates can be applied immediately.\n")

	if !ok || pending != 0 || security != 0 {
		t.Fatalf("pending=%d security=%d ok=%v", pending, security, ok)
	}
}

func TestAnUnparseableStampReportsNothingRatherThanZero(t *testing.T) {
	// On a machine where update-notifier is absent or its timer is off, there is no answer — and "0
	// updates" is exactly the wrong thing to show, because somebody acts on it by not patching.
	if _, _, ok := parseUpdateStamp("Welcome to your server!\n"); ok {
		t.Fatal("invented an update count from a file that had none")
	}
}

func TestTheSecurityLineIsNotCountedTwice(t *testing.T) {
	// Both lines start with a number and both contain "update". Without the security check running
	// first, the security line would also be read as the pending total.
	pending, security, _ := parseUpdateStamp(
		"14 updates can be applied immediately.\n3 of these updates are standard security updates.\n")

	if pending != 14 || security != 3 {
		t.Fatalf("pending=%d security=%d", pending, security)
	}
}
