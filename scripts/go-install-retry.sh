#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
#
# Retry `go install <pkg>@<version>` across transient module-proxy failures.
#
# WHY THIS EXISTS. CI's `lint` and `govulncheck` jobs each install a tool from
# source, and each install pulls ~150 transitive modules from proxy.golang.org
# while verifying every one against sum.golang.org. That is a large network
# surface for a step that tests nothing — any single HTTP/2 stream erroring
# fails the whole job, and the failure looks exactly like a real lint failure in
# the run list. Observed twice; the second time (2026-08-31) it went red on a PR
# whose only change was deleting a dead function:
#
#   verifying module: github.com/ghostiam/protogetter@v0.3.21: reading
#   https://sum.golang.org/tile/8/1/818: stream error: stream ID 333;
#   INTERNAL_ERROR; received from peer
#
# ⚠ IT ANNOUNCES EVERY RETRY, AND THAT IS THE POINT. A retry loop that quietly
# absorbs failures converts a DEGRADING proxy into an invisible one — the job
# goes green, nobody learns the network is getting worse, and the eventual hard
# failure arrives with no history. Every attempt past the first prints a warning
# that survives in the log, so `succeeded on attempt 3` is greppable evidence.
#
# ⚠ IT DOES NOT WEAKEN VERIFICATION. The obvious alternative — GONOSUMCHECK or
# GOFLAGS=-mod=mod to skip sum.golang.org — would cut the failing half of the
# fetch by turning off checksum verification on the toolchain that lints and
# vuln-scans this project. That is a bad trade for a flake. This retries the
# same verified fetch instead.
#
# Usage: scripts/go-install-retry.sh <package@version> [more...]
set -eu

[ $# -ge 1 ] || { echo "usage: $0 <package@version> [...]" >&2; exit 2; }

attempts=3

for pkg in "$@"; do
  n=1
  while :; do
    if go install "$pkg"; then
      if [ "$n" -gt 1 ]; then
        echo "⚠ go install $pkg succeeded on attempt $n of $attempts —"
        echo "  the module proxy is flaking. If this line keeps appearing, cache the"
        echo "  built binary instead of re-fetching (see this script's header)."
      fi
      break
    fi
    if [ "$n" -ge "$attempts" ]; then
      echo "go install $pkg FAILED after $attempts attempts — not a flake, or the" >&2
      echo "proxy is down. The last error is above; this script did not hide it." >&2
      exit 1
    fi
    # Linear backoff: 10s, 20s. Long enough for a transient proxy blip, short
    # enough that a genuinely broken install still fails the job promptly.
    delay=$((n * 10))
    echo "go install $pkg failed (attempt $n of $attempts); retrying in ${delay}s"
    sleep "$delay"
    n=$((n + 1))
  done
done
