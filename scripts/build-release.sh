#!/usr/bin/env bash
# Build every published target into dist/ (or the directory given). The one definition of "the release
# build": release.yml runs it to make the release, and runs it again on a clean runner to check the result
# is reproducible (InfraNest #5969). Two copies of the flags would drift, and a drift between them reads as
# a build that is not reproducible.
#
# `-trimpath` removes the build machine's directory layout from the binary, and CGO_ENABLED=0 means no
# libc is linked in. Together with a fixed SOURCE_DATE_EPOCH they make the output a function of the source
# and the Go version alone — which is the difference between "trust our release" and "check it yourself".
# GOTOOLCHAIN=local: the Go that was set up is the Go that builds, never one fetched because of go.mod.
set -euo pipefail
: "${VERSION:?}" "${COMMIT:?}" "${DATE:?}" "${SOURCE_DATE_EPOCH:?}"
cd "$(dirname "$0")/.."
out="${1:-dist}"
export CGO_ENABLED=0 GOTOOLCHAIN=local SOURCE_DATE_EPOCH

ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}"
mkdir -p "$out"

# Only the platforms that collect something (InfraNest #3838). macOS and FreeBSD still build in CI on every
# commit, so the compiler keeps seeing that code; publishing a binary that installs and then reports
# nothing read as a broken product rather than an unsupported platform.
for target in \
  linux/amd64 linux/arm64 linux/arm linux/386 linux/riscv64 linux/ppc64le linux/s390x \
  windows/amd64 windows/arm64; do
  os=${target%/*}; arch=${target#*/}
  file="$out/infranest-agent_${os}_${arch}"
  [ "$os" = "windows" ] && file="${file}.exe"
  GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$file" ./cmd/infranest-agent
  echo "  built $file"
done
