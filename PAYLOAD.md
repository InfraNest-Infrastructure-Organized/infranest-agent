# The push payload

What the agent sends, field by field, and what the server does with it.

This is a **public contract**. The agent is one implementation of it; anyone may write another, and the
endpoint does not care which is talking to it. So this document is the specification rather than a
description of our code, and where the two disagree the endpoint's validation is the authority.

**Contract version 17** — current as of the first agent release after `v0.15.0`.

Every change so far has been *additive*, and that is the rule rather than a run of luck: a field is added,
never repurposed, and never made required after the fact. A sender written against version 1 keeps working
against a version 3 endpoint, and a newer sender talking to an older endpoint has its unknown fields
ignored. If that ever has to break, this document gets a section for each shape and both are served for as
long as anyone is running the old one.

| Version | Added |
|---|---|
| 17 | `listening` and `collectors.listening` — every listening TCP port and bound UDP port, the address each is bound to, and for TCP how many distinct peers are connected by class (loopback, private, public). **Counts only — never a peer's address, never a program name** |
| 16 | `renewers[].installer` and `renewers[].hooks` — whether anything tells the web server about a certbot renewal: the installer plugin, or a deploy or post hook |
| 15 | `pressure_cpu`, `pressure_memory`, `pressure_memory_full`, `pressure_io`, `pressure_io_full` — how long work waited on each resource, from Linux pressure stall information |
| 14 | `system.raid[]` — Linux software RAID arrays and whether any is running degraded |
| 13 | `system.time_synced` — whether the kernel's clock is synchronised, as systemd-timedated reports it (v0.14.0 sent `time_error_ms` too, from `adjtimex`, and was killed by its own sandbox: InfraNest #4391) |
| 12 | `network_in_bps`, `network_out_bps`, `disk_read_bps`, `disk_write_bps` sent by the agent — throughput since the previous sample, which until now only a provider's metrics API supplied |
| 11 | `mounts[].inodes_used` / `inodes_total` — a filesystem that has run out of inodes while showing free space |
| 10 | `system.auto_update` — whether unattended-upgrades or dnf-automatic installs updates and reboots by itself, and when |
| 9 | `system.reboot_reason` and `system.newest_kernel` — why a reboot is required, now that it is reported on every Linux and not only Debian and Ubuntu |
| 8 | `renewers` and `collectors.renewers` — what renews this machine's certificates (certbot, Caddy, Traefik), which names, and certbot's last run |
| 7 | `system.cpus`, `system.pro` and `containers` — how many CPUs the kernel was given, whether Ubuntu Pro (Livepatch, ESM) is still attached, and the running containers with how often each has restarted |
| 6 | `system.oom_kills` and `system.boot_id` — how many processes the kernel has killed for lack of memory since this boot, and which boot that is |
| 5 | `collectors` — which optional collectors this agent has switched on |
| 4 | `services[].result`, `services[].exec_main_code` and `services[].exec_main_status` — why a failed unit failed, without its log output |
| 3 | `services[].restarts`, `services[].memory_bytes`, `state_changed_at` on every unit rather than only the failed ones, and `processes[].cpu_percent` / `processes[].started_at` actually being sent |
| 2 | `disk_usage.accounted_bytes`, `disk_usage.unreadable[]`, `disk_usage.more_unreadable`, the `too_frequent` skip reason, `min_interval_seconds` in the response, and the 8 MB body limit stated with the rule that snapshots ride the newest sample only |
| 1 | The batch envelope, samples, mounts, processes, services, system facts, `disk_usage` |

The quickest way to see it for real is on your own machine, with your own data:

```sh
infranest-agent print
```

That runs one collection cycle and writes exactly what would be posted, sending nothing.

## The request

```
POST /api/metrics/push
Authorization: Bearer sat_...
Content-Type: application/json
```

The token identifies **one server**. The payload never names which machine it came from, and it must not:
two machines sharing a token write into one series, and nothing detects it — the numbers become a blend of
two boxes and the agent-silence rule can never fire, because whichever machine is still up keeps checking
in for both.

Two shapes are accepted. A batch:

```json
{ "agent_version": "v0.1.0", "samples": [ { … }, { … } ], "disk_usage": { … }, "failed": { … } }
```

…or a single bare sample, which is normalised into a batch of one. The bare form exists because the
in-app install snippets post it, and it is answered in the shape it was sent: an error about `mounts` says
`mounts`, not `samples.0.mounts` — a field the sender did not write, in an envelope it does not know about.

At most **60 samples** per push.

## A sample

Every field is optional. **Absent and zero are different claims**, and the distinction is load-bearing
throughout: a missing value is excluded from an alert's window, while a zero is a measurement. Send
nothing rather than a guess.

| Field | Type | Notes |
|---|---|---|
| `collected_at` | RFC3339 | Defaults to arrival. Send it with an explicit offset or in UTC — the column has no zone, so a local offset lands hours from where it belongs |
| `cpu_percent` | 0–100 | **User + system.** Not `100 - idle`, which counts steal — CPU the hypervisor gave another tenant — and iowait. On a shared vCPU steal is routinely most of the apparent load, and it is not load this machine can act on |
| `memory_percent` | 0–100 | |
| `memory_used_bytes` | int | Total minus `MemAvailable`, not minus `MemFree`: counting the page cache as used makes a healthy Linux box look permanently full |
| `memory_total_bytes` | int | |
| `swap_used_bytes` | int | |
| `load_1`, `load_5`, `load_15` | float | Omit where the concept does not exist. Windows has no load average, and Processor Queue Length is not one |
| `uptime_seconds` | int | |
| `pressure_cpu` / `pressure_memory` / `pressure_io` | float 0–100 | Pressure stall information from `/proc/pressure/*`: the percentage of the last minute (`avg60`) in which **at least one** runnable task was stalled on that resource. Says how long work waited, which a load average — a count of tasks — cannot. Absent on kernels without PSI (before 4.20, or built without it) |
| `pressure_memory_full` / `pressure_io_full` | float 0–100 | The same for **all** runnable tasks stalled at once — time in which the machine did no useful work. CPU has none worth sending: system-wide it is always zero |
| `network_in_bps` / `network_out_bps` | int ≥0 | Bytes per second received and sent **since the previous sample**, from `/proc/net/dev`. Only interfaces backed by a device (`/sys/class/net/<if>/device`), so a bridge or veth pair does not count a container's traffic twice; where none is (inside a container), every interface but loopback. Absent on the first sample after the agent starts |
| `disk_read_bps` / `disk_write_bps` | int ≥0 | Bytes per second read and written since the previous sample, from `/proc/diskstats`, whole disks with a device link only — partitions, LVM, md, loop and zram are views of a disk already counted. Absent on the first sample, and where no such disk exists |
| `mounts[]` | array | Max 64 |
| `mounts[].mount_point` | string ≤191 | |
| `mounts[].device` | string ≤191 | |
| `mounts[].inodes_used` / `inodes_total` | int | From the same `statfs`. A filesystem out of inodes refuses every new file while its bytes still show free space. **Absent where the filesystem keeps no fixed inode table** (btrfs reports zero), which is not "none used" |
| `mounts[].used_bytes` / `total_bytes` | int | **No percentage.** The server computes it, so a percentage cannot disagree with its own bytes — which is what an alert would then fail to fire on |
| `processes[]` | array | Max 32 |
| `processes[].command` | string ≤255 | Executable name by default. Full command lines carry credentials often enough that shipping them must be asked for |
| `processes[].memory_bytes` | int ≥0 | Resident set size. What the list is ranked by |
| `processes[].cpu_percent` | 0–100 | Share of one core over the sampling interval, measured for the reported few only. **Clamped at 100**: a threaded process genuinely exceeds one core, and the endpoint validates this field as a percentage — a push carrying 340 fails validation for the whole batch |
| `processes[].pid` | int ≥0 | |
| `processes[].started_at` | RFC3339 | When the process started, from the machine's boot time plus its start offset. Absent rather than wrong where boot time could not be read: a date computed from a boot time we do not have is believed in a way an absent field is not |
| `services[]` | array | Max 200 |
| `services[].unit` | string ≤255 | |
| `services[].description` | string ≤255 | |
| `services[].active_state` | string ≤32 | systemd's own vocabulary — `active`, `inactive`, `failed`, `activating`, `deactivating` — sent verbatim rather than mapped. What "exited" means for a oneshot is a judgement the page makes with the whole picture |
| `services[].sub_state` | string ≤32 | |
| `services[].state_changed_at` | RFC3339 | When the unit entered its current state. This is what "failed 2 days ago" is read from, and what makes "changed in the last day" answerable — so it is sent for **every** unit, not only the failed ones |
| `services[].restarts` | int ≥0 | How many times the service manager has restarted this unit. Absent for units that cannot have it — a timer is not a service — rather than zero, because "never restarted" and "cannot restart" are different facts. A unit in a crash-restart loop reads as `active` every time anyone looks; this is the only field here that makes it visible |
| `services[].memory_bytes` | int ≥0 | What this unit's cgroup is using. Per *unit*, not per process: a service that forks twenty workers is twenty rows in `processes[]` and one number here. **Absent where the platform reports it as unknown** — systemd answers `(uint64) -1` for a unit with no memory accounting, and a sender that passes that through reports sixteen exabytes |
| `services[].result` | string ≤32 | Why the unit last finished the way it did, from a fixed vocabulary the service manager chooses from: `exit-code`, `signal`, `core-dump`, `timeout`, `watchdog`, `start-limit-hit`, `oom-kill`, `resources`, `protocol`. Sent **only for failed units**, and only when it is not `success` — a healthy unit would carry it on every row to say nothing happened. This is the answer to "why did it fail" that carries no log output: see [Why there is no log excerpt](#why-there-is-no-log-excerpt) |
| `services[].exec_main_code` | int | How the main process ended, as a POSIX `si_code`: 1 exited, 2 killed, 3 dumped. **Send it with `exec_main_status` or not at all** |
| `services[].exec_main_status` | int | The exit status, or the signal number when the code says killed. Meaningless alone: `0` is a clean exit beside code 1 and is not a status at all beside code 2. Both are absent together where the service manager has no record of a main process ending — which is a unit that failed before it ever started, and is a different fact from exiting with zero |
| `containers` | array, max 100 | Running containers, from systemd's `docker-<id>.scope` / `libpod-<id>.scope` units — **never the Docker socket**, which is root on the host. A snapshot like `services`: on the newest sample only. Absent when the agent could not ask systemd; `[]` means none that systemd can see, which includes every host on Docker's `cgroupfs` driver |
| `containers[].id` | string ≤64 | The short container ID (twelve hex digits). Names live in the runtime's own state, which only root can read; `docker ps --filter id=<id>` gives the name |
| `containers[].runtime` | string ≤32 | `docker` or `podman` |
| `containers[].active_state` | string ≤32 | The scope's systemd state |
| `containers[].started_at` | RFC3339 | When this incarnation started — the scope's `ActiveEnterTimestamp`. A restart is a new scope, so this moves on every restart |
| `containers[].restarts` | int ≥0 | Restarts this agent has seen since it began watching the container. **A counter and a lower bound**: counted once per collection, so several restarts between two readings count as one, and it starts at 0 when the agent starts — the receiver judges rises and treats a fall as a reset |
| `containers[].memory_bytes` | int ≥0 | The scope's cgroup memory. Absent where systemd keeps no accounting |
| `renewers` | array, max 50 | What renews certificates on this machine. A snapshot like `services`: on the newest sample only. Absent when the collector is off; `[]` means it looked and found nothing. Read **only from world-readable files** — see below |
| `renewers[].tool` | string | `certbot`, `caddy` or `traefik` |
| `renewers[].name` | string ≤255 | certbot's lineage (the renewal config's name), or the certificate's main name |
| `renewers[].domains` | array of string ≤253, max 100 | The names it renews — from the certificate when it could be read, otherwise from certbot's webroot map, otherwise the lineage name |
| `renewers[].domains_guessed` | bool | The names are the lineage name, not read from anything that lists them. A weaker claim, and the receiver treats it as one |
| `renewers[].not_after` | RFC3339 | The certificate's expiry, **only when its file could be read** — on most certbot installs `live/` is root-only and this is absent |
| `renewers[].source` | string ≤512 | The file this was read from, or the store that refused it |
| `renewers[].authenticator` | string ≤64 | certbot's: `webroot`, `nginx`, `apache`, `standalone`, `dns-…` |
| `renewers[].installer` | string ≤64 | certbot's installer — `nginx` or `apache` — which reloads that web server itself after a renewal. Absent where there is none (`installer = None`, or not set), the usual case with `webroot`, `standalone` and DNS authenticators |
| `renewers[].hooks` | array of string | certbot only: which kinds of hook run after renewing this certificate — `deploy` (the lineage's `renew_hook`, an executable file in `/etc/letsencrypt/renewal-hooks/deploy/`, or `deploy-hook`/`renew-hook` in `/etc/letsencrypt/cli.ini`) and `post` (`post_hook`, `renewal-hooks/post/`, `post-hook`). **Never the commands**, which can carry secrets. `[]` means every one of those places was read and none sets a hook: without an installer, a renewal then replaces the files and nothing tells the web server, which keeps serving the old certificate. **Absent is not `[]`**: a hooks directory or `cli.ini` that exists and refused the agent makes the answer unknown — unless the lineage sets a hook itself, which is known whatever the rest says. Hooks in root's own `~/.config/letsencrypt/cli.ini` are not readable and not seen |
| `renewers[].unit` | string ≤255 | The systemd timer that runs it. Absent for certbot from cron, and for Caddy, which renews in-process |
| `renewers[].last_run_at` | RFC3339 | When that timer last fired. **Absent is not "never"** — it is "systemd could not say" |
| `renewers[].last_result` | string ≤32 | How the renewal service last finished, systemd's own word: `success`, or `exit-code`, `timeout`, `signal`… — one `certbot renew` run renews every lineage, so this is every certbot certificate's result |
| `renewers[].next_run_at` | RFC3339 | When the timer fires next |
| `renewers[].unreadable` | bool | The store exists and refused the agent — Traefik's `acme.json` is always owner-only, Caddy's storage usually is. `source` names it; `mode` and `root_owned` say why, as in `disk_usage.unreadable[]`. Not a failure of the collector: it is the true answer to "can an unprivileged agent read this" |
| `listening` | object | What this machine accepts connections on — see [`listening`](#listening) below. A snapshot like `services`: on the newest sample only |
| `system.kernel` / `system.os` | string ≤128 | |
| `system.pending_updates` / `security_updates` | int | Absent means "could not tell", which is not zero |
| `system.reboot_required` | bool | A reboot is needed to finish applying updates. False where nothing says so, which is not a check that nothing is pending |
| `system.reboot_reason` | string ≤16 | Which evidence said so: `flag_file` (Debian/Ubuntu's `/var/run/reboot-required`), `zypper` (SUSE's `/run/reboot-needed`), `newer_kernel`, or on Windows `windows_update` / `component_servicing` (the registry's `RebootRequired` / `RebootPending` keys) — a newer build of the running kernel is installed under `/lib/modules`. That last is judged only when the running kernel is itself installed there (so never in a container or on an unmanaged kernel), only against the same flavour, and only for directories `depmod` has written. Absent unless `reboot_required` is true |
| `services[]` on Windows | | The automatic-start services from the Service Control Manager, in the systemd shape: `active`/`running`, `inactive`/`stopped` (stopped cleanly, or never started — many automatic services stop on purpose), `failed`/`stopped` with `result: exit-code`, `exec_main_code: 1` (exited) and the Win32 exit code in `exec_main_status` only for a stop with an error. A Win32 code is 32 bits, so `exec_main_status` is an int32 — not the byte a POSIX status is; receivers before InfraNest #3837 refused anything past ±256 |
| `system.newest_kernel` | string ≤128 | The newer installed kernel, when the reason is `newer_kernel` |
| `system.auto_update` | object | How the machine patches and reboots itself, from the tool's own world-readable configuration — APT's (`/etc/apt/apt.conf.d/*` in name order, then `/etc/apt/apt.conf`, a later setting replacing an earlier one) or dnf-automatic's (`/etc/dnf/automatic.conf`). **Absent where neither tool is installed, which is "a person does it", not "off".** The receiver uses it to judge a pending reboot against the machine's own schedule |
| `system.auto_update.source` | string | `unattended-upgrades` or `dnf-automatic` |
| `system.auto_update.enabled` | bool | It installs updates on its own. For unattended-upgrades, `APT::Periodic::Unattended-Upgrade` set and not `0`, and `APT::Periodic::Enable` not `0`; for dnf-automatic, a timer enabled that applies updates. False is "installed, never runs" |
| `system.auto_update.reboot` | bool | It reboots by itself when an update needs one (`Unattended-Upgrade::Automatic-Reboot`; dnf-automatic's `reboot = when-needed`/`when-changed`) |
| `system.auto_update.reboot_time` | string ≤32 | unattended-upgrades only, when `reboot` is true: `Automatic-Reboot-Time` as written — `HH:MM` in the machine's local time, or `now` (its default) |
| `system.auto_update.reboot_with_users` | bool | unattended-upgrades only, when `reboot` is true: `Automatic-Reboot-WithUsers` (default true). False blocks the reboot while anybody is logged in |
| `system.auto_update.utc_offset_minutes` | int | The machine's current offset from UTC, so `reboot_time` can be placed on the receiver's clock |
| `system.raid[]` | array ≤32 | Software RAID arrays from `/proc/mdstat` (world-readable). **Absent where there are none**, which is most machines |
| `system.raid[].name` / `level` / `state` | string | `md0`, `raid1`, `active` or `inactive` (assembled from too few members to run) |
| `system.raid[].devices` / `active` | int | Members the array is built for, and how many are in it and working — `[2/1]` is 2 and 1. Fewer active than devices is **degraded**. Absent for arrays with no redundancy (raid0, linear) |
| `system.raid[].failed` | int | Members marked `(F)` |
| `system.raid[].sync` / `sync_percent` | string / float | A `resync`, `recovery` (rebuilding onto a replacement), `reshape` or `check` in progress, and how far it is |
| `system.time_synced` | bool | Whether the kernel considers its clock synchronised — systemd-timedated's `NTPSynchronized`, which it computes from `adjtimex` (not `TIME_ERROR`, `STA_UNSYNC` clear). The kernel's own view, which chrony, ntpd and timesyncd all report into; asked over D-Bus because the unit forbids `adjtimex` itself. Rides on the services collector's switch. **A VM whose clock the hypervisor keeps (no NTP daemon) reads false while being right**, which is why the receiver alerts only for a machine it has seen synchronised. Absent where it cannot be asked — no systemd, no timedated, or `INFRANEST_SERVICES=0` |
| `system.time_error_ms` | int ≥0 | **No longer sent** (InfraNest #4391). Only `adjtimex` has the kernel's `maxerror`, and the unit forbids that syscall. The receiver still accepts it |
| `system.oom_kills` | int ≥0 | How many processes the kernel has killed for lack of memory **since this boot** — Linux's `oom_kill` in `/proc/vmstat`. A counter, not a level: `memory_percent` cannot see an OOM kill, because the kill is what frees the memory. The receiver alerts when it rises between pushes. Absent where the kernel keeps no such counter, which is not zero |
| `system.boot_id` | string ≤64 | An identifier that changes on every boot — Linux's `/proc/sys/kernel/random/boot_id`. **Send it with `oom_kills`.** Without it a reboot is indistinguishable from a counter that did not move, and the receiver will not judge a rise it cannot place in a boot |
| `system.cpus` | int ≥1 | CPUs the kernel will schedule this agent on. What the machine **has**, which after a resize is not what a provider's last sync says it bought — the receiver prefers this for capacity and keeps the provider's figure for the plan. Absent means "could not tell" |
| `system.pro` | object | Ubuntu Pro, from the client's world-readable cache at `/var/lib/ubuntu-advantage/status.json`. **Absent where there is no Pro client or its cache could not be read — which is not detached.** Only `attached: false` is a claim of detached, and the receiver alerts only when a machine that *was* attached makes it. Nothing else from the cache is sent: not the account, the contract or the machine id |
| `system.pro.attached` | bool | |
| `system.pro.livepatch` / `esm_infra` | string ≤32 | The client's own state word — `enabled`, `disabled`, `warning`, `n/a` — verbatim. Absent when the cache does not list the service, as an unattached machine's does not |
| `system.pro.expires` | RFC3339 | When the subscription ends. The cache is only as fresh as the client's last refresh, so a contract that lapsed since still reads `attached: true`; an expiry in the past is how the receiver sees through that. Absent unless attached |

## Alongside the samples

| Field | Notes |
|---|---|
| `agent_version` | string ≤32. Shown in the UI, so a fleet running a version with a known bug is visible rather than something to be discovered |
| `collectors` | object | Which optional collectors are on: `processes`, `process_args`, `services`, `renewers`, `listening`, all booleans. **Sent on every push, and never omitted when false** — the false is the answer that matters. Without it an absent `processes` array means either "switched off" or "nothing to report yet", and a receiver that guesses tells somebody their agent is misconfigured when it was installed a minute ago |
| `failed` | `{"collector": "reason"}`, max 32, reasons ≤255. Collectors that could not read what they were asked for. Reported rather than hidden — silently sending fewer fields looks identical to a machine that has less to say |
| `disk_usage` | One mount's directory breakdown. Rides on whichever push follows the walk — see below |

## `disk_usage`

A bounded walk of one mount, sent at most hourly and on whichever push happens to follow it. Absent from
nearly every push, and **absent is not empty**: absent means no walk has finished since the last one, while
an empty `dirs` means the walk ran and found nothing to report.

| Field | Type | Notes |
|---|---|---|
| `mount_point` | string ≤191 | Which mount was walked |
| `scanned_at` | RFC3339 | |
| `dirs[]` | array, max 16 | The largest directories found. Not a tree: bucketed at four levels deep, so there is no parent node above a reported path |
| `dirs[].path` | string ≤512 | Longer than a mount point on purpose — truncating a deep path names nothing |
| `dirs[].bytes` | int ≥0 | Apparent file sizes summed, not block accounting |
| `dirs[].kind` | string ≤64 | A plain-language name where the walker recognises the location — "docker images and containers". Absent rather than guessed: being wrong here has somebody delete the wrong thing |
| `partial` | bool | The walk ran out of time. Means *there may be more of the same* |
| `accounted_bytes` | int ≥0 | **Everything the walk summed**, including directories that did not make `dirs`. The server subtracts it from what `statfs` reports for the mount, and the difference is the honest size of what could not be seen. Summing `dirs` instead would put that remainder out by whatever lost the ranking |
| `unreadable[]` | array, max 16 | Directories the walk was refused. A different claim from `partial`: *there is definitely more, and here is where* |
| `unreadable[].path` | string ≤512 | |
| `unreadable[].mode` | string ≤8 | Octal as `ls -l` shows it — `0710`. `stat` needs execute permission on the *parent*, not the target, so a directory that cannot be listed can still be named |
| `unreadable[].root_owned` | bool | Owned by uid 0 — the difference between "this is how the distribution ships it" and "somebody's permissions are wrong" |
| `more_unreadable` | int ≥0 | Refusals collected but not named. **Send this.** A truncated list that looks complete is what made the first version of this feature misleading rather than merely incomplete |

**No size is ever attached to an unreadable directory.** There is no unprivileged way to learn the size of
a directory you cannot enter, so any number beside the path could only be invented. The subtraction above
is the honest form of that answer.

Rank before truncating. Refusals arrive in walk order, walk order is alphabetical, and on a stock Linux
host the first several are `/etc/credstore`, `/etc/ssl/private`, `/etc/sudoers.d` and `/lost+found` — a few
kilobytes between them — while `/var/lib/docker`, which is usually the answer, arrives long after a
first-come list is full.

## `listening`

What this machine accepts connections on, read from the kernel's socket tables — `/proc/net/tcp`, `tcp6`,
`udp` and `udp6`, which every user may read. It exists so a receiver can compare a firewall's open ports
with what actually listens behind them: a rule open for nothing, or a database listening on every address.

```json
"listening": {
  "ports": [
    { "protocol": "tcp", "port": 22,    "bind": "all",      "addresses": ["0.0.0.0", "::"],
      "peers": { "loopback": 0, "private": 4, "public": 2 } },
    { "protocol": "udp", "port": 51820, "bind": "all",      "addresses": ["::"] },
    { "protocol": "tcp", "port": 3306,  "bind": "specific", "addresses": ["10.0.0.10"],
      "peers": { "loopback": 0, "private": 0, "public": 0 } },
    { "protocol": "tcp", "port": 5432,  "bind": "loopback", "addresses": ["127.0.0.1", "::1"],
      "peers": { "loopback": 1, "private": 0, "public": 0 } }
  ],
  "more": 0,
  "partial": false
}
```

| Field | Type | Notes |
|---|---|---|
| `listening` | object | **Absent** where it was not read: the collector is off (`collectors.listening: false`), the platform has none (Windows — `collectors.listening` is `false` there too), `/proc/net/tcp` could not be read (then `failed.listening` says why), or the sender predates contract 17 (no `listening` key in `collectors` at all). **Present with `ports: []` means it read the tables and nothing listens** — a claim, and the receiver may act on it. Absent never means that |
| `ports[]` | array, max 100 | **One entry per protocol and port**, however many sockets and addresses hold it — see below. Ranked before truncating: bound to every address first, then to a specific address, then loopback only; then TCP before UDP; then by port. So what the cap drops, if anything, is what nobody outside can reach |
| `ports[].protocol` | string | `tcp` (a socket in `LISTEN`) or `udp` (a bound socket with no connected peer) |
| `ports[].port` | int 1–65535 | |
| `ports[].bind` | string | The **widest** of its addresses: `all` (`0.0.0.0` or `::`), `specific` (one of this machine's own addresses), or `loopback` (`127.0.0.0/8` or `::1`) |
| `ports[].addresses` | array of string ≤45, max 8 | The local addresses it is bound to, wildcards first, then IPv4 before IPv6, then numerically. This machine's own addresses — never a peer's. A port bound to more than eight drops the rest; `bind` is computed over all of them |
| `ports[].peers` | object | TCP only. **Distinct remote addresses** with an established connection to this port, counted by class: `loopback`, `private` (RFC 1918, RFC 4193 `fc00::/7`, CGNAT `100.64.0.0/10`, link-local `169.254.0.0/16` and `fe80::/10`) and `public` (everything else). Counts of addresses, not of connections: one client with twenty connections is one. A snapshot of this moment, not of the minute. Absent on UDP, which has no connections to count |
| `more` | int ≥0 | Ports read but not sent because of the cap. **Always sent**, so a truncated list cannot pass for a complete one |
| `partial` | bool | A socket table was longer than the agent reads (100,000 rows per table), or more than 65,536 distinct peers were connected. The kernel lists every listening TCP socket before any connection, so **the TCP ports are complete and the peer counts are lower bounds**; UDP has one list, and ports may be missing from it |

**How duplicates are folded.** The kernel lists sockets, and one service is often several: nginx with
`SO_REUSEPORT` holds one per worker, and `listen 80; listen [::]:80;` is one IPv4 and one IPv6 socket.
Those become one entry per protocol and port, with every distinct local address in `addresses`. An
IPv4-mapped address (`::ffff:a.b.c.d`) — how a dual-stack socket lists an IPv4 address — is reported as the
IPv4 address, for binds and for peers, so one client reaching a port over both tables counts once. Ports
are never folded across protocols: `tcp 53` and `udp 53` are two entries.

**What `::` cannot tell you.** A socket bound to `::` accepts IPv6 on every address and, unless the program
set `IPV6_V6ONLY`, IPv4 too. The socket tables do not record that option, so an entry whose addresses are
only `["::"]` may or may not be reachable over IPv4. Treat it as exposed on both.

**A UDP client looks like a server.** A program that sends from an unconnected UDP socket — rare, as most
resolvers connect theirs — is listed exactly like one waiting for packets, on a port from the ephemeral
range, for as long as the socket is open. It is reported rather than filtered: filtering the ephemeral
range would also hide WireGuard's 51820 and mosh's 60000s, which are exactly the ports worth seeing.

**One network namespace.** These are the sockets of the namespace the agent runs in — the host's, under
the shipped unit. A container with its own namespace is not seen, so a port Docker publishes is listed only
when something on the host listens for it (`docker-proxy`, the default); with Docker's userland proxy off,
the port is forwarded by the firewall and **nothing listens on the host**. A receiver must not call such a
port "open for nothing" on this evidence alone.

### No peer addresses

**A peer's address is never sent.** The machines connected to a server are its users and its customers'
users: personal data, and none of it is needed to judge a firewall rule. The agent reads each remote
address, classifies it in memory, and discards it; only the three counts leave the machine. A sender
implementing this contract must do the same — the receiver has no field to store an address in, and must
not grow one.

### No program names

**There is no field for which program owns a port, and a receiver must never show one it did not
observe.** The socket tables name a socket by inode; finding its process means reading `/proc/<pid>/fd`,
which the kernel allows only for the reader's own processes and root's own. An unprivileged agent would
know the owner of its own sockets and of nobody else's — a list that is right where it does not matter and
silent everywhere else. A receiver may show a guess from a well-known port ("5432 is usually PostgreSQL"),
**labelled as a guess**, and never as something the agent saw.

## Snapshots belong on the newest sample only

`services`, `processes`, `system`, `renewers` and `listening` describe **now**, not the moment a reading was taken. The server
applies them from the newest sample in the batch and ignores them on every other one — deliberately,
because applying each in turn would leave the disk card showing whichever happened to be last in the
payload: an hour-old state presented as current.

So sending them on every sample is sixty copies of one answer, and it is not free. At the caps this
document publishes:

| | |
|---|---|
| worst-case sample **with** snapshots | 180,119 bytes |
| worst-case sample, metrics only | 30,013 bytes |
| 180 samples all carrying snapshots | **32.4 MB** |
| 179 metrics-only + 1 with snapshots | **5.6 MB** |

**The endpoint accepts a request body of 8 MB.** The first shape does not fit and is rejected by the web
server *before* any of this validation runs — which means no per-reading verdict, so the batch is never
settled, so a store-and-forward sender keeps it and retries it for ever. The second fits with room.

`listening` arrived after these figures were measured and adds at most **46,441 bytes** to the one sample
that carries it (100 ports, each with eight IPv6 addresses and seven-digit peer counts) — still well inside
the limit on the second shape, and one more reason the first cannot be sent.

`mounts` is **not** a snapshot and must stay on every sample. It looks like one, but `worst_mount_percent`
is derived per reading and is what a disk rule averages over its window — a backfilled sample without
mounts is a hole in that series rather than a saving.

## Why there is no log excerpt

There is no field here for a unit's log output, and that is a decision rather than a gap.

Reading the journal would mean one of three things, and each costs more than it returns. Shelling out to
`journalctl` is not available: the agent's dependency graph is checked in CI on every platform to contain
nothing that can run a subprocess, and that check is the promise the agent is distributed on. Linking the
journal's own library needs cgo, which ends the static, dependency-free binary. Parsing the journal's
on-disk format means hand-writing a reader for a compressed, hash-indexed binary format — the most
security-sensitive code in the project, to read the file most likely to contain somebody's credentials.

And it would not answer the question. Log output is prose: it needs a size cap that truncates the one line
that mattered, a redaction pass that cannot know what a secret looks like in an arbitrary service's output,
and a decision to put customer credentials in a database. `result` is a word from a fixed list. It says
"killed by the OOM killer" or "start limit hit" the same way every time, so a rule can act on it, a filter
can find it, and it can be translated — none of which prose can do.

A future version may attach the last lines of output at the moment a unit enters `failed`, which is bounded
and needs no inbound channel. It still needs a journal reader, so it inherits the cost above, and it will
only be built if somebody asks for it.

## The lengths are not advisory

**A string over its limit fails validation for the whole push, not for that field.** A validation failure
carries no per-reading verdict, so the batch is never settled: a store-and-forward sender keeps it, retries
it for ever, and queues everything behind it. One long command line ends monitoring on that machine.

Clip before sending.

## Timestamps

Deliberately asymmetric, because the future and the past are different problems.

- **Ahead of the server** — refused beyond 300 seconds of skew. Nothing ages a sample parked in the future
  out of the window it poisons.
- **Behind** — accepted, and the point of the batch envelope: the minutes around a network problem are the
  minutes somebody will ask about. Backfill may not reach at or before the newest reading already stored,
  so a sender can fill its own silence but cannot paint over a period already measured.

## What comes back

```json
{
  "message": "Metrics recorded.",
  "accepted": 2,
  "skipped": [ { "index": 0, "reason": "already_covered", "collected_at": "…" } ],
  "server_time": "2026-08-26T08:19:51Z",
  "ingest_url": "https://ingest.infranest.io",
  "min_interval_seconds": 60
}
```

| Status | Meaning |
|---|---|
| `201` | At least one reading stored |
| `200` | Nothing stored, and that is fine — every reading was already held. A retry of a push whose response was lost is a sender that is up to date, not a failure |
| `403` + `reason: monitoring_not_activated` | Monitoring is switched off for this server. Durable, reversible, and nothing to do with the token — back off, keep sending later |
| `401`/`403` otherwise | The token is not valid, usually because it was deleted. Do not retry quickly |
| `422` | Understood and stored none of it |
| `429` | Rate limited |

**`skipped` is a verdict, not an error.** Every reason it can carry — `already_covered`, `too_old`,
`ahead_of_server_time`, `too_frequent` — is terminal: none comes good by being sent again, and holding one
blocks everything behind it. A sender must drop those readings.

`too_frequent` means the reading arrived faster than the plan stores them. It is not the sender doing
anything wrong — the server is keeping less than was offered — and a push whose readings are **all**
dropped that way answers `200`, not `422`. That is the ordinary case for a fast sender on a slow plan, and
answering it as a failure would have the sender keep those readings and offer them again for ever.

`server_time` lets a sender measure its own clock against the server's and say so, rather than having
every push refused for a reason only the response knows.

`min_interval_seconds` is how often the server will **store** a reading, from the plan. Absent when there
is no floor. Honouring it saves the request rather than only the write, which is the expensive half — but
adopt it **only downward**: a response saying "I keep one every five minutes" is a reason to send less
often, never a reason to send more. A response able to lower a sender's interval could make every agent
busier, which is the same shape as the redirect guard below.

`ingest_url` names where readings should go from now on. **A client should adopt it only over HTTPS and
only within the same registrable domain as where it is already sending** — that allows a fleet to be moved
between the operator's own hosts, and stops anyone who manages to answer a single push from redirecting a
customer's servers for good.
