# The push payload

What the agent sends, field by field, and what the server does with it.

This is a **public contract**. The agent is one implementation of it; anyone may write another, and the
endpoint does not care which is talking to it. So this document is the specification rather than a
description of our code, and where the two disagree the endpoint's validation is the authority.

**Contract version 26** — current as of the first agent release after `v0.22.0`.

Every change so far has been *additive*, and that is the rule rather than a run of luck: a field is added,
never repurposed, and never made required after the fact. A sender written against version 1 keeps working
against a version 3 endpoint, and a newer sender talking to an older endpoint has its unknown fields
ignored. If that ever has to break, this document gets a section for each shape and both are served for as
long as anyone is running the old one.

| Version | Added |
|---|---|
| 26 | `journal.hours[].auth_failures` — how many times in the hour somebody tried to sign in and was refused, counted from the system journal's auth and authpriv lines (InfraNest #5868). **A number only**: the lines are read on the machine to be classified and are never sent. Same opt-in as the rest of `journal` |
| 25 | `journal` alongside the samples — whether the system journal is read here and why not, on every push from Linux; a unit's last lines, masked, once, when it fails, is killed for memory or starts restarting in a loop; and each hour's error and warning counts per unit (InfraNest #5848). **Read only when the machine's owner has added the agent's user to `systemd-journal`**, and never with `INFRANEST_JOURNAL=0`. **Login records are never sent**: a login service's event carries no lines and `lines_withheld: "login"`, and a login record is dropped from any unit's lines (InfraNest #5859, agent v0.23.0) |
| 24 | `system.setup` — how the machine itself is set up, from world-readable files only (InfraNest #5815): sshd's effective global login settings with the file and line that decided each, the members of the `sudo`, `wheel` and `admin` groups, and where automatic updates are switched on or off. Linux only; Windows sends nothing new |
| 23 | `cron[]` — what cron runs: the jobs in `/etc/crontab` and `/etc/cron.d`, and the scripts in the run-parts folders, with schedule, user, program and time zone (InfraNest #5801); `job_runs[]` alongside the samples — how each run of a job wrapped in `infranest-agent run --job` ended; `collectors.cron` and `collectors.job_reports` |
| 22 | `services[].package` — the dpkg package that installed each unit's file, matched on the file systemd loaded the unit from, and `services[].local` — a unit written on this machine (InfraNest #5789); `priority` and `section` on every package, `processes[].package` included |
| 21 | `processes[].package` — the dpkg package a process's executable belongs to, with its version and one-line description (InfraNest #5665); `processes[].services` and `services[].pid` — on Windows, which running services each process hosts, and the process each service runs in (#5670); `containers[].main_pid`, `main_process`, `main_command` and `ports[]` — what runs in each container, and the ports Docker publishes for it (#5710) |
| 20 | `disks[]` — read and write throughput and the share of time busy, **per whole disk** rather than summed (InfraNest #4922); `mounts[].disks` — the whole disks each filesystem is stored on, so a mount can be joined to its disk's activity |
| 19 | `listening.known_peers` — established connections to and from the addresses the receiver sent back as `peer_addresses`, the same organization's other servers, by address, port and direction (InfraNest #4903). Never any other address |
| 18 | `processes[].unit` / `container_id` — the systemd service or container each reported process belongs to (InfraNest #4917); `services[].last_trigger_at` / `next_elapse_at` — when a timer last fired and fires next (#4945) |
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
| `disks[]` | array, max 32 | The same whole disks one by one, since the previous sample — the totals above are these rows summed. **Absent on the first sample, and where no such disk exists**. Busiest first; past 32, the least busy are left out. A disk that appeared, vanished or whose counters went backwards since the previous sample is left out of that sample, as it is from the totals |
| `disks[].name` | string ≤32 | The kernel's name for the disk (`sda`, `nvme0n1`, `vda`) — the name `mounts[].disks` uses |
| `disks[].read_bps` / `write_bps` | int ≥0 | Bytes per second read and written since the previous sample |
| `disks[].busy_percent` | float 0–100 | The share of the interval the disk had at least one request in flight (`io_ticks` in `/proc/diskstats`), to one decimal. On a spinning disk 100 means saturated; **an SSD or NVMe disk serves many requests at once, so there 100 means never idle, not unable to take more** — a receiver judging saturation should read it beside `pressure_io`. Absent where the kernel's line carries no `io_ticks` |
| `mounts[]` | array | Max 64 |
| `mounts[].mount_point` | string ≤191 | |
| `mounts[].device` | string ≤191 | |
| `mounts[].inodes_used` / `inodes_total` | int | From the same `statfs`. A filesystem out of inodes refuses every new file while its bytes still show free space. **Absent where the filesystem keeps no fixed inode table** (btrfs reports zero), which is not "none used" |
| `mounts[].disks` | array of string ≤32, max 8 | The whole disks this filesystem is stored on, by the names `disks[]` uses (InfraNest #4922). The device above cannot be joined to a disk by its name — `/dev/mapper/ubuntu--vg-ubuntu--lv` and `/dev/root` name none, and an md mirror or a volume group spans several — so the agent follows the kernel's links in `/sys` (world-readable) from the filesystem's device number down through partitions, LVM, dm-crypt and md. For btrfs, whose subvolumes have anonymous device numbers, it starts from the device the mount names, which for a btrfs over several devices is only the one it was mounted from. **Absent where the filesystem is stored on no whole disk** — a network share, tmpfs, a loop device — or the links could not be followed |
| `mounts[].used_bytes` / `total_bytes` | int | **No percentage.** The server computes it, so a percentage cannot disagree with its own bytes — which is what an alert would then fail to fire on |
| `processes[]` | array | Max 32 |
| `processes[].command` | string ≤255 | Executable name by default. Full command lines carry credentials often enough that shipping them must be asked for |
| `processes[].memory_bytes` | int ≥0 | Resident set size. What the list is ranked by |
| `processes[].cpu_percent` | 0–100 | Share of one core over the sampling interval, measured for the reported few only. **Clamped at 100**: a threaded process genuinely exceeds one core, and the endpoint validates this field as a percentage — a push carrying 340 fails validation for the whole batch |
| `processes[].pid` | int ≥0 | |
| `processes[].started_at` | RFC3339 | When the process started, from the machine's boot time plus its start offset. Absent rather than wrong where boot time could not be read: a date computed from a boot time we do not have is believed in a way an absent field is not |
| `processes[].unit` | string ≤255 | The systemd service the process runs under (`php8.3-fpm.service`), from its world-readable `/proc/<pid>/cgroup`. Read for the reported processes only. Absent when the cgroup names no service — a login session, a kernel thread, or anything inside a container |
| `processes[].container_id` | string ≤64 | The short ID of the container the process runs in, from the same cgroup (`docker-<id>.scope`, `libpod-<id>.scope`, or `/docker/<id>` on the `cgroupfs` driver). Set instead of `unit`, never with it |
| `processes[].package` | object | The installed package the process's executable belongs to (contract 21). **Absent when it could not be placed on a file of an installed package, which is not "none"** — and always for a process in a container, whose files are its image's, not this machine's. Read from dpkg's world-readable database (`/var/lib/dpkg/status`, `/var/lib/dpkg/info/*.list`), never by running a package manager; the agent indexes the files under `bin`, `sbin` and `libexec` directories, and rebuilds that index only when the status file changes (at most every five minutes). RPM-based systems are not read yet |
| `processes[].package.name` / `version` | string ≤128 | dpkg's `Package` and `Version`, verbatim — `postgresql-16`, `16.4-0ubuntu0.24.04.2` |
| `processes[].package.description` | string ≤255 | The first line of the package's `Description:` — the packager's own one-line summary, in English |
| `processes[].package.source` | string | `dpkg`. Room for `rpm` without a new shape |
| `processes[].package.priority` / `section` | string ≤32 / ≤64 | dpkg's `Priority` (`required`, `important`, `standard`, `optional`, `extra`) and `Section` (`admin`, `universe/net`), verbatim (contract 22). The first three priorities are what a minimal Debian or Ubuntu system installs — what tells the base system from software somebody added. Absent when the package does not say |
| `processes[].package.match` | string | How the process was placed on the package's file, strongest first: `executable` — the kernel's link to the running file (`/proc/<pid>/exe`), readable only for the agent's own processes; `command` — the absolute path the process was started by, the first word of its command line, accepted only when its base name is the kernel's name for the process; `name` — the kernel's name for the process looked up in `/usr/local/sbin`, `/usr/local/bin`, `/usr/sbin`, `/usr/bin`, `/sbin` and `/bin`, accepted only when exactly one package installs a program of that name there. **The command line is read for its first word, in memory, and never sent** unless command lines are switched on, as `command` already says. A process running a file an upgrade has since replaced is not matched: the package now holds another version |
| `processes[].services` | array of string ≤128, max 16 | Windows only (contract 21): the display names of the running services this process hosts, whatever their start type, sorted. What tells one `svchost.exe` from thirty others — a process's command line is not readable on Windows without privilege. Absent where it hosts none |
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
| `services[].last_trigger_at` | RFC3339 | For a `.timer` unit only: when it last fired. Absent for a timer that has never fired, and for every other unit |
| `services[].next_elapse_at` | RFC3339 | For a `.timer` unit only: when it fires next, on the wall clock. Absent when it is not scheduled |
| `services[].pid` | int ≥1 | Windows only (contract 21): the process the service runs in while it is running, from the Service Control Manager's own answer. Joins a service to its row in `processes[]`. Absent when it is not running. The automatic-start services listed here are only some of those running; `processes[].services` names the rest |
| `services[].package` | object | The installed package the unit's file belongs to (contract 22), in the shape of `processes[].package` with `match` set to `unit_file`: the file systemd loaded the unit from — its `FragmentPath`, one property read over the same system bus connection, for the units being sent only — is one the package lists. What names a unit that is not running, a finished one-shot or a timer, by the package that installed it. The agent indexes the `.service` and `.timer` files directly in `/lib/systemd/system`, `/usr/lib/systemd/system`, `/etc/systemd/system` and `/usr/local/lib/systemd/system` from dpkg's file lists, in the index `processes[].package` uses. An instance (`getty@tty1.service`) is matched on its template's file. **Absent when it could not be placed, which is not "none"**: a unit file no installed package lists, one a generator wrote under `/run`, a transient unit with no file, a system without dpkg. Neither the unit file's path nor the unit's bus path is sent |
| `services[].local` | boolean | `true` when systemd loaded the unit from `/etc/systemd/system` or `/usr/local/lib/systemd/system` and no installed package owns that file — written on this machine, by an administrator or an installer script (contract 22). Absent otherwise, including when it could not be told; never `false` |
| `cron[]` | array | Max 200. What cron runs on this machine (contract 23): every job line in `/etc/crontab` and `/etc/cron.d/*`, and every script in `/etc/cron.hourly`, `daily`, `weekly` and `monthly` — files any user may read, on Debian and Ubuntu (cron) and the RHEL family (cronie) alike. **The users' own crontabs (`crontab -e`, `/var/spool/cron`) are never read**: only root may read them, and the agent does not run as root, so a receiver must say they are not covered rather than imply there are none. A line cron would refuse, and a file cron skips (Debian's cron reads only names of letters, digits, `_` and `-`), is left out. A snapshot: absent when the collector is off or the platform has no cron, `[]` when it looked and found none |
| `cron[].source` | string ≤255 | The file the job is defined in: `/etc/crontab`, `/etc/cron.d/<name>`, or the script itself for a run-parts folder |
| `cron[].line` | int ≥1 | The line in `source`. Absent for a run-parts script |
| `cron[].schedule` | string ≤64 | Five fields as written (`15 3 * * *`, `*/30 * * * mon-fri`) or a nickname (`@daily`, `@reboot`). For a run-parts script, the schedule of the line that runs its folder, or the folder's nickname when anacron runs it. Absent when nothing on the machine says when the folder runs |
| `cron[].user` | string ≤64 | The user it runs as. `root` for a run-parts script |
| `cron[].program` | string ≤128 | The program the line runs, by its file name — `backup.sh`, `sessionclean` — past conditions (`[ -x … ] &&`), assignments and wrappers (`nice`, `flock`, `timeout`); an interpreter is named by its script (`php /var/www/bin/report.php` is `report.php`). **Never its arguments** |
| `cron[].command` | string ≤1024 | The whole command, without what follows an unescaped `%` (cron's input). **Only when `collectors.process_args` is on** — a cron line is where the database password goes |
| `cron[].folder` | string ≤8 | `hourly`, `daily`, `weekly` or `monthly` for a run-parts script |
| `cron[].anacron` | bool | The folder is run by anacron, which picks the time inside its own window: `schedule` is a period, not a time |
| `cron[].timezone` | string ≤64 | The IANA zone `schedule` is read in: the file's `CRON_TZ`, else the machine's own (`/etc/localtime`, `/etc/timezone`). **Absent when neither says, which is not UTC** |
| `cron[].job` | string ≤128 | The name given to `infranest-agent run --job` when the line goes through it — what joins the line to its `job_runs[]`. A name somebody chose for the job, so sent whatever `process_args` says |
| `containers` | array, max 100 | Running containers, from systemd's `docker-<id>.scope` / `libpod-<id>.scope` units — **never the Docker socket**, which is root on the host. A snapshot like `services`: on the newest sample only. Absent when the agent could not ask systemd; `[]` means none that systemd can see, which includes every host on Docker's `cgroupfs` driver |
| `containers[].id` | string ≤64 | The short container ID (twelve hex digits). Names live in the runtime's own state, which only root can read; `docker ps --filter id=<id>` gives the name |
| `containers[].runtime` | string ≤32 | `docker` or `podman` |
| `containers[].active_state` | string ≤32 | The scope's systemd state |
| `containers[].started_at` | RFC3339 | When this incarnation started — the scope's `ActiveEnterTimestamp`. A restart is a new scope, so this moves on every restart |
| `containers[].restarts` | int ≥0 | Restarts this agent has seen since it began watching the container. **A counter and a lower bound**: counted once per collection, so several restarts between two readings count as one, and it starts at 0 when the agent starts — the receiver judges rises and treats a fall as a reset |
| `containers[].memory_bytes` | int ≥0 | The scope's cgroup memory. Absent where systemd keeps no accounting |
| `containers[].main_pid` | int ≥1 | The container's main process (contract 21): the lowest process ID in the scope's world-readable `cgroup.procs` (one cgroup down where the runtime nests them, as Podman does), past an init shim — `tini`, `docker-init`, `dumb-init`, `catatonit`. Absent when the cgroup could not be read |
| `containers[].main_process` | string ≤255 | That process's name, from `/proc/<pid>/stat` — the kernel's, at most 15 bytes. What names a container whose processes are too small to be among `processes[]` |
| `containers[].main_command` | string ≤255 | Its command line, **only when command lines are switched on** (`collectors.process_args`), clipped like a process's |
| `containers[].ports[]` | array, max 32 | The ports Docker publishes on the host for this container, read from the command lines of Docker's `docker-proxy` processes. Only the flags `-proto`, `-host-ip`, `-host-port`, `-container-ip` and `-container-port` are read, in memory — they hold addresses and port numbers and nothing else — and the command line itself is never sent, so this needs no switch. A proxy names its container by address, which is matched against the addresses the container's own network namespace lists as local (`/proc/<pid>/net/fib_trie` and `if_inet6`, world-readable like `/proc/net/tcp`); those addresses are not sent. **Absent is not "none published"**: with Docker's userland proxy off the firewall forwards the port and no process says so. By host port. An IPv4 and an IPv6 binding of one port are two entries |
| `containers[].ports[].protocol` | string | `tcp`, `udp` or `sctp` |
| `containers[].ports[].host_ip` | string ≤45 | The host address it is published on — `0.0.0.0`, `::`, or a specific one. Absent when the proxy names none |
| `containers[].ports[].host_port` / `container_port` | int 1–65535 | |
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
| `system.setup` | object | How the machine itself is set up (contract 24): read from `/etc/ssh`, `/etc/group`, `/etc/passwd`, APT's and dnf-automatic's configuration and dpkg's status database — files any user can read. Nothing is run: not `sshd -T`, not `getent`, not a package manager. Cached until one of the files read changes. **Linux only; absent on Windows and elsewhere, and absent when none of its parts could be read** |
| `system.setup.ssh` | object | sshd's **global** settings as sshd resolves them: `/etc/ssh/sshd_config` and every file it `Include`s, in place (relative paths under `/etc/ssh`, globs in lexical order), **the first value of a keyword winning** — which is why the drop-ins Debian and Ubuntu include at the top of the file beat what is written below. Keywords and `yes`/`no` are case-insensitive and sent in lower case. **Absent where there is no `/etc/ssh/sshd_config`** (no OpenSSH server) |
| `system.setup.ssh.password_authentication` / `kbd_interactive_authentication` / `use_pam` | string | `yes`, `no`, or **`unknown`**: a file sshd reads before the value is decided could not be read, so it may set it. Never sshd's default in that case. `ChallengeResponseAuthentication` is the old name of `KbdInteractiveAuthentication` and is read as the same setting. Not set anywhere: sshd's own default (`yes`, `yes`, `no`) |
| `system.setup.ssh.authentication_methods` | string ≤128 | `AuthenticationMethods` as written (`publickey`, `publickey,password publickey,keyboard-interactive`), `any` when not set, or `unknown` |
| `system.setup.ssh.permit_root_login` | string ≤128 | `yes`, `prohibit-password` (or its old name `without-password`), `forced-commands-only`, `no`, or `unknown`. Not set: `prohibit-password`, sshd's default |
| `system.setup.ssh.ports` | array of int, max 16 | Every `Port` line, which accumulate rather than override; `[22]` when there is none. **Absent when unknown** — any file that could not be read may add one. `ListenAddress` with a port is not read |
| `system.setup.ssh.set_in` | object of string ≤255 | Where each setting was decided, by its keyword in lower case (and `port`): `"/etc/ssh/sshd_config.d/50-cloud-init.conf:1"`, the file and line. A keyword not listed took sshd's default |
| `system.setup.ssh.match` | array of string | The keywords above that a `Match` block sets: for some users or addresses they differ from the global values. A `Match` holds to the end of the file it is in. The values in a `Match` block are not sent |
| `system.setup.ssh.unreadable` | array of string ≤255, max 100 | Files and folders sshd reads that the agent could not. One read inside a `Match` block leaves the global values known |
| `system.setup.admin_groups[]` | array, max 3 | The administrator groups this machine has — `sudo`, `wheel`, `admin`, in that order — each with `group` and `members` (sorted names, max 64): the group's list in `/etc/group` and the users whose primary group it is in `/etc/passwd`. Names only. A group the machine does not have is not listed; **absent when `/etc/group` could not be read**, which is not "no administrators" |
| `system.setup.updates` | object | Where automatic updates are decided, as evidence beside `system.auto_update`. Absent on a system that is neither Debian-like (`/etc/debian_version` or dpkg's database) nor RHEL-like (`/etc/redhat-release`) |
| `system.setup.updates.family` / `tool` | string | `debian` and `unattended-upgrades`, or `rhel` and `dnf-automatic` |
| `system.setup.updates.installed` | bool | Debian: dpkg's status database says the package is installed. RHEL: dnf-automatic's config file exists (the RPM database is not a format the agent reads). Absent when it could not be told |
| `system.setup.updates.setting` / `set_in` | string ≤128 / string | Debian: `APT::Periodic::Unattended-Upgrade` as APT resolves it — `/etc/apt/apt.conf.d` in name order, then `/etc/apt/apt.conf`, the **last** value winning — and the file that set it. RHEL: `apply_updates` from `[commands]`. Absent when nothing sets it, which for APT is off |
| `system.setup.updates.periodic_enable` | string ≤128 | Debian: `APT::Periodic::Enable` when something sets it; `0` switches every periodic task off |
| `system.setup.updates.timer` | string | RHEL: `install` (dnf-automatic-install.timer, which applies whatever the file says) or `plain` (dnf-automatic.timer, which applies only when `apply_updates` is on), from the symlinks `systemctl enable` makes. Absent when neither is enabled |
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
| `collectors` | object | Which optional collectors are on: `processes`, `process_args`, `services`, `renewers`, `listening`, `cron`, all booleans — and `job_reports` (contract 23), whether the folder `run --job` reports into exists on this machine. **Sent on every push, and never omitted when false** — the false is the answer that matters. Without it an absent `processes` array means either "switched off" or "nothing to report yet", and a receiver that guesses tells somebody their agent is misconfigured when it was installed a minute ago |
| `failed` | `{"collector": "reason"}`, max 32, reasons ≤255. Collectors that could not read what they were asked for. Reported rather than hidden — silently sending fewer fields looks identical to a machine that has less to say |
| `disk_usage` | One mount's directory breakdown. Rides on whichever push follows the walk — see below |
| `job_runs` | What `infranest-agent run --job` reported since the last push (contract 23) — see below. Absent when there is nothing |
| `journal` | Whether the system journal is read here, and what it said since the last push (contract 25) — see below. **Linux only: absent on every other system** |

## `job_runs`

Each run of a job somebody put behind the wrapper (contract 23):

```
15 3 * * * root infranest-agent run --job "Nightly backup" -- /opt/acme/backup.sh
```

The wrapper runs the command as the line's own user, passes its output through and exits with its code, and
leaves a small report in `/var/spool/infranest-agent` (mode `1733`: anyone may add a file, nobody but the agent
may list or read one). The agent sends what it finds there on its next push and deletes a report only once a push
carrying it is answered, so a report survives an outage; one not sent within a week is dropped. **Events, not a
snapshot**: a report is sent once more after a lost response, and the receiver keeps one per `id`.

| Field | Type | Notes |
|---|---|---|
| `job_runs[]` | array, max 50 | The oldest first; the rest wait for the next push |
| `job_runs[].id` | string, 32 hex | Random, chosen by the wrapper. What a report sent twice is recognised by |
| `job_runs[].job` | string ≤128 | The `--job` name — the same as `cron[].job` on its line |
| `job_runs[].started_at` / `ended_at` | RFC3339 | |
| `job_runs[].duration_ms` | int ≥0 | On the monotonic clock: a clock step during the run does not change it |
| `job_runs[].exit_code` | int 0–255 | The command's own. Killed by a signal: 128 + its number, as a shell reports it. Could not be started: 126, or 127 when not found |
| `job_runs[].signal` | string ≤16 | The signal that ended it |
| `job_runs[].error` | string ≤255 | Why it could not be started |
| `job_runs[].output` | array of string ≤512, max 20 | **Only when it failed**, and never with `--no-output`: its last lines, stdout and stderr in the order they came, with anything shaped like a password, token or key replaced by `[masked]` — the rules InfraNest applies to a ticket it files, and the receiver applies them again. Both ends are tested against one corpus of lines and expected results, [`testdata/masking-corpus.json`](testdata/masking-corpus.json) |
| `job_runs[].output_withheld` | bool | It failed, and `--no-output` kept its output on the machine |
| `job_runs[].user` | string ≤64 | Who ran it: **the report file's owner, read by the agent** — never what the report says about itself, because the folder is writable by anyone. A receiver should not believe a run reported for a job on another user's line |

## `journal`

Why a service failed, from the system log (contract 25). **Off until the machine's owner opts in**: journald's files
are readable by the `systemd-journal` group (and `adm` or `wheel` on some distributions, through an ACL), and the agent's
user is in none of them unless somebody runs

```
sudo usermod -aG systemd-journal infranest-agent && sudo systemctl restart infranest-agent
```

— undone by `sudo gpasswd -d infranest-agent systemd-journal` and a restart. The group grants reading the journal and
nothing else: no root, no capability, no command. It does grant reading *all* of it, which can name users and paths;
that is why it is not done by the installer. `INFRANEST_JOURNAL=0` keeps it off even with the group.

The agent reads the journal files itself — `/var/log/journal/*/system*.journal`, else `/run/log/journal` — with a small
reader of the documented file format. It starts no process (`journalctl` is not used) and links no library. A user's
own journal is never opened. It reads seven things from an entry: its time, `PRIORITY`, `MESSAGE`, the unit it is
about (`UNIT=` on systemd's own lines about a unit, else `_SYSTEMD_UNIT`), and — only to decide what is a login record —
`SYSLOG_FACILITY`, `SYSLOG_IDENTIFIER` and `_TRANSPORT` (the first two also count [failed logins](#failed-logins)). A message journald stored compressed (over 512 bytes, by
default) is **skipped and counted**, never decompressed or guessed at.

**Login records are never sent** (InfraNest #5859, agent v0.23.0). Masking looks for secrets; it does not touch a user
name or an address, and a login service's last lines are made of them (`Accepted publickey for jaspar from
203.0.113.7`). So, on the machine:

1. **A login service's lines are not read at all.** Its event is still sent — that it failed, and how — with
   `lines: []` and `lines_withheld: "login"`. A login service is one of these, matched on the unit's name without
   `.service` and without a template instance (a `*` is a prefix): `ssh`, `sshd`, `sshd-session`, `dropbear` (remote
   logins, and `ssh@`/`sshd@` per-connection instances); `systemd-logind`, `login`, `getty`, `serial-getty`,
   `console-getty`, `container-getty`, `autovt` (console and session logins); `su`, `sudo`; `polkit`, `polkitd`;
   `gdm*`, `lightdm`, `sddm`, `xrdp*` (graphical and remote-desktop logins); `cockpit*` (the web console);
   `sssd*`, `winbind`, `nslcd` (directory logins); `fail2ban`, `crowdsec`, `sshguard` (their log is the addresses that
   tried to log in). Not listed, on purpose: cron, and servers that authenticate their own clients (mail, FTP) — their
   own lines are why they failed; what they write as login records is dropped by the next rule.
2. **A login record is dropped from every other unit's lines**, before its message is read: a line written to the
   `auth` or `authpriv` syslog facility (`SYSLOG_FACILITY` 4 or 10 — where PAM, sshd, sudo, su and login write, so
   cron's `pam_unix(cron:session)` lines go), a line whose `SYSLOG_IDENTIFIER` is a login program (`sshd`,
   `sshd-session`, `sshd-auth`, `dropbear`, `sudo`, `su`, `login`, `systemd-logind`, `polkitd`,
   `polkit-agent-helper-1`, `unix_chkpwd` — sshd run with `-e` in a container logs to stderr with no facility), and
   the kernel's audit records (`_TRANSPORT=audit`).
3. **A login service's per-connection instance is named by its template**: `sshd@3-10.0.0.1:22-203.0.113.7:51234.service`
   carries the peer's address, so `journal` names it `sshd@.service`, in its event and in the hourly counts.

The hourly counts are numbers per unit and still count these lines, and an hour's [failed logins](#failed-logins) are counted from the auth lines before they are dropped — a number, never the line.

| Field | Type | Notes |
|---|---|---|
| `journal.readable` | bool | Whether the journal could be read on this push. **Sent on every push from Linux, never omitted when false** — it is what a receiver says Off or On from |
| `journal.reason` | string | `ok`; `off` (`INFRANEST_JOURNAL=0`); `no_permission` (the files are there and the agent may not read them — the opt-in has not been made, or the agent has not been restarted since); `no_journal` (no system journal on disk: `Storage=none`, or no journald); `unknown_format` (a file this agent cannot read — a newer format, or a damaged header) |
| `journal.events[]` | array, max 10 | A unit's last lines, **once per event**. Absent when there are none, and when the journal is not read |
| `journal.events[].unit` | string | A `.service` unit. A login service's template instance is sent as its template (`sshd@.service`) |
| `journal.events[].event` | string | `failed` — it entered `failed` (told again only after it recovered and failed anew); `oom_kill` — it failed with systemd's result `oom-kill`; `restart_loop` — its restart counter rose three times within a day (then not again for that unit for six hours) |
| `journal.events[].at` | RFC3339 | When it failed (systemd's state change), or when the loop was seen |
| `journal.events[].lines[]` | array, max 40 | Its last 20 entries at priority 6 (info) or more urgent, from the hour before `at` up to a minute after, oldest first — a message holding several lines is one line each. **Masked** with the rules `job_runs[].output` uses; the receiver masks again. `[]` when the journal held nothing from the unit in that hour |
| `journal.events[].lines[].at` | RFC3339 | The entry's time |
| `journal.events[].lines[].priority` | int 0–7 | syslog's: 0 emerg … 3 err, 4 warning, 5 notice, 6 info. An entry with none is sent as 6, journald's own default |
| `journal.events[].lines[].message` | string ≤300 bytes | Control characters removed, invalid UTF-8 replaced |
| `journal.events[].compressed` | int | Entries in the window whose message was stored compressed and not read |
| `journal.events[].truncated` | bool | A line was cut at 300 bytes, a message at 2 KB, or the oldest lines were left out to keep the event within 6 KB |
| `journal.events[].lines_withheld` | string | `login` — the unit is a login service (above): `lines` is `[]` because its lines are login records, and the agent did not read them. Absent otherwise. Agent v0.23.0 and later; an older agent never sends it |
| `journal.hours[]` | array, max 6 | Each complete hour's counts, sent on the first push after the hour ends (and the hour before the agent started, on its first push). Kept until a push lands |
| `journal.hours[].hour` | RFC3339 | The hour's start, UTC |
| `journal.hours[].units[]` | array, max 20 | The units that logged at priority 4 or more urgent in the hour, the most errors first. `[]` for a quiet hour |
| `journal.hours[].units[].unit` | string ≤256 | |
| `journal.hours[].units[].errors` / `warnings` | int | Priority 0–3, and priority 4 |
| `journal.hours[].more` | int | Units left out past the twenty |
| `journal.hours[].partial` | bool | The hour had more than 200,000 entries; the counts are of its newest 200,000 |
| `journal.hours[].auth_failures` | int | Refused login attempts in the hour (contract 26) — see [Failed logins](#failed-logins) below. **Always sent, `0` included**; absent only from an agent before v0.23.0, which does not count them |

**What is never sent**: the log itself, any line from a unit that did not fail or loop, a line at debug priority, a
user's journal, a login record, or any field of an entry beyond the message, its time and priority, and its unit.

### Failed logins

`journal.hours[].auth_failures` (contract 26, agent v0.23.0) is how many times in the hour somebody tried to sign in to
the machine and was refused. It is a count and nothing else: no user name, no address, no program, no line. To make it,
the agent reads the message of each entry written to the `auth` or `authpriv` syslog facility (`SYSLOG_FACILITY` 4 or
10, where sshd, dropbear and PAM write on every distribution), decides whether it is a refusal, and drops it. No other
entry is read for this.

One attempt is one count:

| Counted | Written by | Why this line and not its neighbours |
|---|---|---|
| `Failed password for …`, `Failed keyboard-interactive/pam for …` | sshd (`SYSLOG_IDENTIFIER` `sshd`, `sshd-session` or `sshd-auth`) | A password tried against an account that exists |
| `Invalid user … from …` | sshd | A connection naming an account that does not exist — once per connection, so its `Failed password for invalid user …` lines are not counted again |
| `Bad password attempt for …`, `Login attempt for nonexistent user …` | dropbear | dropbear's wording for the same two |
| `pam_<module>(<service>:auth): authentication failure` | PAM, for any service **but sshd** | A password refused by sudo, su, login, the web console. For sshd the line above already counted the attempt |

Not counted: `Failed publickey` (a client offering its keys in turn is how key login works), `Failed none` (a probe for
which methods are offered), sudo's `N incorrect password attempts` summary and su's `FAILED SU` (PAM's line already
counted those), and sshd's wording from any other program or on any other facility. A program that words a refusal
differently is missed rather than over-counted. Like the other hourly counts, it covers at most the hour's newest
200,000 entries (`partial`).

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
| `known_peers` | array, max 200 | Established TCP connections to and from the addresses the **receiver** named in its last `peer_addresses` (see [What comes back](#what-comes-back)) — the organization's other servers — and to no other address. **Absent** when the agent holds no list: a receiver that sends none, or the first reading after the agent starts. `[]` when it holds a list and none of them is connected. Most connections first |
| `known_peers[].address` | string ≤45 | One of the addresses the receiver sent, IPv4-mapped addresses unmapped |
| `known_peers[].port` | int | The port that names the service: this machine's own listening port for `in`, the peer's port for `out`. The other side's ephemeral port is not sent |
| `known_peers[].direction` | string | `in` — the peer connects to a port this machine listens on; `out` — this machine connects to the peer |
| `known_peers[].connections` | int ≥1 | Established connections right now. A pool of twenty to one database is twenty |
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

**A peer's address is never sent, unless the receiver named it.** The machines connected to a server are its users and its customers'
users: personal data, and none of it is needed to judge a firewall rule. The agent reads each remote
address, classifies it in memory, and discards it; only the three counts leave the machine. A sender
implementing this contract must do the same.

The one exception is `known_peers` (contract 19). It reports only addresses the receiver itself sent as
`peer_addresses`: the same organization's other servers, which the receiver already holds. Matching against
that list in memory tells the receiver which of its own servers talk to each other, and nothing about
anyone else. A connection to any address not on the list is counted by class as before and never named.

### No program names

**There is no field for which program owns a port, and a receiver must never show one it did not
observe.** The socket tables name a socket by inode; finding its process means reading `/proc/<pid>/fd`,
which the kernel allows only for the reader's own processes and root's own. An unprivileged agent would
know the owner of its own sockets and of nobody else's — a list that is right where it does not matter and
silent everywhere else. A receiver may show a guess from a well-known port ("5432 is usually PostgreSQL"),
**labelled as a guess**, and never as something the agent saw.

## Snapshots belong on the newest sample only

`services`, `processes`, `system`, `renewers`, `listening` and `cron` describe **now**, not the moment a reading was taken. The server
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

`disks` and `mounts[].disks` (contract 20) are measurements too and ride on every sample. At their caps —
32 disks with 32-byte names and ten-digit rates, and eight such names on each of 64 mounts — they add
**22,026 bytes** to a sample. A full batch of 60 at every cap, snapshots and `listening` on the newest, is
then about **3.3 MB**: still well inside the limit.

Contract 21 adds to the snapshot sample only. At the receiver's caps — 32 processes each with a package of
maximum-length strings and sixteen 128-byte service names, and 100 containers each with a command line and 32
IPv6-bound ports — it is **478,588 bytes**, about 3.8 MB for a full batch: still inside the limit. The agent
itself reports ten processes, and a real Windows process hosts a handful of services, so the ordinary figure is
a few kilobytes.

Contract 22 adds to the snapshot sample only. At the receiver's caps — 200 services each with a package of
maximum-length strings and the local mark, and a priority and section on each of 32 processes' packages — it is
**150,936 bytes**, a little over 4 MB for a full batch with the contract-21 figure: still inside the limit. A real
server sends seventy-odd units with packages of a few dozen bytes each.

Contract 23 adds `cron[]` to the snapshot sample — at its caps, 200 jobs each with every string at its limit, it is
**376,200 bytes** — and `job_runs` beside the samples, at most **552,800 bytes** (50 runs of 20 maximum-length
lines). Both together still leave a full batch well inside the limit. A real server has a dozen cron lines, and a
report from a successful run carries no output at all.

Contract 26 adds one integer per hour to it. Contract 25 adds `journal` beside the samples. At its caps — 10 events of 40 lines of 300 bytes, and 6 hours of 20
units with 256-byte names — it is under **150 KB**; the agent's own 6 KB budget per event keeps a real one far below
that, and a push with nothing to tell carries `{"readable": true, "reason": "ok"}`.

Contract 24 adds `system.setup` to the snapshot sample. At its caps — 16 ports, 100 file names in `unreadable`, three
groups of 64 names — it is under **30 KB**; a real machine's is a few hundred bytes.

`mounts` is **not** a snapshot and must stay on every sample. It looks like one, but `worst_mount_percent`
is derived per reading and is what a disk rule averages over its window — a backfilled sample without
mounts is a hole in that series rather than a saving.

## Why there was no log excerpt, and what changed

**Until contract 25** there was no field for a unit's log output, and the reasoning below was the decision. It is kept
because contract 25 answers each point rather than overruling it.


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

`job_runs[].output` (contract 23) is not a journal excerpt and does not change this. It is the last lines a command
printed *to the wrapper somebody put in front of it*, held in the wrapper's memory, masked before they are written,
and sent only when that run failed — no journal is read, and nothing runs that the line's owner did not write.

Contract 25 attaches the last lines at the moment a unit enters `failed` — bounded, and needing no inbound channel —
because customers asked for the *why*. What it does about each cost:

- **No subprocess, no cgo**: the agent reads the files with a reader of the documented format in Go, kept small on
  purpose — no hash tables, no decompression, every offset checked against the file before it is read, tested against
  files systemd's own writer produced in both of its layouts and against those files cut and damaged.
- **The file most likely to hold credentials**: it is not readable by the agent until the owner adds it to the
  `systemd-journal` group, and the owner is told what that group can read before doing it.
- **Prose, redaction, storage**: only a failed or looping unit's last lines leave, masked with the job output's rules
  (and masked again on arrival); InfraNest keeps them 30 days. `result` stays the field a rule acts on — the lines are
  for the person reading the card.

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
  "min_interval_seconds": 60,
  "peer_addresses": ["10.0.0.5", "203.0.113.20"]
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

`peer_addresses` are the addresses of the organization's **other** servers, public and private, at most
4,096 (contract 19). The sender keeps the latest list in memory and reports connections to them, and only
them, in `listening.known_peers`. Absent from a receiver that does not ask for it; the sender then sends no
`known_peers`. An empty list means the organization has no other servers.

`ingest_url` names where readings should go from now on. **A client should adopt it only over HTTPS and
only within the same registrable domain as where it is already sending** — that allows a fleet to be moved
between the operator's own hosts, and stops anyone who manages to answer a single push from redirecting a
customer's servers for good.
