# The login fixture's entries (InfraNest #5859), in journald's export format: login records written the ways they
# really are — by sshd under its own unit, by PAM into cron's log, by sshd running with -e under an app's unit, by sudo,
# and by the kernel's audit — next to the ordinary lines that must still be sent. 2026-11-01, 12:00-12:59 UTC.
import sys
from datetime import datetime, timezone

BOOT = '0123456789abcdef0123456789abcdef'
BASE = int(datetime(2026, 11, 1, 12, 0, tzinfo=timezone.utc).timestamp()) * 1_000_000
out = sys.stdout.buffer

def entry(minute, fields):
    at = BASE + minute * 60 * 1_000_000
    out.write(b'__REALTIME_TIMESTAMP=%d\n' % at)
    out.write(b'__MONOTONIC_TIMESTAMP=%d\n' % (at - BASE + 1_000_000))
    out.write(b'_BOOT_ID=%s\n' % BOOT.encode())
    out.write(b'_HOSTNAME=web-01\n')
    fields.setdefault('_TRANSPORT', 'syslog')
    for k, v in fields.items():
        out.write(k.encode() + b'=' + v.encode() + b'\n')
    out.write(b'\n')

# A login unit: sshd's own lines, auth facility.
entry(0, {'_SYSTEMD_UNIT': 'ssh.service', 'SYSLOG_FACILITY': '4', 'SYSLOG_IDENTIFIER': 'sshd', 'PRIORITY': '6',
          'MESSAGE': 'Accepted publickey for jaspar from 203.0.113.7 port 51234 ssh2'})
entry(1, {'_SYSTEMD_UNIT': 'ssh.service', 'SYSLOG_FACILITY': '4', 'SYSLOG_IDENTIFIER': 'sshd', 'PRIORITY': '3',
          'MESSAGE': 'fatal: Cannot bind any address.'})

# cron: its PAM line is authpriv; its own line about the job is not.
entry(10, {'_SYSTEMD_UNIT': 'cron.service', 'SYSLOG_FACILITY': '10', 'SYSLOG_IDENTIFIER': 'CRON', 'PRIORITY': '6',
           'MESSAGE': 'pam_unix(cron:session): session opened for user root(uid=0) by (uid=0)'})
entry(11, {'_SYSTEMD_UNIT': 'cron.service', 'SYSLOG_FACILITY': '9', 'SYSLOG_IDENTIFIER': 'CRON', 'PRIORITY': '3',
           'MESSAGE': '(CRON) error (grandchild #4242 failed with exit status 1)'})

# app.service: sshd -e logging to stderr (no facility), sudo, an audit record with no priority, and the app's own line.
entry(20, {'_SYSTEMD_UNIT': 'app.service', '_TRANSPORT': 'stdout', 'SYSLOG_IDENTIFIER': 'sshd', 'PRIORITY': '6',
           'MESSAGE': 'Failed password for invalid user admin from 198.51.100.9 port 4242 ssh2'})
entry(21, {'_SYSTEMD_UNIT': 'app.service', 'SYSLOG_FACILITY': '10', 'SYSLOG_IDENTIFIER': 'sudo', 'PRIORITY': '5',
           'MESSAGE': 'jaspar : TTY=pts/0 ; PWD=/srv/app ; USER=root ; COMMAND=/usr/bin/systemctl restart app'})
entry(22, {'_SYSTEMD_UNIT': 'app.service', '_TRANSPORT': 'audit',
           'MESSAGE': 'USER_LOGIN pid=4242 uid=0 auid=1000 msg=\'op=login acct="jaspar" addr=203.0.113.7 res=success\''})
entry(23, {'_SYSTEMD_UNIT': 'app.service', '_TRANSPORT': 'stdout', 'SYSLOG_IDENTIFIER': 'app', 'SYSLOG_FACILITY': '3',
           'PRIORITY': '3', 'MESSAGE': 'worker crashed: out of file descriptors'})
