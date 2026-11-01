package journal

import "strings"

// Login records never leave the machine (InfraNest #5859). A unit's last lines are sent to say why it failed; the
// lines of a service whose job is letting people in are records of who came in, from where — `Accepted publickey for
// jaspar from 203.0.113.7`, `Failed password for invalid user admin` — and masking, which looks for secrets, does not
// touch a user name or an address. So two rules, both applied here on the machine:
//
//  1. A **login unit** — the list below — has no lines read at all. Its event is still sent: that it failed, and how.
//  2. From **every other unit**, a line is dropped when it is a login record by where it was written: the auth or
//     authpriv syslog facility (which PAM, sshd, sudo, su and login write to, wherever they run — cron's
//     `pam_unix(cron:session): session opened for user root` lands in cron.service's log), a login program's own
//     identifier (sshd started with `-e` in a container logs to stderr, with no facility), or the kernel's audit
//     records (`USER_LOGIN … acct="jaspar" addr=…`, attributed to the unit whose process caused them).
//
// The hourly counts are numbers per unit, and still count these lines.

// WithheldLogin is `lines_withheld` on an event: why it carries no lines.
const WithheldLogin = "login"

// syslog facilities that are login records: 4 auth, 10 authpriv.
const (
	facilityAuth     = 4
	facilityAuthPriv = 10
)

// loginUnits are the services whose log is a record of people signing in, matched on the unit's name without
// `.service` and without a template instance (`sshd@3-10.0.0.1:22-203.0.113.7:51234.service` is `sshd`). A name ending
// in `*` matches by prefix.
//
//   - ssh, sshd, sshd-session, dropbear: remote logins — every accepted and refused attempt, with user and address.
//     `ssh@` / `sshd@` are the per-connection instances of socket-activated sshd, named after the peer's address.
//   - systemd-logind, login, getty, serial-getty, console-getty, autovt, container-getty: console and session logins.
//   - su, sudo: who became whom. Rarely units themselves, listed so a distribution that ships them as one is covered.
//   - polkit, polkitd: who was allowed which privileged action.
//   - gdm*, lightdm, sddm, xrdp*: graphical and remote-desktop logins.
//   - cockpit*: the web console's logins, with the browser's address.
//   - sssd*, winbind, nslcd: directory logins — the user names of a whole domain pass through them.
//   - fail2ban, crowdsec, sshguard: their log is the addresses that tried to log in and were banned.
//
// Not listed, on purpose: cron (its own lines say why a job failed; its PAM lines are authpriv and dropped by rule 2),
// and services that happen to authenticate their own clients — a mail or FTP server's failure lines are the reason it
// failed. What they write to auth or authpriv is dropped all the same.
var loginUnits = []string{
	"ssh", "sshd", "sshd-session", "dropbear",
	"systemd-logind", "login", "getty", "serial-getty", "console-getty", "container-getty", "autovt",
	"su", "sudo",
	"polkit", "polkitd",
	"gdm*", "lightdm", "sddm", "xrdp*",
	"cockpit*",
	"sssd*", "winbind", "nslcd",
	"fail2ban", "crowdsec", "sshguard",
}

// loginIdentifiers are the programs whose own lines are login records whatever unit they run under (rule 2).
var loginIdentifiers = map[string]bool{
	"sshd": true, "sshd-session": true, "sshd-auth": true, "dropbear": true,
	"sudo": true, "su": true, "login": true, "systemd-logind": true,
	"polkitd": true, "polkit-agent-helper-1": true, "unix_chkpwd": true,
}

// LoginUnit is whether a unit is a login service, whose lines are never read (rule 1).
func LoginUnit(unit string) bool {
	name := strings.TrimSuffix(unit, ".service")
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	for _, l := range loginUnits {
		if p, ok := strings.CutSuffix(l, "*"); ok {
			if strings.HasPrefix(name, p) {
				return true
			}
		} else if name == l {
			return true
		}
	}

	return false
}

// ReportedUnit is the name a unit is sent under in `journal`. A login unit's template instance is named after the
// connection it serves — `sshd@3-10.0.0.1:22-203.0.113.7:51234.service` carries the peer's address — so it is sent as
// its template, `sshd@.service`. Every other name is sent as it is.
func ReportedUnit(unit string) string {
	at := strings.IndexByte(unit, '@')
	if at < 0 || !LoginUnit(unit) {
		return unit
	}
	suffix := ".service"
	if dot := strings.LastIndexByte(unit, '.'); dot > at {
		suffix = unit[dot:]
	}

	return unit[:at+1] + suffix
}

// loginRecord is whether one entry is a login record by where it was written (rule 2).
func loginRecord(e Entry) bool {
	return e.Facility == facilityAuth || e.Facility == facilityAuthPriv || loginIdentifiers[e.Identifier] || e.Transport == "audit"
}
