// Package jobrun is `infranest-agent run --job NAME -- CMD …` (InfraNest #5801): a wrapper somebody puts in front of
// a cron job, so InfraNest hears when the job ran, how long it took and how it ended.
//
// **This is the only package in the agent that starts a process, and the running agent never reaches it.** The
// service — `infranest-agent run` with no `--job`, as its unit starts it — collects and sends and executes nothing;
// CI checks that none of its packages can reach `os/exec`. This package is reached only when a person writes it into
// their own crontab, and then it runs exactly the command they wrote after `--`, as the user cron already runs that
// line as: no shell unless the command is one, no new rights, nothing it was not given.
//
// It never changes how the job behaves. The command's output passes through unchanged, its input is its own, and the
// wrapper exits with the command's exit code — a failure to report is said on stderr and changes nothing else.
package jobrun

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/jobs"
)

// Usage is the subcommand's own help.
const Usage = `Usage:
  infranest-agent run --job NAME [--no-output] -- COMMAND [ARGUMENTS…]

Runs COMMAND exactly as it would run without this, as the same user, and tells
InfraNest — through the agent on this machine — when it started, when it ended
and its exit code. When it fails, its last 20 lines of output go too, with
passwords, tokens and keys masked; --no-output sends the exit code alone.

Exits with COMMAND's own exit code. Put it in front of a cron line:

  15 3 * * * root infranest-agent run --job "Nightly backup" -- /opt/acme/backup.sh
`

// Wants says whether the arguments after `run` are the wrapper's rather than the service's: a `--job` or a `--`.
// The service is started as a bare `run`, so nothing it is given ever matches.
func Wants(args []string) bool {
	for _, a := range args {
		if a == "--job" || strings.HasPrefix(a, "--job=") || a == "--" || a == "-job" || strings.HasPrefix(a, "-job=") {
			return true
		}
	}

	return false
}

// Options is the wrapper's command line, parsed.
type Options struct {
	Job      string
	NoOutput bool
	Command  []string
}

// Parse reads `--job NAME [--no-output] -- COMMAND …`. Everything after `--` is the command, untouched.
func Parse(args []string) (Options, error) {
	var o Options
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			o.Command = args[i+1:]
			i = len(args)
		case a == "--job" || a == "-job":
			if i+1 >= len(args) {
				return o, errors.New("--job needs a name")
			}
			o.Job = args[i+1]
			i++
		case strings.HasPrefix(a, "--job="), strings.HasPrefix(a, "-job="):
			o.Job = a[strings.Index(a, "=")+1:]
		case a == "--no-output" || a == "-no-output":
			o.NoOutput = true
		default:
			return o, fmt.Errorf("unexpected %q before -- (the command goes after --)", a)
		}
	}
	o.Job = strings.TrimSpace(o.Job)
	switch {
	case o.Job == "":
		return o, errors.New("--job needs a name: the job's name in InfraNest")
	case len(o.Job) > jobs.MaxJobBytes:
		return o, fmt.Errorf("--job is longer than %d bytes", jobs.MaxJobBytes)
	case len(o.Command) == 0:
		return o, errors.New("nothing to run: put the command after --")
	}

	return o, nil
}

// Main runs the wrapper and returns the exit code to leave with. dir is the reports folder (jobs.Dir).
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer, dir string) int {
	opts, err := Parse(args)
	if err != nil {
		fmt.Fprintf(stderr, "infranest-agent run: %v\n\n%s", err, Usage)

		return 2
	}

	// Never with borrowed rights: should the binary ever be made set-user-ID or set-group-ID, it would run
	// somebody else's command as the binary's owner. It runs only as whoever started it (InfraNest #5801).
	if os.Geteuid() != os.Getuid() || os.Getegid() != os.Getgid() {
		fmt.Fprintln(stderr, "infranest-agent run: refusing to run with set-user-ID or set-group-ID rights")

		return 126
	}

	report, code := Run(opts, stdin, stdout, stderr, time.Now)
	if err := jobs.Write(dir, report); err != nil {
		// Said, and nothing else: the job's own result is what cron and anyone reading its mail act on.
		fmt.Fprintf(stderr, "infranest-agent run: could not report this run to the agent: %v\n", err)
	}

	return code
}

// Run starts the command, passes its input and output through, and returns what happened and the code to exit with.
func Run(opts Options, stdin io.Reader, stdout, stderr io.Writer, now func() time.Time) (jobs.Report, int) {
	tail := &tailBuffer{max: jobs.MaxLines}
	cmd := exec.Command(opts.Command[0], opts.Command[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = io.MultiWriter(stdout, tail)
	cmd.Stderr = io.MultiWriter(stderr, tail)

	started := now()
	report := jobs.Report{ID: jobs.NewID(), Job: opts.Job, StartedAt: started.UTC()}

	// A stop asked of the wrapper is asked of the command: cron, a shutdown or somebody's Ctrl-C meant the job.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, forwarded...)
	defer signal.Stop(signals)

	err := cmd.Start()
	if err == nil {
		done := make(chan struct{})
		go func() {
			for {
				select {
				case s := <-signals:
					if cmd.Process != nil {
						_ = cmd.Process.Signal(s)
					}
				case <-done:
					return
				}
			}
		}()
		err = cmd.Wait()
		close(done)
	}
	ended := now()
	report.EndedAt = ended.UTC()
	report.DurationMs = ended.Sub(started).Milliseconds()

	code := 0
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
		if sig, ok := signalOf(exit); ok {
			code = 128 + int(sig)
			report.Signal = sig.String()
		}
	default:
		// It never started: the shell's own codes, so a crontab reads the same with or without the wrapper.
		code = 126
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			code = 127
		}
		report.Error = err.Error()
		fmt.Fprintf(stderr, "infranest-agent run: %v\n", err)
	}
	report.ExitCode = code

	if code != 0 {
		if opts.NoOutput {
			report.OutputWithheld = true
		} else {
			report.Output = jobs.MaskLines(tail.lines())
		}
	}

	return report.Clipped(), code
}

var forwarded = []os.Signal{os.Interrupt, syscall.SIGTERM}

// tailBuffer keeps the last lines written to it, from both of the command's outputs in the order they arrived.
type tailBuffer struct {
	mu      sync.Mutex
	max     int
	partial bytes.Buffer
	kept    []string
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			t.push(t.partial.String())
			t.partial.Reset()

			continue
		}
		// A line longer than a report keeps is cut, not grown without end.
		if t.partial.Len() < jobs.MaxLineBytes*2 {
			t.partial.WriteByte(b)
		}
	}

	return len(p), nil
}

func (t *tailBuffer) push(line string) {
	line = strings.TrimRight(line, "\r")
	t.kept = append(t.kept, line)
	if len(t.kept) > t.max {
		t.kept = t.kept[len(t.kept)-t.max:]
	}
}

func (t *tailBuffer) lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := append([]string(nil), t.kept...)
	if t.partial.Len() > 0 {
		out = append(out, strings.TrimRight(t.partial.String(), "\r"))
	}
	if len(out) > t.max {
		out = out[len(out)-t.max:]
	}

	return out
}
