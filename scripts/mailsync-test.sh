#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Sync-engine end-to-end harness â the EXPUNGE REAPER (#86), the UIDVALIDITY ROLL
# (#88) and the GAP BACKFILL (#93). Spec Â§4.
#
# Brings up BOTH a throwaway PostgreSQL 18 (schema applied, RLS enforced,
# mailroom_app connectable) AND a throwaway Dovecot IMAP server on one docker
# network, then runs `go test -tags integration ./internal/mailsync/...` against
# the pair.
#
# Why both: these are the parts of Mailroom that reason about what a REMOTE SERVER
# does or does not hold, and getting that reasoning wrong destroys the user's mail
# â in both directions. The reaper concludes "the server no longer lists this UID,
# therefore the message is gone" and DELETES on it. The gap detector concludes "the
# server lists a UID below our own cursor that we never ingested, therefore our own
# FETCH silently lost a message" and RE-FETCHES on it. An in-memory double cannot
# tell you whether either inference survives contact with a real IMAP server â what
# Dovecot actually returns from UID SEARCH ALL on an emptied folder, what UIDNEXT it
# reports, what a UID MOVE does to the source UID, what a recreated folder does to
# UIDVALIDITY. So all three are proven against the real thing: a message expunged on
# a real server really does leave the Mailroom folder; a real sync failure really
# does reap nothing; and a message a truncated FETCH silently dropped really is
# found again and ingested, instead of being lost forever.
#
# CI wiring (orchestrator to add to .forgejo/workflows/ci.yml):
#
#   mailsync-tests:
#     runs-on: ubuntu-latest
#     steps:
#       - uses: actions/checkout@v4
#       - name: Sync engine end-to-end (live Dovecot + PostgreSQL)
#         run: sh scripts/mailsync-test.sh
#
# Everything goes through the Docker API only (docker create/cp/exec,
# container-to-container networking) â no published ports, no bind mounts, no
# host toolchain. Same shape and same reason as scripts/integration-test.sh and
# scripts/sync-test.sh: the Forgejo runner executes jobs in containers with a
# mounted docker socket, so host-port mappings and host paths do not resolve from
# inside a job. Works identically on the runner, on Linux, and in Git Bash on
# Windows.
set -eu

# Keep Git Bash (MSYS) from rewriting container-side paths; host paths are
# passed in native form below. No-op on Linux.
MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
HOSTDIR="$(cd "$DIR" && { pwd -W 2>/dev/null || pwd; })"
PG_IMAGE="${MAILROOM_PG_IMAGE:-postgres:18}"
DOVECOT_IMAGE="${MAILROOM_DOVECOT_IMAGE:-dovecot/dovecot:2.3.21}"
GO_IMAGE="${MAILROOM_GO_IMAGE:-golang:1.27}"

PW=mailroom-test
TESTUSER="reap-tester"
TESTPASS="reap-test-pass"

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

# ⚠ `$$` IS NOT UNIQUE ACROSS CONCURRENT RUNS, AND THE PRE-CLEAN BELOW DELETES
# BY NAME — so a collision is one run reaping another's containers mid-test, not
# a harmless name clash. MEASURED on 2026-09-05: two restore-drill runs on
# different refs collided on PID 5055 and the victim died with
# `network mailroom-drill-5055 not found`. CI's concurrency group cancels only
# within a REF, so a post-merge run on main and a PR run overlap by design.
UNIQ="$$-$(od -An -N4 -tu4 < /dev/urandom 2>/dev/null | tr -dc '0-9' || date +%s)"
[ -n "${UNIQ#*-}" ] || UNIQ="$$-$(date +%s)"
WORK="$DIR/.mailsync-test-tmp.$UNIQ"
mkdir -p "$WORK"
WORK_HOST="$(cd "$WORK" && { pwd -W 2>/dev/null || pwd; })"

NET="mailroom-mst-$UNIQ"
# $$ is not unique across runner invocations, and a run cancelled mid-flight can
# leave its network behind â clear any stale one (and its containers) first.
docker ps -aq --filter "network=$NET" 2>/dev/null | xargs -r docker rm -fv >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null
PGCID=""
DVCID=""
GOCID=""
cleanup() {
  [ -n "$PGCID" ] && docker rm -fv "$PGCID" >/dev/null 2>&1 || true
  [ -n "$DVCID" ] && docker rm -fv "$DVCID" >/dev/null 2>&1 || true
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

# --- PostgreSQL ---------------------------------------------------------
PGCID=$(docker run -d --network "$NET" --network-alias pg \
  -e POSTGRES_PASSWORD="$PW" -e POSTGRES_DB=mailroom "$PG_IMAGE")

echo "waiting for postgres..."
i=0
until docker exec "$PGCID" pg_isready -h 127.0.0.1 -U postgres -d mailroom -q; do
  i=$((i + 1))
  [ "$i" -gt 60 ] && { echo "postgres did not start" >&2; exit 1; }
  sleep 1
done

docker cp "$HOSTDIR/db/schema.sql" "$PGCID:/schema.sql"
docker exec "$PGCID" psql -v ON_ERROR_STOP=1 -q -U postgres -d mailroom -f /schema.sql
# Test-only bootstrap: the schema creates the roles NOLOGIN; the suite must
# connect as mailroom_app to prove the grants + RLS hold for the role the
# application actually uses.
docker exec "$PGCID" psql -v ON_ERROR_STOP=1 -q -U postgres -d mailroom \
  -c "ALTER ROLE mailroom_app LOGIN PASSWORD '$PW';" \
  -c "ALTER ROLE mailroom_admin LOGIN PASSWORD '$PW';"

# --- Dovecot ------------------------------------------------------------
# Same config shape as scripts/sync-test.sh (see its comments for the rationale
# behind the static passdb, plaintext auth, and the SPECIAL-USE namespace).
cat > "$WORK/dovecot.conf" <<EOF
mail_home=/srv/mail/%Lu
mail_location=sdbox:~/Mail
mail_uid=1000
mail_gid=1000

protocols = imap

first_valid_uid = 1000
last_valid_uid = 1000

passdb {
  driver = static
  args = password=$TESTPASS
}

disable_plaintext_auth = no

namespace inbox {
  inbox = yes
  separator = /
  mailbox Archive {
    special_use = \\Archive
    auto = subscribe
  }
  mailbox Sent {
    special_use = \\Sent
    auto = subscribe
  }
  mailbox Trash {
    special_use = \\Trash
    auto = subscribe
  }
  # Drafts carries SPECIAL-USE because the draft-retirement paths resolve it by
  # role, and liveDraftMailboxTargets matches special_use = '\\Drafts' only
  # (#686's end-to-end test, draft_expunge_integration_test.go).
  mailbox Drafts {
    special_use = \\Drafts
    auto = subscribe
  }
  # Junk carries SPECIAL-USE because "Not junk" is rescued OUT of it by role,
  # and the #576 test needs a folder whose messages carry the \$Junk keyword a
  # Thunderbird-style filter leaves behind (junk_rescue_integration_test.go).
  mailbox Junk {
    special_use = \\Junk
    auto = subscribe
  }
}

listen = *

log_path=/dev/stdout
info_log_path=/dev/stdout
debug_log_path=/dev/stdout
EOF

DVCID=$(docker create --network "$NET" --network-alias dovecot "$DOVECOT_IMAGE")
docker cp "$WORK_HOST/dovecot.conf" "$DVCID:/etc/dovecot/dovecot.conf"
docker start "$DVCID" >/dev/null

echo "waiting for dovecot..."
i=0
until docker exec "$DVCID" doveadm mailbox list -u "$TESTUSER" >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -gt 60 ] && { echo "dovecot did not start" >&2; docker logs "$DVCID" >&2 || true; exit 1; }
  sleep 1
done

# The folders the suite uses. Mailroom NEVER creates IMAP folders (SyncSession
# deliberately has no CREATE), so the harness owns them; the tests empty them
# between cases and take a fresh Postgres user each time, which is what keeps the
# suite order-independent without needing folder creation.
for mb in INBOX Archive Sent Trash Drafts Junk; do
  docker exec "$DVCID" doveadm mailbox create -u "$TESTUSER" "$mb" >/dev/null 2>&1 || true
done
echo "dovecot seeded: INBOX + Archive + Sent + Trash + Drafts + Junk (empty) for $TESTUSER"

# --- Run the mailsync end-to-end suite -----------------------------------
# -p 1 (serial packages) and the suite's own per-test mailbox reset are what let
# several tests share one IMAP account safely.
# shellcheck disable=SC2086 â GOTESTFLAGS is intentionally word-split
GOCID=$(docker create --network "$NET" -w /src \
  -v mailroom-gomod:/go/pkg/mod \
  -v mailroom-gobuild:/root/.cache/go-build \
  -e MAILROOM_TEST_SUPER_URL="postgres://postgres:$PW@pg:5432/mailroom" \
  -e MAILROOM_TEST_APP_URL="postgres://mailroom_app:$PW@pg:5432/mailroom" \
  -e MAILROOM_TEST_ADMIN_URL="postgres://mailroom_admin:$PW@pg:5432/mailroom" \
  -e MAILROOM_TEST_IMAP_HOST=dovecot \
  -e MAILROOM_TEST_IMAP_PORT=143 \
  -e MAILROOM_TEST_IMAP_USER="$TESTUSER" \
  -e MAILROOM_TEST_IMAP_PASS="$TESTPASS" \
  "$GO_IMAGE" go test -tags integration -count=1 -p 1 ${GOTESTFLAGS:-} ./internal/mailsync/...)
docker cp "$HOSTDIR/." "$GOCID:/src"
docker start -a "$GOCID"
echo "mailsync-test passed"
