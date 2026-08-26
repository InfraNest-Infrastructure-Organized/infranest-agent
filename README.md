<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/assets/infranest-logo-dark.svg">
    <img alt="InfraNest" src=".github/assets/infranest-logo-light.svg" width="300">
  </picture>
</p>

<h1 align="center">infranest-agent</h1>

<p align="center">
  <strong>It only ever sends.</strong><br>
  No instructions, no execution, no open ports.
</p>

<p align="center">
  <a href="https://github.com/InfraNest-Infrastructure-Organized/infranest-agent/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/InfraNest-Infrastructure-Organized/infranest-agent/actions/workflows/ci.yml/badge.svg"></a>
  <img alt="Dependencies: none" src="https://img.shields.io/badge/dependencies-none-brightgreen">
  <img alt="Licence: Apache-2.0" src="https://img.shields.io/badge/licence-Apache--2.0-blue">
</p>

---

The [InfraNest](https://infranest.app) monitoring agent. It reads a handful of numbers from the machine it
runs on and posts them to your InfraNest account.

It takes no instructions, executes nothing, and opens no ports: there is no listening socket and no remote
command channel. CI checks that mechanically on every commit — see
[the invariant job](.github/workflows/ci.yml).

The one thing the server can influence is **where the next reading goes**. A reply may name a different
ingest URL, and the agent adopts it only if it is HTTPS and on the same registrable domain — so it can be
moved between our own hosts and cannot be pointed anywhere else by whoever manages to answer one push.
Everything else in a reply is read to be *reported*: how many readings landed, which were refused and why,
and how far this machine's clock is from ours. None of it runs anything.

## See exactly what it would send

Before you install anything, on your own machine, with your own data:

```sh
infranest-agent print
```

That runs one collection cycle and writes the exact JSON that *would* be posted to stdout — and sends
nothing. It is a better answer to "what does this collect?" than anything this README could claim, so it
is the first thing documented rather than a debugging flag buried at the bottom.

## What it collects

| | |
|---|---|
| **CPU** | user + system time, as a percentage. Deliberately **not** `100 - idle`, which counts steal (CPU the hypervisor gave another tenant) and iowait — on a shared vCPU those are most of a false alarm, and neither is load this machine can do anything about |
| **Memory** | used and total, plus swap. "Used" is total minus `MemAvailable`, not minus `MemFree` — counting the page cache as used makes a healthy Linux box look permanently full |
| **Disk space** | per mount: device, mount point, used, total |
| **Load average** | 1 / 5 / 15 minute |
| **Uptime** | seconds since boot |
| **Processes** | the largest few by memory, off by default. Sorted by memory rather than CPU because a CPU share needs two readings per process, which means walking all of `/proc` twice. **Arguments are omitted** — command lines routinely carry credentials, so the executable name is what gets sent unless you ask otherwise |
| **Services** | systemd units that were set up to run, and which of them have failed. Watched set is "enabled, plus anything currently failed", so it works with no configuration. Read over D-Bus; nothing here can start, stop or restart a unit |

A failed unit is worth its own line here because it is the failure that moves no number: a backup timer
that died leaves CPU, memory, disk and load exactly where they were, and the machine looks perfectly
healthy until somebody needs the backup.

Reading it needs the systemd D-Bus socket, which is the one place this agent talks to something other than
InfraNest. It cannot command systemd — `StartUnit` and its relatives are gated behind polkit and this
service has no session to authenticate with, so the restriction is the operating system's rather than a
promise about our code. CI fails the build if a unit-control method name ever appears in the source.
`INFRANEST_SERVICES=0` turns the collector off, and the socket is then never opened.

Every field, with its type, its bounds and what the server does with it, is in
**[PAYLOAD.md](PAYLOAD.md)** — a public contract rather than a description of this code, so anyone can
write another agent against it.

Memory, disk space, load average and processes cannot be read from a cloud provider's API at all. They are
the reason this exists: a full disk and an out-of-memory kill are what actually take a server down, and no
provider API reports either.

## Platforms

One static binary per architecture, built with `CGO_ENABLED=0`, so the same Linux build runs on glibc and
musl alike — Debian, Ubuntu, RHEL, Alpine, and anything else with a mounted `/proc`. Linux on amd64,
arm64, armv7, 386, riscv64, ppc64le and s390x; Windows on amd64 and arm64.

macOS and FreeBSD binaries are published too, and collect nothing — they build so the compiler and `go vet`
see that code on every commit, and running one tells you plainly that the platform is not implemented
rather than reporting a machine full of zeros. Do not install them expecting readings.

Windows collects CPU, memory, disk and uptime, and processes when asked. Two things Linux reports are
absent there, and are left absent rather than approximated:

- **Load average** does not exist on Windows. Processor Queue Length is sometimes offered as an
  equivalent and is not one — it counts waiting threads rather than a decaying average of runnable work,
  so a threshold carried over from Linux would mean something quite different. A missing value is
  excluded from an alert's window; a fabricated one would fire alerts nobody could interpret.
- **Swap**: Windows has a page file, and the arithmetic usually offered for "swap used" measures commit
  charge, which is a different thing.

## Install

> **Not yet.** There is no release to download, so the one-liners below do not work today. Everything
> else on this page is real and you can try it now — build from source and run `print`. This note goes
> when the first release does.

You need a **token** first. In InfraNest, open the server you want to monitor, go to its **Metrics** tab
and choose **Install agent**. It gives you a line to copy that already has the token in it.

### Linux

```sh
curl -fsSL https://get.infranest.app/agent.sh | sudo sh -s -- --token sat_YOUR_TOKEN
```

Prefer not to pipe a script into a shell? That is a reasonable position — download it, read it, then run it:

```sh
curl -fsSLO https://get.infranest.app/agent.sh
less agent.sh                       # it is about 200 lines
sudo sh agent.sh --token sat_YOUR_TOKEN
```

### Windows

In PowerShell, **as administrator**:

```powershell
irm https://get.infranest.app/agent.ps1 -OutFile agent.ps1
.\agent.ps1 -Token sat_YOUR_TOKEN
```

### Verifying a release before you install it

The installer already refuses to install a binary whose checksum does not match. If you want to check
more than that, each release carries a build attestation and is reproducible:

```sh
# Who built it — which commit, which workflow, which runner.
gh attestation verify infranest-agent_linux_amd64 \
  --repo InfraNest-Infrastructure-Organized/infranest-agent

# What it is. Clone the tag, build it, compare the hash to SHA256SUMS.
# This one needs no trust in us at all.
CGO_ENABLED=0 go build -trimpath -o /tmp/infranest-agent ./cmd/infranest-agent
```

There is no signing key anywhere — releases are signed with a short-lived certificate issued to the
release workflow and recorded in a public transparency log, so there is nothing to steal and a forged
release is attributable rather than deniable. Full instructions are on every release.

### What the installer does

No surprises, in this order:

1. Works out which build fits this machine
2. Downloads it, and **checks the checksum** — if that does not match it stops and installs nothing
3. Creates a user called `infranest-agent` with no login and no privileges (Windows: uses the built-in
   `LOCAL SERVICE` account)
4. Puts the token in a file only that user can read
5. Starts it, and sets it to start again after a reboot

It does not touch anything else, and it does not need to reach the internet again except to send readings.

### Check it worked

`status` needs `sudo`: the token file is readable only by the agent's own user, which is the point of it.
`print` does not — it reads the machine, not the configuration.

```sh
infranest-agent print          # exactly what this machine sends, printed instead of sent
sudo infranest-agent status    # what is being collected, and when the last send succeeded
```

### Remove it

Completely, leaving nothing behind:

```sh
sudo sh agent.sh --uninstall          # Linux
.\agent.ps1 -Uninstall                # Windows
```

### Keeping the token out of your shell history

The token is a credential, and a command line ends up in `~/.bash_history` and is visible in `ps` while it
runs. On a shared machine, put it in a file instead:

```sh
sudo sh agent.sh --token-file /root/token && shred -u /root/token
```

### Building it yourself

You need Go, and nothing else — there are no dependencies to fetch.

```sh
git clone https://github.com/InfraNest-Infrastructure-Organized/infranest-agent
cd infranest-agent
go build ./cmd/infranest-agent
./infranest-agent print
```

To install the binary you just built rather than a downloaded one:

```sh
sudo sh install.sh --from ./infranest-agent --token sat_YOUR_TOKEN
```

### If this machine does not use systemd

Alpine, and anything on OpenRC, runit or s6. The installer still installs the agent, tells you it could
not start it, and leaves you to wire it up. The command it needs to run is:

```sh
/usr/local/bin/infranest-agent run
```

Run it as the `infranest-agent` user, with `/etc/infranest/agent.conf` in its environment.

## How it runs

A dedicated unprivileged system user under a hardened systemd unit. It does not run as root: CPU, memory,
load, network and mount usage are all readable without it.

```
User=infranest-agent
NoNewPrivileges=yes
CapabilityBoundingSet=
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
RestrictAddressFamilies=AF_INET AF_INET6
MemoryMax=64M
CPUQuota=5%
```

The token lives in `/etc/infranest/agent.conf`, mode 0600, loaded with `EnvironmentFile=` — never
`Environment=` in the unit, which `systemctl show` exposes to any local user. On Windows the same file
lives in `%ProgramData%\InfraNest\agent.conf`, readable only by Administrators, SYSTEM and the account
the task runs as; the agent reads it directly there, because a scheduled task inherits no environment.

## A network problem costs nothing

Readings are collected on a fixed cadence and delivered separately. If a delivery fails — the network is
down, our end is down, a proxy is having a moment — the readings are **kept on disk** and sent when the
connection comes back, in one batch.

This matters more than it sounds. The readings that fail to send are, by definition, the ones from the
minutes something was wrong, and those are the minutes anyone will ask about afterwards. An agent that
drops them has dropped the useful part.

Nothing is deleted from the spool until InfraNest confirms it stored it. A request that merely *completed*
is not a delivery: a 502 from something in front of the API completes too.

```
Every 60s:   collect  →  write to /var/lib/infranest-agent/spool
             deliver whatever is waiting  →  on success, and only then, delete it
```

The spool holds about eight hours and then discards the oldest first. A monitoring agent must not become
the reason a server falls over, so it will not fill your disk waiting for us to come back.

## Settings

All optional except the token, and all set for you by the installer.

| | |
|---|---|
| `INFRANEST_TOKEN` | the server token. Required |
| `INFRANEST_URL` | where to send. Default `https://ingest.infranest.app` |
| `INFRANEST_INTERVAL` | how often to collect. Default `60s`, between `10s` and `5m` |
| `INFRANEST_STATE_DIR` | spool and state. Default `/var/lib/infranest-agent` |
| `INFRANEST_PROCESSES` | collect the busiest processes. Off by default |
| `INFRANEST_PROCESS_ARGS` | include full command lines. Off by default, and for good reason — see below |
| `INFRANEST_SERVICES` | watch systemd units and report the ones that have failed. **On** by default — see below |

`INFRANEST_URL` must be `https`. The token is a bearer credential and this is the wire it crosses; there
is no configuration for which plaintext is the right trade, so the agent refuses to start rather than
send it in the clear.

## Commands

| | |
|---|---|
| `infranest-agent print` | one collection cycle to stdout, sending nothing |
| `infranest-agent run` | collect and send continuously — this is what the service runs |
| `infranest-agent status` | whether readings are being delivered, and if not, why not |
| `infranest-agent flare` | a redacted bundle for a support ticket, with secrets stripped *(not yet)* |
| `infranest-agent uninstall` | removes the unit, user, binary, config and state *(not yet)* |
| `infranest-agent --version` | version, commit, build date |

`status` answers from what is on the machine and reaches nothing. That is deliberate: the question is
usually asked *because* the machine cannot reach us, and a diagnostic that has to reach us to run is no
use in the one case it exists for. It never prints your token, so it is safe to paste into a ticket.

```
$ infranest-agent status
Sending to:   https://ingest.infranest.app/api/metrics/push
Every:        1m0s
State in:     /var/lib/infranest-agent
Waiting:      2 reading(s) not yet delivered

FAILING — last delivery succeeded 4m12s ago, but the most recent attempt did not.
  Last error: cannot reach InfraNest: dial tcp: i/o timeout
  Readings are being kept and will be sent when this clears.
```

## Where it sends

One destination: your InfraNest ingest host, over HTTPS. Nothing else — no CDN, no analytics, no error
reporting to a third party. Allowlist that one host and block the rest if you want to.

**If that address ever has to change**, you should not have to visit your servers. Two things make that
true, and neither needs you:

- `ingest.infranest.app` is a name we intend never to change. What is behind it — host, region, provider —
  can move at any time, and DNS is what makes that invisible.
- If the *name itself* has to change, a push response can say so and the agent will follow it. It follows
  only over HTTPS and only to a host in the same domain it is already sending to, so this can move a fleet
  between our own hosts and cannot be used, by anyone who manages to answer a single push, to point your
  servers somewhere else for good. HTTP redirects are refused outright, for the same reason.

Nothing about this is hardcoded either: `INFRANEST_URL` is yours to set, and self-hosted installations
point it at their own address.

## Contributing

Building and testing it is covered under [Install](#building-it-yourself) above; the rules that shape the
code are in [CONTRIBUTING.md](CONTRIBUTING.md). The short version: it only sends, and it has no
dependencies — both are checked in CI, not just asked for.

## InfraNest

This agent is one piece of [InfraNest](https://infranest.app) — domains, DNS, cloud servers, certificates
and uptime monitoring in one place, across every provider.

- **[infranest.app](https://infranest.app)** — what the platform does
- **[infranest.app/docs](https://infranest.app/docs/)** — documentation and help centre
- **[dashboard.infranest.app](https://dashboard.infranest.app)** — sign in

You do not need an InfraNest account to read this code, and the agent is useful to look at either way: it
is a small, dependency-free example of reading `/proc` and `statfs` from Go.

## Licence

Apache 2.0. See [LICENSE](LICENSE).

Security policy: [SECURITY.md](https://github.com/InfraNest-Infrastructure-Organized/.github/blob/main/SECURITY.md).
