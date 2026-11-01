package journal

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/jobs"
)

// Dirs are where journald keeps the system journal: persistent first, then the volatile copy a machine without
// /var/log/journal keeps in memory.
var Dirs = []string{"/var/log/journal", "/run/log/journal"}

// Why the journal is or is not read — `journal.reason` in the payload.
const (
	ReasonOK = "ok"
	// INFRANEST_JOURNAL=0: switched off on this machine, whatever its groups say.
	ReasonOff = "off"
	// The files are there and the agent's user may not read them: the opt-in has not been made. The one command is
	// `usermod -aG systemd-journal infranest-agent`, then a restart.
	ReasonNoPermission = "no_permission"
	// No system journal on disk: journald keeps none here (Storage=none), or something else does the logging.
	ReasonNoJournal = "no_journal"
	// A journal file this agent cannot read: a format newer than it, or a damaged header.
	ReasonUnknownFormat = "unknown_format"
)

// Bounds on one question. An hour of a very busy machine can be hundreds of thousands of entries; the agent reads at
// most this many and says the answer is partial, rather than becoming the busiest thing on the machine.
const (
	MaxScan = 200_000
	// The last lines sent for a unit.
	MaxLines = 20
	// One line, after masking.
	MaxLineBytes = 300
	// All of one unit's lines together.
	MaxSnippetBytes = 6 << 10
	// Priority 6 (info) and more urgent: debug is noise, and the most likely to carry a request's full contents.
	MaxPriority = 6
)

// Status is whether this machine's journal can be read, and why not.
type Status struct {
	Readable bool   `json:"readable"`
	Reason   string `json:"reason"`
}

// Check says whether the system journal can be read by this process. It opens the newest file and reads its header:
// group membership is what decides it, and only the attempt can say whether the operating system agrees.
func Check() Status {
	files := systemFiles()
	if len(files) == 0 {
		return Status{Reason: ReasonNoJournal}
	}
	denied, unknown := false, false
	for _, path := range files {
		fh, err := os.Open(path)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				denied = true
			}

			continue
		}
		info, err := fh.Stat()
		if err == nil {
			_, err = Open(fh, info.Size())
		}
		_ = fh.Close()
		if err == nil {
			return Status{Readable: true, Reason: ReasonOK}
		}
		unknown = true
	}

	switch {
	case denied:
		return Status{Reason: ReasonNoPermission}
	case unknown:
		return Status{Reason: ReasonUnknownFormat}
	default:
		return Status{Reason: ReasonNoJournal}
	}
}

// systemFiles are the system journal's files, newest first by name: `system.journal` (the one being written) and the
// archived `system@….journal`. A user's own journal (`user-1000.journal`) is not read — it is that person's, not the
// machine's — and a `.journal~` is a file journald itself gave up on.
func systemFiles() []string {
	var out []string
	for _, dir := range Dirs {
		matches, _ := filepath.Glob(filepath.Join(dir, "*", "system*.journal"))
		for _, m := range matches {
			base := filepath.Base(m)
			if base == "system.journal" || strings.HasPrefix(base, "system@") {
				out = append(out, m)
			}
		}
	}

	return out
}

// open opens every system journal file it can, newest entries first. The caller closes them.
func open() []*openFile {
	var out []*openFile
	for _, path := range systemFiles() {
		fh, err := os.Open(path)
		if err != nil {
			continue
		}
		info, err := fh.Stat()
		if err != nil {
			_ = fh.Close()

			continue
		}
		f, err := Open(fh, info.Size())
		if err != nil {
			_ = fh.Close()

			continue
		}
		out = append(out, &openFile{File: f, closer: fh})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Tail().After(out[j].Tail()) })

	return out
}

type openFile struct {
	*File
	closer interface{ Close() error }
}

func closeAll(files []*openFile) {
	for _, f := range files {
		_ = f.closer.Close()
	}
}

// Line is one line of a unit's log, masked.
type Line struct {
	At       time.Time `json:"at"`
	Priority int       `json:"priority"`
	Message  string    `json:"message"`
}

// Snippet is a unit's last lines before something happened to it.
type Snippet struct {
	Lines []Line
	// Entries whose message is stored compressed, which this agent does not read.
	Compressed int
	// Lines cut to MaxLineBytes, or left out to stay within MaxSnippetBytes.
	Truncated bool
}

// LastLines reads a unit's last lines written between `since` and `until`, at priority MaxPriority or more urgent,
// masked with the rules a job's output is masked with — oldest first.
func LastLines(unit string, since, until time.Time) Snippet {
	files := open()
	defer closeAll(files)

	return lastLines(files, unit, since, until)
}

func lastLines(files []*openFile, unit string, since, until time.Time) Snippet {
	type kept struct {
		at       time.Time
		priority int
		message  string
		cut      bool
	}
	var newestFirst []kept
	snippet := Snippet{}
	scanned := 0
	for _, f := range files {
		if len(newestFirst) >= MaxLines || scanned >= MaxScan {
			break
		}
		if t := f.Tail(); !t.IsZero() && t.Before(since) {
			continue
		}
		f.Backward(since, MaxScan-scanned, func(e Entry) bool {
			if e.Unit != unit || e.At.After(until) {
				return true
			}
			// No priority is a line written to stdout by something that set none: journald's default is info.
			p := e.Priority
			if p < 0 {
				p = 6
			}
			if p > MaxPriority {
				return true
			}
			if !e.HasMessage() {
				if e.Compressed {
					snippet.Compressed++
				}

				return true
			}
			msg, cut := f.Message(e)
			newestFirst = append(newestFirst, kept{at: e.At, priority: p, message: msg, cut: cut})

			return len(newestFirst) < MaxLines
		})
		scanned += f.EntriesScanned
	}

	// Oldest first, one entry's message per line — a message can hold several, and a private key spread over them is
	// masked whole only when the lines are masked together.
	var raw []string
	type origin struct {
		at       time.Time
		priority int
	}
	var from []origin
	for i := len(newestFirst) - 1; i >= 0; i-- {
		k := newestFirst[i]
		if k.cut {
			snippet.Truncated = true
		}
		for _, l := range strings.Split(strings.TrimRight(k.message, "\n"), "\n") {
			raw = append(raw, clean(l))
			from = append(from, origin{k.at, k.priority})
		}
	}
	masked := jobs.MaskLines(raw)

	total := 0
	// The newest lines are the ones that say why; when the budget runs out, the oldest go.
	start := len(masked)
	for start > 0 {
		l := clip(masked[start-1], MaxLineBytes)
		if len(l) < len(masked[start-1]) {
			snippet.Truncated = true
		}
		if total+len(l) > MaxSnippetBytes || len(masked)-start >= MaxLines*2 {
			snippet.Truncated = true

			break
		}
		masked[start-1] = l
		total += len(l)
		start--
	}
	for i := start; i < len(masked); i++ {
		snippet.Lines = append(snippet.Lines, Line{At: from[i].at, Priority: from[i].priority, Message: masked[i]})
	}

	return snippet
}

// UnitCount is how many errors and warnings one unit logged in an hour.
type UnitCount struct {
	Unit string `json:"unit"`
	// Priority 0-3: emerg, alert, crit, err.
	Errors int `json:"errors"`
	// Priority 4.
	Warnings int `json:"warnings"`
}

// Hour is one hour's counts.
type Hour struct {
	Hour  time.Time   `json:"hour"`
	Units []UnitCount `json:"units"`
	// Units left out past MaxUnits, the quietest first.
	More int `json:"more,omitempty"`
	// The hour had more entries than MaxScan, so the counts are of its newest part only.
	Partial bool `json:"partial,omitempty"`
}

// MaxUnits is how many units one hour names: the noisiest.
const MaxUnits = 20

// CountHour counts each unit's errors and warnings in the hour starting at `hour`.
func CountHour(hour time.Time) Hour {
	files := open()
	defer closeAll(files)

	return countHour(files, hour)
}

func countHour(files []*openFile, hour time.Time) Hour {
	end := hour.Add(time.Hour)
	counts := map[string]*UnitCount{}
	scanned := 0
	partial := false
	for _, f := range files {
		if scanned >= MaxScan {
			partial = true

			break
		}
		if t := f.Tail(); !t.IsZero() && t.Before(hour) {
			continue
		}
		f.Backward(hour, MaxScan-scanned, func(e Entry) bool {
			if e.Unit == "" || !e.At.Before(end) || e.Priority < 0 || e.Priority > 4 {
				return true
			}
			c := counts[e.Unit]
			if c == nil {
				c = &UnitCount{Unit: clip(clean(e.Unit), 256)}
				counts[e.Unit] = c
			}
			if e.Priority <= 3 {
				c.Errors++
			} else {
				c.Warnings++
			}

			return true
		})
		scanned += f.EntriesScanned
		if scanned >= MaxScan {
			partial = true
		}
	}

	out := Hour{Hour: hour.UTC(), Units: []UnitCount{}, Partial: partial}
	for _, c := range counts {
		out.Units = append(out.Units, *c)
	}
	sort.Slice(out.Units, func(i, j int) bool {
		a, b := out.Units[i], out.Units[j]
		if a.Errors != b.Errors {
			return a.Errors > b.Errors
		}
		if a.Warnings != b.Warnings {
			return a.Warnings > b.Warnings
		}

		return a.Unit < b.Unit
	})
	if len(out.Units) > MaxUnits {
		out.More = len(out.Units) - MaxUnits
		out.Units = out.Units[:MaxUnits]
	}

	return out
}

// clean makes a line safe to send: valid UTF-8, no control characters but tabs.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}

		return r
	}, s)
}

// clip cuts a string to n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}
