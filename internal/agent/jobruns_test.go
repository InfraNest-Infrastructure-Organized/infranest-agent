package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/jobs"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/push"
)

// A report the wrapper left is sent on the next push and deleted once that push is answered — and kept when it is
// not, so a job that failed during an outage is still heard about (InfraNest #5801).
func TestJobReportsAreSentAndDeletedOnlyOnceTaken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("job reports are collected on Unix only")
	}
	folder := t.TempDir()
	at := time.Now()
	if err := jobs.Write(folder, jobs.Report{Job: "Nightly backup", StartedAt: at, EndedAt: at.Add(time.Minute), ExitCode: 1}); err != nil {
		t.Fatal(err)
	}

	failing := &fakeSender{answer: func(int) (push.Result, error) { return push.Result{}, errors.New("cannot reach InfraNest") }}
	r, _, _ := harnessWith(t, failing, 3, func(r *Runner) { r.Config.JobsDir = folder })
	if len(failing.jobRuns) == 0 || failing.jobRuns[0] == nil {
		t.Fatalf("the report was not offered: %#v", failing.jobRuns)
	}
	if left, _ := os.ReadDir(folder); len(left) != 1 {
		t.Fatalf("a push that failed must leave the report for the next one, found %d files", len(left))
	}
	if r.jobsFolder != true {
		t.Fatal("collectors.job_reports should say the folder is there")
	}

	sender := &fakeSender{}
	harnessWith(t, sender, 3, func(r *Runner) { r.Config.JobsDir = folder })
	if len(sender.jobRuns) == 0 {
		t.Fatal("nothing was pushed")
	}
	runs, ok := sender.jobRuns[0].([]jobs.Report)
	if !ok || len(runs) != 1 || runs[0].Job != "Nightly backup" || runs[0].ExitCode != 1 || runs[0].User == "" {
		t.Fatalf("unexpected reports: %#v", sender.jobRuns[0])
	}
	if left, _ := os.ReadDir(folder); len(left) != 0 {
		t.Fatalf("a report InfraNest took must be deleted, %d left", len(left))
	}
	for _, later := range sender.jobRuns[1:] {
		if later != nil {
			t.Fatalf("a report must be sent once, not again: %#v", later)
		}
	}
	if !sender.collectors[0].JobReports {
		t.Fatal("collectors.job_reports should be true with a folder")
	}
}

// No folder — an agent upgraded by a binary swap, before the installer ran — is said in the collectors, and a push
// carries no `job_runs` at all.
func TestNoReportsFolderIsSaidAndSendsNothing(t *testing.T) {
	sender := &fakeSender{}
	harnessWith(t, sender, 3, func(r *Runner) { r.Config.JobsDir = filepath.Join(t.TempDir(), "missing") })
	if len(sender.jobRuns) == 0 {
		t.Fatal("nothing was pushed")
	}
	if sender.jobRuns[0] != nil {
		t.Fatalf("nothing to send must be an untyped nil, got %#v", sender.jobRuns[0])
	}
	if sender.collectors[0].JobReports {
		t.Fatal("collectors.job_reports must be false without the folder")
	}
}
