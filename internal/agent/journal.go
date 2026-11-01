package agent

import (
	"runtime"
	"strings"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/collect"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/journal"
)

// Bounds on what the journal adds to a push (InfraNest #5848).
const (
	// Units read in one collection: a machine where everything failed at once is told about the first few, and the
	// rest on the collections after.
	maxJournalReadsPerTick = 5
	// Events waiting for a push that lands. Past this, the oldest are dropped: a backlog of last lines from a week
	// of outage is worth less than the newest.
	maxJournalEvents = 10
	// Hours of counts waiting for a push that lands.
	maxJournalHours = 6
	// A restart loop is this many restarts in a day — the receiver's own line for one.
	loopRestarts = 3
	// After a loop's lines are sent, the same unit's next ones wait this long.
	loopQuiet = 6 * time.Hour
	// How far back a unit's last lines are looked for, before the moment it failed.
	journalLookBack = time.Hour
)

// The events a unit's lines are sent for.
const (
	eventFailed      = "failed"
	eventOOMKill     = "oom_kill"
	eventRestartLoop = "restart_loop"
)

// journalEvent is a unit's last lines, sent once, when it failed, was killed for memory, or started restarting in a loop.
type journalEvent struct {
	Unit  string    `json:"unit"`
	Event string    `json:"event"`
	At    time.Time `json:"at"`
	// Oldest first, masked; `[]` when the journal held nothing from the unit in the hour before.
	Lines []journal.Line `json:"lines"`
	// Lines whose message journald stored compressed, which the agent does not read.
	Compressed int `json:"compressed,omitempty"`
	// A line was cut, or the oldest were left out to stay within the budget.
	Truncated bool `json:"truncated,omitempty"`
	// Why `lines` is empty on purpose: `login` for a login service, whose lines are never read (InfraNest #5859).
	LinesWithheld string `json:"lines_withheld,omitempty"`
}

// journalPayload is `journal` in the push: on every push from Linux, whether the journal is read; the events and the
// hourly counts only on the push that follows them.
type journalPayload struct {
	Readable bool           `json:"readable"`
	Reason   string         `json:"reason"`
	Events   []journalEvent `json:"events,omitempty"`
	Hours    []journal.Hour `json:"hours,omitempty"`
}

// journalWatch is the run loop's memory between collections: which failure was already told, and each unit's restarts.
type journalWatch struct {
	status journal.Status
	// unit → the moment of the failure already sent, so a unit that stays failed is told about once.
	failed map[string]time.Time
	// unit → its restart counter last time, and when each recent restart was seen.
	restarts     map[string]uint64
	restartTimes map[string][]time.Time
	loopSent     map[string]time.Time

	events []journalEvent
	hours  []journal.Hour
	// The last hour counted. Zero until the first count, which is then the hour before the agent started.
	counted time.Time
}

func newJournalWatch() *journalWatch {
	return &journalWatch{
		failed:       map[string]time.Time{},
		restarts:     map[string]uint64{},
		restartTimes: map[string][]time.Time{},
		loopSent:     map[string]time.Time{},
	}
}

// journalWatcher is the run loop's journal watch, made on first use: nil off Linux, where there is no journal.
func (r *Runner) journalWatcher() *journalWatch {
	if r.JournalCheck == nil && runtime.GOOS != "linux" {
		return nil
	}
	if r.journal == nil {
		r.journal = newJournalWatch()
	}

	return r.journal
}

func (r *Runner) journalStatus() journal.Status {
	if !r.Config.Journal {
		return journal.Status{Reason: journal.ReasonOff}
	}
	if r.JournalCheck != nil {
		return r.JournalCheck()
	}

	return journal.Check()
}

func (r *Runner) journalLines(unit string, since, until time.Time) journal.Snippet {
	if r.JournalLines != nil {
		return r.JournalLines(unit, since, until)
	}

	return journal.LastLines(unit, since, until)
}

func (r *Runner) journalCount(hour time.Time) journal.Hour {
	if r.JournalCount != nil {
		return r.JournalCount(hour)
	}

	return journal.CountHour(hour)
}

// observeJournal looks at a reading's units for what is worth a unit's last lines: one that failed (or was killed for
// memory), and one that restarted three times in a day. Each is told once — a unit that stays failed is not told
// again until it has recovered and failed anew — and its lines are read only when the journal can be.
func (r *Runner) observeJournal(services []collect.Service) {
	w := r.journalWatcher()
	if w == nil {
		return
	}
	w.status = r.journalStatus()
	if services == nil {
		// The services collector did not answer: nothing is known about any unit, so nothing is forgotten either.
		return
	}

	now := r.Now()
	type due struct {
		unit, event string
		at          time.Time
	}
	var found []due
	seen := map[string]bool{}
	for _, s := range services {
		if !strings.HasSuffix(s.Unit, ".service") {
			continue
		}
		seen[s.Unit] = true

		if s.ActiveState == "failed" {
			at := time.Time{}
			if s.StateChangedAt != nil {
				at = s.StateChangedAt.UTC()
			}
			if was, told := w.failed[s.Unit]; !told || !was.Equal(at) {
				w.failed[s.Unit] = at
				event := eventFailed
				if s.Result == "oom-kill" {
					event = eventOOMKill
				}
				if at.IsZero() {
					at = now
				}
				found = append(found, due{s.Unit, event, at})
			}
		} else {
			delete(w.failed, s.Unit)
		}

		if s.Restarts == nil {
			continue
		}
		n := *s.Restarts
		if was, ok := w.restarts[s.Unit]; ok && n > was {
			// The counter says how many; the collections say roughly when. Bounded: a counter that jumped by
			// thousands is one loop, not thousands of entries.
			for i := uint64(0); i < n-was && i < loopRestarts; i++ {
				w.restartTimes[s.Unit] = append(w.restartTimes[s.Unit], now)
			}
		}
		w.restarts[s.Unit] = n
		recent := w.restartTimes[s.Unit][:0]
		for _, t := range w.restartTimes[s.Unit] {
			if now.Sub(t) < 24*time.Hour {
				recent = append(recent, t)
			}
		}
		w.restartTimes[s.Unit] = recent
		if len(recent) >= loopRestarts && now.Sub(w.loopSent[s.Unit]) >= loopQuiet {
			w.loopSent[s.Unit] = now
			found = append(found, due{s.Unit, eventRestartLoop, now})
		}
	}
	// A unit that left the watch set is forgotten, so the maps never outgrow the machine's units.
	for unit := range w.restarts {
		if !seen[unit] {
			delete(w.restarts, unit)
			delete(w.restartTimes, unit)
			delete(w.loopSent, unit)
		}
	}
	for unit := range w.failed {
		if !seen[unit] {
			delete(w.failed, unit)
		}
	}

	if !w.status.Readable {
		return
	}
	for i, d := range found {
		if i >= maxJournalReadsPerTick {
			break
		}
		// A login service's lines are records of who logged in: its event is sent, its lines are not even read.
		var s journal.Snippet
		if journal.LoginUnit(d.unit) {
			s = journal.Snippet{Withheld: journal.WithheldLogin}
		} else {
			s = r.journalLines(d.unit, d.at.Add(-journalLookBack), d.at.Add(time.Minute))
		}
		lines := s.Lines
		if lines == nil {
			lines = []journal.Line{}
		}
		w.events = append(w.events, journalEvent{Unit: journal.ReportedUnit(d.unit), Event: d.event, At: d.at, Lines: lines, Compressed: s.Compressed, Truncated: s.Truncated, LinesWithheld: s.Withheld})
	}
	if len(w.events) > maxJournalEvents {
		w.events = w.events[len(w.events)-maxJournalEvents:]
	}
}

// journalForPush counts the hour that just ended, once, and builds `journal` for the push. Untyped nil off Linux, for
// the same reason as the usage scan: a typed nil in an `any` is not omitted.
func (r *Runner) journalForPush() any {
	w := r.journalWatcher()
	if w == nil {
		return nil
	}
	if w.status.Reason == "" {
		w.status = r.journalStatus()
	}
	if w.status.Readable {
		hour := r.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
		if w.counted.Before(hour) {
			w.hours = append(w.hours, r.journalCount(hour))
			w.counted = hour
			if len(w.hours) > maxJournalHours {
				w.hours = w.hours[len(w.hours)-maxJournalHours:]
			}
		}
	}

	return journalPayload{Readable: w.status.Readable, Reason: w.status.Reason, Events: w.events, Hours: w.hours}
}

// journalDelivered forgets what a landed push carried.
func (r *Runner) journalDelivered() {
	if r.journal != nil {
		r.journal.events = nil
		r.journal.hours = nil
	}
}
