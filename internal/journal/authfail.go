package journal

import "strings"

// Failed logins, counted (InfraNest #5868). Each hour's `auth_failures` is how many times somebody tried to sign in to
// this machine and was refused. It is a number and nothing else: the line is read on the machine to decide whether it
// is a refusal, and then dropped — no user name, no address, no program and no line ever leaves with it.
//
// Only lines written to the auth or authpriv syslog facility are looked at, which is where sshd, dropbear and PAM write
// on every distribution; the rest of the journal is not read for this at all. Among those, one attempt is one count:
//
//   - sshd: `Failed password for …` and `Failed keyboard-interactive/pam for …` — a password tried against a real
//     account. `Invalid user … from …` — a connection naming an account that does not exist, counted once however many
//     passwords it then tries, so its `Failed password for invalid user …` lines are not counted again. Not counted:
//     `Failed publickey` (a client offering its keys in turn is how key login works, and sshd logs it only past half
//     of MaxAuthTries) and `Failed none` (a probe for which methods are offered, not a credential).
//   - dropbear: `Bad password attempt for …` and `Login attempt for nonexistent user …`.
//   - PAM: `pam_<module>(<service>:auth): authentication failure` for any service but sshd — a password refused by
//     sudo, su, login, the web console. sshd's own line above already counted that attempt; PAM writes a second one
//     for it, and a summary like sudo's `3 incorrect password attempts` is not counted either.
//
// The patterns are sshd's, dropbear's and pam_unix's own wording, which has not changed in two decades. A line that
// matches none of them is not counted, so a program that words a refusal differently is missed, never over-counted.

// authFacility is whether an entry was written to auth (4) or authpriv (10) — the only entries read to be counted.
func authFacility(e Entry) bool {
	return e.Facility == 4 || e.Facility == 10
}

// sshdIdentifiers are the names sshd writes under: `sshd`, and since OpenSSH 9.8 its per-connection `sshd-session` and
// `sshd-auth`. A line in sshd's wording from any other program is not sshd's.
var sshdIdentifiers = map[string]bool{"sshd": true, "sshd-session": true, "sshd-auth": true}

// authFailure is whether one auth or authpriv line is a refused login attempt, by the rules above.
func authFailure(identifier, message string) bool {
	// One line: a message of several lines is not a login program's.
	if nl := strings.IndexByte(message, '\n'); nl >= 0 {
		message = message[:nl]
	}
	switch {
	case sshdIdentifiers[identifier]:
		if strings.HasPrefix(message, "Invalid user ") {
			return true
		}
		for _, method := range []string{"password", "keyboard-interactive/pam"} {
			if rest, ok := strings.CutPrefix(message, "Failed "+method+" for "); ok {
				return !strings.HasPrefix(rest, "invalid user ")
			}
		}

		return false
	case identifier == "dropbear":
		return strings.HasPrefix(message, "Bad password attempt for ") ||
			strings.HasPrefix(message, "Login attempt for nonexistent user")
	}

	// pam_unix(sudo:auth): authentication failure; logname=… — any module, any service but sshd's.
	if !strings.HasPrefix(message, "pam_") {
		return false
	}
	open := strings.IndexByte(message, '(')
	end := strings.Index(message, ":auth): authentication failure")
	if open < 0 || end < open {
		return false
	}

	return message[open+1:end] != "sshd"
}
