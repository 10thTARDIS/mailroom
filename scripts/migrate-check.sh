#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Migration check (mailroom-spec.md Â§16 / ADR-008 / docs/migrations.md):
#
#   1. Apply db/migrations/*.sql to a clean PostgreSQL 18 via goose and
#      assert success.
#   2. Drift check: build a second clean PostgreSQL 18 straight from
#      db/schema.sql and prove `pg_dump --schema-only` of both databases
#      match. A nonzero diff means db/schema.sql and the migration history
#      have diverged â that must block merge, same as a red RLS test.
#
# Everything goes through the Docker API only (docker cp, docker exec,
# container-to-container networking) â no published ports, no bind mounts,
# no host toolchain â the same shape as scripts/integration-test.sh, for the
# same reason (CI runner jobs are themselves containers with a
# socket-mounted Docker; host ports/paths don't resolve from in here).
set -eu

# Keep Git Bash (MSYS) from rewriting container-side paths like /schema.sql;
# host paths are passed in native form below. No-op on Linux.
MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
# Native host path for docker (C:/â¦ under Git Bash, unchanged on Linux).
HOSTDIR="$(cd "$DIR" && { pwd -W 2>/dev/null || pwd; })"
PG_IMAGE="${MAILROOM_PG_IMAGE:-postgres:18}"
GO_IMAGE="${MAILROOM_GO_IMAGE:-golang:1.27}"
PW=mailroom-migrate-check

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

# Local scratch dir for the two pg_dump captures and the throwaway goose
# runner (below). Plain shell/coreutils (grep/sed/diff/cat) operate on the
# POSIX-style $WORK form; anything passed to `docker cp` as a *host* path
# needs the native-form $WORK_HOST instead (MSYS_NO_PATHCONV disables Git
# Bash's usual auto-conversion for docker.exe args â same reason $HOSTDIR
# exists above).
# ⚠ `$$` IS NOT UNIQUE ACROSS CONCURRENT RUNS, AND THE PRE-CLEAN BELOW DELETES
# BY NAME — so a collision is one run reaping another's containers mid-test, not
# a harmless name clash. MEASURED on 2026-09-05: two restore-drill runs on
# different refs collided on PID 5055 and the victim died with
# `network mailroom-drill-5055 not found`. CI's concurrency group cancels only
# within a REF, so a post-merge run on main and a PR run overlap by design.
UNIQ="$$-$(od -An -N4 -tu4 < /dev/urandom 2>/dev/null | tr -dc '0-9' || date +%s)"
[ -n "${UNIQ#*-}" ] || UNIQ="$$-$(date +%s)"
WORK="$DIR/.migrate-check-tmp.$UNIQ"
mkdir -p "$WORK"

NET="mailroom-mc-$UNIQ"
# $$ is not unique across runner invocations, and a run cancelled mid-flight can
# leave its network behind â clear any stale one (and its containers) first.
docker ps -aq --filter "network=$NET" 2>/dev/null | xargs -r docker rm -fv >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null
PGCID=$(docker run -d --network "$NET" --network-alias pg \
  -e POSTGRES_PASSWORD="$PW" -e POSTGRES_DB=postgres "$PG_IMAGE")
GOCID=""
cleanup() {
  docker rm -fv "$PGCID" >/dev/null 2>&1 || true
  [ -n "$GOCID" ] && docker rm -fv "$GOCID" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$WORK"
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
until docker exec "$PGCID" pg_isready -h 127.0.0.1 -U postgres -d postgres -q; do
  i=$((i + 1))
  [ "$i" -gt 60 ] && { echo "postgres did not start" >&2; exit 1; }
  sleep 1
done

# Two throwaway databases in the one Postgres instance: one built from the
# migration history, one built straight from the reference DDL.
docker exec "$PGCID" psql -v ON_ERROR_STOP=1 -q -U postgres -d postgres \
  -c "CREATE DATABASE mailroom_migrations;" \
  -c "CREATE DATABASE mailroom_schema;"

# --- 1. Apply db/schema.sql directly to mailroom_schema --------------------
docker cp "$HOSTDIR/db/schema.sql" "$PGCID:/schema.sql"
docker exec "$PGCID" psql -v ON_ERROR_STOP=1 -q -U postgres -d mailroom_schema -f /schema.sql
echo "db/schema.sql applied to mailroom_schema"

# --- 2. Apply db/migrations to mailroom_migrations via cmd/mailroom-migrate --
# Runs the real embedded-FS migrate binary inside a golang container on the
# same docker network â reproducible (no host Go toolchain), mirroring how
# scripts/integration-test.sh runs `go test` in a container. This exercises
# exactly the goose-over-embedded-FS path that ops runs in production.
GOCID=$(docker create --network "$NET" -w /src \
  -v mailroom-gomod:/go/pkg/mod \
  -v mailroom-gobuild:/root/.cache/go-build \
  -e MAILROOM_MIGRATE_DATABASE_URL="postgres://postgres:$PW@pg:5432/mailroom_migrations?sslmode=disable" \
  "$GO_IMAGE" go run ./cmd/mailroom-migrate up)
docker cp "$HOSTDIR/." "$GOCID:/src"
docker start -a "$GOCID"
echo "db/migrations applied to mailroom_migrations via cmd/mailroom-migrate"

# --- 3. Drift check: dump both schemas and diff -----------------------------
docker exec "$PGCID" pg_dump --schema-only --no-owner --no-privileges \
  -U postgres -d mailroom_migrations > "$WORK/dump-migrations.sql"
docker exec "$PGCID" pg_dump --schema-only --no-owner --no-privileges \
  -U postgres -d mailroom_schema > "$WORK/dump-schema.sql"

# Normalize: drop things that legitimately differ between the two databases
# without meaning the SCHEMAS differ â
#   - goose's own public.goose_db_version bookkeeping table (only exists in
#     the migrations-built database; not part of the application schema at
#     all, so it must not count as drift). Removed paragraph-wise (pg_dump
#     separates statements with a blank line) so the multi-line CREATE
#     TABLE / identity-sequence / constraint statements all go together.
#   - pg_dump's \restrict / \unrestrict lock tokens (PG16+ pg_dump), which
#     are random per invocation even for byte-identical schemas.
#   - pg_dump's version/comment banner lines and blank-line runs.
normalize() {
  awk -v RS='' -v ORS='\n\n' '!/goose_db_version/' "$1" \
    | grep -Ev '^-- (Dumped from|Dumped by|PostgreSQL database dump)' \
    | grep -Ev '^\\(restrict|unrestrict)( |$)' \
    | sed -e '/^--$/d' -e '/^[[:space:]]*$/d'
}
normalize "$WORK/dump-migrations.sql" > "$WORK/migrations.norm.sql"
normalize "$WORK/dump-schema.sql" > "$WORK/schema.norm.sql"

if ! diff -u "$WORK/schema.norm.sql" "$WORK/migrations.norm.sql" > "$WORK/drift.diff" 2>&1; then
  echo "DRIFT: db/schema.sql and db/migrations/*.sql produce different schemas." >&2
  echo "(- = db/schema.sql, + = db/migrations/*.sql via goose)" >&2
  cat "$WORK/drift.diff" >&2
  exit 1
fi

echo "migrate-check passed: migrations apply cleanly and match db/schema.sql"
