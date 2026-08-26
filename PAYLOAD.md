# The push payload

What the agent sends, field by field, and what the server does with it.

This is a **public contract**. The agent is one implementation of it; anyone may write another, and the
endpoint does not care which is talking to it. So this document is the specification rather than a
description of our code, and where the two disagree the endpoint's validation is the authority.

**Contract version 2** — current as of agent `v0.5.0`.

Every change so far has been *additive*, and that is the rule rather than a run of luck: a field is added,
never repurposed, and never made required after the fact. A sender written against version 1 keeps working
against a version 2 endpoint, and a version 2 sender talking to an older endpoint has its unknown fields
ignored. If that ever has to break, this document gets a version 3 section and both are served for as long
as anyone is running the old one.

| Version | Added |
|---|---|
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
| `mounts[]` | array | Max 64 |
| `mounts[].mount_point` | string ≤191 | |
| `mounts[].device` | string ≤191 | |
| `mounts[].used_bytes` / `total_bytes` | int | **No percentage.** The server computes it, so a percentage cannot disagree with its own bytes — which is what an alert would then fail to fire on |
| `processes[]` | array | Max 32 |
| `processes[].command` | string ≤255 | Executable name by default. Full command lines carry credentials often enough that shipping them must be asked for |
| `processes[].cpu_percent`, `memory_bytes`, `pid`, `started_at` | | |
| `services[]` | array | Max 200 |
| `services[].unit` | string ≤255 | |
| `services[].description` | string ≤255 | |
| `services[].active_state` | string ≤32 | systemd's own vocabulary — `active`, `inactive`, `failed`, `activating`, `deactivating` — sent verbatim rather than mapped. What "exited" means for a oneshot is a judgement the page makes with the whole picture |
| `services[].sub_state` | string ≤32 | |
| `services[].state_changed_at` | RFC3339 | When the unit entered its current state. This is what "failed 2 days ago" is read from |
| `system.kernel` / `system.os` | string ≤128 | |
| `system.pending_updates` / `security_updates` | int | Absent means "could not tell", which is not zero |
| `system.reboot_required` | bool | |

## Alongside the samples

| Field | Notes |
|---|---|
| `agent_version` | string ≤32. Shown in the UI, so a fleet running a version with a known bug is visible rather than something to be discovered |
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

## Snapshots belong on the newest sample only

`services`, `processes` and `system` describe **now**, not the moment a reading was taken. The server
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

`mounts` is **not** a snapshot and must stay on every sample. It looks like one, but `worst_mount_percent`
is derived per reading and is what a disk rule averages over its window — a backfilled sample without
mounts is a hole in that series rather than a saving.

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
  "ingest_url": "https://ingest.infranest.app",
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
