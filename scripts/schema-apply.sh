#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Apply db/schema.sql to a clean PostgreSQL 18 — the first CI job named by
# mailroom-data-model.md §8. Throwaway container via the local Docker; no
# psql install needed on the host. Works on Linux CI and Git Bash on Windows.
set -eu

# Keep Git Bash (MSYS) from rewriting container-side paths like /schema.sql;
# host paths are passed in native form below. No-op on Linux.
MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
# Native host path for docker (C:/… under Git Bash, unchanged on Linux).
HOSTDIR="$(cd "$DIR" && { pwd -W 2>/dev/null || pwd; })"
IMAGE="${MAILROOM_PG_IMAGE:-postgres:18}"

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

CID=$(docker run -d -e POSTGRES_PASSWORD=schema-apply -e POSTGRES_DB=mailroom "$IMAGE")
# EXIT alone does not fire on a CI cancellation (SIGTERM) — see mailroom#346.
trap 'docker rm -fv "$CID" >/dev/null 2>&1 || true' EXIT INT TERM

# -h 127.0.0.1 (TCP) so the check cannot hit initdb's socket-only temp server.
echo "waiting for postgres..."
i=0
until docker exec "$CID" pg_isready -h 127.0.0.1 -U postgres -d mailroom -q; do
  i=$((i + 1))
  [ "$i" -gt 60 ] && { echo "postgres did not start" >&2; exit 1; }
  sleep 1
done

docker cp "$HOSTDIR/db/schema.sql" "$CID:/schema.sql"
docker exec "$CID" psql -v ON_ERROR_STOP=1 -q -U postgres -d mailroom -f /schema.sql
echo "db/schema.sql applied cleanly to $IMAGE"
