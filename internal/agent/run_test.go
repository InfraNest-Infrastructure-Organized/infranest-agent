package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/collect"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/config"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/push"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/spool"
)

// fakeSender records what it was asked to send and answers however the test wants.
type fakeSender struct {
	batches [][]json.RawMessage
	urls    []string
	answer  func(n int) (push.Result, error)
}

func (f *fakeSender) Send(_ context.Context, url string, samples []json.RawMessage, _ map[string]string) (push.Result, error) {
	f.batches = append(f.batches, samples)
	f.urls = append(f.urls, url)

	if f.answer != nil {
		return f.answer(len(f.batches))
	}

	return push.Result{Accepted: len(samples)}, nil
}

// harness runs the loop for exactly n ticks, with time under the test's control.
//
// It returns the waits the loop asked for as well as the runner, because in fake time a longer backoff
// does not mean fewer sends — the clock simply jumps. What backing off actually *is*, observably, is the
// loop asking to wait longer, so that is what the tests read.
func harness(t *testing.T, sender *fakeSender, ticks int) (*Runner, *strings.Builder, *[]time.Duration) {
	t.Helper()

	dir := t.TempDir()
	sp, err := spool.New(filepath.Join(dir, "spool"))
	if err != nil {
		t.Fatal(err)
	}

	log := &strings.Builder{}
	clock := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	fired := 0

	r := &Runner{
		Config: config.Config{Token: "sat_x", URL: "https://ingest.infranest.app", Interval: time.Minute, StateDir: dir},
		Sender: sender,
		Spool:  sp,
		Log:    log,
		Now:    func() time.Time { return clock },
		// A reading that always works, so what is under test is the loop and not the machine running it.
		Collect: func(collect.Options) (collect.Sample, error) {
			return collect.Sample{CollectedAt: clock}, nil
		},
	}

	waits := &[]time.Duration{}

	ctx, cancel := context.WithCancel(context.Background())
	r.After = func(d time.Duration) <-chan time.Time {
		fired++
		*waits = append(*waits, d)
		clock = clock.Add(d)
		if fired >= ticks {
			cancel()
		}
		ch := make(chan time.Time, 1)
		ch <- clock

		return ch
	}

	if err := r.Run(ctx); err != nil {
		t.Fatalf("run returned %v", err)
	}

	return r, log, waits
}

func TestAnOutageCostsNothingButTheDelay(t *testing.T) {
	// The reason the server had to learn to take a batch and a backfill. Under the old design these
	// readings were simply lost — and the readings that fail to send are, by definition, the ones from
	// the minutes something was wrong.
	sender := &fakeSender{answer: func(n int) (push.Result, error) {
		if n <= 2 {
			return push.Result{}, errors.New("dial tcp: network is unreachable")
		}

		return push.Result{Accepted: 99}, nil
	}}

	_, _, _ = harness(t, sender, 6)

	// The first two attempts failed, so nothing was dropped; by the time one succeeds it carries
	// everything collected in the meantime rather than only the newest. Which push that is depends on how
	// many ticks ran, so the claim is that the backlog was delivered — not which request carried it.
	largest := 0
	for _, b := range sender.batches {
		if len(b) > largest {
			largest = len(b)
		}
	}
	if largest < 3 {
		t.Fatalf("expected one push to carry the whole backlog, but the largest held %d readings", largest)
	}
}

func TestReadingsSurviveUntilTheServerSaysItStoredThem(t *testing.T) {
	// A request that completes is not a reading that landed: a 502 from something in front of the API
	// completes too, and so does a 422 that stored none of the batch.
	sender := &fakeSender{answer: func(int) (push.Result, error) {
		return push.Result{Accepted: 0}, push.ErrServerRefused
	}}

	r, _, _ := harness(t, sender, 4)

	if r.Spool.Len() == 0 {
		t.Fatal("the spool was emptied by a push that stored nothing")
	}
}

func TestARejectedTokenBacksRightOffInsteadOfHammering(t *testing.T) {
	// Deleting the token in InfraNest is how the agent is turned off from the other end. Retrying that
	// once a minute writes an authentication failure into somebody's logs for as long as the machine
	// runs, carrying a reading nobody will ever want.
	sender := &fakeSender{answer: func(int) (push.Result, error) {
		return push.Result{}, push.ErrTokenRejected
	}}

	r, _, waits := harness(t, sender, 4)

	state := LoadState(r.Config.StateDir)
	if !state.TokenRejected {
		t.Fatal("expected the rejection to be recorded where status can find it")
	}
	if state.TokenRejectedAt.IsZero() {
		t.Fatal("expected the time of the rejection to be recorded")
	}
	// Straight to the ceiling, not a gentle ramp: this failure does not come good on its own, and every
	// retry is an authentication failure written into somebody's logs.
	if last := (*waits)[len(*waits)-1]; last < maxBackoff {
		t.Fatalf("expected the agent to back off to %s, but it asked to wait %s", maxBackoff, last)
	}
}

func TestAMoveIsFollowedOnlyWithinOurOwnDomain(t *testing.T) {
	sender := &fakeSender{answer: func(n int) (push.Result, error) {
		if n == 1 {
			return push.Result{Accepted: 1, IngestURL: "https://ingest-eu.infranest.app"}, nil
		}

		return push.Result{Accepted: 1}, nil
	}}

	r, _, _ := harness(t, sender, 4)

	if len(sender.urls) < 2 || !strings.Contains(sender.urls[1], "ingest-eu.infranest.app") {
		t.Fatalf("expected the second push to follow the move, got %v", sender.urls)
	}
	// Persisted, or every restart would quietly send the fleet back to whatever the installer wrote.
	if !strings.Contains(LoadState(r.Config.StateDir).URL, "ingest-eu") {
		t.Fatal("expected the new destination to survive a restart")
	}
}

func TestAHostileMoveIsIgnored(t *testing.T) {
	sender := &fakeSender{answer: func(int) (push.Result, error) {
		return push.Result{Accepted: 1, IngestURL: "https://ingest.infranest.app.evil.com"}, nil
	}}

	_, _, _ = harness(t, sender, 3)

	for _, u := range sender.urls {
		if strings.Contains(u, "evil.com") {
			t.Fatalf("the agent followed a redirect off our domain: %s", u)
		}
	}
}

func TestSkewIsRecordedSoStatusCanExplainTheSilence(t *testing.T) {
	// A drifted clock is refused by the server and looks, from every other angle, exactly like a machine
	// that has gone quiet. This is the only place the difference is visible.
	serverTime := time.Date(2026, 8, 25, 11, 55, 0, 0, time.UTC)
	sender := &fakeSender{answer: func(int) (push.Result, error) {
		return push.Result{Accepted: 1, ServerTime: serverTime}, nil
	}}

	r, _, _ := harness(t, sender, 2)

	if skew := LoadState(r.Config.StateDir).ClockSkewSeconds; skew < 60 {
		t.Fatalf("expected a recorded skew of several minutes, got %v", skew)
	}
}

func TestStatusExplainsARejectedTokenWithoutReachingAnything(t *testing.T) {
	// The question is usually asked *because* the machine cannot reach us, so an answer that has to reach
	// us is no use in the one case it exists for.
	dir := t.TempDir()
	cfg := config.Config{URL: "https://ingest.infranest.app", Interval: time.Minute, StateDir: dir}

	if err := SaveState(dir, State{TokenRejected: true, TokenRejectedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	out := &strings.Builder{}
	Status(out, cfg, time.Now())

	got := out.String()
	for _, want := range []string{"NOT SENDING", "rejected this token", "Metrics tab"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status did not mention %q:\n%s", want, got)
		}
	}
}

func TestStatusNeverPrintsTheToken(t *testing.T) {
	// It is written to be pasted into a support ticket.
	dir := t.TempDir()
	out := &strings.Builder{}
	Status(out, config.Config{Token: "sat_super_secret_value", URL: "https://ingest.infranest.app", StateDir: dir}, time.Now())

	if strings.Contains(out.String(), "sat_super_secret") {
		t.Fatal("status printed the token")
	}
}

var _ io.Writer = (*strings.Builder)(nil)
