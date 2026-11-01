//go:build !windows

package jobrun

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/jobs"
)

func run(t *testing.T, args ...string) (jobs.Report, int, string, string) {
	t.Helper()
	opts, err := Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	r, code := Run(opts, strings.NewReader(""), &out, &errOut, time.Now)

	return r, code, out.String(), errOut.String()
}

func TestTheExitCodeIsTheCommandsOwn(t *testing.T) {
	r, code, out, _ := run(t, "--job", "ok", "--", "/bin/sh", "-c", "echo hello")
	if code != 0 || r.ExitCode != 0 || out != "hello\n" || len(r.Output) != 0 {
		t.Fatalf("success: code %d, report %+v, out %q", code, r, out)
	}
	r, code, _, _ = run(t, "--job", "fails", "--", "/bin/sh", "-c", "exit 3")
	if code != 3 || r.ExitCode != 3 {
		t.Fatalf("failure: code %d, report %+v", code, r)
	}
	r, code, _, errOut := run(t, "--job", "missing", "--", "/no/such/program")
	if code != 127 || r.ExitCode != 127 || r.Error == "" || errOut == "" {
		t.Fatalf("not found: code %d, report %+v", code, r)
	}
	r, code, _, _ = run(t, "--job", "killed", "--", "/bin/sh", "-c", "kill -9 $$")
	if code != 128+9 || r.Signal == "" {
		t.Fatalf("killed: code %d, report %+v", code, r)
	}
}

func TestAFailedRunSendsItsLastLinesMasked(t *testing.T) {
	script := `i=0; while [ $i -lt 30 ]; do echo "line $i"; i=$((i+1)); done; echo "connecting with password=hunter2" >&2; exit 1`
	r, code, out, errOut := run(t, "--job", "backup", "--", "/bin/sh", "-c", script)
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(out, "line 0\n") || !strings.Contains(errOut, "hunter2") {
		t.Fatal("the output must pass through unchanged")
	}
	if len(r.Output) != jobs.MaxLines || r.Output[len(r.Output)-1] != "connecting with password=[masked]" {
		t.Fatalf("last lines: %q", r.Output)
	}
	if strings.Contains(strings.Join(r.Output, "\n"), "hunter2") {
		t.Fatal("a password left the machine")
	}

	r, _, _, _ = run(t, "--job", "backup", "--no-output", "--", "/bin/sh", "-c", script)
	if len(r.Output) != 0 || !r.OutputWithheld {
		t.Fatalf("--no-output: %+v", r)
	}
}

func TestTheReportShape(t *testing.T) {
	r, _, _, _ := run(t, "--job=Nightly backup", "--", "/bin/sh", "-c", "sleep 0.05; exit 2")
	if r.Job != "Nightly backup" || len(r.ID) != 32 || r.StartedAt.IsZero() || !r.EndedAt.After(r.StartedAt) || r.DurationMs < 40 || r.User != "" {
		t.Fatalf("report: %+v", r)
	}
}

func TestMainReportsIntoTheFolderAndExitsWithTheCode(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o1733); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Main([]string{"--job", "x", "--", "/bin/sh", "-c", "exit 4"}, strings.NewReader(""), &out, &errOut, dir)
	if code != 4 {
		t.Fatalf("code %d (%s)", code, errOut.String())
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("want one report, got %v", files)
	}

	// No folder: the job still runs and still exits with its code; the reason is said on stderr.
	errOut.Reset()
	code = Main([]string{"--job", "x", "--", "/bin/sh", "-c", "exit 5"}, strings.NewReader(""), &out, &errOut, filepath.Join(dir, "missing"))
	if code != 5 || !strings.Contains(errOut.String(), "could not report") {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
}

func TestParse(t *testing.T) {
	for _, bad := range [][]string{
		{"--", "/bin/true"},
		{"--job", "x"},
		{"--job", "x", "/bin/true"},
		{"--job", strings.Repeat("x", 200), "--", "/bin/true"},
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
	o, err := Parse([]string{"--no-output", "--job", "x", "--", "/bin/echo", "--job", "--"})
	if err != nil || !o.NoOutput || o.Job != "x" || strings.Join(o.Command, " ") != "/bin/echo --job --" {
		t.Fatalf("everything after -- is the command: %+v %v", o, err)
	}
	if Wants(nil) || !Wants([]string{"--job", "x"}) || !Wants([]string{"--"}) {
		t.Fatal("Wants")
	}
	var w bytes.Buffer
	if code := Main([]string{"--job"}, strings.NewReader(""), &w, &w, ""); code != 2 {
		t.Fatalf("a usage error exits 2, got %d", code)
	}
}
