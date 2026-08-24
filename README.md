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

It takes no instructions, executes nothing, and opens no ports: there is no listening socket, no remote
command channel, and the HTTP response is ignored beyond its status code. CI checks that mechanically on
every commit — see [the invariant job](.github/workflows/ci.yml).

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
| **Services** | the state of watched systemd units — *not implemented yet* |

Memory, disk space, load average and processes cannot be read from a cloud provider's API at all. They are
the reason this exists: a full disk and an out-of-memory kill are what actually take a server down, and no
provider API reports either.

## Install

```sh
curl -fsSL https://get.infranest.app/agent.sh | sh -s -- --token sat_YOUR_TOKEN
```

If you would rather not pipe a script into a shell — a reasonable position — verify it first:

```sh
curl -fsSLO https://get.infranest.app/agent.sh
curl -fsSLO https://get.infranest.app/agent.sh.sha256
sha256sum -c agent.sh.sha256
sh agent.sh --token-file /path/to/token
```

The installer is a thin wrapper: it detects your OS and architecture, downloads a **versioned** binary with
its checksum and signature, verifies both, refuses to continue if either fails, and installs a systemd
unit. The script is never the payload — a script that *is* the payload cannot be verified against anything.

Get a token from your server's page in InfraNest.

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
`Environment=` in the unit, which `systemctl show` exposes to any local user.

## Commands

| | |
|---|---|
| `infranest-agent print` | one collection cycle to stdout, sending nothing |
| `infranest-agent status` | what is collecting, what each collector last returned, when the last push succeeded, the last error |
| `infranest-agent flare` | a redacted bundle for a support ticket, with secrets stripped |
| `infranest-agent uninstall` | removes the unit, user, binary, config and state |
| `infranest-agent --version` | version, commit, build date |

## Where it sends

One destination: your InfraNest ingest host, over HTTPS. Nothing else — no CDN, no analytics, no error
reporting to a third party. Allowlist that one host and block the rest if you want to.

## Building it yourself

```sh
go build ./cmd/infranest-agent
go test ./...
```

No third-party dependencies. Everything here is the Go standard library, so the only supply chain is Go's.

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
