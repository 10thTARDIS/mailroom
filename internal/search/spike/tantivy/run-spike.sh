#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# ADR-006 search spike harness (Forgejo #2).
#
# Builds and runs the tantivy-go CGO spike in a throwaway musl-native Go
# container, mirroring scripts/integration-test.sh: everything goes through the
# Docker API (docker cp + docker start -a), no bind mounts, no host toolchain,
# no published ports. Works identically on a Linux host, the Forgejo runner
# (docker socket mounted), and Git Bash on Windows.
#
# Why Alpine/musl: anyproto/tantivy-go ships the Linux static lib ONLY as a
# musl build (linux-amd64-musl.tar.gz). Alpine's gcc links musl natively, so
# the prebuilt .a drops in with no cross-toolchain. On a glibc image you would
# additionally need x86_64-linux-musl-gcc.
#
#   sh run-spike.sh            # default corpus
#   N=20000 sh run-spike.sh    # larger corpus for latency
set -eu
MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
HOSTDIR="$(cd "$DIR" && { pwd -W 2>/dev/null || pwd; })"
GO_IMAGE="${MAILROOM_GO_IMAGE:-golang:1.26-alpine}"
TANTIVY_VERSION="${TANTIVY_VERSION:-v1.0.6}"
N="${N:-2000}"
ITERS="${ITERS:-500}"

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

CID=$(docker create \
  -v mailroom-spike-gomod:/go/pkg/mod \
  -v mailroom-spike-gobuild:/root/.cache/go-build \
  -w /src -e "GOFLAGS=-mod=mod" -e "CGO_ENABLED=1" \
  -e "TANTIVY_VERSION=$TANTIVY_VERSION" -e "N=$N" -e "ITERS=$ITERS" \
  "$GO_IMAGE" sh -c '
    set -e
    apk add --no-cache gcc musl-dev curl tar >/dev/null
    curl -fsSL "https://github.com/anyproto/tantivy-go/releases/download/${TANTIVY_VERSION}/linux-amd64-musl.tar.gz" \
      | tar -xz -C /usr/local/lib
    go mod tidy
    gofmt -l . | (grep . && { echo "gofmt not clean"; exit 1; } || true)
    go vet ./...
    go build -o /tmp/harness .
    /tmp/harness -n "$N" -dir /tmp/spike-index -iters "$ITERS"
  ')
trap 'docker rm -f "$CID" >/dev/null 2>&1 || true' EXIT

docker cp "$HOSTDIR/." "$CID:/src"
docker start -a "$CID"
