package collect

import (
	"math"
	"strings"
	"testing"
)

// Captured from a real Linux box. The point of splitting the parsers from the file reading is that these
// run anywhere, including the machine this was developed on.
const sampleStat = `cpu  100 20 50 800 15 5 3 7 0 0
cpu0 50 10 25 400 7 2 1 3 0 0
intr 12345
ctxt 67890
`

const sampleMeminfo = `MemTotal:        8009424 kB
MemFree:          204812 kB
MemAvailable:    3059276 kB
Buffers:          124560 kB
Cached:          2985644 kB
SwapTotal:       2097148 kB
SwapFree:        2097148 kB
`

func TestParseStatReadsTheAggregateLine(t *testing.T) {
	got, err := parseStat(strings.NewReader(sampleStat))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.user != 100 || got.system != 50 || got.idle != 800 {
		t.Errorf("wrong fields: %+v", got)
	}
	if got.steal != 7 || got.iowait != 15 {
		t.Errorf("steal/iowait not read: %+v", got)
	}
}

// The reason this agent exists in the shape it does. Steal is CPU the hypervisor gave another tenant and
// iowait is time waiting on a disk; neither is work this machine did, and neither is something anyone
// logged into it can fix. Counting them produced alerts nobody could act on.
func TestCPUPercentExcludesStealAndIowait(t *testing.T) {
	before := cpuTimes{}
	// 100 ticks of real work, 100 of steal, 100 of iowait, 700 idle: 1000 ticks of wall time.
	after := cpuTimes{user: 60, system: 40, steal: 100, iowait: 100, idle: 700}

	got, err := cpuPercent(before, after)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if math.Abs(got-10) > 0.001 {
		t.Errorf("want 10%% (user+system of 1000 ticks), got %v — is steal or iowait being counted?", got)
	}

	// The naive alternative, for contrast: 100 - idle would call this 30%.
	naive := float64(after.total()-after.idle) / float64(after.total()) * 100
	if math.Abs(naive-30) > 0.001 {
		t.Fatalf("test premise wrong: naive calculation gives %v", naive)
	}
}

func TestCPUPercentRefusesCountersThatWentBackwards(t *testing.T) {
	// A reboot between samples. Reporting the wrap-around as a spike would alert on a machine that is
	// fine, which is the failure this whole family of bugs keeps coming back to.
	if _, err := cpuPercent(cpuTimes{user: 500, idle: 500}, cpuTimes{user: 1, idle: 1}); err == nil {
		t.Error("expected an error when counters decrease")
	}
}

func TestCPUPercentRefusesAnEmptyInterval(t *testing.T) {
	c := cpuTimes{user: 10, idle: 10}
	if _, err := cpuPercent(c, c); err == nil {
		t.Error("expected an error when no time elapsed")
	}
}

func TestParseMeminfoCountsCacheAsAvailable(t *testing.T) {
	used, total, swap, err := parseMeminfo(strings.NewReader(sampleMeminfo))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if total != 8009424*1024 {
		t.Errorf("total: got %d", total)
	}

	// total - MemAvailable, not total - MemFree. Using MemFree counts the page cache as used and makes a
	// healthy Linux machine look permanently full.
	want := int64(8009424-3059276) * 1024
	if used != want {
		t.Errorf("used: got %d, want %d — is MemFree being used instead of MemAvailable?", used, want)
	}

	if swap != 0 {
		t.Errorf("swap: got %d, want 0", swap)
	}
}

func TestParseMeminfoFallsBackWhenMemAvailableIsAbsent(t *testing.T) {
	old := `MemTotal:        1000 kB
MemFree:          100 kB
Buffers:           50 kB
Cached:           200 kB
`
	used, total, _, err := parseMeminfo(strings.NewReader(old))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Kernels before 3.14. Approximately right beats absent.
	if total != 1000*1024 || used != (1000-350)*1024 {
		t.Errorf("used=%d total=%d", used, total)
	}
}

func TestParseMeminfoNeedsATotal(t *testing.T) {
	if _, _, _, err := parseMeminfo(strings.NewReader("Cached: 10 kB\n")); err == nil {
		t.Error("expected an error when MemTotal is missing")
	}
}

func TestParseLoadavg(t *testing.T) {
	one, five, fifteen, err := parseLoadavg(strings.NewReader("0.24 0.31 0.28 1/523 12345\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if one != 0.24 || five != 0.31 || fifteen != 0.28 {
		t.Errorf("got %v %v %v", one, five, fifteen)
	}
}

func TestParseUptimeTakesWholeSeconds(t *testing.T) {
	got, err := parseUptime(strings.NewReader("1573200.42 6218879.11\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1573200 {
		t.Errorf("got %d", got)
	}
}

func TestParsersRejectRubbish(t *testing.T) {
	if _, err := parseStat(strings.NewReader("not a stat file\n")); err == nil {
		t.Error("parseStat accepted rubbish")
	}
	if _, _, _, err := parseLoadavg(strings.NewReader("only-one-field\n")); err == nil {
		t.Error("parseLoadavg accepted rubbish")
	}
	if _, err := parseUptime(strings.NewReader("\n")); err == nil {
		t.Error("parseUptime accepted an empty file")
	}
}

func TestCPUPercentRefusesWhenBusyGoesBackwardsButTotalDoesNot(t *testing.T) {
	// The subtle half of the backwards-counter case, and the dangerous one. A vCPU hot-unplug, or a
	// kernel whose iowait and steal columns are not monotonic, can drop the busy components while total
	// still rises. Unsigned subtraction wraps to ~1.8e19, and clamping turns that into exactly 100.0 —
	// a machine reported as pinned at 100% CPU, with no error and nothing in `failed` to contradict it.
	before := cpuTimes{user: 100, system: 100}
	after := cpuTimes{user: 50, system: 50, iowait: 500, steal: 500}

	if after.total() <= before.total() {
		t.Fatal("test premise wrong: total must still rise for this to be the interesting case")
	}

	got, err := cpuPercent(before, after)
	if err == nil {
		t.Errorf("accepted a wrapped reading and returned %v%%", got)
	}
}
