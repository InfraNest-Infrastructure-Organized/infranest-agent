package journal

import "github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/loginunit"

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

// loginIdentifiers are the programs whose own lines are login records whatever unit they run under (rule 2).
var loginIdentifiers = map[string]bool{
	"sshd": true, "sshd-session": true, "sshd-auth": true, "dropbear": true,
	"sudo": true, "su": true, "login": true, "systemd-logind": true,
	"polkitd": true, "polkit-agent-helper-1": true, "unix_chkpwd": true,
}

// LoginUnit is whether a unit is a login service, whose lines are never read (rule 1). The list is `loginunit`'s,
// shared with the collectors that name units in `services[]` and `processes[]` (#5864).
func LoginUnit(unit string) bool { return loginunit.Is(unit) }

// ReportedUnit is the name a unit is sent under in `journal`: a login unit's template instance as its template
// (`sshd@.service`), every other name as it is. See `loginunit.Reported`.
func ReportedUnit(unit string) string { return loginunit.Reported(unit) }

// loginRecord is whether one entry is a login record by where it was written (rule 2).
func loginRecord(e Entry) bool {
	return e.Facility == facilityAuth || e.Facility == facilityAuthPriv || loginIdentifiers[e.Identifier] || e.Transport == "audit"
}
