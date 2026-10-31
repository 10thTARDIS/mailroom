#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# The category-tab JMAP surface, proven over REAL HTTP against the real image
# (Forgejo #228, #219; spec §8).
#
# WHY THIS EXISTS AT ALL. #228's checklist ends with "exercise the JMAP methods
# over HTTP at least once" — and that item is not busywork, it is #159 by name.
# In v1.4.0 the client called `Triage/correct` without declaring
# `urn:mailroom:triage`, the server refused it with `unknownMethod` (mayCall is
# fail-closed, RFC 8620 §3.2), and marking a message important died at the
# operator's fingertips. Nothing caught it, because:
#
#   - internal/api's tests call handlers DIRECTLY (`call(t, h.api.categoryTabSet, …)`),
#     which bypasses the dispatcher — and the `using` gate lives in the dispatcher;
#   - the read path can look perfectly healthy while only the write path is dead.
#
# So the whole point here is to go through the front door: a real POST to /jmap,
# with a real `using` array, against the real container.
#
# ⚠ THE LOAD-BEARING ASSERTION IS THE NEGATIVE CONTROL (step 6). Proving
# `CategoryTab/set` works when the capability IS declared proves nothing on its
# own — it would also pass if the gate did not exist. So the same call is made a
# second time with the capability OMITTED, and it MUST come back `unknownMethod`.
# That is what makes the positive result evidence about the gate rather than
# evidence that the method happens to be registered.
#
# dev-auth is the vehicle: no OIDC, no login, fixed identity, and therefore no
# secrets in this script (same escape hatch scripts/gate-e2e.sh section 5 uses).
#
# Everything is container-to-container: no published ports, no host toolchain, no
# bind mounts of source. Same shape as gate-e2e.sh and hostile-content-e2e.sh, so
# it runs identically on the Forgejo runner, on Linux, and in Git Bash.
#
# Usage:  scripts/tabs-e2e.sh
#         MAILROOM_IMAGE=mailroom:test scripts/tabs-e2e.sh
set -eu
MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

# ⚠ `$$` IS NOT UNIQUE ACROSS CONCURRENT RUNS, AND CLEANUP REMOVES BY NAME — so a
# fixed (or PID-only) name is one run reaping another's network/containers
# mid-test. MEASURED on 2026-10-15: a post-merge run on main and a PR run overlapped
# on one runner host; the PR's run hit `network with name ... already exists`, its
# cleanup trap then removed the network main's run was using, and main's jobs died
# mid-run. CI's concurrency group cancels only within a REF, so overlap is by
# design. Every Docker object name (and the built image tag) carries $UNIQ so
# cleanup can only ever touch this run's own objects (#502; same idiom as
# scripts/integration-test.sh).
UNIQ="$$-$(od -An -N4 -tu4 < /dev/urandom 2>/dev/null | tr -dc '0-9' || date +%s)"
[ -n "${UNIQ#*-}" ] || UNIQ="$$-$(date +%s)"

IMAGE="${MAILROOM_IMAGE:-mailroom:tabs-e2e-$UNIQ}"
PG_IMAGE="${PG_IMAGE:-postgres:18}"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.10.1}"

NET="mailroom-tabs-e2e-$UNIQ"
VOL="mailroom-tabs-e2e-key-$UNIQ"
PG="mailroom-tabs-e2e-pg-$UNIQ"
APP="mailroom-tabs-e2e-app-$UNIQ"

cd "$(dirname "$0")/.."
# The HOST-side path of the checkout, in the form the docker CLI needs. `pwd -W`
# gives Git Bash the Windows form; MSYS_NO_PATHCONV (set above, and required so
# container-side paths are not rewritten) means a bare MSYS path would reach
# docker unconverted.
HOSTDIR="$(pwd -W 2>/dev/null || pwd)"

cleanup() {
  echo "--- cleaning up"
  docker rm -fv "$APP" "$PG" >/dev/null 2>&1 || true
  docker volume rm -f "$VOL" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  # Only an image THIS run built (uniquely tagged); a MAILROOM_IMAGE override is the caller's.
  if [ -z "${MAILROOM_IMAGE:-}" ]; then docker rmi "$IMAGE" >/dev/null 2>&1 || true; fi
}
# ⚠ INT and TERM are NOT decoration: a CI cancellation sends SIGTERM, and a
# shell with only an EXIT trap dies WITHOUT running it. Measured in an alpine
# container — `trap cleanup EXIT` left the marker unwritten on SIGTERM, while
# `trap cleanup EXIT INT TERM` wrote it. That is how cancelled runs leaked
# long-lived side containers into the runners (mailroom#346); three were found
# aged 7h, 8h and 11h. cleanup is idempotent, so the EXIT trap firing again
# after the TERM trap is harmless.
trap cleanup EXIT INT TERM
cleanup >/dev/null 2>&1 || true

# wait_for retries a readiness command until it succeeds; on exhaustion it FAILS
# LOUDLY and dumps the container. A bare `for … do <check> && break; done` falls
# through silently and the failure then surfaces three commands later as
# something unrelated (the v1.0.0 lesson — diagnose at the point of failure).
#   $1 label, $2 attempts (~seconds), $3 container to dump ("" none), $4.. command
wait_for() {
  _label="$1"; _attempts="$2"; _dump="$3"; shift 3
  _i=0
  while [ "$_i" -lt "$_attempts" ]; do
    if "$@" >/dev/null 2>&1; then return 0; fi
    _i=$((_i + 1))
    sleep 1
  done
  echo "FATAL: $_label did not become ready after ${_attempts}s" >&2
  if [ -n "$_dump" ]; then
    echo "--- docker inspect $_dump ---" >&2
    docker inspect -f 'status={{.State.Status}} exit={{.State.ExitCode}} error={{.State.Error}}' \
      "$_dump" >&2 2>&1 || true
    echo "--- docker logs $_dump (last 50) ---" >&2
    docker logs --tail 50 "$_dump" >&2 2>&1 || true
  fi
  exit 1
}

fails=0
pass() { printf 'ok    %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }

# jmap <json> -> the raw response body. No cookie and no token: under dev-auth the
# request IS the operator, which is exactly what makes this runnable in CI.
jmap() {
  docker run --rm --network "$NET" "$CURL_IMAGE" -sS \
    -H 'Content-Type: application/json' -X POST --data-raw "$1" \
    http://tabsmailroom:8080/jmap
}

if [ -z "${MAILROOM_IMAGE:-}" ]; then
  echo "--- building $IMAGE"
  docker build -t "$IMAGE" .
fi

echo "--- network + master key"
docker network create "$NET" >/dev/null
docker volume create "$VOL" >/dev/null
docker run --rm -v "$VOL:/k" "$PG_IMAGE" sh -c \
  'openssl rand -hex 32 | tr -d "\r\n" > /k/master && chmod 644 /k/master' >/dev/null

echo "--- postgres"
docker run -d --name "$PG" --network "$NET" --network-alias pg \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=mailroom "$PG_IMAGE" >/dev/null
# -h 127.0.0.1 forces TCP: the image's first-init temp server listens on the Unix
# socket only, so a socket check reports ready before POSTGRES_DB exists.
wait_for postgres 60 "$PG" \
  docker exec "$PG" pg_isready -h 127.0.0.1 -U postgres -d mailroom -q

echo "--- schema + roles"
# docker cp + docker exec, NEVER a host bind mount. The Forgejo runner executes
# each job INSIDE a container with a socket-mounted Docker, so `-v "$(pwd)/db:…"`
# is resolved against the DAEMON's filesystem, where the checkout does not exist.
# It therefore works locally and fails ONLY on CI — which is exactly how this
# shipped red (`psql: /db/schema.sql: No such file or directory`). Same shape as
# scripts/schema-apply.sh, which has always run clean on the runner.
docker cp "$HOSTDIR/db/schema.sql" "$PG:/schema.sql"
docker exec "$PG" psql -v ON_ERROR_STOP=1 -q -U postgres -d mailroom -f /schema.sql >/dev/null
docker exec "$PG" psql -q -v ON_ERROR_STOP=1 -U postgres -d mailroom \
  -c "ALTER ROLE mailroom_app LOGIN PASSWORD 'app';" >/dev/null

echo "--- mailroom (dev-auth: no OIDC, no login)"
docker run -d --name "$APP" --network "$NET" --network-alias tabsmailroom \
  -v "$VOL:/k:ro" \
  -e MAILROOM_DATABASE_URL="postgres://mailroom_app:app@pg:5432/mailroom?sslmode=disable" \
  -e MAILROOM_MASTER_KEY_FILE=/k/master \
  -e MAILROOM_DEV_AUTH_SUBJECT="dev-operator" \
  -e MAILROOM_DEV_AUTH_I_UNDERSTAND="this-instance-has-no-authentication" \
  -e MAILROOM_DEV_AUTH_EMAIL="dev@example.com" \
  -e MAILROOM_LOG_FORMAT=text \
  "$IMAGE" >/dev/null
wait_for mailroom 90 "$APP" \
  docker run --rm --network "$NET" "$CURL_IMAGE" -sf http://tabsmailroom:8080/readyz

echo
echo "=== 1. the session advertises the capability ================================="
SESSION="$(docker run --rm --network "$NET" "$CURL_IMAGE" -sS http://tabsmailroom:8080/jmap/session)"
printf '%s' "$SESSION" | grep -q 'urn:mailroom:categorytabs' \
  && pass "/jmap/session advertises urn:mailroom:categorytabs" \
  || fail "/jmap/session does NOT advertise urn:mailroom:categorytabs"

# accountId == the user's ULID. Read it out of the session rather than assuming.
ACCT="$(printf '%s' "$SESSION" | grep -o '"primaryAccounts":{"[^"]*":"[^"]*"' | sed 's/.*:"\([^"]*\)"$/\1/')"
[ -n "$ACCT" ] && pass "accountId resolved from the session ($ACCT)" \
  || { fail "could not resolve accountId from the session"; echo "$SESSION"; exit 1; }

USING='"urn:ietf:params:jmap:core","urn:mailroom:labels","urn:mailroom:categorytabs"'

echo
echo "=== 2. CategoryTab/get over HTTP ============================================="
GET1="$(jmap "{\"using\":[$USING],\"methodCalls\":[[\"CategoryTab/get\",{\"accountId\":\"$ACCT\",\"ids\":null},\"c0\"]]}")"
for t in primary social promotions updates forums; do
  printf '%s' "$GET1" | grep -q "\"tab\":\"$t\"" \
    && pass "CategoryTab/get returned $t" \
    || fail "CategoryTab/get did not return $t"
done

# Go marshals map[string]any with SORTED keys, so each row is exactly
# {"enabled":…,"id":"…","position":…,"tab":"…"} — parse on that shape.
tab_id() {
  printf '%s' "$1" | grep -o "{\"enabled\":[a-z]*,\"id\":\"[^\"]*\",\"position\":[0-9]*,\"tab\":\"$2\"}" \
    | sed 's/.*"id":"\([^"]*\)".*/\1/'
}
FORUMS="$(tab_id "$GET1" forums)"
PRIMARY="$(tab_id "$GET1" primary)"
[ -n "$FORUMS" ] && [ -n "$PRIMARY" ] \
  && pass "resolved the forums and primary row ids" \
  || { fail "could not resolve tab ids"; echo "$GET1"; exit 1; }

echo
echo "=== 3. CategoryTab/set over HTTP — disable a tab =============================="
# THE CALL THAT HAD NEVER BEEN MADE. Every other path to this method went through
# the store or a direct handler call.
SET1="$(jmap "{\"using\":[$USING],\"methodCalls\":[[\"CategoryTab/set\",{\"accountId\":\"$ACCT\",\"update\":{\"$FORUMS\":{\"enabled\":false}}},\"c0\"]]}")"
printf '%s' "$SET1" | grep -q "\"updated\":{\"$FORUMS\"" \
  && pass "CategoryTab/set reported forums updated" \
  || { fail "CategoryTab/set did not report forums updated"; echo "$SET1"; }
printf '%s' "$SET1" | grep -q 'unknownMethod' \
  && fail "CategoryTab/set answered unknownMethod — the #159 trap, with the capability DECLARED" \
  || pass "CategoryTab/set was not refused as unknownMethod"

echo
echo "=== 4. it PERSISTED (re-read, do not trust the ack) =========================="
GET2="$(jmap "{\"using\":[$USING],\"methodCalls\":[[\"CategoryTab/get\",{\"accountId\":\"$ACCT\",\"ids\":null},\"c0\"]]}")"
printf '%s' "$GET2" | grep -q "{\"enabled\":false,\"id\":\"$FORUMS\"" \
  && pass "forums came back enabled:false on a fresh read" \
  || { fail "forums did not persist as disabled"; echo "$GET2"; }

echo
echo "=== 5. Primary is refused PER OBJECT, in a batch that also succeeds ==========="
# The savepoint-trap shape this project has already shipped once: a failed
# statement aborts the whole transaction, and then the per-object error handling
# above it is decoration. Asserting on the VALID object in the same batch is what
# proves the refusal did not take the good write down with it.
SET2="$(jmap "{\"using\":[$USING],\"methodCalls\":[[\"CategoryTab/set\",{\"accountId\":\"$ACCT\",\"update\":{\"$PRIMARY\":{\"enabled\":false},\"$FORUMS\":{\"enabled\":true}}},\"c0\"]]}")"
printf '%s' "$SET2" | grep -q "\"notUpdated\":{\"$PRIMARY\"" \
  && pass "disabling Primary was refused" \
  || { fail "disabling Primary was NOT refused"; echo "$SET2"; }
printf '%s' "$SET2" | grep -q 'invalidProperties' \
  && pass "…as invalidProperties, not serverFail" \
  || fail "the Primary refusal was not invalidProperties (a whole-method failure?)"
printf '%s' "$SET2" | grep -q "\"updated\":{\"$FORUMS\"" \
  && pass "…and the VALID update in the same batch still applied" \
  || fail "the valid update was lost with the refused one (the savepoint trap)"

echo
echo "=== 6. NEGATIVE CONTROL: the same call WITHOUT the capability ================="
# Without this, step 3 is not evidence about the gate — it would pass just as
# happily if no gate existed at all. This is the assertion that can tell those
# two worlds apart.
NOCAP="$(jmap "{\"using\":[\"urn:ietf:params:jmap:core\"],\"methodCalls\":[[\"CategoryTab/set\",{\"accountId\":\"$ACCT\",\"update\":{\"$FORUMS\":{\"enabled\":true}}},\"c0\"]]}")"
printf '%s' "$NOCAP" | grep -q 'unknownMethod' \
  && pass "undeclared capability ⇒ unknownMethod (the gate is real and fail-closed)" \
  || { fail "undeclared capability was ANSWERED — the using gate is not enforcing"; echo "$NOCAP"; }

echo
echo "=== 7. Label/set with a tab patch over HTTP =================================="
MK="$(jmap "{\"using\":[$USING],\"methodCalls\":[[\"Label/set\",{\"accountId\":\"$ACCT\",\"create\":{\"n1\":{\"name\":\"Drill Label\"}}},\"c0\"]]}")"
LABEL="$(printf '%s' "$MK" | grep -o '"created":{"n1":{[^}]*"id":"[^"]*"' | sed 's/.*"id":"\([^"]*\)".*/\1/')"
[ -n "$LABEL" ] && pass "created a label to map ($LABEL)" \
  || { fail "could not create a label"; echo "$MK"; exit 1; }

SET3="$(jmap "{\"using\":[$USING],\"methodCalls\":[[\"Label/set\",{\"accountId\":\"$ACCT\",\"update\":{\"$LABEL\":{\"tab\":\"promotions\"}}},\"c0\"]]}")"
printf '%s' "$SET3" | grep -q "\"updated\":{\"$LABEL\"" \
  && pass "Label/set accepted a tab patch" \
  || { fail "Label/set did not accept a tab patch"; echo "$SET3"; }

LGET="$(jmap "{\"using\":[$USING],\"methodCalls\":[[\"Label/get\",{\"accountId\":\"$ACCT\",\"ids\":null},\"c0\"]]}")"
printf '%s' "$LGET" | grep -q '"tab":"promotions"' \
  && pass "the mapping persisted and Label/get reports it" \
  || { fail "the tab did not persist on the label"; echo "$LGET"; }

# Clearing is the operator's way back out, and null is a different code path from
# a string — an unmap that silently did nothing would strand a label in a tab.
SET4="$(jmap "{\"using\":[$USING],\"methodCalls\":[[\"Label/set\",{\"accountId\":\"$ACCT\",\"update\":{\"$LABEL\":{\"tab\":null}}},\"c0\"]]}")"
printf '%s' "$SET4" | grep -q "\"updated\":{\"$LABEL\"" \
  && pass "Label/set accepted tab:null" \
  || { fail "Label/set refused tab:null"; echo "$SET4"; }
LGET2="$(jmap "{\"using\":[$USING],\"methodCalls\":[[\"Label/get\",{\"accountId\":\"$ACCT\",\"ids\":null},\"c0\"]]}")"
printf '%s' "$LGET2" | grep -q '"tab":"promotions"' \
  && { fail "the label is STILL mapped after tab:null"; echo "$LGET2"; } \
  || pass "the label is unmapped again"

echo
echo "=== 8. every mutation left a change_log row =================================="
# The load-bearing invariant (CLAUDE.md). Two CategoryTab updates that changed
# something (forums off, forums back on) plus the five lazy `created` rows.
CT_ROWS="$(docker exec "$PG" psql -U postgres -d mailroom -tAc \
  "SELECT count(*) FROM app.change_log WHERE object_type = 'CategoryTab'" | tr -d '[:space:]')"
[ "${CT_ROWS:-0}" -ge 7 ] \
  && pass "CategoryTab change_log rows = $CT_ROWS (5 created + 2 updated)" \
  || fail "CategoryTab change_log rows = $CT_ROWS, want >= 7"

echo
if [ "$fails" -eq 0 ]; then
  echo "=== the category-tab JMAP surface answers over real HTTP ==="
else
  echo "=== TABS E2E FAILED: $fails assertion(s) ==="
  echo "--- mailroom logs ---"; docker logs "$APP" 2>&1 | tail -40
  exit 1
fi
