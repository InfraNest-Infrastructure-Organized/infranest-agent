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

func TestTheOOMCounterIsReadFromVmstat(t *testing.T) {
	// An excerpt of a real /proc/vmstat: the line we want sits among a hundred we do not.
	const vmstat = `nr_free_pages 123456
pgmajfault 9001
oom_kill 3
numa_hit 42
`
	n, ok := parseOOMKills(strings.NewReader(vmstat))

	if !ok || n != 3 {
		t.Fatalf("n=%d ok=%v", n, ok)
	}
}

func TestAKernelWithoutTheOOMCounterReportsNothingRatherThanZero(t *testing.T) {
	// Before 4.13 the line does not exist. Zero would claim nothing was killed; the honest answer is that
	// this kernel cannot say.
	if _, ok := parseOOMKills(strings.NewReader("nr_free_pages 123456\npgmajfault 9001\n")); ok {
		t.Fatal("invented an OOM count for a kernel that keeps none")
	}
}

func TestAGarbledOOMCounterReportsNothing(t *testing.T) {
	if _, ok := parseOOMKills(strings.NewReader("oom_kill lots\n")); ok {
		t.Fatal("accepted a counter that is not a number")
	}
}

/*
Ubuntu Pro (#2811). The cache is the Pro client's own; these are trimmed from real ones, keeping the keys
the parser reads and enough of the rest to show it ignores them.
*/

const attachedProCache = `{
  "_schema_version": "0.1",
  "account": {"name": "Somebody Ltd", "id": "aAbBcC"},
  "attached": true,
  "contract": {"name": "Ubuntu Pro (Infra-only)", "id": "cCdDeE"},
  "expires": "2027-03-01T00:00:00+00:00",
  "machine_id": "0123456789abcdef",
  "services": [
    {"name": "esm-apps", "entitled": "yes", "status": "enabled"},
    {"name": "esm-infra", "entitled": "yes", "status": "enabled"},
    {"name": "livepatch", "entitled": "yes", "status": "warning"}
  ]
}`

func TestAnAttachedMachineReportsItsServicesAndExpiry(t *testing.T) {
	pro, ok := parseProStatus(strings.NewReader(attachedProCache))

	if !ok || !pro.Attached {
		t.Fatalf("pro=%+v ok=%v", pro, ok)
	}
	if pro.Livepatch != "warning" || pro.ESMInfra != "enabled" {
		t.Fatalf("services: %+v", pro)
	}
	if pro.Expires != "2027-03-01T00:00:00Z" {
		t.Fatalf("expires: %q", pro.Expires)
	}
}

func TestAnUnattachedCacheIsAClaimOfDetached(t *testing.T) {
	// An unattached machine's cache lists what *could* be enabled, with no status — and no contract.
	const cache = `{"attached": false, "expires": null,
	  "services": [{"name": "esm-infra", "available": "yes"}, {"name": "livepatch", "available": "yes"}]}`

	pro, ok := parseProStatus(strings.NewReader(cache))

	if !ok || pro.Attached {
		t.Fatalf("pro=%+v ok=%v", pro, ok)
	}
	if pro.Livepatch != "" || pro.ESMInfra != "" || pro.Expires != "" {
		t.Fatalf("invented a state for an unattached machine: %+v", pro)
	}
}

func TestACacheWithoutAnAttachedKeyYieldsNothingRatherThanDetached(t *testing.T) {
	// `attached: false` raises an alert, so a file this parser does not recognise must not become one.
	for _, body := range []string{`{"services": []}`, `not json`, ``, `{"attached": "yes"}`} {
		if pro, ok := parseProStatus(strings.NewReader(body)); ok {
			t.Fatalf("%q read as %+v", body, pro)
		}
	}
}

func TestAPlaceholderExpiryIsDroppedRatherThanSent(t *testing.T) {
	pro, ok := parseProStatus(strings.NewReader(`{"attached": true, "expires": "n/a"}`))

	if !ok || pro.Expires != "" {
		t.Fatalf("pro=%+v ok=%v", pro, ok)
	}
}
