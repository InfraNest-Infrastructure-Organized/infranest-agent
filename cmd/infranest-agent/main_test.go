package main

import (
	"os"
	"strings"
	"testing"
)

// capture runs the command with args and returns what it wrote to stdout.
func capture(t *testing.T, args ...string) (string, error) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	runErr := run(args, w, os.Stderr)
	w.Close()

	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, readErr := r.Read(buf)
		sb.Write(buf[:n])
		if readErr != nil {
			break
		}
	}

	return sb.String(), runErr
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
