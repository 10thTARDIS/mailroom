#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Build + test the CGO search backend (ADR-006, Forgejo #20).
#
# Everything goes through the Docker API only (docker cp + docker start -a) —
# no bind mounts, no published ports, no host toolchain. That is deliberate and
# matches scripts/integration-test.sh: the Forgejo runner executes jobs in
# containers with a mounted docker socket, so host paths and host ports do not
# resolve from inside a job. This shape works identically on the runner, on a
# Linux host, and in Git Bash on Windows.
#
# Why Alpine/musl: anyproto/tantivy-go publishes the Linux static library ONLY
# as a musl build (linux-amd64-musl.tar.gz). Alpine's gcc links musl natively,
# so the prebuilt .a drops straight in — no Rust toolchain, no cross-compiler.
# On a glibc image you would additionally need x86_64-linux-musl-gcc.
#
#   sh scripts/tantivy-test.sh
#   GOTESTFLAGS='-run TestTantivyTypoTolerance -v' sh scripts/tantivy-test.sh
set -eu
MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
HOSTDIR="$(cd "$DIR" && { pwd -W 2>/dev/null || pwd; })"
GO_IMAGE="${MAILROOM_GO_ALPINE_IMAGE:-golang:1.27-alpine}"
TANTIVY_VERSION="${TANTIVY_VERSION:-v1.0.6}"

# The package fetch and the archive download retry a transient resolver failure
# (#577) — this backs a required check, and one DNS blip used to fail it.
# Bounded (5 attempts, 30 s at most), so a resolver that stays down still fails
# with the tool's own error, and each attempt is bounded too (#586) because a
# fetch that STALLS never exits and so never reaches the retry loop. Identical to
# the copy in scripts/restore-drill.sh, which explains it at length.
NET_RETRY='RETRY_TIMEOUT=${RETRY_TIMEOUT:-180}
retry() {
  n=1
  while :; do
    # ⚠ BOUNDING EACH ATTEMPT IS A SEPARATE PROPERTY FROM RETRYING IT (#586).
    # This loop only ever sees a command that RETURNS. A fetch that STALLS never
    # returns, so without the timeout the helper is inert exactly when it looks
    # busiest: the restore drill sat inside `apk add` for 18+ minutes with a
    # byte-identical log, never printing one retry line, until the run was
    # killed. timeout converts the stall into an ordinary non-zero exit, which
    # the rest of this loop already knows how to handle.
    #
    # ⚠ rc MUST be captured in the else branch. After `fi`, $? is the status of
    # the IF STATEMENT (0), not of the condition. The first version read `rc=$?`
    # after `fi`, so every message said rc=0 and the 124|143 case below was dead
    # code that looked correct — the probe is what caught it, not review.
    if timeout "$RETRY_TIMEOUT" "$@"; then
      return 0
    else
      rc=$?
    fi
    # MEASURED, because it differs by image and this helper runs in both:
    # busybox timeout exits 143 (alpine), GNU coreutils exits 124 (golang).
    # Both are non-zero so the retry fires either way — but a message that knows
    # only one of them misreports a timeout as a plain failure in the other.
    case $rc in
      124|143) why="TIMED OUT after ${RETRY_TIMEOUT}s" ;;
      *)       why="failed (rc=$rc)" ;;
    esac
    if [ "$n" -ge 5 ]; then echo "retry: gave up after $n attempts, last $why: $*" >&2; return 1; fi
    echo "retry: attempt $n $why, retrying in $((n * 3))s: $*" >&2
    sleep $((n * 3))
    n=$((n + 1))
  done
}
'

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

# shellcheck disable=SC2086 — GOTESTFLAGS is intentionally word-split
CID=$(docker create \
  -v mailroom-gomod-alpine:/go/pkg/mod \
  -v mailroom-gobuild-alpine:/root/.cache/go-build \
  -w /src -e "CGO_ENABLED=1" -e "TANTIVY_VERSION=$TANTIVY_VERSION" \
  -e "GOTESTFLAGS=${GOTESTFLAGS:-}" \
  "$GO_IMAGE" sh -c "$NET_RETRY"'
    set -e
    retry apk add --no-cache gcc musl-dev curl tar >/dev/null
    # ⚠ DOWNLOAD, CHECK, THEN EXTRACT — never `curl … | tar` (Forgejo #311).
    #
    # In a pipeline the shell reports the exit status of the LAST command, so a
    # failed curl (404 on a bumped TANTIVY_VERSION, a proxy error page, a
    # truncated transfer) is masked by tar. Today tar happens to fail too on the
    # empty or malformed input, so nothing has ever slipped through — but that
    # is an accident of tar behaviour, not a guarantee, and the failure this
    # script exists to catch is "the CGO engine was not actually built".
    #
    # `set -o pipefail` is the usual answer and is NOT available here: the
    # container runs this with /bin/sh, which on Alpine is dash/busybox and
    # rejects it. So the portable form is two statements with `set -e` doing the
    # work, plus an explicit size check because a 0-byte 200 response is a
    # download that "succeeded".
    retry curl -fsSL -o /tmp/tantivy.tar.gz \
      "https://github.com/anyproto/tantivy-go/releases/download/${TANTIVY_VERSION}/linux-amd64-musl.tar.gz"
    if [ ! -s /tmp/tantivy.tar.gz ]; then
      echo "tantivy-test: the ${TANTIVY_VERSION} musl archive downloaded as 0 bytes" >&2
      exit 1
    fi
    tar -xzf /tmp/tantivy.tar.gz -C /usr/local/lib
    echo "--- build (tags: tantivy)"
    go build -tags tantivy ./...
    echo "--- vet (tags: tantivy)"
    go vet -tags tantivy ./internal/search/...
    # ⚠ THE ONLY PLACE THE tantivy-TAGGED CODE MEETS THE FULL LINTER (#311).
    #
    # The ordinary lint job runs golangci-lint with build-tags
    # [integration, interop] (.golangci.yml) and deliberately WITHOUT tantivy,
    # so internal/search/tantivy is analysed there as its `!tantivy || !cgo`
    # stub. Until now the real engine got `go vet` and nothing else — no
    # errcheck, no staticcheck, no unused — which is most of what golangci-lint
    # is for.
    #
    # It has to run HERE rather than in the lint job, even though linting never
    # LINKS (so the 58 MB static library is not what makes the difference).
    # tantivy.go is `//go:build tantivy && cgo`: with CGO_ENABLED=0 the `cgo`
    # tag is off, the engine file drops out and the STUB is analysed instead —
    # green, fast, and about the wrong file. This container is the one place
    # where CGO_ENABLED=1 and a C toolchain are both guaranteed, so the tag
    # combination cannot silently degrade into linting nothing.
    #
    # --build-tags REPLACES the config set for this pass, which is intended: the
    # integration/interop files under ./internal/search/... are covered by the
    # main lint job. Every file is analysed by one pass or the other.
    echo "--- lint (tags: tantivy)"
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
    PATH="$(go env GOPATH)/bin:$PATH" \
      golangci-lint run --build-tags tantivy ./internal/search/...
    echo "--- test (tags: tantivy)"
    go test -tags tantivy -count=1 ${GOTESTFLAGS:-} ./internal/search/...
  ')
# EXIT alone does not fire on a CI cancellation (SIGTERM) — see mailroom#346.
trap 'docker rm -fv "$CID" >/dev/null 2>&1 || true' EXIT INT TERM

docker cp "$HOSTDIR/." "$CID:/src"
docker start -a "$CID"
echo "tantivy backend suite passed"
