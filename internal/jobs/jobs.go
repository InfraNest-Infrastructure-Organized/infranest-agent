// Package jobs is the drop folder between `infranest-agent run --job` and the running agent (InfraNest #5801).
//
// The wrapper runs inside somebody's cron job, as that job's user, and the agent runs as its own unprivileged user;
// neither can talk to the other and neither should. So the wrapper leaves a small report in a folder anyone may
// write to and nobody but the agent may read — mode 1733, owned by the agent's user — and the agent sends what it
// finds there on its next push and deletes it. Nothing is executed on either side by this package, and the agent
// opens no socket for it.
package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounds. A report is small by construction; anything bigger was not written by the wrapper and is dropped unread.
const (
	MaxLines       = 20
	MaxLineBytes   = 512
	MaxJobBytes    = 128
	MaxErrorBytes  = 255
	MaxReportBytes = 32 << 10
	// MaxPerPush is how many reports one push carries; the rest wait for the next.
	MaxPerPush = 50
	// MaxAge is how long an unsent report is kept: a week of an agent that cannot reach InfraNest.
	MaxAge = 7 * 24 * time.Hour
	// MaxWaiting bounds the folder: past this many reports, the oldest are dropped first.
	MaxWaiting = 500
)

// Report is one run of a job, as the wrapper saw it.
type Report struct {
	// Random, and the file's name: what InfraNest deduplicates a report sent twice by.
	ID        string    `json:"id"`
	Job       string    `json:"job"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	// Milliseconds, measured on the monotonic clock: a clock step during a run does not change it.
	DurationMs int64 `json:"duration_ms"`
	// The command's exit code. A command killed by a signal is 128 + the signal's number, as a shell reports it, and
	// Signal names it; one that could not be started is 126 or 127, and Error says why.
	ExitCode int    `json:"exit_code"`
	Signal   string `json:"signal,omitempty"`
	Error    string `json:"error,omitempty"`
	// The last lines the command printed, masked — only when it failed, and never with --no-output.
	Output []string `json:"output,omitempty"`
	// It failed, and --no-output kept its output on the machine.
	OutputWithheld bool `json:"output_withheld,omitempty"`
	// Who ran it — set by the agent from the report file's owner, never read from the file: the folder is writable
	// by anyone, so whatever a report says about itself is only a claim.
	User string `json:"user,omitempty"`
}

// Dir is the drop folder: the configured one, else the platform's. Empty where there is none (Windows, for now).
func Dir(configured string) string {
	if configured != "" {
		return configured
	}
	if runtime.GOOS == "windows" {
		return ""
	}

	return "/var/spool/infranest-agent"
}

// ErrNoFolder means the folder is not there: the installer that creates it has not run since the upgrade.
var ErrNoFolder = errors.New("the job reports folder does not exist — re-run the installer to create it")

var idShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// NewID is a random report ID: 128 bits, so nobody else writing to the folder can guess a report's name.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])

	return hex.EncodeToString(b[:])
}

// Write leaves a report in the folder: written to a hidden file first and renamed into place, so the agent never
// reads half of one. Readable by the agent only through the folder's own permissions — the file is 0644 because the
// writer cannot give it to the agent's user, and the folder lets nobody else list it.
func Write(dir string, r Report) error {
	if dir == "" {
		return errors.New("job reports are not collected on this system yet")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ErrNoFolder
	}
	r = r.Clipped()
	if r.ID == "" || !idShape.MatchString(r.ID) {
		r.ID = NewID()
	}
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+r.ID+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", dir, err)
	}
	// Explicitly, past the job's umask: a cron job run with umask 077 would otherwise leave a report only its own
	// user can read, and the agent would never see it.
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		_ = os.Remove(tmp)

		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		_ = os.Remove(tmp)

		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)

		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, r.ID+".json")); err != nil {
		_ = os.Remove(tmp)

		return err
	}

	return nil
}

// Clipped is the report within the contract's bounds — what InfraNest refuses a whole push over.
func (r Report) Clipped() Report {
	r.Job = clip(strings.TrimSpace(r.Job), MaxJobBytes)
	r.Error = clip(r.Error, MaxErrorBytes)
	r.Signal = clip(r.Signal, 16)
	r.User = clip(r.User, 64)
	if len(r.Output) > MaxLines {
		r.Output = r.Output[len(r.Output)-MaxLines:]
	}
	for i, l := range r.Output {
		r.Output[i] = clip(strings.ToValidUTF8(l, "?"), MaxLineBytes)
	}
	if r.DurationMs < 0 {
		r.DurationMs = 0
	}

	return r
}

// Waiting is a report found in the folder, with the file it came from.
type Waiting struct {
	Report Report
	Path   string
}

// Take reads up to max reports from the folder, oldest first, and deletes what can never be sent: a file that is
// not a report, one too large to be one, one older than MaxAge, and the oldest beyond MaxWaiting. What it returns is
// deleted by Remove once InfraNest has taken it — a push that fails leaves the files for the next one.
func Take(dir string, max int, now time.Time) ([]Waiting, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoFolder
		}

		return nil, err
	}

	type candidate struct {
		path string
		mod  time.Time
	}
	var found []candidate
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		// A half-written report the wrapper never renamed: kept for an hour in case it is still being written.
		if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp") {
			if now.Sub(info.ModTime()) > time.Hour {
				_ = os.Remove(path)
			}

			continue
		}
		stem := strings.TrimSuffix(name, ".json")
		// Anything that is not a plain report file — a link, a folder, a name the wrapper does not write — is not
		// read, and is removed: the folder is writable by anyone, and the agent follows nothing it did not put there.
		if !strings.HasSuffix(name, ".json") || !idShape.MatchString(stem) || !info.Mode().IsRegular() || info.Size() > MaxReportBytes || now.Sub(info.ModTime()) > MaxAge {
			_ = os.Remove(path)

			continue
		}
		found = append(found, candidate{path: path, mod: info.ModTime()})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].mod.Before(found[j].mod) })
	if len(found) > MaxWaiting {
		for _, c := range found[:len(found)-MaxWaiting] {
			_ = os.Remove(c.path)
		}
		found = found[len(found)-MaxWaiting:]
	}

	var out []Waiting
	for _, c := range found {
		if len(out) >= max {
			break
		}
		r, owner, err := readReport(c.path)
		if err != nil {
			_ = os.Remove(c.path)

			continue
		}
		r.ID = strings.TrimSuffix(filepath.Base(c.path), ".json")
		r.User = owner
		if r.Job == "" || r.StartedAt.IsZero() || r.EndedAt.IsZero() {
			_ = os.Remove(c.path)

			continue
		}
		out = append(out, Waiting{Report: r.Clipped(), Path: c.path})
	}

	return out, nil
}

// Remove deletes reports InfraNest has taken.
func Remove(waiting []Waiting) {
	for _, w := range waiting {
		_ = os.Remove(w.Path)
	}
}

// Reports is the reports alone, for the push.
func Reports(waiting []Waiting, withOutput bool) []Report {
	out := make([]Report, 0, len(waiting))
	for _, w := range waiting {
		r := w.Report
		// The server's own setting wins over the cron line (InfraNest #5801): with INFRANEST_JOB_OUTPUT=0 no
		// job's output leaves the machine, whatever the wrapper was told.
		if !withOutput && len(r.Output) > 0 {
			r.Output = nil
			r.OutputWithheld = true
		}
		out = append(out, r)
	}

	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}
