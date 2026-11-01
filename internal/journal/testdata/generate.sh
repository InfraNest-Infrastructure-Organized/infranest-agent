#!/usr/bin/env bash
# Regenerates the fixture journal files the tests read, with systemd's own writer — not ours — so the reader is
# tested against the format as systemd writes it. Run from anywhere with Docker:
#
#   internal/journal/testdata/generate.sh
#
# export.py writes a few dozen entries in journald's export format (fixed times, fixed boot id), and
# systemd-journal-remote (Ubuntu 24.04, systemd 255) turns them into a journal file twice: once in the default compact
# layout, once with SYSTEMD_JOURNAL_COMPACT=0 for the layout systemd before 252 writes. Messages over 512 bytes are
# stored compressed, which is what the "skipped, never guessed" test needs. Each file is cut at the end of its last
# object (the writer preallocates megabytes of zeros the reader never reaches) and gzipped.
#
# login.py writes the login fixture (InfraNest #5859): sshd, PAM, sudo and audit lines beside ordinary ones, in the
# compact layout only — the reader's handling of the two layouts is what the first two files test.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
python3 "$here/export.py" > "$here/entries.export"
python3 "$here/login.py" > "$here/login.export"
docker run --rm -v "$here:/fx" ubuntu:24.04 bash -euc '
  apt-get update -qq >/dev/null && apt-get install -y -qq systemd-journal-remote python3 >/dev/null
  rm -f /tmp/*.journal
  /usr/lib/systemd/systemd-journal-remote --split-mode=none --output=/tmp/compact.journal /fx/entries.export
  SYSTEMD_JOURNAL_COMPACT=0 /usr/lib/systemd/systemd-journal-remote --split-mode=none --output=/tmp/regular.journal /fx/entries.export
  /usr/lib/systemd/systemd-journal-remote --split-mode=none --output=/tmp/login.journal /fx/login.export
  for f in compact regular login; do
    python3 /fx/trim.py /tmp/$f.journal /fx/$f.journal
    gzip -9nf /fx/$f.journal
  done
'
rm -f "$here/entries.export" "$here/login.export"
