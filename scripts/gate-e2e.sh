#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# The login gate + OIDC multi-user provisioning, proven end to end against the
# REAL image and a REAL OIDC provider (#92, #98).
#
# cmd/mailroom/gate_test.go proves the gate over the real mux, in process, with
# no network. This script proves the other half: that the thing we actually ship
# — the container, behind a real Authorization-Code + PKCE flow, against a
# provider that really signs JWTs and really serves a JWKS — holds the same line,
# AND that the multi-user provisioning spine works:
#
#   (a) a first-time subject is AUTO-PROVISIONED on login and reaches
#       /jmap/session as an ordinary user (app-binding is the access gate);
#   (b) a subject in the `mailroom-admin` group gets role=admin, re-evaluated on
#       every login (log in again without the group -> back to role=user);
#   (c) TWO different subjects each get their OWN isolated user (distinct id and
#       email; each session reports only its own address);
#   (d) the dev-auth escape hatch still works (no OIDC, no login, fixed identity).
#
# The provider is ghcr.io/navikt/mock-oauth2-server: a full OIDC provider whose
# interactive login form lets us choose the `sub` per login AND inject arbitrary
# token `claims` (email, name, groups) — which is what makes (a)/(b)/(c) testable
# against a real signed token rather than a stub.
#
# Everything runs on a private docker network, including the "browser" (a curl
# container) — so the issuer URL resolves identically for the server doing
# discovery and for the client following redirects, exactly as a real deployment
# must. Role assertions the JMAP API does not expose are read straight from
# Postgres via `docker exec` (the authoritative source of app.users.role).
#
# Usage:  scripts/gate-e2e.sh            # builds mailroom:gate-e2e-<unique>
#         MAILROOM_IMAGE=mailroom:test scripts/gate-e2e.sh
set -eu

# Keep Git Bash (MSYS) from rewriting CONTAINER-side paths into Windows ones.
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

IMAGE="${MAILROOM_IMAGE:-mailroom:gate-e2e-$UNIQ}"
OIDC_IMAGE="${OIDC_IMAGE:-ghcr.io/navikt/mock-oauth2-server:2.1.10}"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.10.1}"
PG_IMAGE="${PG_IMAGE:-postgres:18}"

NET="mailroom-gate-e2e-$UNIQ"
VOL="mailroom-gate-e2e-key-$UNIQ"
PG="mailroom-gate-e2e-pg-$UNIQ"
OIDC="mailroom-gate-e2e-oidc-$UNIQ"
APP="mailroom-gate-e2e-app-$UNIQ"
DEVAPP="mailroom-gate-e2e-devapp-$UNIQ"

cd "$(dirname "$0")/.."

cleanup() {
  echo "--- cleaning up"
  docker rm -fv "$APP" "$DEVAPP" "$OIDC" "$PG" >/dev/null 2>&1 || true
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
cleanup

# wait_for retries a readiness command until it succeeds. If it never does, it
# FAILS THE SCRIPT LOUDLY and dumps the container's log and final state.
#
# Without this a bare `for ... do <check> && break; done` falls through on
# exhaustion and the script carries on against a service that never came up, so
# the failure surfaces later as something unrelated — a real CI run died with
# "container ... is not running" from the NEXT docker exec, which says nothing
# about postgres having failed to start (#135, ported from
# hostile-content-e2e.sh).
#
#   $1 label, $2 attempts (~seconds), $3 container to dump on failure ("" none),
#   $4.. the command to run.
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
    docker inspect -f 'status={{.State.Status}} exit={{.State.ExitCode}} oomkilled={{.State.OOMKilled}} error={{.State.Error}}' \
      "$_dump" >&2 2>&1 || true
    echo "--- docker logs $_dump (last 50) ---" >&2
    docker logs --tail 50 "$_dump" >&2 2>&1 || true
  fi
  exit 1
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
# -h 127.0.0.1 forces TCP so we wait for the real post-init server (the temp
# init server listens on the Unix socket only, before POSTGRES_DB exists).
wait_for postgres 60 "$PG" \
  docker exec "$PG" pg_isready -h 127.0.0.1 -U postgres -d mailroom -q

echo "--- roles + migrations (the documented deploy path: mailroom-migrate, as mailroom_owner)"
docker exec "$PG" psql -U postgres -d mailroom -v ON_ERROR_STOP=1 \
  -c "CREATE ROLE mailroom_owner LOGIN PASSWORD 'owner' CREATEROLE;" \
  -c "GRANT ALL ON SCHEMA public TO mailroom_owner;" \
  -c "GRANT ALL ON DATABASE mailroom TO mailroom_owner;" >/dev/null
docker run --rm --network "$NET" --entrypoint /mailroom-migrate \
  -e MAILROOM_MIGRATE_DATABASE_URL="postgres://mailroom_owner:owner@pg:5432/mailroom?sslmode=disable" \
  "$IMAGE" up >/dev/null
docker exec "$PG" psql -U postgres -d mailroom -v ON_ERROR_STOP=1 \
  -c "ALTER ROLE mailroom_app LOGIN PASSWORD 'app';" >/dev/null

echo "--- oidc provider ($OIDC_IMAGE)"
# interactiveLogin gives us a login form: the username typed in becomes the `sub`,
# and the `claims` textarea is a JSON object merged into the issued token — which
# is how we attach email/name/groups to a specific login.
docker run -d --name "$OIDC" --network "$NET" --network-alias oidc \
  -e JSON_CONFIG='{"interactiveLogin":true}' "$OIDC_IMAGE" >/dev/null
wait_for "oidc discovery" 60 "$OIDC" \
  docker run --rm --network "$NET" "$CURL_IMAGE" -sf \
    http://oidc:8080/default/.well-known/openid-configuration

echo "--- mailroom (OIDC; app-binding is the gate, mailroom-admin group => admin)"
# Operator env is still set to exercise the boot-provision/accounts path, but it
# is NO LONGER an allowlist: logins below use entirely different subjects and are
# auto-provisioned regardless.
docker run -d --name "$APP" --network "$NET" --network-alias mailroom \
  -v "$VOL:/k:ro" \
  -e MAILROOM_DATABASE_URL="postgres://mailroom_app:app@pg:5432/mailroom?sslmode=disable" \
  -e MAILROOM_MASTER_KEY_FILE=/k/master \
  -e MAILROOM_OIDC_ISSUER="http://oidc:8080/default" \
  -e MAILROOM_OIDC_CLIENT_ID=mailroom \
  -e MAILROOM_OIDC_CLIENT_SECRET=mailroom-secret \
  -e MAILROOM_OIDC_REDIRECT_URL="http://mailroom:8080/auth/callback" \
  -e MAILROOM_SESSION_KEY="$(openssl rand -hex 16)" \
  -e MAILROOM_OPERATOR_SUBJECT="operator" \
  -e MAILROOM_OPERATOR_EMAIL="operator@example.com" \
  -e MAILROOM_LOG_FORMAT=text \
  "$IMAGE" >/dev/null

wait_for "mailroom /readyz" 60 "$APP" \
  docker run --rm --network "$NET" "$CURL_IMAGE" -sf http://mailroom:8080/readyz

fails=0
pass() { printf 'ok    %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }

# role <subject> -> prints app.users.role, or empty if the subject has no row.
role() {
  docker exec "$PG" psql -U postgres -d mailroom -tAc \
    "SELECT role FROM app.users WHERE oidc_subject = '$1'" 2>/dev/null | tr -d '[:space:]'
}
user_count() { # count rows for a set of subjects (SQL IN-list, single-quoted)
  docker exec "$PG" psql -U postgres -d mailroom -tAc \
    "SELECT count(DISTINCT id) FROM app.users WHERE oidc_subject IN ($1)" 2>/dev/null | tr -d '[:space:]'
}

# login_session <sub> <claims-json> -> prints the /jmap/session body for a full
# Authorization-Code + PKCE login as <sub> with <claims-json> merged into the
# token, or the literal NO_SESSION when the callback set no session cookie.
login_session() {
  docker run -i --rm --network "$NET" --entrypoint sh "$CURL_IMAGE" -s "$1" "$2" <<'INNER'
set -u
sub="$1"; claims="$2"
BASE=http://mailroom:8080
jar=/tmp/jar; rm -f "$jar"

authz=$(curl -sS -c "$jar" -o /dev/null -w '%{redirect_url}' "$BASE/auth/start")
case "$authz" in
  http://oidc:8080/default/authorize*) ;;
  *) echo "NO_SESSION"; exit 0 ;;
esac
case "$authz" in *code_challenge=*) ;; *) echo "NO_SESSION"; exit 0 ;; esac

curl -sS -c "$jar" -b "$jar" "$authz" -o /tmp/idp.html
action=$(sed -n 's/.*<form[^>]*action="\([^"]*\)".*/\1/p' /tmp/idp.html | head -1)
[ -n "$action" ] || action="$authz"
case "$action" in
  http*) ;;
  /*) action="http://oidc:8080$action" ;;
  *) action="$authz" ;;
esac

cb=$(curl -sS -c "$jar" -b "$jar" -o /dev/null -w '%{redirect_url}' \
      --data-urlencode "username=$sub" --data-urlencode "claims=$claims" "$action")
case "$cb" in
  http://mailroom:8080/auth/callback*) ;;
  *) echo "NO_SESSION"; exit 0 ;;
esac

curl -sS -c "$jar" -b "$jar" -o /dev/null -D /tmp/cb.h "$cb"
grep -qi "set-cookie: mailroom_session=" /tmp/cb.h || { echo "NO_SESSION"; exit 0; }
curl -sS -b "$jar" "$BASE/jmap/session"
INNER
}

echo
echo "=== 1. the anonymous surface ==============================================="
docker run -i --rm --network "$NET" --entrypoint sh "$CURL_IMAGE" -s <<'CLIENT'
set -u
BASE=http://mailroom:8080
rc=0
code() { curl -sS -o /dev/null -w '%{http_code}' "$@"; }
expect() { if [ "$2" = "$3" ]; then printf 'ok    %-46s %s\n' "$1" "$3";
  else printf 'FAIL  %-46s got %s want %s\n' "$1" "$3" "$2"; rc=$((rc+1)); fi; }

expect "GET  /                 -> sign-in page" 302 "$(code $BASE/)"
expect "GET  /auth/login       -> the page"     200 "$(code $BASE/auth/login)"
expect "GET  /jmap/session     -> 401"          401 "$(code $BASE/jmap/session)"
expect "POST /jmap             -> 401"          401 "$(code -X POST $BASE/jmap)"
expect "GET  /.well-known/jmap -> 401"          401 "$(code $BASE/.well-known/jmap)"
expect "GET  /jmap/download    -> 401"          401 "$(code $BASE/jmap/download/a/b/c.pdf)"
expect "GET  /healthz          -> 200"          200 "$(code $BASE/healthz)"
expect "GET  /readyz           -> 200"          200 "$(code $BASE/readyz)"

curl -sS "$BASE/metrics" -o /tmp/metrics.txt
if grep -qi "operator@example" /tmp/metrics.txt; then
  printf 'FAIL  /metrics leaks an address\n'; rc=$((rc+1))
else printf 'ok    /metrics carries no address\n'; fi
exit "$rc"
CLIENT
[ $? -eq 0 ] || fails=$((fails + 1))

echo
echo "=== 2. a first-time subject is AUTO-PROVISIONED (app-binding is the gate) ==="
alice=$(login_session alice '{"email":"alice@example.com","name":"Alice Adams"}')
if printf '%s' "$alice" | grep -q '"capabilities"'; then
  pass "alice reaches a real JMAP Session on first login"
else
  fail "alice did not reach /jmap/session (got: $(printf '%s' "$alice" | head -c 80))"
fi
printf '%s' "$alice" | grep -q "alice@example.com" \
  && pass "the session reports alice's own email" \
  || fail "the session did not report alice's email"
[ "$(role alice)" = "user" ] \
  && pass "alice is provisioned with role=user" \
  || fail "alice role = '$(role alice)', want user"

echo
echo "=== 3. the mailroom-admin group grants admin (re-evaluated each login) ====="
boss=$(login_session boss '{"email":"boss@example.com","name":"Boss","groups":["staff","mailroom-admin"]}')
printf '%s' "$boss" | grep -q '"capabilities"' \
  && pass "boss reaches a real JMAP Session" \
  || fail "boss did not reach /jmap/session"
[ "$(role boss)" = "admin" ] \
  && pass "a mailroom-admin member gets role=admin" \
  || fail "boss role = '$(role boss)', want admin"

# Log in again with NO admin group: the role must revert on this login.
boss2=$(login_session boss '{"email":"boss@example.com","name":"Boss","groups":["staff"]}')
printf '%s' "$boss2" | grep -q '"capabilities"' \
  && pass "boss can still log in after losing the group" \
  || fail "boss re-login failed"
[ "$(role boss)" = "user" ] \
  && pass "removing the group demotes to role=user next login" \
  || fail "boss role = '$(role boss)' after demotion, want user"

echo
echo "=== 4. two subjects are two ISOLATED users ================================="
[ "$(user_count "'alice','boss'")" = "2" ] \
  && pass "alice and boss are two distinct app.users rows" \
  || fail "expected 2 distinct users for alice+boss, got $(user_count "'alice','boss'")"
if printf '%s' "$alice" | grep -q "boss@example.com"; then
  fail "alice's session leaked boss's address"
else
  pass "alice's session never names boss (each identity is its own)"
fi

echo
echo "=== 5. dev-auth escape hatch still works (no OIDC, no login) ==============="
docker run -d --name "$DEVAPP" --network "$NET" --network-alias devmailroom \
  -v "$VOL:/k:ro" \
  -e MAILROOM_DATABASE_URL="postgres://mailroom_app:app@pg:5432/mailroom?sslmode=disable" \
  -e MAILROOM_MASTER_KEY_FILE=/k/master \
  -e MAILROOM_DEV_AUTH_SUBJECT="dev-operator" \
  -e MAILROOM_DEV_AUTH_I_UNDERSTAND="this-instance-has-no-authentication" \
  -e MAILROOM_DEV_AUTH_EMAIL="dev@example.com" \
  -e MAILROOM_LOG_FORMAT=text \
  "$IMAGE" >/dev/null
devready=no
for _ in $(seq 1 60); do
  if docker run --rm --network "$NET" "$CURL_IMAGE" -sf http://devmailroom:8080/readyz >/dev/null 2>&1; then devready=yes; break; fi
  sleep 1
done
if [ "$devready" != yes ]; then
  fail "dev-auth instance never became ready"; docker logs "$DEVAPP" 2>&1 | tail -20
else
  # No cookie, no token: the request IS the operator.
  devcode=$(docker run --rm --network "$NET" "$CURL_IMAGE" -sS -o /tmp/dev.json -w '%{http_code}' http://devmailroom:8080/jmap/session)
  devbody=$(docker run --rm --network "$NET" "$CURL_IMAGE" -sS http://devmailroom:8080/jmap/session)
  [ "$devcode" = "200" ] \
    && pass "dev-auth serves /jmap/session with NO auth (200)" \
    || fail "dev-auth /jmap/session = $devcode, want 200"
  printf '%s' "$devbody" | grep -q "dev@example.com" \
    && pass "dev-auth session reports the fixed dev identity" \
    || fail "dev-auth session did not report dev@example.com"
fi

echo
if [ "$fails" -eq 0 ]; then
  echo "=== the gate holds and multi-user provisioning works end to end ==="
else
  echo "=== GATE/PROVISIONING FAILED: $fails assertion(s) ==="
  echo "--- mailroom (OIDC) logs ---";    docker logs "$APP"    2>&1 | tail -40
  echo "--- mailroom (dev-auth) logs ---"; docker logs "$DEVAPP" 2>&1 | tail -20
  exit 1
fi
