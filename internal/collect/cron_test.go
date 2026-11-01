package collect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A Debian machine's cron, with the shapes people actually write: an environment, CRON_TZ, nicknames, ranges and
// steps, names of days, `%` for input, a line cron would refuse, a file Debian's cron skips, and a job going through
// the wrapper.
func cronFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("etc/debian_version", "12.7\n", 0o644)
	write("usr/sbin/anacron", "", 0o755)
	write("etc/crontab", `# /etc/crontab: system-wide crontab
SHELL=/bin/sh
PATH=/usr/local/sbin:/usr/local/bin:/sbin:/bin:/usr/sbin:/usr/bin

# m h dom mon dow user	command
17 *	* * *	root	cd / && run-parts --report /etc/cron.hourly
25 6	* * *	root	test -x /usr/sbin/anacron || { cd / && run-parts --report /etc/cron.daily; }
47 6	* * 7	root	test -x /usr/sbin/anacron || { cd / && run-parts --report /etc/cron.weekly; }
52 6	1 * *	root	test -x /usr/sbin/anacron || { cd / && run-parts --report /etc/cron.monthly; }
0 17 1 * * www-data /usr/bin/php /var/www/shop/bin/report.php --month=last
`, 0o644)
	write("etc/cron.d/acme-backup", `MAILTO=ops@example.com
CRON_TZ=Europe/Amsterdam
15 3 * * * root PGPASSWORD=hunter2 /opt/acme/backup.sh --full
*/30 * * * mon-fri root nice -n 10 /usr/local/bin/sync-files
@daily postgres pg_dump shop | gzip > /backups/shop.sql.gz
0 4 * * * root /usr/bin/mail -s "report" ops%line one%line two
this is not a cron line at all
99 * * root
0 1 * * * root infranest-agent run --job "Nightly backup" -- /opt/acme/backup.sh
`, 0o644)
	write("etc/cron.d/php", `09,39 *     * * *     root   [ -x /usr/lib/php/sessionclean ] && if [ ! -d /run/systemd/system ]; then /usr/lib/php/sessionclean; fi
`, 0o644)
	write("etc/cron.d/e2scrub_all", "30 3 * * 0 root test -e /run/systemd/system || SERVICE_MODE=1 /usr/lib/x86_64-linux-gnu/e2fsprogs/e2scrub_all_cron\n", 0o644)
	write("etc/cron.d/backup.sh", "0 2 * * * root /never/runs\n", 0o644)
	write("etc/cron.d/acme-backup.dpkg-old", "0 2 * * * root /never/runs\n", 0o644)
	write("etc/cron.d/.placeholder", "", 0o644)
	write("etc/cron.daily/logrotate", "#!/bin/sh\n", 0o755)
	write("etc/cron.daily/not-executable", "#!/bin/sh\n", 0o644)
	write("etc/cron.daily/.placeholder", "", 0o644)
	write("etc/cron.hourly/check-disk", "#!/bin/sh\n", 0o755)
	if err := os.Symlink("/usr/share/zoneinfo/Europe/London", filepath.Join(root, "etc/localtime")); err != nil {
		t.Fatal(err)
	}

	return root
}

func byProgram(jobs []CronJob) map[string]CronJob {
	out := map[string]CronJob{}
	for _, j := range jobs {
		out[j.Program] = j
	}

	return out
}

func TestCronReadsWhatAnyUserCanReadAndSkipsWhatCronSkips(t *testing.T) {
	jobs := readCron(cronFixture(t), false)
	got := byProgram(jobs)

	for _, want := range []string{"report.php", "backup.sh", "sync-files", "pg_dump", "mail", "sessionclean", "e2scrub_all_cron", "logrotate", "check-disk"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s is not listed; got %v", want, keys(got))
		}
	}
	for _, never := range []string{"runs", "not-executable", "run-parts", "cd", "[", "test", "nice", "infranest-agent"} {
		if _, ok := got[never]; ok {
			t.Errorf("%s must not be listed", never)
		}
	}
	if len(jobs) != 10 {
		t.Errorf("want 10 jobs, got %d: %v", len(jobs), keys(got))
	}

	backup := got["sync-files"]
	if backup.Schedule != "*/30 * * * mon-fri" || backup.User != "root" || backup.Source != "/etc/cron.d/acme-backup" || backup.Line != 4 {
		t.Errorf("sync-files: %+v", backup)
	}
	// CRON_TZ in the file wins over the machine's zone; a line without one is read in the machine's.
	if backup.Timezone != "Europe/Amsterdam" {
		t.Errorf("CRON_TZ not applied: %q", backup.Timezone)
	}
	if got["report.php"].Timezone != "Europe/London" || got["report.php"].User != "www-data" {
		t.Errorf("report.php: %+v", got["report.php"])
	}
	if got["pg_dump"].Schedule != "@daily" || got["pg_dump"].User != "postgres" {
		t.Errorf("a nickname: %+v", got["pg_dump"])
	}

	// The run-parts folders: the hourly one at its crontab line's time, the daily one handed to anacron.
	if h := got["check-disk"]; h.Schedule != "17 * * * *" || h.Folder != "hourly" || h.Anacron || h.Line != 0 {
		t.Errorf("hourly script: %+v", h)
	}
	if d := got["logrotate"]; d.Schedule != "@daily" || !d.Anacron || d.Folder != "daily" || d.Source != "/etc/cron.daily/logrotate" {
		t.Errorf("daily script: %+v", d)
	}
}

// Command lines carry passwords: off by default, the program's name is all that leaves the machine.
func TestCronCommandsOnlyWithCommandLinesOn(t *testing.T) {
	root := cronFixture(t)
	for _, j := range readCron(root, false) {
		if j.Command != "" {
			t.Fatalf("a command was sent with command lines off: %+v", j)
		}
		if strings.Contains(j.Program, "hunter2") || strings.Contains(j.Program, " ") {
			t.Fatalf("a program name carries more than the name: %q", j.Program)
		}
	}
	withArgs := readCron(root, true)
	got := byProgram(withArgs)
	full := ""
	for _, j := range withArgs {
		if j.Source == "/etc/cron.d/acme-backup" && j.Line == 3 {
			full = j.Command
		}
	}
	if full != "PGPASSWORD=hunter2 /opt/acme/backup.sh --full" {
		t.Errorf("with command lines on, the whole command: %q", full)
	}
	// What follows `%` is the command's input, not the command.
	if got["mail"].Command != `/usr/bin/mail -s "report" ops` {
		t.Errorf("%% not honoured: %q", got["mail"].Command)
	}
}

// A line going through `infranest-agent run --job` names its job, and the program is what the wrapper runs.
func TestCronNamesAWrappedJob(t *testing.T) {
	var wrapped *CronJob
	for _, j := range readCron(cronFixture(t), false) {
		if j.Job != "" {
			j := j
			wrapped = &j
		}
	}
	if wrapped == nil || wrapped.Job != "Nightly backup" || wrapped.Program != "backup.sh" || wrapped.Schedule != "0 1 * * *" {
		t.Fatalf("wrapped job: %+v", wrapped)
	}
}

func TestProgramOf(t *testing.T) {
	for cmd, want := range map[string]string{
		"/opt/acme/backup.sh --full":                           "backup.sh",
		"FOO=1 BAR=2 /usr/bin/thing":                           "thing",
		"cd /srv/app && php artisan schedule:run >> /dev/null": "php",
		"nice -n 19 ionice -c3 /usr/local/bin/heavy":           "heavy",
		"flock -n /tmp/lock /opt/job.sh":                       "job.sh",
		"timeout 300 /opt/job.sh":                              "job.sh",
		"sudo -u www-data /opt/job.sh":                         "job.sh",
		"test -x /usr/sbin/certbot -a \\! -d /run/systemd/system && perl -e 'sleep int(rand(43200))' && certbot -q renew": "perl",
		"/usr/bin/php /var/www/shop/bin/report.php --month=last":                                                          "report.php",
		"python3 -u manage.py clearsessions":                                                                              "manage.py",
		"bash -c 'echo hi'":                                                                                               "bash",
		"":                                                                                                                "",
	} {
		if got := programOf(cmd); got != want {
			t.Errorf("programOf(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// The RHEL family: cronie, no /etc/debian_version, the daily folder run by anacron from /etc/anacrontab with no
// crontab line, and file names Debian would skip.
func TestCronOnTheRHELFamily(t *testing.T) {
	root := t.TempDir()
	must := func(path, body string, mode os.FileMode) {
		full := filepath.Join(root, path)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	must("etc/crontab", "SHELL=/bin/bash\n", 0o644)
	must("etc/cron.d/0hourly", "SHELL=/bin/bash\n01 * * * * root run-parts /etc/cron.hourly\n", 0o644)
	must("etc/cron.d/raid-check", "0 1 * * Sun root /usr/sbin/raid-check\n", 0o644)
	must("etc/cron.d/app.cron", "*/5 * * * * apache /var/www/bin/queue\n", 0o644)
	must("etc/anacrontab", "1\t5\tcron.daily\t\tnice run-parts /etc/cron.daily\n", 0o644)
	must("etc/cron.hourly/0anacron", "#!/bin/sh\n", 0o755)
	must("etc/cron.daily/rhsmd", "#!/bin/sh\n", 0o755)
	must("etc/timezone", "America/New_York\n", 0o644)

	got := byProgram(readCron(root, false))
	if got["queue"].Source != "/etc/cron.d/app.cron" {
		t.Errorf("cronie reads a dotted name: %v", keys(got))
	}
	if got["raid-check"].Schedule != "0 1 * * Sun" {
		t.Errorf("raid-check: %+v", got["raid-check"])
	}
	if a := got["0anacron"]; a.Schedule != "01 * * * *" || a.Anacron {
		t.Errorf("hourly: %+v", a)
	}
	if d := got["rhsmd"]; d.Schedule != "@daily" || !d.Anacron || d.Timezone != "America/New_York" {
		t.Errorf("daily, from anacrontab: %+v", d)
	}
}

// Read once and kept until a file changes.
func TestCronIsCachedUntilAFileChanges(t *testing.T) {
	root := cronFixture(t)
	r := NewCronReader()
	first := r.Read(root, false)
	path := filepath.Join(root, "etc/cron.d/php")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// The folder's own mtime moves when a file goes; make sure a coarse clock sees it.
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(filepath.Join(root, "etc/cron.d"), future, future)
	second := r.Read(root, false)
	if len(second) != len(first)-1 {
		t.Fatalf("a removed file was not noticed: %d then %d", len(first), len(second))
	}
	if third := r.Read(root, false); &third[0] != &second[0] {
		t.Fatal("an unchanged machine must be answered from the cache")
	}
}

// Nothing to read is an empty list, never nil — the receiver clears its list on `[]`.
func TestCronOnAMachineWithoutCron(t *testing.T) {
	jobs := NewCronReader().Read(t.TempDir(), false)
	if jobs == nil || len(jobs) != 0 {
		t.Fatalf("want an empty list, got %#v", jobs)
	}
}

func keys(m map[string]CronJob) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}
