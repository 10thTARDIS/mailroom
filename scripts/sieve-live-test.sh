#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Live ManageSieve verification (#50).
#
# Runs the `sievelive`-tagged tests in internal/sieve against a REAL Dovecot
# Pigeonhole ManageSieve server. Same container-only shape as
# scripts/clamav-test.sh and for the same reason: CI runner jobs are
# themselves containers with a socket-mounted Docker, so host paths and host
# ports do not resolve from inside one.
#
# The stock dovecot/dovecot image needs NO configuration for this: it runs
# managesieve on 4190 with STARTTLS (a self-signed certificate), and its static
# passdb accepts any user name with the password "pass". Each test logs in as a
# fresh random user, so tests never see each other's scripts.
#
# internal/sieve/sievetest copies Pigeonhole's wire bytes; this suite is what
# keeps that copy honest. It FAILS rather than skips when the server is
# missing (#263, #159).
set -eu

MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
HOSTDIR="$(cd "$DIR" && { pwd -W 2>/dev/null || pwd; })"
GO_IMAGE="${MAILROOM_GO_IMAGE:-golang:1.27}"
# Pinned: the fake server's bytes were captured from this exact version.
DOVECOT_IMAGE="${MAILROOM_DOVECOT_IMAGE:-dovecot/dovecot:2.3.21}"

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

# A unique suffix: `$$` alone repeats across runner invocations (see
# scripts/clamav-test.sh for the measured collision).
UNIQ="$$-$(od -An -N4 -tu4 < /dev/urandom 2>/dev/null | tr -dc '0-9' || date +%s)"
[ -n "${UNIQ#*-}" ] || UNIQ="$$-$(date +%s)"
NET="mailroom-sieve-$UNIQ"
EMITVOL="mailroom-sieve-emit-$UNIQ"
DOVECID=""
GOCID=""
BEHCID=""

cleanup() {
  [ -n "$GOCID" ] && docker rm -fv "$GOCID" >/dev/null 2>&1
  [ -n "$BEHCID" ] && docker rm -fv "$BEHCID" >/dev/null 2>&1
  docker volume rm "$EMITVOL" >/dev/null 2>&1
  [ -n "$DOVECID" ] && docker rm -fv "$DOVECID" >/dev/null 2>&1
  docker network rm "$NET" >/dev/null 2>&1
  return 0
}
trap cleanup EXIT INT TERM

docker ps -aq --filter "network=$NET" 2>/dev/null | xargs -r docker rm -fv >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null
docker volume create "$EMITVOL" >/dev/null

echo "starting $DOVECOT_IMAGE..."
DOVECID=$(docker run -d --network "$NET" --network-alias dovecot "$DOVECOT_IMAGE")

# Ready when the ManageSieve greeting's final line arrives on 4190 (the image
# ships bash, whose /dev/tcp is the probe).
ready=0
i=0
while [ "$i" -lt 30 ]; do
  if docker exec "$DOVECID" bash -c 'exec 3<>/dev/tcp/127.0.0.1/4190 && timeout 2 cat <&3' \
      2>/dev/null | grep -q '^OK "Dovecot ready."'; then
    ready=1
    break
  fi
  i=$((i + 1))
  sleep 2
done
[ "$ready" -eq 1 ] || { echo "dovecot managesieve never greeted" >&2; docker logs "$DOVECID" 2>&1 | tail -20 >&2; exit 1; }

echo "--- running internal/sieve and internal/outofoffice live tests"
# The generated out-of-office scripts land in a per-run volume that the
# dovecot container then reads (scripts/sieve-behaviour.sh, below).
GOCID=$(docker create --network "$NET" -w /src \
  -v mailroom-gomod:/go/pkg/mod \
  -v mailroom-gobuild:/root/.cache/go-build \
  -v "$EMITVOL":/emit \
  -e MAILROOM_TEST_SIEVE_HOST=dovecot \
  -e MAILROOM_TEST_SIEVE_PORT=4190 \
  -e MAILROOM_SIEVE_EMIT_DIR=/emit \
  "$GO_IMAGE" go test -tags sievelive -count=1 -timeout 5m -run Live -v ./internal/sieve/ ./internal/outofoffice/)
docker cp "$HOSTDIR/." "$GOCID:/src"
docker start -a "$GOCID"

echo "--- the generated out-of-office scripts on Pigeonhole's own interpreter"
BEHCID=$(docker create -v "$EMITVOL":/emit --entrypoint bash "$DOVECOT_IMAGE" /emit/sieve-behaviour.sh /emit)
docker cp "$HOSTDIR/scripts/sieve-behaviour.sh" "$BEHCID:/emit/sieve-behaviour.sh"
docker start -a "$BEHCID"

echo "sieve live suite passed"
