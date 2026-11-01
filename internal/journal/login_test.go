package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The login fixture (testdata/login.py) is written by systemd's own writer: sshd under ssh.service, PAM into cron's
// log, sshd -e, sudo and an audit record under app.service — each beside an ordinary line that must still be sent.
const loginFixture = "login"

// Nothing that names who logged in, or from where, may be in any line.
var loginWords = []string{"jaspar", "admin", "root", "203.0.113.7", "198.51.100.9", "Accepted", "Failed password", "USER_LOGIN"}

func TestTheReaderReadsWhereALineWasWritten(t *testing.T) {
	f := openFixture(t, loginFixture)[0]
	var got []Entry
	f.Backward(at(12, 0), MaxScan, func(e Entry) bool { got = append(got, e); return true })
	if len(got) != 8 {
		t.Fatalf("%d entries, want 8", len(got))
	}
	// Newest first: the app's own line, the audit record, sudo, sshd -e …, sshd's own first line last.
	if e := got[0]; e.Facility != 3 || e.Identifier != "app" || e.Transport != "stdout" {
		t.Errorf("app's line: %+v", e)
	}
	if e := got[1]; e.Transport != "audit" || e.Facility != -1 || e.Priority != -1 {
		t.Errorf("the audit record: %+v", e)
	}
	if e := got[2]; e.Facility != 10 || e.Identifier != "sudo" {
		t.Errorf("sudo's line: %+v", e)
	}
	if e := got[7]; e.Facility != 4 || e.Identifier != "sshd" || e.Unit != "ssh.service" {
		t.Errorf("sshd's line: %+v", e)
	}
}

func TestALoginRecordIsDroppedFromAnyUnitsLines(t *testing.T) {
	cases := map[string]string{
		// PAM's line is authpriv; cron's own word about the job stays.
		"cron.service": "(CRON) error (grandchild #4242 failed with exit status 1)",
		// sshd -e (no facility, its own identifier), sudo (authpriv) and an audit record (no priority at all) go.
		"app.service": "worker crashed: out of file descriptors",
	}
	for unit, want := range cases {
		s := lastLines(openFixture(t, loginFixture), unit, at(12, 0), at(13, 0))
		if len(s.Lines) != 1 || s.Lines[0].Message != want {
			t.Errorf("%s: %+v, want only %q", unit, s.Lines, want)
		}
		if s.Withheld != "" {
			t.Errorf("%s is not a login unit, yet withheld %q", unit, s.Withheld)
		}
	}
	// ssh.service's lines are all auth: even read directly, none is kept.
	if s := lastLines(openFixture(t, loginFixture), "ssh.service", at(12, 0), at(13, 0)); len(s.Lines) != 0 {
		t.Errorf("sshd's own lines were kept: %+v", s.Lines)
	}
}

func TestALoginUnitsLinesAreNeverRead(t *testing.T) {
	dir := t.TempDir()
	was := Dirs
	Dirs = []string{dir}
	t.Cleanup(func() { Dirs = was })
	machine := filepath.Join(dir, "0123456789abcdef0123456789abcdef")
	if err := os.MkdirAll(machine, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(machine, "system.journal"), fixture(t, loginFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	s := LastLines("ssh.service", at(12, 0), at(13, 0))
	if s.Withheld != WithheldLogin || s.Lines != nil || s.Compressed != 0 || s.Truncated {
		t.Errorf("a login unit: %+v, want withheld %q and nothing else", s, WithheldLogin)
	}
	var all []string
	for _, unit := range []string{"ssh.service", "cron.service", "app.service"} {
		for _, l := range LastLines(unit, at(12, 0), at(13, 0)).Lines {
			all = append(all, l.Message)
		}
	}
	text := strings.Join(all, "\n")
	for _, w := range loginWords {
		if strings.Contains(text, w) {
			t.Errorf("%q left the reader:\n%s", w, text)
		}
	}
}

// The hourly counts are numbers per unit, and still count a login unit's errors.
func TestALoginUnitsErrorsAreStillCounted(t *testing.T) {
	h := countHour(openFixture(t, loginFixture), at(12, 0))
	want := map[string]UnitCount{
		"app.service":  {Unit: "app.service", Errors: 1},
		"cron.service": {Unit: "cron.service", Errors: 1},
		"ssh.service":  {Unit: "ssh.service", Errors: 1},
	}
	if len(h.Units) != len(want) {
		t.Fatalf("%+v", h.Units)
	}
	for _, u := range h.Units {
		if u != want[u.Unit] {
			t.Errorf("%+v, want %+v", u, want[u.Unit])
		}
	}
}

func TestLoginUnitsAreKnownByName(t *testing.T) {
	login := []string{
		"ssh", "ssh.service", "sshd.service", "sshd@3-10.0.0.1:22-203.0.113.7:51234.service", "ssh@0-1.service",
		"sshd-session.service", "dropbear.service", "systemd-logind.service", "getty@tty1.service",
		"serial-getty@ttyS0.service", "autovt@tty2.service", "login", "su", "sudo.service", "polkit.service",
		"polkitd", "gdm.service", "gdm3.service", "lightdm.service", "sddm.service", "xrdp-sesman.service",
		"cockpit.service", "cockpit-wsinstance-https@x.service", "sssd.service", "sssd-nss.service", "winbind.service",
		"nslcd.service", "fail2ban.service", "crowdsec.service", "sshguard.service",
	}
	for _, u := range login {
		if !LoginUnit(u) {
			t.Errorf("%s is a login unit", u)
		}
	}
	other := []string{
		"nginx.service", "cron.service", "app.service", "sshfs.service", "sudoers-sync.service", "logind-watch.service",
		"supervisor.service", "postfix@-.service", "dovecot.service", "polkit-exporter.service", "ssh-agent.service",
	}
	for _, u := range other {
		if LoginUnit(u) {
			t.Errorf("%s is not a login unit", u)
		}
	}
}

func TestALoginUnitsInstanceIsSentAsItsTemplate(t *testing.T) {
	cases := map[string]string{
		"sshd@3-10.0.0.1:22-203.0.113.7:51234.service":      "sshd@.service",
		"ssh@0-[2001:db8::1]:22-[2001:db8::7]:4242.service": "ssh@.service",
		"getty@tty1.service":                                "getty@.service",
		"ssh.service":                                       "ssh.service",
		"nginx@site-a.service":                              "nginx@site-a.service",
		"postgresql@16-main.service":                        "postgresql@16-main.service",
	}
	for unit, want := range cases {
		if got := ReportedUnit(unit); got != want {
			t.Errorf("%s is sent as %q, want %q", unit, got, want)
		}
	}
}
