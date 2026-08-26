#!/bin/sh
# Install the InfraNest monitoring agent.
#
# POSIX sh, not bash: Alpine and several minimal images ship no bash, and an installer that needs a shell
# the machine does not have is an installer that does not work on the machines most likely to be running
# a container.
#
# This script is a thin wrapper and never the payload. It downloads a versioned binary, verifies its
# checksum, and refuses to continue if that fails — a script that *is* the payload cannot be verified
# against anything.
set -eu

REPO="InfraNest-Infrastructure-Organized/infranest-agent"
BIN_DIR="/usr/local/bin"
CONF_DIR="/etc/infranest"
STATE_DIR="/var/lib/infranest-agent"
USER_NAME="infranest-agent"
SERVICE="infranest-agent"

VERSION="latest"
TOKEN=""
TOKEN_FILE=""
API_URL="https://ingest.infranest.app"
FROM_FILE=""
DO_UNINSTALL=0

usage() {
  cat <<'USAGE'
Install the InfraNest monitoring agent.

  install.sh --token sat_xxxxx

Options:
  --token <token>        the server token, from your server's page in InfraNest
  --token-file <path>    read the token from a file instead, so it never reaches your shell history
  --url <url>            where to send readings (default: https://ingest.infranest.app)
  --version <version>    install a specific version instead of the latest
  --from <path>          install a binary you already have, instead of downloading one
  --uninstall            remove the agent, its user, its config and its data
  --help                 show this

The agent only sends. It takes no instructions, runs no commands, and opens no ports.
USAGE
}

log()  { printf '  %s\n' "$*"; }
warn() { printf '  ! %s\n' "$*" >&2; }
die()  { printf '\nerror: %s\n' "$*" >&2; exit 1; }

# `shift 2` with only one argument left exits the shell immediately under `set -e`, with no output at
# all — so `--token` with a forgotten value looked like a silent crash rather than a missing value.
need_value() { [ $# -ge 2 ] || die "$1 needs a value"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --token)      need_value "$@"; TOKEN="$2"; shift 2 ;;
    --token-file) need_value "$@"; TOKEN_FILE="$2"; shift 2 ;;
    --url)        need_value "$@"; API_URL="$2"; shift 2 ;;
    --version)    need_value "$@"; VERSION="$2"; shift 2 ;;
    --from)       need_value "$@"; FROM_FILE="$2"; shift 2 ;;
    --uninstall)  DO_UNINSTALL=1; shift ;;
    --help|-h)    usage; exit 0 ;;
    *)            die "unknown option: $1 (try --help)" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "this needs root, to create a system user and a service. Try: sudo sh $0 ..."

# ── Uninstall ────────────────────────────────────────────────────────────────────────────────────────
if [ "$DO_UNINSTALL" -eq 1 ]; then
  echo "Removing the InfraNest agent."
  if command -v systemctl >/dev/null 2>&1; then
    systemctl stop "$SERVICE" 2>/dev/null || true
    systemctl disable "$SERVICE" 2>/dev/null || true
    rm -f "/etc/systemd/system/${SERVICE}.service"
    systemctl daemon-reload 2>/dev/null || true
  fi
  rm -f "${BIN_DIR}/infranest-agent"
  rm -rf "$CONF_DIR" "$STATE_DIR"
  if id "$USER_NAME" >/dev/null 2>&1; then
    userdel "$USER_NAME" 2>/dev/null || deluser "$USER_NAME" 2>/dev/null || true
  fi
  echo
  echo "Done. Nothing of the agent is left on this machine."
  exit 0
fi

# ── The token ────────────────────────────────────────────────────────────────────────────────────────
if [ -n "$TOKEN_FILE" ]; then
  [ -r "$TOKEN_FILE" ] || die "cannot read the token file: $TOKEN_FILE"
  TOKEN="$(cat "$TOKEN_FILE")"
fi
[ -n "$TOKEN" ] || { usage; die "a token is required — get one from your server's page in InfraNest"; }

# ── Which build ──────────────────────────────────────────────────────────────────────────────────────
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
[ "$os" = "linux" ] || die "this installer is for Linux. On Windows use install.ps1."

case "$(uname -m)" in
  x86_64|amd64)  arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  armv7l|armv7)  arch="arm" ;;
  i386|i686)     arch="386" ;;
  riscv64)       arch="riscv64" ;;
  *)             die "unsupported architecture: $(uname -m)" ;;
esac

echo
echo "Installing the InfraNest agent (linux/${arch})."
echo

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

if [ -n "$FROM_FILE" ]; then
  # Installing a binary you built yourself. Nothing to verify — you made it.
  [ -r "$FROM_FILE" ] || die "cannot read $FROM_FILE"
  cp "$FROM_FILE" "$tmp/infranest-agent"
  log "using $FROM_FILE"
else
  command -v curl >/dev/null 2>&1 || die "curl is required to download the agent"

  base="https://github.com/${REPO}/releases"
  if [ "$VERSION" = "latest" ]; then
    base="${base}/latest/download"
  else
    base="${base}/download/${VERSION}"
  fi

  name="infranest-agent_linux_${arch}"
  log "downloading ${name}"
  curl -fsSL "${base}/${name}" -o "$tmp/infranest-agent" \
    || die "download failed. If this is a new install, check that a release exists at ${base}"
  curl -fsSL "${base}/${name}.sha256" -o "$tmp/sha256" \
    || die "could not download the checksum — refusing to install something unverified"

  log "verifying the checksum"
  expected="$(cut -d' ' -f1 < "$tmp/sha256")"
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$tmp/infranest-agent" | cut -d' ' -f1)"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$tmp/infranest-agent" | cut -d' ' -f1)"
  else
    die "no sha256sum or shasum available — refusing to install something unverified"
  fi
  [ "$expected" = "$actual" ] || die "checksum mismatch. Not installing. Expected $expected, got $actual"
fi

# ── The unprivileged user ────────────────────────────────────────────────────────────────────────────
if ! id "$USER_NAME" >/dev/null 2>&1; then
  log "creating the ${USER_NAME} system user"
  # useradd on most distributions, adduser on Alpine. Neither exists everywhere, so try both.
  # `--user-group` explicitly: on distributions with USERGROUPS_ENAB no (SLES, openSUSE) useradd creates
  # no matching group, the chown below then fails under `set -e`, and systemd would refuse Group= anyway.
  useradd --system --user-group --no-create-home --shell /usr/sbin/nologin "$USER_NAME" 2>/dev/null \
    || { addgroup -S "$USER_NAME" 2>/dev/null || groupadd --system "$USER_NAME" 2>/dev/null || true
         adduser -S -D -H -G "$USER_NAME" -s /sbin/nologin "$USER_NAME" 2>/dev/null; } \
    || die "could not create the $USER_NAME user"
fi

# ── Files ────────────────────────────────────────────────────────────────────────────────────────────
log "installing to ${BIN_DIR}/infranest-agent"
install -m 0755 "$tmp/infranest-agent" "${BIN_DIR}/infranest-agent"

mkdir -p "$CONF_DIR" "$STATE_DIR"
chown "$USER_NAME":"$USER_NAME" "$STATE_DIR"
chmod 0750 "$STATE_DIR"

# The token goes in a file the agent user can read and nobody else can — never into the unit file, which
# `systemctl show` prints to any local user.
umask 077
cat > "${CONF_DIR}/agent.conf" <<CONF
# InfraNest agent configuration.
#
# Read by systemd through EnvironmentFile=, so this is a plain KEY=value file — no quotes, no shell
# expansion, one setting per line. Restart the service after editing:
#
#   sudo systemctl restart infranest-agent
#
# This file contains a credential. It is mode 0600 and owned by ${USER_NAME} on purpose, which is also
# why \`infranest-agent status\` needs sudo: as yourself you cannot read it.

# The server this agent reports as. Issued per server in InfraNest, and it identifies which one — so it
# must not be copied to a second machine. Two machines sharing a token write into one series, and
# nothing detects it: the numbers blend and the agent-silence rule can never fire, because whichever
# machine is still up keeps checking in for both.
INFRANEST_TOKEN=${TOKEN}

# Where readings go. The base URL only — the agent appends /api/metrics/push itself.
INFRANEST_URL=${API_URL}

# How often to collect. Between 10s and 5m; 60s if unset. Every reading is spooled to disk first, so a
# shorter interval costs bandwidth rather than data loss.
#INFRANEST_INTERVAL=60s

# The busiest few processes, by memory. Off by default: full command lines routinely carry credentials,
# and INFRANEST_PROCESS_ARGS is a second, separate decision for exactly that reason.
#INFRANEST_PROCESSES=1
#INFRANEST_PROCESS_ARGS=1

# systemd units that were set up to run, and which have failed. On by default — it carries no command
# lines, and a unit that has given up moves no metric at all, so a machine without this looks healthy
# while its backup timer is dead. Set to 0 and the agent never opens the D-Bus socket.
#INFRANEST_SERVICES=0

# The spool and the last-run record. Everything in it is disposable: losing it costs undelivered
# readings and nothing else.
#INFRANEST_STATE_DIR=/var/lib/infranest-agent
CONF
chown "$USER_NAME":"$USER_NAME" "${CONF_DIR}/agent.conf"
chmod 0600 "${CONF_DIR}/agent.conf"
log "wrote ${CONF_DIR}/agent.conf (0600, ${USER_NAME} only)"

# ── The service ──────────────────────────────────────────────────────────────────────────────────────
if command -v systemctl >/dev/null 2>&1; then
  log "installing the systemd service"
  # The unit that matches this binary, not whatever is on main — otherwise `--version v1.0.0` pairs an
  # old agent with a newer unit, and a rollback rolls back only half of the install.
  unit_ref="main"
  [ "$VERSION" = "latest" ] || unit_ref="$VERSION"
  curl -fsSL "https://raw.githubusercontent.com/${REPO}/${unit_ref}/packaging/infranest-agent.service" \
    -o "/etc/systemd/system/${SERVICE}.service" 2>/dev/null \
    || cp "$(dirname "$0")/packaging/infranest-agent.service" "/etc/systemd/system/${SERVICE}.service" 2>/dev/null \
    || die "could not install the service file"

  systemctl daemon-reload

  # Started, and then checked. Nothing here runs `run` to find out whether it works: `run` is a daemon
  # loop now, so a probe that waits for it to exit waits for ever — the installer would hang on the last
  # step with no output, which reads as a broken download. An earlier build did exactly this on purpose,
  # because back then `run` returned an error immediately; the check outlived the reason for it.
  #
  # Configuration errors are fatal inside the agent, so a service that comes up is one that read its
  # token and its URL. That is what makes `is-active` worth asking.
  systemctl enable --now "$SERVICE" >/dev/null 2>&1 || true

  if systemctl is-active --quiet "$SERVICE"; then
    log "service enabled and started"
  else
    warn "the service did not stay up. What it says:"
    systemctl status "$SERVICE" --no-pager --lines=10 2>&1 | sed 's/^/    /' || true
  fi
else
  warn "no systemd here. The agent is installed but nothing is running it."
  warn "Start it however this machine starts things:  ${BIN_DIR}/infranest-agent run"
  warn "On Alpine/OpenRC an init script goes in /etc/init.d/. See the README."
fi

echo
echo "Done."
echo
echo "See exactly what this machine will send, right now:"
echo "    ${BIN_DIR}/infranest-agent print"
echo
echo "Check it is working:"
# `sudo`, because agent.conf is 0600 and owned by the agent's own user — which is the point of it, and
# which makes `status` as yourself fail with a message about the token rather than about permissions.
echo "    sudo ${BIN_DIR}/infranest-agent status"
echo
echo "Remove it completely:"
echo "    sudo sh $0 --uninstall"
echo
