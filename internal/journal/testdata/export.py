# The fixture's entries, in journald's export format (https://systemd.io/JOURNAL_EXPORT_FORMATS/). The times are fixed
# so the tests can name them: 2026-11-01, 09:00-10:59 UTC.
import struct, sys
from datetime import datetime, timezone

BOOT = '0123456789abcdef0123456789abcdef'
BASE = int(datetime(2026, 11, 1, 9, 0, tzinfo=timezone.utc).timestamp()) * 1_000_000
out = sys.stdout.buffer

def entry(minute, second, fields):
    at = BASE + (minute * 60 + second) * 1_000_000
    out.write(b'__REALTIME_TIMESTAMP=%d\n' % at)
    out.write(b'__MONOTONIC_TIMESTAMP=%d\n' % (at - BASE + 1_000_000))
    out.write(b'_BOOT_ID=%s\n' % BOOT.encode())
    out.write(b'_HOSTNAME=web-01\n')
    out.write(b'_TRANSPORT=stdout\n')
    for k, v in fields.items():
        v = v.encode() if isinstance(v, str) else v
        if b'\n' in v:
            # The binary form, for a value with a newline in it.
            out.write(k.encode() + b'\n' + struct.pack('<Q', len(v)) + v + b'\n')
        else:
            out.write(k.encode() + b'=' + v + b'\n')
    out.write(b'\n')

# 09:00-09:59 — app.service: five errors and three warnings; sshd: one notice.
entry(0, 0, {'_SYSTEMD_UNIT': 'ssh.service', 'PRIORITY': '5', 'MESSAGE': 'Server listening on 0.0.0.0 port 22.'})
for i in range(5):
    entry(30 + i, 0, {'_SYSTEMD_UNIT': 'app.service', 'PRIORITY': '3', 'MESSAGE': 'queue worker crashed (%d)' % i})
for i in range(3):
    entry(40 + i, 0, {'_SYSTEMD_UNIT': 'app.service', 'PRIORITY': '4', 'MESSAGE': 'slow job %d' % i})
# The kernel: no unit, so it is counted nowhere.
entry(45, 0, {'_TRANSPORT': 'kernel', 'PRIORITY': '3', 'MESSAGE': 'EXT4-fs error (device sda1)'})

# 10:00-10:59 — nginx fails.
entry(65, 0, {'_SYSTEMD_UNIT': 'nginx.service', 'PRIORITY': '6', 'MESSAGE': 'starting nginx'})
entry(70, 0, {'_SYSTEMD_UNIT': 'nginx.service', 'PRIORITY': '7', 'MESSAGE': 'debug: worker table sized'})
entry(80, 0, {'_SYSTEMD_UNIT': 'nginx.service', 'PRIORITY': '3', 'MESSAGE': 'upstream failed: postgres://shop:hunter2@db.internal/shop'})
entry(81, 0, {'_SYSTEMD_UNIT': 'nginx.service', 'PRIORITY': '3', 'MESSAGE': 'auth token=s3cr3tvalue rejected'})
# Longer than journald's 512-byte threshold: stored compressed, so it is skipped and counted, never guessed.
entry(82, 0, {'_SYSTEMD_UNIT': 'nginx.service', 'PRIORITY': '4', 'MESSAGE': 'config dump: ' + 'abc ' * 300})
entry(83, 0, {'_SYSTEMD_UNIT': 'nginx.service', 'PRIORITY': '2', 'MESSAGE': 'emerg: bind() to 0.0.0.0:80 failed\n(98: Address already in use)'})
# systemd's own word about the unit: written by PID 1, so `_SYSTEMD_UNIT` is init.scope and `UNIT` names nginx.
entry(84, 0, {'_SYSTEMD_UNIT': 'init.scope', 'UNIT': 'nginx.service', 'PRIORITY': '4', 'MESSAGE': "nginx.service: Failed with result 'exit-code'."})
entry(90, 0, {'_SYSTEMD_UNIT': 'app.service', 'PRIORITY': '3', 'MESSAGE': 'queue worker crashed (5)'})
entry(91, 0, {'_SYSTEMD_UNIT': 'app.service', 'PRIORITY': '3', 'MESSAGE': 'queue worker crashed (6)'})
