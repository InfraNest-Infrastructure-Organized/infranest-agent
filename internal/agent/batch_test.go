package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func sample(t *testing.T, body string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(body)) {
		t.Fatalf("test fixture is not valid JSON: %s", body)
	}

	return json.RawMessage(body)
}

func TestOnlyTheNewestSampleKeepsItsSnapshots(t *testing.T) {
	// The server applies snapshots from the newest reading and ignores the rest, so a backfill used to
	// carry sixty copies of the same service list. Measured at the caps the published contract allows,
	// that is the difference between 32.4 MB and 5.6 MB — between exceeding the deployed request limit
	// and fitting inside it.
	in := []json.RawMessage{
		sample(t, `{"collected_at":"2026-08-26T10:00:00Z","cpu_percent":1,"services":[{"unit":"a"}],"system":{"os":"x"},"processes":[{"command":"p"}]}`),
		sample(t, `{"collected_at":"2026-08-26T10:01:00Z","cpu_percent":2,"services":[{"unit":"b"}],"system":{"os":"x"}}`),
		sample(t, `{"collected_at":"2026-08-26T10:02:00Z","cpu_percent":3,"services":[{"unit":"c"}],"system":{"os":"x"}}`),
	}

	out := trimSnapshots(in)

	for i, want := range []bool{false, false, true} {
		has := strings.Contains(string(out[i]), `"services"`)
		if has != want {
			t.Fatalf("sample %d: services present = %v, want %v (%s)", i, has, want, out[i])
		}
	}

	// The newest is untouched, byte for byte: it is the one the server actually reads.
	if string(out[2]) != string(in[2]) {
		t.Fatalf("the newest sample was rewritten:\n got %s\nwant %s", out[2], in[2])
	}
}

func TestTrimmingKeepsEveryReadingAndItsMeasurements(t *testing.T) {
	// The saving must cost nothing. Every sample survives, and `mounts` survives on all of them —
	// `worst_mount_percent` is derived per sample and is what a disk rule averages over its window, so
	// dropping mounts from a backfilled reading is a hole in that series rather than a saving.
	in := []json.RawMessage{
		sample(t, `{"collected_at":"2026-08-26T10:00:00Z","cpu_percent":1,"mounts":[{"mount_point":"/"}],"services":[{"unit":"a"}]}`),
		sample(t, `{"collected_at":"2026-08-26T10:01:00Z","cpu_percent":2,"mounts":[{"mount_point":"/"}],"services":[{"unit":"b"}]}`),
	}

	out := trimSnapshots(in)

	if len(out) != len(in) {
		t.Fatalf("readings were lost: %d in, %d out", len(in), len(out))
	}
	for i, s := range out {
		if !strings.Contains(string(s), `"mounts"`) {
			t.Fatalf("sample %d lost its mounts: %s", i, s)
		}
		if !strings.Contains(string(s), `"cpu_percent"`) {
			t.Fatalf("sample %d lost its measurements: %s", i, s)
		}
	}
}

func TestASampleThatCannotBeParsedIsPassedThroughUntouched(t *testing.T) {
	// This is an optimisation. An optimisation that drops readings when it meets something unexpected is
	// worse than the cost it saves.
	broken := json.RawMessage(`{not json`)
	in := []json.RawMessage{broken, sample(t, `{"collected_at":"2026-08-26T10:01:00Z"}`)}

	out := trimSnapshots(in)

	if string(out[0]) != string(broken) {
		t.Fatalf("an unparseable sample was altered: %s", out[0])
	}
	if len(out) != 2 {
		t.Fatalf("a reading was dropped: %d", len(out))
	}
}

func TestASingleSampleBatchIsLeftAlone(t *testing.T) {
	// The common case by far: one reading per push. It is the newest, so it keeps everything.
	in := []json.RawMessage{sample(t, `{"collected_at":"2026-08-26T10:00:00Z","services":[{"unit":"a"}]}`)}

	if out := trimSnapshots(in); !strings.Contains(string(out[0]), `"services"`) {
		t.Fatalf("the only sample lost its snapshots: %s", out[0])
	}
}

// A clock that steps backwards mid-batch must not strip the sample the server will read.
//
// The spool appends in sequence order, so position and time agreed "by construction" — right up until an
// NTP correction or a resumed VM stamps a later collection with an earlier time. Trimming by position
// then removes the snapshot from the sample the server picks by timestamp, and that whole batch updates
// no services, processes or system facts at all. It self-heals on the next push, which is exactly why it
// would never be caught in the wild.
func TestTheSampleTheServerReadsKeepsItsSnapshotsWhenTheClockStepsBack(t *testing.T) {
	in := []json.RawMessage{
		sample(t, `{"collected_at":"2026-08-26T10:00:00Z","cpu_percent":1,"services":[{"unit":"a"}]}`),
		// The newest by time, and NOT last in the batch — the clock was corrected after it was collected.
		sample(t, `{"collected_at":"2026-08-26T10:05:00Z","cpu_percent":2,"services":[{"unit":"b"}]}`),
		sample(t, `{"collected_at":"2026-08-26T10:02:00Z","cpu_percent":3,"services":[{"unit":"c"}]}`),
	}

	out := trimSnapshots(in)

	if !strings.Contains(string(out[1]), `"services"`) {
		t.Fatalf("the newest sample by timestamp lost its snapshot, so the server has nothing to apply: %s", out[1])
	}
	for _, i := range []int{0, 2} {
		if strings.Contains(string(out[i]), `"services"`) {
			t.Fatalf("sample %d kept a snapshot the server will ignore: %s", i, out[i])
		}
	}
}

// With a well-behaved clock, nothing changes: the last sample is both newest and last.
func TestAMonotonicClockStillTrimsToTheLastSample(t *testing.T) {
	in := []json.RawMessage{
		sample(t, `{"collected_at":"2026-08-26T10:00:00Z","services":[{"unit":"a"}]}`),
		sample(t, `{"collected_at":"2026-08-26T10:01:00Z","services":[{"unit":"b"}]}`),
	}

	out := trimSnapshots(in)

	if strings.Contains(string(out[0]), `"services"`) || !strings.Contains(string(out[1]), `"services"`) {
		t.Fatalf("ordinary batch trimmed wrongly: %s | %s", out[0], out[1])
	}
}

// No usable timestamps at all falls back to position — the behaviour this replaced.
func TestSamplesWithNoTimestampFallBackToPosition(t *testing.T) {
	in := []json.RawMessage{
		sample(t, `{"services":[{"unit":"a"}]}`),
		sample(t, `{"services":[{"unit":"b"}]}`),
	}

	out := trimSnapshots(in)

	if !strings.Contains(string(out[1]), `"services"`) {
		t.Fatalf("with no timestamps the last sample should be kept: %s", out[1])
	}
}
