# infranest-agent

The [InfraNest](https://infranest.app) monitoring agent. It reads a handful of numbers from the machine it
runs on and posts them to your InfraNest account.

**It only ever sends.** It takes no instructions, executes nothing, and opens no ports. There is no
listening socket, no remote command channel, and the HTTP response is ignored beyond its status code.

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
| **Memory** | used, cached, swap |
| **Disk space** | per mount: device, mount point, used, total |
| **Load average** | 1 / 5 / 15 minute |
| **Uptime** | seconds since boot |
| **Processes** | the busiest few, by CPU and memory. **Arguments are redacted by default** — command lines routinely carry credentials |
| **Services** | the state of watched systemd units |

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

## Licence

Apache 2.0. See [LICENSE](LICENSE).

Security policy: [SECURITY.md](https://github.com/InfraNest-Infrastructure-Organized/.github/blob/main/SECURITY.md).
