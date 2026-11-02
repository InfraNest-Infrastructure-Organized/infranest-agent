// Package loginunit knows which systemd units let people in (InfraNest #5859, #5864), for every collector that names a
// unit: what such a unit writes is a record of who came in and from where, and a per-connection instance of one is
// named after the connection itself — `sshd@3-10.0.0.1:22-203.0.113.7:51234.service` carries the peer's address. One
// list, so `journal`, `services[]` and `processes[].unit` cannot disagree about which units those are.
package loginunit

import "strings"

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
var units = []string{
	"ssh", "sshd", "sshd-session", "dropbear",
	"systemd-logind", "login", "getty", "serial-getty", "console-getty", "container-getty", "autovt",
	"su", "sudo",
	"polkit", "polkitd",
	"gdm*", "lightdm", "sddm", "xrdp*",
	"cockpit*",
	"sssd*", "winbind", "nslcd",
	"fail2ban", "crowdsec", "sshguard",
}

// Is whether a unit is a login service.
func Is(unit string) bool {
	name := strings.TrimSuffix(unit, ".service")
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	for _, l := range units {
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

// Instance is whether a unit is a template instance of a login service — `sshd@3-10.0.0.1:22-203.0.113.7:51234.service`,
// `getty@tty1.service` — rather than the service itself or its template (`sshd@.service`).
func Instance(unit string) bool {
	at := strings.IndexByte(unit, '@')
	return at >= 0 && !strings.HasPrefix(unit[at+1:], ".") && Is(unit)
}

// Reported is the name a unit is sent under. A login service's template instance is sent as its template,
// `sshd@.service`, because the instance's name can be the connection it serves. Every other name is sent as it is.
func Reported(unit string) string {
	if !Instance(unit) {
		return unit
	}
	at := strings.IndexByte(unit, '@')
	// The unit's type suffix, when it has one: letters after the last dot. An address in the instance has dots too
	// (`…203.0.113.7:51234` with no suffix ends in ".7:51234"), so a "suffix" with anything else in it is not one.
	suffix := ".service"
	if dot := strings.LastIndexByte(unit, '.'); dot > at && letters(unit[dot+1:]) {
		suffix = unit[dot:]
	}

	return unit[:at+1] + suffix
}

func letters(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}
