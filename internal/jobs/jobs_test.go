package jobs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMaskMirrorsInfraNestsRules(t *testing.T) {
	for in, want := range map[string]string{
		`FATAL: password authentication failed for user "backup"`: `FATAL: password authentication failed for user "backup"`,
		"uploading with password=hunter2 now":                     "uploading with password=[masked] now",
		"export API_KEY: abc123":                                  "export API_KEY: [masked]",
		"token = ghp_abcdefghijklmnopqrstuvwxyz0123":              "token = [masked]",
		"aws AKIAABCDEFGHIJKLMNOP done":                           "aws [masked] done",
		"psql postgres://backup:s3cret@db.internal/shop":          "psql postgres://backup:[masked]@db.internal/shop",
		"Authorization: Bearer abc.def.ghi":                       "Authorization: Bearer [masked]",
		"sum 0123456789abcdef0123456789abcdef ok":                 "sum [masked] ok",
		"stripe sk_live_0123456789abcdefABCD":                     "stripe [masked]",
		"/var/backups/shop.sql.gz written":                        "/var/backups/shop.sql.gz written",
	} {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
	key := MaskLines([]string{"before", "-----BEGIN OPENSSH PRIVATE KEY-----", "b3BlbnNzaC1rZXk", "-----END OPENSSH PRIVATE KEY-----", "after"})
	if key[0] != "before" || key[4] != "after" || strings.Contains(strings.Join(key, ""), "b3Blbn") {
		t.Errorf("a private key block was not masked: %q", key)
	}
}

func TestWrittenReportsAreTakenWithTheirOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix only")
	}
	dir := t.TempDir()
	// A cron job with a strict umask still leaves a report the agent can read.
	old := syscallUmask(0o077)
	defer syscallUmask(old)
	start := time.Date(2026, 11, 2, 3, 15, 0, 0, time.UTC)
	if err := Write(dir, Report{Job: "Nightly backup", StartedAt: start, EndedAt: start.Add(48 * time.Second), DurationMs: 48000, ExitCode: 1, Output: []string{"pg_dump: error"}, User: "root-claimed"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("want one report file, got %v", entries)
	}
	info, _ := os.Stat(filepath.Join(dir, entries[0].Name()))
	if info.Mode().Perm() != 0o644 {
		t.Errorf("a report is 0644, the folder keeps it private: %v", info.Mode())
	}

	got, err := Take(dir, MaxPerPush, time.Now())
	if err != nil || len(got) != 1 {
		t.Fatalf("take: %v %v", got, err)
	}
	r := got[0].Report
	if r.Job != "Nightly backup" || r.ExitCode != 1 || r.DurationMs != 48000 || len(r.Output) != 1 || r.ID != strings.TrimSuffix(entries[0].Name(), ".json") {
		t.Errorf("report: %+v", r)
	}
	// Who ran it is the file's owner, never what the file says.
	if r.User == "root-claimed" || r.User == "" {
		t.Errorf("the user must come from the file's owner, got %q", r.User)
	}
	Remove(got)
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("Remove left %d files", len(left))
	}
}

// The folder is writable by anyone: the agent reads only plain report files it can account for, and deletes the rest.
func TestTakeFollowsNothingItDidNotPutThere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix only")
	}
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "agent.conf")
	_ = os.WriteFile(secret, []byte(`{"job":"x","started_at":"2026-11-02T03:15:00Z","ended_at":"2026-11-02T03:16:00Z"}`), 0o600)
	id := func(c string) string { return strings.Repeat(c, 32) }
	_ = os.Symlink(secret, filepath.Join(dir, id("a")+".json"))
	_ = os.Link(secret, filepath.Join(dir, id("b")+".json"))
	_ = os.WriteFile(filepath.Join(dir, "not-a-report.json"), []byte("{}"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, id("c")+".json"), []byte(strings.Repeat("x", MaxReportBytes+1)), 0o644)
	_ = os.WriteFile(filepath.Join(dir, id("d")+".json"), []byte("not json"), 0o644)
	old := filepath.Join(dir, id("e")+".json")
	_ = os.WriteFile(old, []byte(`{"job":"x","started_at":"2026-11-02T03:15:00Z","ended_at":"2026-11-02T03:16:00Z"}`), 0o644)
	long := time.Now().Add(-MaxAge - time.Hour)
	_ = os.Chtimes(old, long, long)
	_ = os.Mkdir(filepath.Join(dir, id("f")+".json"), 0o755)

	got, err := Take(dir, MaxPerPush, time.Now())
	if err != nil || len(got) != 0 {
		t.Fatalf("nothing here is a report: %+v %v", got, err)
	}
	left, _ := os.ReadDir(dir)
	for _, e := range left {
		if e.Name() != id("f")+".json" {
			t.Errorf("%s was left behind", e.Name())
		}
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatal("a link's target must never be touched")
	}
}

func TestAMissingFolderIsSaid(t *testing.T) {
	if _, err := Take(filepath.Join(t.TempDir(), "nope"), 10, time.Now()); err != ErrNoFolder {
		t.Fatalf("want ErrNoFolder, got %v", err)
	}
	if err := Write(filepath.Join(t.TempDir(), "nope"), Report{Job: "x"}); err != ErrNoFolder {
		t.Fatalf("want ErrNoFolder, got %v", err)
	}
}

func TestReportsAreClippedToTheContract(t *testing.T) {
	r := Report{Job: strings.Repeat("j", 300), Output: make([]string, 30)}
	for i := range r.Output {
		r.Output[i] = strings.Repeat("é", 600)
	}
	c := r.Clipped()
	if len(c.Job) > MaxJobBytes || len(c.Output) != MaxLines || len(c.Output[0]) > MaxLineBytes || !strings.HasPrefix(c.Output[0], "é") {
		t.Fatalf("clipped: job %d, lines %d, line %d", len(c.Job), len(c.Output), len(c.Output[0]))
	}
}
