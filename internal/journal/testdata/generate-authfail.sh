#!/usr/bin/env bash
# Regenerates the failed-login fixture (InfraNest #5868) with systemd's own writer, as generate.sh does for the others:
#
#   internal/journal/testdata/generate-authfail.sh
#
# authfail.py writes the entries; systemd-journal-remote (Ubuntu 24.04, systemd 255) writes them in the compact layout
# only — the reader's handling of the two layouts is what generate.sh's files test.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
python3 "$here/authfail.py" > "$here/authfail.export"
docker run --rm -v "$here:/fx" ubuntu:24.04 bash -euc '
  apt-get update -qq >/dev/null && apt-get install -y -qq systemd-journal-remote python3 >/dev/null
  rm -f /tmp/*.journal
  /usr/lib/systemd/systemd-journal-remote --split-mode=none --output=/tmp/authfail.journal /fx/authfail.export
  python3 /fx/trim.py /tmp/authfail.journal /fx/authfail.journal
  gzip -9nf /fx/authfail.journal
'
rm -f "$here/authfail.export"
