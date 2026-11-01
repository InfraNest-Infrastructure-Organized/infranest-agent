package agent

import (
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/collect"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/journal"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/push"
)

// journalHarness runs the loop with a journal under the test's control: `services` answers each collection, and every
// read of a unit's lines is recorded.
type journalHarness struct {
	readable bool
	reason   string
	reads    []string
	counted  []time.Time
}

func (h *journalHarness) install(r *Runner, services func(n int) []collect.Service) {
	r.Config.Journal = true
	r.Config.Services = true
	r.JournalCheck = func() journal.Status { return journal.Status{Readable: h.readable, Reason: h.reason} }
	r.JournalLines = func(unit string, since, until time.Time) journal.Snippet {
		h.reads = append(h.reads, unit)

		return journal.Snippet{Lines: []journal.Line{{At: until, Priority: 3, Message: "bind() failed"}}, Compressed: 1}
	}
	r.JournalCount = func(hour time.Time) journal.Hour {
		h.counted = append(h.counted, hour)

		return journal.Hour{Hour: hour, Units: []journal.UnitCount{{Unit: "app.service", Errors: 2}}}
	}
	n := 0
	collectBefore := r.Collect
	r.Collect = func(o collect.Options) (collect.Sample, error) {
		s, err := collectBefore(o)
		n++
		s.Services = services(n)

		return s, err
	}
}

func payloads(t *testing.T, sender *fakeSender) []journalPayload {
	t.Helper()
	var out []journalPayload
	for _, j := range sender.journals {
		p, ok := j.(journalPayload)
		if !ok {
			t.Fatalf("journal is %T, want journalPayload", j)
		}
		out = append(out, p)
	}

	return out
}

func failedUnit(unit string, at time.Time, result string) collect.Service {
	return collect.Service{Unit: unit, ActiveState: "failed", StateChangedAt: &at, Result: result}
}

// A unit that failed is told about once, with its last lines — and again only when it has recovered and failed anew.
func TestAFailedUnitsLastLinesAreSentOnce(t *testing.T) {
	failedAt := time.Date(2026, 8, 25, 11, 58, 0, 0, time.UTC)
	again := time.Date(2026, 8, 25, 12, 3, 0, 0, time.UTC)
	h := &journalHarness{readable: true, reason: journal.ReasonOK}
	sender := &fakeSender{}
	harnessWith(t, sender, 6, func(r *Runner) {
		h.install(r, func(n int) []collect.Service {
			switch {
			case n <= 2:
				return []collect.Service{failedUnit("nginx.service", failedAt, "exit-code"), {Unit: "ssh.service", ActiveState: "active"}}
			case n == 3:
				return []collect.Service{{Unit: "nginx.service", ActiveState: "active"}}
			default:
				return []collect.Service{failedUnit("nginx.service", again, "oom-kill")}
			}
		})
	})

	got := payloads(t, sender)
	var events []journalEvent
	for _, p := range got {
		if !p.Readable || p.Reason != journal.ReasonOK {
			t.Fatalf("every push says the journal is read: %+v", p)
		}
		events = append(events, p.Events...)
	}
	if len(events) != 2 || len(h.reads) != 2 {
		t.Fatalf("want the failure and the second failure, told once each; got %+v (reads %v)", events, h.reads)
	}
	if events[0].Event != eventFailed || !events[0].At.Equal(failedAt) || events[0].Compressed != 1 || len(events[0].Lines) != 1 {
		t.Errorf("first: %+v", events[0])
	}
	// Killed for memory is its own event, read from systemd's result.
	if events[1].Event != eventOOMKill || !events[1].At.Equal(again) {
		t.Errorf("second: %+v", events[1])
	}
}

// Three restarts in a day is a loop: its lines are sent once, and the unit's next loop waits.
func TestARestartLoopsLinesAreSentOnce(t *testing.T) {
	h := &journalHarness{readable: true, reason: journal.ReasonOK}
	sender := &fakeSender{}
	harnessWith(t, sender, 8, func(r *Runner) {
		h.install(r, func(n int) []collect.Service {
			restarts := uint64(n)

			return []collect.Service{{Unit: "worker.service", ActiveState: "active", Restarts: &restarts}}
		})
	})
	var events []journalEvent
	for _, p := range payloads(t, sender) {
		events = append(events, p.Events...)
	}
	if len(events) != 1 || events[0].Event != eventRestartLoop || events[0].Unit != "worker.service" {
		t.Fatalf("want one restart loop, got %+v", events)
	}
}

// Without the group, nothing is read, and every push says why — that is what the page's Off is drawn from.
func TestAJournalTheAgentMayNotReadIsNotReadAndSaysWhy(t *testing.T) {
	h := &journalHarness{readable: false, reason: journal.ReasonNoPermission}
	sender := &fakeSender{}
	harnessWith(t, sender, 3, func(r *Runner) {
		h.install(r, func(int) []collect.Service {
			return []collect.Service{failedUnit("nginx.service", time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC), "")}
		})
	})
	for _, p := range payloads(t, sender) {
		if p.Readable || p.Reason != journal.ReasonNoPermission || p.Events != nil || p.Hours != nil {
			t.Fatalf("%+v", p)
		}
	}
	if len(h.reads) != 0 || len(h.counted) != 0 {
		t.Fatalf("read a journal it may not: %v %v", h.reads, h.counted)
	}
}

// INFRANEST_JOURNAL=0 is the machine's own word: off, even with the group.
func TestTheSwitchKeepsTheJournalOffEvenWhenReadable(t *testing.T) {
	h := &journalHarness{readable: true, reason: journal.ReasonOK}
	sender := &fakeSender{}
	harnessWith(t, sender, 3, func(r *Runner) {
		h.install(r, func(int) []collect.Service {
			return []collect.Service{failedUnit("nginx.service", time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC), "")}
		})
		r.Config.Journal = false
	})
	for _, p := range payloads(t, sender) {
		if p.Readable || p.Reason != journal.ReasonOff {
			t.Fatalf("%+v", p)
		}
	}
	if len(h.reads) != 0 || len(h.counted) != 0 {
		t.Fatal("read the journal with INFRANEST_JOURNAL=0")
	}
}

// The hour that ended is counted once, and what a failed push carried is offered again on the next.
func TestTheHourIsCountedOnceAndKeptUntilAPushLands(t *testing.T) {
	h := &journalHarness{readable: true, reason: journal.ReasonOK}
	sender := &fakeSender{answer: func(n int) (push.Result, error) {
		if n == 1 {
			return push.Result{}, errors.New("cannot reach InfraNest")
		}

		return push.Result{Accepted: 1}, nil
	}}
	harnessWith(t, sender, 4, func(r *Runner) {
		h.install(r, func(int) []collect.Service { return []collect.Service{} })
	})
	got := payloads(t, sender)
	if len(h.counted) != 1 || !h.counted[0].Equal(time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("counted %v, want 11:00 once", h.counted)
	}
	if len(got) < 3 || len(got[0].Hours) != 1 || len(got[1].Hours) != 1 || got[2].Hours != nil {
		t.Fatalf("the hour should ride the failed push and the next, then stop: %+v", got)
	}
}

// Off Linux there is no journal: `journal` is absent, never `{readable: false}` with a reason nobody can act on.
func TestThereIsNoJournalOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("reads the real journal on Linux")
	}
	sender := &fakeSender{}
	harness(t, sender, 2)
	for _, j := range sender.journals {
		if j != nil {
			t.Fatalf("journal on %s: %#v", runtime.GOOS, j)
		}
	}
}

// A login service that fails is told about — that it failed, and how — with no lines, and its lines are never read:
// they are records of who logged in, from where (InfraNest #5859).
func TestALoginServicesEventCarriesNoLinesAndSaysWhy(t *testing.T) {
	failedAt := time.Date(2026, 8, 25, 11, 58, 0, 0, time.UTC)
	h := &journalHarness{readable: true, reason: journal.ReasonOK}
	sender := &fakeSender{}
	harnessWith(t, sender, 3, func(r *Runner) {
		h.install(r, func(int) []collect.Service {
			return []collect.Service{
				failedUnit("ssh.service", failedAt, "exit-code"),
				failedUnit("sshd@3-10.0.0.1:22-203.0.113.7:51234.service", failedAt, "exit-code"),
				failedUnit("nginx.service", failedAt, "exit-code"),
			}
		})
	})
	var events []journalEvent
	for _, p := range payloads(t, sender) {
		events = append(events, p.Events...)
	}
	if len(events) != 3 {
		t.Fatalf("want three events, got %+v", events)
	}
	if len(h.reads) != 1 || h.reads[0] != "nginx.service" {
		t.Fatalf("only nginx's lines may be read, read %v", h.reads)
	}
	for _, e := range events {
		login := e.Unit != "nginx.service"
		if login && (e.LinesWithheld != journal.WithheldLogin || len(e.Lines) != 0 || e.Lines == nil || e.Event != eventFailed) {
			t.Errorf("a login service: %+v", e)
		}
		if !login && (e.LinesWithheld != "" || len(e.Lines) != 1) {
			t.Errorf("an ordinary service: %+v", e)
		}
		b, _ := json.Marshal(e)
		if login != strings.Contains(string(b), `"lines_withheld":"login"`) || login != strings.Contains(string(b), `"lines":[]`) {
			t.Errorf("on the wire: %s", b)
		}
		// A per-connection instance is named after its peer; it is sent as its template.
		if strings.Contains(string(b), "203.0.113.7") {
			t.Errorf("the peer's address left in the unit's name: %s", b)
		}
	}
	if events[1].Unit != "sshd@.service" {
		t.Errorf("the instance is sent as %q, want sshd@.service", events[1].Unit)
	}
}
