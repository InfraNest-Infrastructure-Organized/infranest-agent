#!/usr/bin/env bash
# The agent only sends (InfraNest #5801): nothing the service is built from can start a process or serve anything,
# `internal/jobrun` is the only package that imports os/exec, and only the command imports `internal/jobrun` — the
# wrapper a person puts in front of their own cron command. One script, called by both ci.yml and release.yml, so the
# rule a release enforces is the rule every commit was checked against (two copies drifted once and blocked v0.20.0).
set -euo pipefail
cd "$(dirname "$0")/.."
fail=0
module=github.com/InfraNest-Infrastructure-Organized/infranest-agent
service="./internal/agent ./internal/collect ./internal/push ./internal/spool ./internal/jobs ./internal/journal ./internal/config ./internal/dbus"
for os in linux windows darwin freebsd; do
  deps=$(GOOS=$os go list -deps $service)
  for pkg in os/exec net/rpc net/http/cgi net/http/fcgi "$module/internal/jobrun"; do
    if echo "$deps" | grep -qx "$pkg"; then
      echo "::error::$pkg is in the $os graph of the service. The agent only sends: it runs nothing and serves nothing."
      fail=1
    fi
  done
  binary=$(GOOS=$os go list -deps ./cmd/infranest-agent)
  for pkg in net/rpc net/http/cgi net/http/fcgi; do
    if echo "$binary" | grep -qx "$pkg"; then
      echo "::error::$pkg is in the $os dependency graph. The agent serves nothing."
      fail=1
    fi
  done
  importers=$(GOOS=$os go list -deps -f '{{.ImportPath}}: {{join .Imports " "}}' ./cmd/infranest-agent \
    | grep -E ' os/exec( |$)' | cut -d: -f1 | grep -vx "$module/internal/jobrun" || true)
  if [ -n "$importers" ]; then
    echo "::error::Only internal/jobrun may import os/exec ($os): $importers"
    fail=1
  fi
  wrappers=$(GOOS=$os go list -f '{{.ImportPath}}: {{join .Imports " "}}' ./... \
    | grep -E " $module/internal/jobrun( |$)" | cut -d: -f1 | grep -vx "$module/cmd/infranest-agent" || true)
  if [ -n "$wrappers" ]; then
    echo "::error::Only the command may import internal/jobrun ($os): $wrappers"
    fail=1
  fi
done
exit $fail
