# Contributing

Thanks for looking. Bug reports, portability fixes and clearer docs are all welcome.

## The rule that shapes everything here

> **The agent only sends. It never receives instructions, never executes anything derived from a response,
> and exposes no local interface.**

A change that breaks that will be declined however useful it is, so it is worth knowing before you start.
In practice it means:

- The HTTP response is ignored beyond its status code. No server-driven configuration — "tell it to sample
  every 20s" sounds harmless, but a value from the server is a control channel, and *interval* is one
  review away from *which paths to walk*, which is arbitrary read.
- No `exec`, no subprocess, no shell.
- No listening socket, no local API, no IPC endpoint.

CI asserts the first two mechanically, so a change that breaks them fails rather than relying on a
reviewer noticing.

Monitoring agents have gone wrong here before: a widely used one shipped a "run this command" feature that
became a well-known remote-execution vector and was eventually disabled by default. That is the road, and
it always starts with a reasonable request.

## Also load-bearing

- **No third-party dependencies.** Standard library only. Every dependency is another maintainer whose
  compromise becomes our users'. If something seems to need one, open an issue first — the answer is
  usually that it does not.
- **Collect nothing you would not want published.** Process arguments are redacted by default because
  command lines carry credentials. New collectors get the same scrutiny: if it could contain a secret,
  redact it or leave it.
- **A failing collector must not take the agent down.** It reports itself failed and the others carry on.
  An agent that exits because it could not stat one mount takes the alerting with it.
- **Bounded.** Timeouts on every network call, caps on every collection, no unbounded buffers. This runs
  on other people's production servers and must be structurally incapable of being the reason one falls
  over.

## Working on it

```sh
go build ./cmd/infranest-agent
go test ./...
go vet ./...
gofmt -l .          # no output means formatted
```

Go and the standard library, nothing else to install.

`infranest-agent print` is the fastest way to see the effect of a change: it runs one collection cycle and
prints the exact payload without sending it.

## Pull requests

`main` is append-only and lands changes by squash merge, so **one pull request is one commit** in the
history. Please write the description as you would want to read it in a year: what changed, and why that
was the right call rather than an alternative.

CI runs build, tests, `vet`, formatting and the invariant checks on every pull request.

## Reporting a vulnerability

Not here — see the
[security policy](https://github.com/InfraNest-Infrastructure-Organized/.github/blob/main/SECURITY.md).
Please use private vulnerability reporting rather than a public issue.
