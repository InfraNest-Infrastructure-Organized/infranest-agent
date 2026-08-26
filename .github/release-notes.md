Built from this tag by GitHub Actions with `CGO_ENABLED=0 -trimpath` and the commit's own timestamp, then
built a second time and compared byte for byte.

## Verify what you are about to run

```
sha256sum -c infranest-agent_linux_amd64.sha256
```

That checks the download arrived intact. It does **not** prove the release is what the source says — the
binary and the checksum come from the same place, so anyone who could replace one could replace both.

## Check who built it

```
gh attestation verify infranest-agent_linux_amd64 \
  --repo InfraNest-Infrastructure-Organized/infranest-agent
```

Every artefact here carries a build attestation: which commit, which workflow, which runner. It is
signed without a key — a short-lived certificate issued to this workflow and recorded in a public
transparency log — so there is no signing key anywhere to be stolen or lost, and a forged release would
be attributable rather than deniable.

This proves who built the binary. It does not prove the source is what you think it is.

## Or rebuild it yourself, which does

```
git clone https://github.com/InfraNest-Infrastructure-Organized/infranest-agent
cd infranest-agent && git checkout __TAG__

CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=__TAG__ -X main.commit=$(git rev-parse HEAD) -X main.date=$(git log -1 --date=format:%Y-%m-%d --pretty=%cd)" \
  -o /tmp/infranest-agent ./cmd/infranest-agent

sha256sum /tmp/infranest-agent
```

The hash should match `SHA256SUMS`. If it does, the binary is the source — not because we say so, but
because you built it.

The agent has no third-party dependencies, so there is nothing else to audit: `go.sum` is empty, and CI
fails the build if it stops being.
