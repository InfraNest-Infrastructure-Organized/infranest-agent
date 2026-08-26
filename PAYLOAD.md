# The push payload

What the agent sends, field by field, and what the server does with it.

This is a **public contract**. The agent is one implementation of it; anyone may write another, and the
endpoint does not care which is talking to it. So this document is the specification rather than a
description of our code, and where the two disagree the endpoint's validation is the authority.

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
| `disk_usage` | One mount's directory breakdown. Rides on whichever push follows the walk |

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
  "ingest_url": "https://ingest.infranest.app"
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
`ahead_of_server_time` — is terminal: none comes good by being sent again, and holding one blocks
everything behind it. A sender must drop those readings.

`server_time` lets a sender measure its own clock against the server's and say so, rather than having
every push refused for a reason only the response knows.

`ingest_url` names where readings should go from now on. **A client should adopt it only over HTTPS and
only within the same registrable domain as where it is already sending** — that allows a fleet to be moved
between the operator's own hosts, and stops anyone who manages to answer a single push from redirecting a
customer's servers for good.
