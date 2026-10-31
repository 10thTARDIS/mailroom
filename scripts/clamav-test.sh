#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Live ClamAV verification (Forgejo #394, ADR-027 increment 3).
#
# Runs the `clamav`-tagged tests in internal/clamd against a REAL clamd, in the
# configuration ADR-027 Â§5a requires. Same container-only shape as
# scripts/pgp-interop-test.sh and integration-test.sh, and for the same reason:
# Forgejo runner jobs are themselves containers with a socket-mounted Docker, so
# host paths and host ports do not resolve from inside one.
#
# â  WHY THE CONFIGURATION IS PART OF THE TEST, NOT AN ASSUMPTION.
#
# Stock clamd CANNOT produce the `unknown` verdict at all. Measured against
# clamav/clamav:latest on 2026-09-01:
#
#     AlertExceedsMax   disabled
#     AlertEncrypted    disabled
#
# With those off, an encrypted archive and an archive too deep to open BOTH
# return plain "stream: OK" â a clean verdict on a file nothing looked inside,
# which is precisely the evasion Â§5b describes. Turning them on makes those
# cases arrive as DETECTIONS, which a naive client maps to `flagged` and
# quarantines every password-protected zip as malware.
#
# So the daemon this suite starts is configured the way a deployment must be,
# and the tests assert the four-state model actually exists on it. A suite run
# against a default daemon would pass three tests and silently prove nothing
# about the fourth state.
#
# â  AND IT FAILS RATHER THAN SKIPS. internal/clamd's live tests t.Fatal when
# MAILROOM_TEST_CLAMD_ADDR is unset. A scanner suite that skips when the scanner
# is missing goes green in exactly the situation it exists to detect â the
# lesson from #263 and #159.
set -eu

MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
HOSTDIR="$(cd "$DIR" && { pwd -W 2>/dev/null || pwd; })"
GO_IMAGE="${MAILROOM_GO_IMAGE:-golang:1.27}"
CLAMAV_IMAGE="${MAILROOM_CLAMAV_IMAGE:-clamav/clamav:latest}"

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

# ⚠ `$$` IS NOT UNIQUE ACROSS CONCURRENT RUNS, AND THE PRE-CLEAN BELOW DELETES
# BY NAME — so a collision is one run reaping another's containers mid-test, not
# a harmless name clash. MEASURED on 2026-09-05: two restore-drill runs on
# different refs collided on PID 5055 and the victim died with
# `network mailroom-drill-5055 not found`. CI's concurrency group cancels only
# within a REF, so a post-merge run on main and a PR run overlap by design.
UNIQ="$$-$(od -An -N4 -tu4 < /dev/urandom 2>/dev/null | tr -dc '0-9' || date +%s)"
[ -n "${UNIQ#*-}" ] || UNIQ="$$-$(date +%s)"
NET="mailroom-clamav-$UNIQ"
CLAMCID=""
GOCID=""

cleanup() {
  # â  EXIT INT TERM, not EXIT alone (#346): a cancelled Actions step gets
  # SIGTERM and a shell with only an EXIT trap dies without running it, which
  # is how cancelled runs leaked containers into the runners' daemons.
  [ -n "$GOCID" ] && docker rm -fv "$GOCID" >/dev/null 2>&1
  [ -n "$CLAMCID" ] && docker rm -fv "$CLAMCID" >/dev/null 2>&1
  docker network rm "$NET" >/dev/null 2>&1
  return 0
}
trap cleanup EXIT INT TERM

# â  CLEAR A STALE NETWORK BEFORE CREATING ONE, because the trap above is not
# enough and this job proved it: run 1075 died on
#
#     Error response from daemon: network with name mailroom-clamav-163 already exists
#
# `$$` IS NOT UNIQUE ACROSS RUNNER INVOCATIONS. Each job is a fresh container, so
# the shell's PID is a small number that repeats â 163 twice in one afternoon â
# and an earlier run cancelled by a push (concurrency, #347) left its network
# behind. The EXIT INT TERM trap covers SIGTERM but not SIGKILL, and even on a
# clean TERM `docker network rm` fails while a container is still attached.
#
# â  IT MUST FILTER BY NETWORK MEMBERSHIP, not by name. The four e2e scripts
# pre-clean by calling their own cleanup at startup, which works because they
# name their containers; the two containers here are ANONYMOUS (`docker run -d`
# with no --name), so on a fresh run CLAMCID and GOCID are empty and a startup
# cleanup call would remove nothing while the network stayed. This is
# integration-test.sh's form, and clamav-test.sh was the ONE script of ten
# without any pre-clean at all.
docker ps -aq --filter "network=$NET" 2>/dev/null | xargs -r docker rm -fv >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null

echo "starting clamd ($CLAMAV_IMAGE) with the mandatory alert settings..."
CLAMCID=$(docker run -d --network "$NET" --network-alias clamd \
  -e CLAMAV_NO_MILTERD=true \
  "$CLAMAV_IMAGE")

# The alerts are appended AFTER the image's own entrypoint has written its
# config, then clamd is restarted so they take effect. Verified below rather
# than assumed â see the clamconf assertion.
docker exec "$CLAMCID" sh -c '
  printf "\nAlertExceedsMax yes\nAlertEncrypted yes\nAlertEncryptedArchive yes\nAlertEncryptedDoc yes\n" \
    >> /etc/clamav/clamd.conf' >/dev/null

echo "waiting for the signature database to load (this takes a few minutes on a cold image)..."
ready=0
i=0
while [ "$i" -lt 90 ]; do
  if docker exec "$CLAMCID" sh -c 'echo PING | nc -w 3 127.0.0.1 3310' 2>/dev/null | grep -q PONG; then
    ready=1
    break
  fi
  i=$((i + 1))
  sleep 10
done
[ "$ready" -eq 1 ] || { echo "clamd never answered PING" >&2; docker logs "$CLAMCID" 2>&1 | tail -20 >&2; exit 1; }

# Restart so the appended config is live, then wait again.
docker restart "$CLAMCID" >/dev/null
ready=0
i=0
while [ "$i" -lt 90 ]; do
  if docker exec "$CLAMCID" sh -c 'echo PING | nc -w 3 127.0.0.1 3310' 2>/dev/null | grep -q PONG; then
    ready=1
    break
  fi
  i=$((i + 1))
  sleep 10
done
[ "$ready" -eq 1 ] || { echo "clamd did not come back after restart" >&2; exit 1; }

# â  ASSERT THE CONFIGURATION TOOK EFFECT. Without this the suite could run
# against a default daemon and report success on three of four states.
echo "--- effective clamd configuration"
docker exec "$CLAMCID" sh -c 'clamconf 2>/dev/null | grep -iE "^AlertExceedsMax|^AlertEncrypted =|^StreamMaxLength"' || true
docker exec "$CLAMCID" sh -c 'clamconf 2>/dev/null | grep -qi "^AlertEncrypted = \"yes\""' || {
  echo "AlertEncrypted is NOT enabled â the unknown state cannot be produced and this suite would prove nothing" >&2
  exit 1
}
docker exec "$CLAMCID" sh -c 'clamconf 2>/dev/null | grep -qi "^AlertExceedsMax = \"yes\""' || {
  echo "AlertExceedsMax is NOT enabled â see above" >&2
  exit 1
}

echo "--- running internal/clamd live tests"
GOCID=$(docker create --network "$NET" -w /src \
  -v mailroom-gomod:/go/pkg/mod \
  -v mailroom-gobuild:/root/.cache/go-build \
  -e MAILROOM_TEST_CLAMD_NET=tcp \
  -e MAILROOM_TEST_CLAMD_ADDR=clamd:3310 \
  "$GO_IMAGE" go test -tags clamav -count=1 -timeout 15m ./internal/clamd/...)
docker cp "$HOSTDIR/." "$GOCID:/src"
docker start -a "$GOCID"

echo "clamav live suite passed"
