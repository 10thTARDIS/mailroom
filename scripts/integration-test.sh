#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# RLS negative tests + store integration tests (mailroom-data-model.md Â§5).
#
# Brings up a throwaway PostgreSQL 18, applies db/schema.sql, makes the
# app/admin roles connectable (test-only bootstrap â production grants LOGIN
# out of band), then runs `go test -tags integration -count=1 -p 1 ./...`
# (every integration-tagged package, not only ./internal/store/...) inside a
# golang container on the same docker network. The run carries an explicit
# -timeout (default 20m, MAILROOM_IT_TIMEOUT to override) so a slow suite and a
# stuck one are different outcomes: go test's 10-minute default is too close to
# a loaded run of internal/store (~400s) to tell them apart (#855).
#
# Everything goes through the Docker API only (docker cp, docker exec,
# container-to-container networking) â no published ports, no bind mounts,
# no host toolchain. That is deliberate: the Forgejo runner executes jobs in
# containers with a mounted docker socket, so host-port mappings and host
# paths do not resolve from inside a job. This shape works identically on
# the runner, Linux hosts, and Git Bash on Windows.
set -eu

# Keep Git Bash (MSYS) from rewriting container-side paths; host paths are
# passed in native form below. No-op on Linux.
MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
HOSTDIR="$(cd "$DIR" && { pwd -W 2>/dev/null || pwd; })"
PG_IMAGE="${MAILROOM_PG_IMAGE:-postgres:18}"
GO_IMAGE="${MAILROOM_GO_IMAGE:-golang:1.27}"
PW=mailroom-test

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

# ⚠ `$$` IS NOT UNIQUE ACROSS CONCURRENT RUNS, AND THE PRE-CLEAN BELOW DELETES
# BY NAME — so a collision is one run reaping another's containers mid-test, not
# a harmless name clash. MEASURED on 2026-09-05: two restore-drill runs on
# different refs collided on PID 5055 and the victim died with
# `network mailroom-drill-5055 not found`. CI's concurrency group cancels only
# within a REF, so a post-merge run on main and a PR run overlap by design.
UNIQ="$$-$(od -An -N4 -tu4 < /dev/urandom 2>/dev/null | tr -dc '0-9' || date +%s)"
[ -n "${UNIQ#*-}" ] || UNIQ="$$-$(date +%s)"
NET="mailroom-it-$UNIQ"
# $$ is not unique across runner invocations, and a run cancelled mid-flight can
# leave its network behind â clear any stale one (and its containers) first.
docker ps -aq --filter "network=$NET" 2>/dev/null | xargs -r docker rm -fv >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null
PGCID=$(docker run -d --network "$NET" --network-alias pg \
  -e POSTGRES_PASSWORD="$PW" -e POSTGRES_DB=mailroom "$PG_IMAGE")
GOCID=""
cleanup() {
  docker rm -fv "$PGCID" >/dev/null 2>&1 || true
  [ -n "$GOCID" ] && docker rm -fv "$GOCID" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
# â  INT and TERM are NOT decoration: a CI cancellation sends SIGTERM, and a
# shell with only an EXIT trap dies WITHOUT running it. Measured in an alpine
# container â `trap cleanup EXIT` left the marker unwritten on SIGTERM, while
# `trap cleanup EXIT INT TERM` wrote it. That is how cancelled runs leaked
# long-lived side containers into the runners (mailroom#346); three were found
# aged 7h, 8h and 11h. cleanup is idempotent, so the EXIT trap firing again
# after the TERM trap is harmless.
trap cleanup EXIT INT TERM

# -h 127.0.0.1 (TCP) so the check cannot hit initdb's socket-only temp server.
echo "waiting for postgres..."
i=0
until docker exec "$PGCID" pg_isready -h 127.0.0.1 -U postgres -d mailroom -q; do
  i=$((i + 1))
  [ "$i" -gt 60 ] && { echo "postgres did not start" >&2; exit 1; }
  sleep 1
done

# â  THE SCHEMA IS APPLIED AS mailroom_owner, NOT AS THE SUPERUSER.
#
# This used to run as postgres, and that one line made CI structurally blind to
# every privilege property that depends on OWNERSHIP. In production
# `mailroom-migrate up` runs as mailroom_owner, so that role OWNS app.* and
# holds every privilege on it implicitly. Applying the schema as postgres makes
# the owner somebody else, so a grant that is decoration in production looks
# narrow in CI and every arrangement of GRANT/REVOKE tests the same.
#
# That blindness shipped two defects (#48's column GRANT, #257's column REVOKE),
# both green the whole way. Matching the deployed ownership model is what makes
# migration 00032's table-level revoke observable at all: with this, asking
# has_column_privilege(mailroom_owner, 'app.user_keys', 'private_key_ct') is a
# real question with a real answer.
#
# The bootstrap below is the part that genuinely needs the superuser, and only
# that part:
#   - CREATE EXTENSION citext (not a trusted extension, so it needs superuser).
#   - Creating mailroom_owner itself, with CREATEROLE so the schema's own
#     guarded CREATE ROLE blocks for mailroom_app/mailroom_admin succeed.
#
# Both statements in db/schema.sql are guarded (IF NOT EXISTS), so re-running
# them as a non-superuser is a NOTICE rather than an error â verified, not
# assumed.
docker exec "$PGCID" psql -v ON_ERROR_STOP=1 -q -U postgres -d mailroom \
  -c "CREATE EXTENSION IF NOT EXISTS citext;" \
  -c "DO \$\$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'mailroom_owner') THEN CREATE ROLE mailroom_owner LOGIN PASSWORD '$PW' CREATEROLE; END IF; END \$\$;" \
  -c "GRANT ALL ON SCHEMA public TO mailroom_owner;" \
  -c "GRANT ALL ON DATABASE mailroom TO mailroom_owner;"

docker cp "$HOSTDIR/db/schema.sql" "$PGCID:/schema.sql"
# -h 127.0.0.1 because a non-postgres role cannot use the image's peer-auth
# socket path.
docker exec -e PGPASSWORD="$PW" "$PGCID" \
  psql -v ON_ERROR_STOP=1 -q -h 127.0.0.1 -U mailroom_owner -d mailroom -f /schema.sql

# Test-only bootstrap: the schema creates mailroom_app / mailroom_admin as
# NOLOGIN; the suite must connect as them to prove the grants + RLS hold for
# the roles the application actually uses.
docker exec "$PGCID" psql -v ON_ERROR_STOP=1 -q -U postgres -d mailroom \
  -c "ALTER ROLE mailroom_app LOGIN PASSWORD '$PW';" \
  -c "ALTER ROLE mailroom_admin LOGIN PASSWORD '$PW';" \
  -c "ALTER DATABASE mailroom SET lock_timeout = '30s';" \
  -c "ALTER DATABASE mailroom SET statement_timeout = '120s';"

# (#855) A lock wait or a runaway statement in a test must FAIL, not hang: one
# once sat 10 minutes in a change_log INSERT with nothing to say what it was
# waiting on. These are database-level defaults, so they reach every connection
# every integration package opens (all four roles, every pool) without touching
# any DSN, and they exist ONLY in this throwaway database, never in production
# configuration. A lock_timeout error names the statement that waited; the store
# harness also dumps who held the lock when a test fails (dumpBlockers).

# Create the test container, copy the source tree in, run attached (exit
# code propagates). Named volumes keep the module/build caches warm.
# shellcheck disable=SC2086 â GOTESTFLAGS is intentionally word-split
GOCID=$(docker create --network "$NET" -w /src \
  -v mailroom-gomod:/go/pkg/mod \
  -v mailroom-gobuild:/root/.cache/go-build \
  -e MAILROOM_TEST_SUPER_URL="postgres://postgres:$PW@pg:5432/mailroom" \
  -e MAILROOM_TEST_APP_URL="postgres://mailroom_app:$PW@pg:5432/mailroom" \
  -e MAILROOM_TEST_ADMIN_URL="postgres://mailroom_admin:$PW@pg:5432/mailroom" \
  -e MAILROOM_TEST_OWNER_URL="postgres://mailroom_owner:$PW@pg:5432/mailroom" \
  "$GO_IMAGE" go test -tags integration -count=1 -p 1 -timeout "${MAILROOM_IT_TIMEOUT:-20m}" ${GOTESTFLAGS:-} ./...)
docker cp "$HOSTDIR/." "$GOCID:/src"
docker start -a "$GOCID"
echo "integration suite passed"
