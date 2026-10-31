#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Cross-implementation OpenPGP interop (#263).
#
# Runs the `interop`-tagged tests against a REAL GnuPG:
#   - internal/pgpverify: gpg verifies a message Mailroom signed, and Mailroom
#     verifies a message gpg signed;
#   - internal/pgpkeyd (#47 6b): gpg encrypts to a key the REAL crypto daemon
#     generated, and the daemon decrypts it through RFC 3156 framing — which
#     also drives the daemon's own key shape through an independent
#     implementation (the C4 circularity carried over from #301);
#   - internal/pgpkeyd (#47 section 6.2): gpg --sign --encrypt (signature inside
#     the encryption, as Thunderbird sends it); the daemon verifies it against a
#     gpg-made candidate key, and reports noKey for a gpg signer it was not given;
#   - internal/pgpkeyd (#47 encrypt): the daemon encrypts to a key GnuPG made
#     (plus the sender's own), and GnuPG decrypts it; a tampered copy must not.
# Every half carries a negative control.
#
# WHY A SCRIPT AND ITS OWN JOB, rather than folding this into `go test ./...`:
#
#   - the build-test job runs on the runner host via setup-go, where gpg is not
#     guaranteed to exist. A test that skipped there would be the same defect
#     #263 describes — an interop check that silently never runs;
#   - golang:1.27 ships GnuPG (2.4.7 at time of writing), so running inside that
#     container makes the tool's presence a property of the image rather than of
#     whatever the runner happens to have installed;
#   - and if gpg is somehow absent, the test FAILS rather than skipping. Opting
#     out needs MAILROOM_SKIP_GPG_INTEROP=1, which is deliberate and visible.
#
# Same container-only shape as scripts/tantivy-test.sh and integration-test.sh,
# for the same reason: CI runner jobs are themselves containers with a
# socket-mounted Docker, so host paths and ports do not resolve from inside one.
set -eu

MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
HOSTDIR="$(cd "$DIR" && { pwd -W 2>/dev/null || pwd; })"
GO_IMAGE="${MAILROOM_GO_IMAGE:-golang:1.27}"

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

CID=$(docker create -w /src \
  -v mailroom-gomod:/go/pkg/mod \
  -v mailroom-gobuild:/root/.cache/go-build \
  "$GO_IMAGE" sh -c '
    set -eu
    # Report the tool being tested against, so the log says which independent
    # implementation actually agreed with us.
    gpg --version | head -1
    go test -tags interop -count=1 -v ./internal/pgpverify/...
    go test -tags interop -count=1 -v -run Interop ./internal/pgpkeyd/
  ')
# shellcheck disable=SC2064
trap "docker rm -fv $CID >/dev/null 2>&1 || true" EXIT INT TERM
docker cp "$HOSTDIR/." "$CID:/src"
docker start -a "$CID"
echo "pgp interop passed: an independent OpenPGP implementation agrees with our framing and decrypts to our keys"
