# The failed-login fixture's entries (InfraNest #5868), in journald's export format: refusals written the ways sshd,
# dropbear, sudo and su really write them, beside the lines on the same facilities that are not refusals or are the
# second line about one attempt. 2026-11-01, 14:00-14:59 UTC, and one refusal at 15:00 that belongs to the next hour.
# The comment on each says whether it counts; the hour's total is 9.
import sys
from datetime import datetime, timezone

BOOT = '0123456789abcdef0123456789abcdef'
BASE = int(datetime(2026, 11, 1, 14, 0, tzinfo=timezone.utc).timestamp()) * 1_000_000
out = sys.stdout.buffer

def entry(minute, unit, facility, identifier, priority, message):
    at = BASE + minute * 60 * 1_000_000
    out.write(b'__REALTIME_TIMESTAMP=%d\n' % at)
    out.write(b'__MONOTONIC_TIMESTAMP=%d\n' % (at - BASE + 1_000_000))
    out.write(b'_BOOT_ID=%s\n' % BOOT.encode())
    out.write(b'_HOSTNAME=web-01\n')
    out.write(b'_TRANSPORT=syslog\n')
    fields = {'_SYSTEMD_UNIT': unit, 'SYSLOG_FACILITY': facility, 'SYSLOG_IDENTIFIER': identifier,
              'PRIORITY': priority, 'MESSAGE': message}
    for k, v in fields.items():
        if v is not None:
            out.write(k.encode() + b'=' + v.encode() + b'\n')
    out.write(b'\n')

# sshd on Debian (auth, 4): two refused passwords for a real account — 2.
entry(1, 'ssh.service', '4', 'sshd', '6', 'Failed password for root from 203.0.113.7 port 51234 ssh2')
entry(2, 'ssh.service', '4', 'sshd', '6', 'Failed password for root from 203.0.113.7 port 51234 ssh2')
# ...and PAM's second line about the same attempt — 0.
entry(2, 'ssh.service', '10', 'sshd', '5', 'pam_unix(sshd:auth): authentication failure; logname= uid=0 euid=0 tty=ssh ruser= rhost=203.0.113.7  user=root')
# sshd-session on RHEL (authpriv, 10) — 1.
entry(3, 'sshd.service', '10', 'sshd-session', '6', 'Failed password for jaspar from 198.51.100.9 port 4242 ssh2')
# An account that does not exist: the connection counts once — 1 — and its passwords not again — 0.
entry(4, 'ssh.service', '4', 'sshd', '6', 'Invalid user admin from 198.51.100.9 port 4243')
entry(4, 'ssh.service', '4', 'sshd', '6', 'Failed password for invalid user admin from 198.51.100.9 port 4243 ssh2')
entry(4, 'ssh.service', '4', 'sshd', '6', 'Connection closed by invalid user admin 198.51.100.9 port 4243 [preauth]')
# keyboard-interactive through PAM — 1.
entry(5, 'ssh.service', '4', 'sshd', '6', 'Failed keyboard-interactive/pam for root from 203.0.113.7 port 51300 ssh2')
# Not refusals of a credential: a login, a key offered in turn, a probe for methods — 0.
entry(6, 'ssh.service', '4', 'sshd', '6', 'Accepted publickey for jaspar from 203.0.113.7 port 51301 ssh2: ED25519 SHA256:abc')
entry(6, 'ssh.service', '4', 'sshd', '6', 'Failed publickey for jaspar from 203.0.113.7 port 51302 ssh2: RSA SHA256:def')
entry(6, 'ssh.service', '4', 'sshd', '6', 'Failed none for root from 203.0.113.7 port 51303 ssh2')
# sudo: PAM's refusal — 1 — and sudo's own summary of it — 0.
entry(10, 'session-3.scope', '10', 'sudo', '5', 'pam_unix(sudo:auth): authentication failure; logname=jaspar uid=1000 euid=0 tty=/dev/pts/0 ruser=jaspar rhost=  user=jaspar')
entry(10, 'session-3.scope', '10', 'sudo', '2', 'jaspar : 3 incorrect password attempts ; TTY=pts/0 ; PWD=/home/jaspar ; USER=root ; COMMAND=/bin/true')
# su: PAM's refusal — 1 — and util-linux su's own line — 0.
entry(11, 'session-3.scope', '10', 'su', '5', 'pam_unix(su:auth): authentication failure; logname=jaspar uid=1000 euid=0 tty=/dev/pts/0 ruser=jaspar rhost=  user=root')
entry(11, 'session-3.scope', '10', 'su', '5', 'FAILED SU (to root) jaspar on pts/0')
# dropbear (authpriv) — 2.
entry(20, 'dropbear.service', '10', 'dropbear', '4', "Bad password attempt for 'root' from 192.0.2.1:40000")
entry(21, 'dropbear.service', '10', 'dropbear', '6', 'Login attempt for nonexistent user from 192.0.2.1:40001')
# cron's PAM session line — 0.
entry(30, 'cron.service', '10', 'CRON', '6', 'pam_unix(cron:session): session opened for user root(uid=0) by (uid=0)')
# sshd's wording on the wrong facility (user, 1: anyone can `logger` it) — 0 — and from a program that is not sshd — 0.
entry(40, 'app.service', '1', 'sshd', '6', 'Failed password for root from 203.0.113.7 port 1 ssh2')
entry(41, 'app.service', '4', 'app', '6', 'Failed password for root from 203.0.113.7 port 1 ssh2')
# The next hour — counted there, not here.
entry(60, 'ssh.service', '4', 'sshd', '6', 'Failed password for root from 203.0.113.7 port 51999 ssh2')
