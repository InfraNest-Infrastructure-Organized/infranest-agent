package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// capture runs the command with args and returns what it wrote to stdout.
//
// The reader runs in its own goroutine because an os.Pipe holds only about 64 KiB: writing more than that
// with nobody draining it blocks the writer forever, so a test of `print --processes` on a busy machine
// would hang until the package timeout rather than fail with a message.
func capture(t *testing.T, args ...string) (string, error) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	read := make(chan string, 1)
	go func() {
		var sb strings.Builder
		io.Copy(&sb, r)
		read <- sb.String()
	}()

	runErr := run(args, w, os.Stderr)
	w.Close()
	out := <-read
	r.Close()

	return out, runErr
}

func TestVersionFlagReportsBuildInfo(t *testing.T) {
	out, err := capture(t, "--version")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "infranest-agent") {
		t.Errorf("version output does not name the binary: %q", out)
	}
}

func TestUsageLeadsWithWhatItWillNotDo(t *testing.T) {
	out, err := capture(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Someone running this for the first time is deciding whether to trust it. The guarantee belongs in
	// front of the command list, not behind it.
	for _, want := range []string{"only sends", "no instructions", "no ports"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage does not state %q:\n%s", want, out)
		}
	}
}

func TestAnUnknownCommandIsAnError(t *testing.T) {
	if _, err := capture(t, "definitely-not-a-command"); err == nil {
		t.Error("expected an unknown command to be an error")
	}
}

func TestFlagsWorkOnEitherSideOfTheSubcommand(t *testing.T) {
	// Go's flag package stops at the first positional, so `print --processes` left the flag false and the
	// command succeeded having silently ignored it — no processes, exit 0, no reason given.
	for _, args := range [][]string{
		{"print", "--processes"},
		{"--processes", "print"},
	} {
		cmd, rest := splitCommand(args)
		if cmd != "print" {
			t.Errorf("%v: command = %q, want print", args, cmd)
		}
		if len(rest) != 1 || rest[0] != "--processes" {
			t.Errorf("%v: rest = %v, want [--processes]", args, rest)
		}
	}
}

func TestSplitCommandTakesOnlyTheFirstPositional(t *testing.T) {
	cmd, rest := splitCommand([]string{"--processes", "print", "extra"})
	if cmd != "print" {
		t.Errorf("command = %q", cmd)
	}
	if len(rest) != 2 || rest[1] != "extra" {
		t.Errorf("rest = %v — a stray argument must survive to be rejected, not vanish", rest)
	}
}

func TestHelpIsAnAnswerNotAnError(t *testing.T) {
	// `--help` exited 1 with an empty stdout and only the auto-generated flag list on stderr — so the
	// statement the usage text leads with, the one TestUsageLeadsWithWhatItWillNotDo guards, was never
	// seen by anyone who asked for help.
	out, err := capture(t, "--help")
	if err != nil {
		t.Fatalf("--help returned an error: %v", err)
	}
	if !strings.Contains(out, "only sends") {
		t.Errorf("--help does not show the guarantee:\n%s", out)
	}
}
