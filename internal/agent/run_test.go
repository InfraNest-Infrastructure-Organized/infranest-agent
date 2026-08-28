package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
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
	batches    [][]json.RawMessage
	urls       []string
	usageSends int
	// What the runner said it collects, so a test can assert the config reaches the wire.
	collectors []push.Collectors
	answer     func(n int) (push.Result, error)
}

func (f *fakeSender) Send(_ context.Context, url string, samples []json.RawMessage, _ map[string]string, usage any, collectors push.Collectors) (push.Result, error) {
	f.batches = append(f.batches, samples)
	f.collectors = append(f.collectors, collectors)
	f.urls = append(f.urls, url)
	if usage != nil {
		f.usageSends++
	}

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

func TestStatusNamesTheCollectorsThatAreFailing(t *testing.T) {
	// "Memory has been unreadable since Tuesday" is a different problem from "we cannot reach InfraNest",
	// and on a machine that cannot reach us this is the only place either one is visible.
	dir := t.TempDir()
	_ = SaveState(dir, State{
		LastSuccessAt:   time.Now().Add(-time.Minute),
		CollectorErrors: map[string]string{"memory": "open /proc/meminfo: permission denied"},
	})

	out := &strings.Builder{}
	Status(out, config.Config{URL: "https://ingest.infranest.app", StateDir: dir}, time.Now())

	got := out.String()
	for _, want := range []string{"COLLECTORS", "memory", "permission denied", "still being collected"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status did not mention %q:\n%s", want, got)
		}
	}
}

func TestAFailedCollectorIsRecordedEvenWhenNothingCanBeSent(t *testing.T) {
	// The case it exists for. A push that cannot go out must not also cost the record of why a chart has
	// a hole in it.
	sender := &fakeSender{answer: func(int) (push.Result, error) {
		return push.Result{}, errors.New("dial tcp: network is unreachable")
	}}

	dir := t.TempDir()
	sp, err := spool.New(filepath.Join(dir, "spool"))
	if err != nil {
		t.Fatal(err)
	}

	clock := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	fired := 0
	r := &Runner{
		Config: config.Config{Token: "sat_x", URL: "https://ingest.infranest.app", Interval: time.Minute, StateDir: dir},
		Sender: sender,
		Spool:  sp,
		Log:    &strings.Builder{},
		Now:    func() time.Time { return clock },
		Collect: func(collect.Options) (collect.Sample, error) {
			s := collect.Sample{CollectedAt: clock}
			s.Failed = map[string]string{"mounts": "statfs /data: permission denied"}

			return s, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.After = func(d time.Duration) <-chan time.Time {
		fired++
		clock = clock.Add(d)
		if fired >= 2 {
			cancel()
		}
		ch := make(chan time.Time, 1)
		ch <- clock

		return ch
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := LoadState(dir).CollectorErrors["mounts"]; got == "" {
		t.Fatal("the failed collector was not recorded while the push was failing")
	}
}

func TestTheSpoolCannotWedgeOnReadingsTheServerIsDoneWith(t *testing.T) {
	// The ordinary case, not an exotic one: a push lands, its response is lost, and the agent retries a
	// batch it cannot know arrived. Every reading comes back `already_covered`. Treating that as a plain
	// failure means keeping readings the server already holds and offering them again for ever — the head
	// of the spool never clears and nothing behind it is ever sent.
	sender := &fakeSender{answer: func(n int) (push.Result, error) {
		return push.Result{
			Accepted: 0,
			Skipped:  []push.Skipped{{Reason: "already_covered"}},
			Settled:  true,
		}, push.ErrServerRefused
	}}

	r, _, _ := harness(t, sender, 6)

	// At most the one collected after the last send. The property is that it *drains* — the failure this
	// guards against is a spool that grows by one every tick for ever.
	if r.Spool.Len() > 1 {
		t.Fatalf("the spool kept %d reading(s) the server had already stored", r.Spool.Len())
	}
}

func TestARefusalOfTheRequestItselfKeepsTheReadings(t *testing.T) {
	// The other half of that rule. A 5xx, a proxy, a malformed request — anything that is not a verdict
	// on the readings — must not cost them, which is the whole point of spooling in the first place.
	sender := &fakeSender{answer: func(int) (push.Result, error) {
		return push.Result{Settled: false}, errors.New("InfraNest answered 502: (no message)")
	}}

	r, _, _ := harness(t, sender, 6)

	// The contrast with the test above, and the reason both are here: identical loop, identical failure
	// count, opposite outcome. These readings accumulate because nothing has said anything about them.
	if r.Spool.Len() < 2 {
		t.Fatalf("readings were dropped on a failure that said nothing about them (spool holds %d)", r.Spool.Len())
	}
}

func TestTheDiskIsWalkedOnceAnHourAtMost(t *testing.T) {
	// The walk is seconds of work where a sample is milliseconds. Doing it every tick would make the
	// agent the heaviest thing on the machine, which is the one failure a monitoring agent must not have.
	sender := &fakeSender{}

	dir := t.TempDir()
	sp, err := spool.New(filepath.Join(dir, "spool"))
	if err != nil {
		t.Fatal(err)
	}

	clock := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	fired, scans := 0, 0

	r := &Runner{
		Config: config.Config{Token: "sat_x", URL: "https://ingest.infranest.app", Interval: time.Minute, StateDir: dir},
		Sender: sender,
		Spool:  sp,
		Log:    &strings.Builder{},
		Now:    func() time.Time { return clock },
		Collect: func(collect.Options) (collect.Sample, error) {
			return collect.Sample{
				CollectedAt: clock,
				Mounts: []collect.Mount{
					{MountPoint: "/", UsedBytes: 30, TotalBytes: 100},
					{MountPoint: "/var", UsedBytes: 87, TotalBytes: 100},
				},
			}, nil
		},
		ScanUsage: func(_ context.Context, mount string) collect.UsageScan {
			scans++
			// The fullest mount, not the first: that is the one a forecast is about and the one somebody
			// wants attributed.
			if mount != "/var" {
				t.Errorf("walked %q, expected the fullest mount", mount)
			}

			return collect.UsageScan{MountPoint: mount, Dirs: []collect.DirUsage{{Path: "/var/log", Bytes: 99}}}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.After = func(d time.Duration) <-chan time.Time {
		fired++
		clock = clock.Add(d)
		if fired >= 10 {
			cancel()
		}
		ch := make(chan time.Time, 1)
		ch <- clock

		return ch
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// Ten minute-long ticks: one scan, not ten.
	if scans != 1 {
		t.Fatalf("walked the disk %d times in ten minutes", scans)
	}
	// And it went out exactly once — held for the next push, then cleared.
	if sender.usageSends != 1 {
		t.Fatalf("the scan was sent %d times", sender.usageSends)
	}
}

func TestAScanIsNotResentAfterItLands(t *testing.T) {
	// Unlike a reading it is not spooled: a directory listing an hour old is worth less than the one the
	// next scan will produce, and queueing them would send a backlog of stale answers after an outage.
	sender := &fakeSender{}
	dir := t.TempDir()
	sp, _ := spool.New(filepath.Join(dir, "spool"))
	clock := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	fired := 0

	r := &Runner{
		Config: config.Config{Token: "sat_x", URL: "https://ingest.infranest.app", Interval: time.Minute, StateDir: dir},
		Sender: sender,
		Spool:  sp,
		Log:    &strings.Builder{},
		Now:    func() time.Time { return clock },
		Collect: func(collect.Options) (collect.Sample, error) {
			return collect.Sample{CollectedAt: clock, Mounts: []collect.Mount{{MountPoint: "/", UsedBytes: 50, TotalBytes: 100}}}, nil
		},
		ScanUsage: func(_ context.Context, mount string) collect.UsageScan {
			return collect.UsageScan{MountPoint: mount}
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.After = func(d time.Duration) <-chan time.Time {
		fired++
		clock = clock.Add(d)
		if fired >= 5 {
			cancel()
		}
		ch := make(chan time.Time, 1)
		ch <- clock

		return ch
	}
	_ = r.Run(ctx)

	if sender.usageSends != 1 {
		t.Fatalf("the same scan rode along on %d pushes", sender.usageSends)
	}
}

func TestDeactivatedMonitoringBacksOffAndKeepsTheReadings(t *testing.T) {
	// The agent cannot be told to stop — it holds no connection and takes no instructions — so a server
	// whose monitoring is switched off keeps offering readings for as long as the machine runs. Backing
	// off is what stops that being 4,320 requests a day for an answer that has not changed.
	sender := &fakeSender{answer: func(int) (push.Result, error) {
		return push.Result{}, push.ErrNotActivated
	}}

	r, spool, waits := harness(t, sender, 4)

	state := LoadState(r.Config.StateDir)
	if !state.NotActivated {
		t.Fatal("expected the refusal to be recorded where status can find it")
	}
	if state.NotActivatedAt.IsZero() {
		t.Fatal("expected the time of the refusal to be recorded")
	}
	// Not the same flag. Told it is a token problem, an operator replaces a working agent and nothing
	// changes, because the switch is in InfraNest rather than on this machine.
	if state.TokenRejected {
		t.Fatal("a deactivated server was recorded as a rejected token")
	}
	if last := (*waits)[len(*waits)-1]; last < maxBackoff {
		t.Fatalf("expected the agent to back off to %s, but it asked to wait %s", maxBackoff, last)
	}
	// Kept, not dropped: switching monitoring back on should not have cost the minutes in between, and
	// the spool's own cap is what stops it growing without bound.
	if spool.Len() == 0 {
		t.Fatal("readings were discarded for a refusal that is reversible from the other end")
	}
}

func TestStatusTellsSomebodyNotToReinstallAWorkingAgent(t *testing.T) {
	// The one failure whose obvious response is the wrong one. "InfraNest is refusing this" reads as a
	// dead credential, and the answer to a dead credential is to install a new one — which replaces an
	// agent that is working perfectly and changes nothing.
	dir := t.TempDir()
	cfg := config.Config{URL: "https://ingest.infranest.app", Interval: time.Minute, StateDir: dir}

	if err := SaveState(dir, State{NotActivated: true, NotActivatedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	out := &strings.Builder{}
	Status(out, cfg, time.Now())

	got := out.String()
	for _, want := range []string{"NOT SENDING", "switched off for this server", "Do not reinstall"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status did not mention %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "rejected this token") {
		t.Fatalf("status blamed the token for a server nobody switched on:\n%s", got)
	}
}

// `status` is a read, and must leave nothing behind.
//
// It used `spool.New` to count queued readings, and New creates the directory. So
// `sudo infranest-agent status` made /var/lib/infranest-agent/spool owned by root, and the agent — which
// runs unprivileged, on purpose — could never write to it again. The command that exists to diagnose the
// agent permanently broke it, and then reported "nothing delivered yet, give it a minute" for ever,
// because a spool it cannot write is also a spool it cannot read.
//
// Found by an operator following our own instructions on a real server, which is the only way this was
// ever going to be found: every test until now ran as one user.
func TestStatusCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{URL: "https://ingest.infranest.app", Interval: time.Minute, StateDir: dir}

	Status(&strings.Builder{}, cfg, time.Now())

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Fatalf("status created %q in the state directory", e.Name())
	}
}

// A reading that cannot be written is never sent — so the send path has nothing to report, and `status`
// said "nothing delivered yet, give it a minute" for as long as the machine ran.
//
// That is the exact sequence an operator hit: `sudo status` created a root-owned spool directory, the
// unprivileged agent could no longer write to it, and the command that exists to answer "why is nothing
// arriving" was the one place the answer was missing.
func TestStatusNamesASpoolItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{URL: "https://ingest.infranest.app", Interval: time.Minute, StateDir: dir}

	if err := SaveState(dir, State{SpoolError: "cannot write the reading: permission denied"}); err != nil {
		t.Fatal(err)
	}

	out := &strings.Builder{}
	Status(out, cfg, time.Now())

	got := out.String()
	for _, want := range []string{"NOT COLLECTING", "permission denied", "chown"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status did not mention %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "give it a minute") {
		t.Fatalf("status still says to wait for a reading that will never be written:\n%s", got)
	}
}

func TestTheServerCanSlowTheAgentDownButNotSpeedItUp(t *testing.T) {
	// A push response saying "I store one reading every five minutes" is a reason to send less often. The
	// reverse is not true: an operator who configured two minutes asked for two minutes, and a server able
	// to *lower* the interval could turn any agent into a busier one by answering a single push — which is
	// the same shape as the redirect guard on `ingest_url`, and refused for the same reason.
	r := &Runner{Config: config.Config{Interval: 2 * time.Minute}}

	if got := r.interval(); got != 2*time.Minute {
		t.Fatalf("with nothing from the server, the configured interval should stand: %s", got)
	}

	r.serverInterval = 5 * time.Minute
	if got := r.interval(); got != 5*time.Minute {
		t.Fatalf("a slower server cadence should win: %s", got)
	}

	r.serverInterval = 30 * time.Second
	if got := r.interval(); got != 2*time.Minute {
		t.Fatalf("a faster server cadence must not win: %s", got)
	}
}
