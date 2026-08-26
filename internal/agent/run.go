package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/collect"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/config"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/push"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/spool"
)

// MaxBatch is how many readings one push may carry.
//
// The server's own cap is higher. Staying under it means a spool that has built up during a long outage
// drains over several pushes instead of arriving as one request large enough to be worth rate-limiting —
// and a rejected oversized batch would be a spool that can never be delivered at all.
const MaxBatch = 60

// Backoff bounds for a send that failed. The floor is short because most failures are a moment of
// network; the ceiling is long because an agent retrying every few seconds against something that is
// down is a load generator aimed at us, multiplied by every server that has it installed.
const (
	minBackoff = 30 * time.Second
	maxBackoff = 15 * time.Minute
)

// usageInterval is how often the disk is walked to answer "what is filling this".
//
// Hourly, and only the fullest mount. The walk is seconds of work where a sample is milliseconds, so
// doing it every tick would make the agent the heaviest thing on the machine — and a directory that grew
// enough to matter in under an hour is one the threshold alert has already caught.
const usageInterval = time.Hour

// Sender is what Run pushes through. An interface so the loop can be tested without a network, which is
// the only way its retry and backoff behaviour is testable at all.
type Sender interface {
	Send(ctx context.Context, url string, samples []json.RawMessage, failed map[string]string, usage any) (push.Result, error)
}

type Runner struct {
	Config config.Config
	Sender Sender
	Spool  *spool.Spool
	Log    io.Writer

	// Collect is injected for the same reason the /proc parsers take an io.Reader rather than a path:
	// the loop's behaviour — spool, retry, back off, drain — has nothing to do with Linux, and tying its
	// tests to a machine that can actually collect would mean they only run on one of the twelve targets
	// this is built for. Left nil it is the real collector.
	Collect func(collect.Options) (collect.Sample, error)

	// ScanUsage is injected for the same reason as Collect: the *scheduling* of the walk is what this
	// package owns, and it should be testable without a filesystem to walk.
	ScanUsage func(context.Context, string) collect.UsageScan

	// Now and After are injected so the loop's timing is testable. Left nil they are the real ones.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time

	// The mounts the last reading saw, so the walk knows which one to attribute without measuring again.
	lastMounts []collect.Mount

	// A finished scan waiting for the next push. Not spooled — see sendOnce.
	pendingUsage *collect.UsageScan
}

// Run collects on a fixed cadence and delivers whatever is waiting, until the context is cancelled.
//
// Collection and delivery are deliberately not the same act. Every tick collects and spools — that part
// cannot fail in a way that loses a reading — and then a send is attempted. If the send fails, the
// readings stay spooled and the next tick tries again with everything that has accumulated. This is what
// turns a ten-minute network outage into ten readings delivered late rather than ten readings lost.
func (r *Runner) Run(ctx context.Context) error {
	r.defaults()

	state := LoadState(r.Config.StateDir)
	url := r.Config.PushURL()
	if state.URL != "" {
		// A migration survives a restart. Without this, every agent restart would quietly send the fleet
		// back to whatever host the installer happened to write months ago.
		if adopted, ok := config.Adopt(url, state.URL); ok {
			url = adopted
		}
	}

	if state.TokenRejected {
		// Refusing to start is wrong — the token may have been recreated, and a service that will not
		// come up needs somebody on the machine. Saying so, once, is right.
		r.logf("the token was rejected at %s; retrying on the usual schedule", state.TokenRejectedAt.Format(time.RFC3339))
	}

	seq := r.Now().UnixNano()
	backoff := time.Duration(0)

	for {
		seq++
		r.collectOnce(seq, &state)
		r.scanUsageIfDue(ctx, &state)

		if backoff > 0 {
			r.logf("waiting %s before the next attempt", backoff.Round(time.Second))
		}

		wait := r.Config.Interval
		if backoff > wait {
			wait = backoff
		}

		select {
		case <-ctx.Done():
			return nil
		case <-r.After(wait):
		}

		sendErr := r.sendOnce(ctx, &url, &state)

		switch {
		case sendErr == nil:
			backoff = 0
		case errors.Is(sendErr, push.ErrNotActivated):
			// Same slow retry as a rejected token, and for a stronger reason: the agent cannot be told
			// to stop, so it will keep offering readings for as long as the machine runs. Backing off
			// means InfraNest is not asked 4,320 times a day for an answer that has not changed — while
			// still resuming on its own, within the hour, when somebody switches it back on.
			backoff = maxBackoff
		case errors.Is(sendErr, push.ErrTokenRejected):
			// Not retried quickly, and not fatal. Somebody deleting the token in InfraNest is how the
			// agent is turned off from the other end; hammering an endpoint that has said no writes an
			// authentication failure into their logs once a minute for as long as the machine runs.
			backoff = maxBackoff
		default:
			backoff = nextBackoff(backoff)
		}
	}
}

// collectOnce takes one reading and spools it. Never returns an error: a collector that failed is
// recorded in the reading itself, and a spool that cannot be written is logged rather than fatal —
// stopping the agent because one write failed would turn a full disk into a monitoring outage, at
// precisely the moment the disk rule was about to fire.
func (r *Runner) collectOnce(seq int64, state *State) {
	sample, err := r.Collect(collect.Options{
		Processes:    r.Config.Processes,
		ProcessArgs:  r.Config.ProcessArgs,
		MaxProcesses: 10,
		CPUInterval:  300 * time.Millisecond,
		Services:     r.Config.Services,
	})
	if err != nil {
		r.logf("collection failed: %v", err)

		return
	}

	// Recorded even when the push cannot go out, which is the case it exists for: "memory has been
	// unreadable since Tuesday" is a different problem from "we cannot reach InfraNest", and on a machine
	// that cannot reach us this is the only place either is visible.
	state.CollectorErrors = sample.Failed
	r.lastMounts = sample.Mounts

	if err := r.Spool.Add(seq, sample); err != nil {
		r.logf("could not spool the reading: %v", err)
		// Recorded where `status` will find it, not only in the journal. A reading that cannot be written
		// is never sent, so `LastError` — which only the *send* path used to set — stayed empty, and
		// `status` reported "nothing delivered yet, give it a minute" for as long as the machine ran.
		// The command that exists to answer "why is nothing arriving" was the one place the answer was
		// missing.
		state.SpoolError = err.Error()
	} else {
		state.SpoolError = ""
	}
	r.save(*state)
}

// scanUsageIfDue walks the fullest mount, at most once an hour, and holds the answer for the next push.
//
// Only the fullest mount, deliberately. It is the one a forecast is about and the one somebody wants
// attributed — walking every mount would multiply the cost by the number of filesystems for answers
// nobody asked for. The walk is bounded inside `collect.ScanUsage`; the schedule is bounded here.
//
// Held rather than sent immediately, because the sending half already exists and has retry, backoff and a
// spool. A second delivery path for this one payload would be a second thing to get wrong.
func (r *Runner) scanUsageIfDue(ctx context.Context, state *State) {
	if !state.LastUsageScanAt.IsZero() && r.Now().Sub(state.LastUsageScanAt) < usageInterval {
		return
	}

	mount := r.fullestMount()
	if mount == "" {
		return
	}

	scan := r.ScanUsage(ctx, mount)
	r.pendingUsage = &scan
	state.LastUsageScanAt = r.Now()

	if scan.Partial {
		r.logf("disk usage scan of %s ran out of budget — reporting what it found", mount)
	}

	// Worth its own line: the two are different claims. Out of budget means there may be more of the
	// same; refused means there is definitely more, the server can say how much by subtracting what was
	// counted from what statfs reports, and this names where to look.
	if len(scan.Unreadable) > 0 {
		names := make([]string, 0, len(scan.Unreadable))
		for _, dir := range scan.Unreadable {
			names = append(names, dir.Path)
		}

		r.logf("disk usage scan of %s could not read %s — their contents are not in the breakdown", mount, strings.Join(names, ", "))
	}
}

// fullestMount is the mount the last reading said was closest to full.
//
// From the reading rather than a fresh statfs: the sample was taken moments ago and re-measuring would be
// a second answer to a question already asked, which is how two numbers on one page come to disagree.
func (r *Runner) fullestMount() string {
	var (
		point string
		worst float64
	)

	for _, m := range r.lastMounts {
		if m.TotalBytes <= 0 {
			continue
		}
		if used := float64(m.UsedBytes) / float64(m.TotalBytes); used > worst {
			worst, point = used, m.MountPoint
		}
	}

	return point
}

// sendOnce delivers what is waiting, and records what happened where `status` can find it.
func (r *Runner) sendOnce(ctx context.Context, url *string, state *State) error {
	entries, err := r.Spool.Take(MaxBatch)
	if err != nil {
		r.logf("could not read the spool: %v", err)

		return err
	}
	if len(entries) == 0 {
		return nil
	}

	samples := make([]json.RawMessage, 0, len(entries))
	for _, e := range entries {
		samples = append(samples, e.Data)
	}

	state.LastAttemptAt = r.Now()

	// Explicitly untyped when there is nothing to send. A nil `*collect.UsageScan` placed in an `any`
	// is a **non-nil interface holding a nil pointer** — so passing the field directly made every push
	// carry `"disk_usage": null`, and `omitempty` does not suppress it because the interface is not nil.
	// One of Go's oldest traps, and it survives review by looking exactly right.
	var usage any
	if r.pendingUsage != nil {
		usage = r.pendingUsage
	}

	result, err := r.Sender.Send(ctx, *url, samples, nil, usage)

	if !result.ServerTime.IsZero() {
		// Measured against our own clock at the moment we asked, which is close enough: the request
		// itself is the only thing between the two readings, and a skew that matters is minutes.
		state.ClockSkewSeconds = math.Round(r.Now().Sub(result.ServerTime).Seconds())
	}

	if err != nil {
		// A refusal that named the readings is still an answer about them. They are dropped, because
		// every reason the server gives is terminal and keeping them would wedge the spool behind a
		// batch that can never be accepted. The error is still recorded and still backs off.
		if result.Settled {
			r.Spool.Drop(entries)
			for _, s := range result.Skipped {
				r.logf("dropped the reading from %s: %s", s.CollectedAt.Format(time.RFC3339), s.Reason)
			}
		}

		state.LastError = err.Error()
		state.TokenRejected = errors.Is(err, push.ErrTokenRejected)
		if state.TokenRejected && state.TokenRejectedAt.IsZero() {
			state.TokenRejectedAt = r.Now()
		}
		state.NotActivated = errors.Is(err, push.ErrNotActivated)
		if state.NotActivated && state.NotActivatedAt.IsZero() {
			state.NotActivatedAt = r.Now()
		}
		r.logf("push failed: %v", err)
		r.save(*state)

		return err
	}

	// Delivered, so it is not offered again. Unlike a reading it is not spooled: a directory listing an
	// hour old is worth less than the one the next scan will produce, and queueing them would send a
	// backlog of stale answers after an outage.
	r.pendingUsage = nil

	// Only now. A reading is deleted when the server has said it stored it — never when the request
	// merely completed, because a 502 from something in front of us completes too.
	r.Spool.Drop(entries)

	state.LastSuccessAt = r.Now()
	state.LastError = ""
	state.TokenRejected = false
	state.TokenRejectedAt = time.Time{}
	state.NotActivated = false
	state.NotActivatedAt = time.Time{}

	for _, s := range result.Skipped {
		// A refusal is not an error, and must not be silent either: "your clock is eight minutes fast"
		// is the answer to a question that has no other answer on this machine.
		r.logf("the server did not store a reading from %s: %s", s.CollectedAt.Format(time.RFC3339), s.Reason)
	}

	if adopted, ok := config.Adopt(*url, result.IngestURL); ok {
		r.logf("InfraNest asked for readings to go to %s from now on", adopted)
		*url = adopted
		state.URL = adopted
	}

	r.save(*state)

	return nil
}

func (r *Runner) save(state State) {
	if err := SaveState(r.Config.StateDir, state); err != nil {
		r.logf("could not write the state file: %v", err)
	}
}

func (r *Runner) defaults() {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.After == nil {
		r.After = time.After
	}
	if r.Collect == nil {
		r.Collect = collect.Collect
	}
	if r.ScanUsage == nil {
		r.ScanUsage = collect.ScanUsage
	}
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log == nil {
		return
	}
	fmt.Fprintf(r.Log, "infranest-agent: "+format+"\n", args...)
}

// nextBackoff doubles, with a floor and a ceiling. No jitter is added here on purpose: agents are not
// synchronised with each other in the first place, since each one's cadence starts when it was installed.
func nextBackoff(current time.Duration) time.Duration {
	if current == 0 {
		return minBackoff
	}
	next := current * 2
	if next > maxBackoff {
		return maxBackoff
	}

	return next
}
