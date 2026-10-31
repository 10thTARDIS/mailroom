#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Sync engine integration harness (issue #4 / mailroom-spec.md ÃÂ§4):
#
# Brings up a throwaway Dovecot IMAP server, seeds a test account with
# INBOX plus the RFC 6154 SPECIAL-USE mailboxes (Sent/Drafts/Trash/Junk/
# Archive) and a handful of fixture messages, then runs
# `go test -tags integration -race ./internal/sync/...` in a golang
# container on the same docker network against it.
#
# CI wiring (not done by this script Ã¢â¬â orchestrator to add to
# .github/workflows/ci.yml):
#
#   sync-tests:
#     runs-on: ubuntu-latest
#     steps:
#       - uses: actions/checkout@v4
#       - name: Sync engine integration tests (live Dovecot)
#         run: sh scripts/sync-test.sh
#
# Everything goes through the Docker API only (docker create/cp/exec,
# container-to-container networking) Ã¢â¬â no published ports, no bind mounts,
# no host toolchain. Same shape as scripts/integration-test.sh and
# scripts/migrate-check.sh, for the same reason: the CI runner executes
# jobs in containers with a mounted docker socket, so host-port mappings and
# host paths do not resolve from inside a job. This works identically on the
# runner, Linux hosts, and Git Bash on Windows.
set -eu

# Keep Git Bash (MSYS) from rewriting container-side paths; host paths are
# passed in native form below. No-op on Linux.
MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
# Native host path for docker (C:/... under Git Bash, unchanged on Linux).
HOSTDIR="$(cd "$DIR" && { pwd -W 2>/dev/null || pwd; })"
DOVECOT_IMAGE="${MAILROOM_DOVECOT_IMAGE:-dovecot/dovecot:2.3.21}"
GO_IMAGE="${MAILROOM_GO_IMAGE:-golang:1.27}"

TESTUSER="sync-tester"
TESTPASS="sync-test-pass"
WRONGPASS="sync-test-wrong-pass"

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

# ⚠ `$$` IS NOT UNIQUE ACROSS CONCURRENT RUNS, AND THE PRE-CLEAN BELOW DELETES
# BY NAME — so a collision is one run reaping another's containers mid-test, not
# a harmless name clash. MEASURED on 2026-09-05: two restore-drill runs on
# different refs collided on PID 5055 and the victim died with
# `network mailroom-drill-5055 not found`. CI's concurrency group cancels only
# within a REF, so a post-merge run on main and a PR run overlap by design.
UNIQ="$$-$(od -An -N4 -tu4 < /dev/urandom 2>/dev/null | tr -dc '0-9' || date +%s)"
[ -n "${UNIQ#*-}" ] || UNIQ="$$-$(date +%s)"
WORK="$DIR/.sync-test-tmp.$UNIQ"
mkdir -p "$WORK"
# Native host form of $WORK, needed for `docker cp` (a real Windows-side
# docker.exe call) â same reason $HOSTDIR exists alongside $DIR above.
WORK_HOST="$(cd "$WORK" && { pwd -W 2>/dev/null || pwd; })"

NET="mailroom-st-$UNIQ"
# $$ is not unique across runner invocations, and a run cancelled mid-flight can
# leave its network behind â clear any stale one (and its containers) first.
docker ps -aq --filter "network=$NET" 2>/dev/null | xargs -r docker rm -fv >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null
DVCID=""
GOCID=""
cleanup() {
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

# --- Dovecot config -----------------------------------------------------
# Replaces the image's default /etc/dovecot/dovecot.conf entirely (the
# image's own default file says as much: "You should mount /etc/dovecot if
# you want to manage this file"). Key choices:
#   - passdb "static" with a single fixed password: any username
#     authenticates with TESTPASS, which is exactly what we want for a
#     disposable single-account test harness â no separate userdb/passwd
#     file to manage. A login with any *other* password is rejected with
#     [AUTHENTICATIONFAILED], which is what the auth-failure test exercises.
#   - disable_plaintext_auth = no + protocols = imap on plaintext port 143:
#     mirrors config.go's TLSInsecure mode, which its own doc comment
#     reserves for exactly this harness ("Only ever appropriate against a
#     local/test IMAP server (the Dovecot harness, issue #4)") â no TLS
#     certificates to manage for a throwaway container.
#   - namespace inbox { mailbox <name> { special_use = ... } } blocks give
#     RFC 6154 SPECIAL-USE attributes on Sent/Drafts/Trash/Junk/Archive once
#     those mailboxes are created below, which is what
#     ListMailboxes/SpecialUse() exercises.
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
  mailbox Sent {
    special_use = \\Sent
    auto = subscribe
  }
  mailbox Drafts {
    special_use = \\Drafts
    auto = subscribe
  }
  mailbox Trash {
    special_use = \\Trash
    auto = subscribe
  }
  mailbox Junk {
    special_use = \\Junk
    auto = subscribe
  }
  mailbox Archive {
    special_use = \\Archive
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

# --- Seed mailboxes + messages ------------------------------------------
for mb in INBOX Sent Drafts Trash Junk Archive; do
  docker exec "$DVCID" doveadm mailbox create -u "$TESTUSER" "$mb" >/dev/null
done

docker cp "$HOSTDIR/internal/sync/testdata/inbox-1.eml" "$DVCID:/tmp/inbox-1.eml"
docker cp "$HOSTDIR/internal/sync/testdata/inbox-2.eml" "$DVCID:/tmp/inbox-2.eml"
docker cp "$HOSTDIR/internal/sync/testdata/sent-1.eml" "$DVCID:/tmp/sent-1.eml"
docker exec "$DVCID" sh -c "doveadm save -u '$TESTUSER' -m INBOX < /tmp/inbox-1.eml"
docker exec "$DVCID" sh -c "doveadm save -u '$TESTUSER' -m INBOX < /tmp/inbox-2.eml"
docker exec "$DVCID" sh -c "doveadm save -u '$TESTUSER' -m Sent < /tmp/sent-1.eml"

echo "dovecot seeded: INBOX (2 msgs), Sent (1 msg), Drafts/Trash/Junk/Archive (special-use, empty)"

# --- Run the sync package's live integration tests ----------------------
# Named alias "dovecot" above is the connect target; port 143 is the
# server's plaintext IMAP listener (TLSInsecure per config.go). -race
# guards the concurrency property test (N goroutines each Select+Fetch on
# independent sessions).
# shellcheck disable=SC2086 â GOTESTFLAGS is intentionally word-split
GOCID=$(docker create --network "$NET" -w /src \
  -v mailroom-gomod:/go/pkg/mod \
  -v mailroom-gobuild:/root/.cache/go-build \
  -e MAILROOM_TEST_IMAP_HOST=dovecot \
  -e MAILROOM_TEST_IMAP_PORT=143 \
  -e MAILROOM_TEST_IMAP_USER="$TESTUSER" \
  -e MAILROOM_TEST_IMAP_PASS="$TESTPASS" \
  -e MAILROOM_TEST_IMAP_WRONG_PASS="$WRONGPASS" \
  "$GO_IMAGE" go test -tags integration -race -count=1 -p 1 ${GOTESTFLAGS:-} ./internal/sync/... ./internal/census/... ./internal/consolidate/...)
docker cp "$HOSTDIR/." "$GOCID:/src"
docker start -a "$GOCID"
echo "sync-test passed"
