#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Mailroom backup — produce ONE encrypted, off-box-able artifact of the RESTORE
# UNIT (spec §11, ADR-005, Forgejo #40).
#
# The restore unit is: PostgreSQL + the blob store + the wrapped key store.
#
#   * PostgreSQL — the system of record. pg_dump custom format.
#   * The blob store — every message's raw MIME, already envelope-encrypted at
#     rest under a key DERIVED from the master key. Copied verbatim; this script
#     never decrypts anything.
#   * The wrapped key store — in Mailroom 0.5 this lives INSIDE Postgres
#     (app.accounts.credentials_ct, app.user_keys.private_key_ct,
#     app.api_keys.api_key_ct), so the dump carries it. There is no separate
#     on-disk key store to copy. The manifest records that fact so a future
#     release that splits keys out cannot quietly leave them behind.
#
# Deliberately NOT in the artifact:
#
#   * THE SEARCH INDEX (spec §7/§11). It is a disposable cache, rebuilt from
#     Postgres. A restored-but-stale index is worse than no index, so it is not
#     backed up — the restore drill REBUILDS it, which is the only honest proof
#     that it can be rebuilt.
#
#   * THE MASTER KEY. This is the line the whole design rests on: an artifact
#     that carried both the ciphertext and the key that opens it would not be a
#     backup, it would be a copy. The master key is escrowed SEPARATELY and
#     OFFLINE (ADR-005). This script never reads it, and refuses to run if it
#     finds anything that looks like key material inside the payload.
#
# THE TRAP THIS SCRIPT EXISTS TO CLOSE
# ------------------------------------
# Every app table is `FORCE ROW LEVEL SECURITY` and every policy keys off
# app.current_user_id(), which is NULL unless the transaction set it — and
# `pg_dump` never sets it. So the mail is invisible to any role without BYPASSRLS,
# INCLUDING `mailroom_owner`: the role that owns the schema, that migrations
# already run as, and therefore the single most natural role to reach for.
#
# What actually happens then (measured against PostgreSQL 16, and RE-MEASURED on
# 18 for #351 — same result, exit 1 with "query would be affected by row-level
# security policy"):
# pg_dump FAILS LOUDLY — `ERROR: query would be affected by row-level security
# policy for table "users"` — and exits non-zero. Postgres is on our side here,
# and it is worth being precise about that rather than repeating a scarier story:
# an operator who believes pg_dump silently emits empty backups will distrust the
# wrong thing. (Pass --enable-row-security and it WOULD quietly dump only the rows
# the role can see, i.e. none. This script never does, and never should.)
#
# The preflight below therefore is not what saves us from silence — it is what
# turns an obscure mid-dump error into a refusal that names the actual problem
# ("this role cannot see the mail; use one with BYPASSRLS") before any work is
# done. The zero-users check after the dump is the backstop for the day someone
# adds --enable-row-security, or a future Postgres relaxes that error.
#
# Usage (Docker API only — no bind mounts, no published ports; see
# scripts/integration-test.sh for why). The two *_CONTAINER values are RAW
# container names, `<project>-<service>-1`: the ones below are for the default
# project (`name: mailroom-deploy` in deploy/compose.yml); a stack brought up
# with `-p` has different ones, so read them from
# `docker compose -f deploy/compose.yml ps` (#544):
#
#   MAILROOM_BACKUP_DSN='postgres://postgres:...@127.0.0.1:5432/mailroom?sslmode=disable' \
#   MAILROOM_BACKUP_PG_CONTAINER=mailroom-deploy-postgres-1 \
#   MAILROOM_BACKUP_BLOB_CONTAINER=mailroom-deploy-mailroom-1 \
#   MAILROOM_BACKUP_RECIPIENTS=/path/to/backup-recipients.txt \
#   MAILROOM_BACKUP_OUT=/backups/mailroom.age \
#   sh scripts/backup.sh
#
# The recipients file holds age PUBLIC keys. The box being backed up therefore
# holds no key that can decrypt its own backups — the private half lives with
# whoever restores. `age-keygen` makes the pair; see docs/backup-restore-runbook.md.
#
# Optional:
#
#   MAILROOM_BACKUP_KEEP=7    keep 7 dated copies beside $OUT (default 0: none)
#
#   sh scripts/backup.sh --self-test
#     Proves the pinned-image property #633 is about: that MAILROOM_AGE_IMAGE
#     can seal AND open an age artifact with NO network at all. Needs only
#     MAILROOM_AGE_IMAGE and docker — no database, no real backup. Runs in CI
#     (docker-image job) right after the image it will test is built.
#
# NOTHING HERE SCHEDULES ITSELF (#539). This script runs when something calls
# it, and until v1.70.0 nothing ever did — there was no timer, no unit, no
# compose service, and no artifact had ever been produced on a deployed instance.
# A scheduled caller belongs in the host's configuration, not here, because the
# invocation needs docker access and a BYPASSRLS DSN that should not live in the
# compose environment. What DOES live here is the other half of a backup you can
# trust: bounded retention (below) and scripts/backup-freshness.sh, the check
# that goes red when runs stop.
set -eu

# Keep Git Bash (MSYS) from rewriting CONTAINER-side paths into Windows ones
# (/payload -> C:/Program Files/Git/payload). Host paths are passed in native
# form via host_path() below. No-op on Linux and on the CI runner.
MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

PG_IMAGE="${MAILROOM_PG_IMAGE:-postgres:18}"
# AGE_IMAGE is resolved further down, once BLOB_CONTAINER has been validated
# (see the comment there): the preferred default is derived from it, not a
# literal here.

# The in-container `apk add` retries a transient resolver failure (#577): a 03:00
# backup must not die of one DNS blip. Bounded — 5 attempts, 30 s at most — so a
# resolver that stays down still fails the backup with apk's own error. Each
# attempt is bounded too (#586), because a fetch that STALLS never exits and so
# never reaches the retry loop at all. Identical to the copy in
# scripts/restore-drill.sh, which explains it at length.
NET_RETRY='RETRY_TIMEOUT=${RETRY_TIMEOUT:-180}
retry() {
  n=1
  while :; do
    # ⚠ BOUNDING EACH ATTEMPT IS A SEPARATE PROPERTY FROM RETRYING IT (#586).
    # This loop only ever sees a command that RETURNS. A fetch that STALLS never
    # returns, so without the timeout the helper is inert exactly when it looks
    # busiest: the restore drill sat inside `apk add` for 18+ minutes with a
    # byte-identical log, never printing one retry line, until the run was
    # killed. timeout converts the stall into an ordinary non-zero exit, which
    # the rest of this loop already knows how to handle.
    #
    # ⚠ rc MUST be captured in the else branch. After `fi`, $? is the status of
    # the IF STATEMENT (0), not of the condition. The first version read `rc=$?`
    # after `fi`, so every message said rc=0 and the 124|143 case below was dead
    # code that looked correct — the probe is what caught it, not review.
    if timeout "$RETRY_TIMEOUT" "$@"; then
      return 0
    else
      rc=$?
    fi
    # MEASURED, because it differs by image and this helper runs in both:
    # busybox timeout exits 143 (alpine), GNU coreutils exits 124 (golang).
    # Both are non-zero so the retry fires either way — but a message that knows
    # only one of them misreports a timeout as a plain failure in the other.
    case $rc in
      124|143) why="TIMED OUT after ${RETRY_TIMEOUT}s" ;;
      *)       why="failed (rc=$rc)" ;;
    esac
    if [ "$n" -ge 5 ]; then echo "retry: gave up after $n attempts, last $why: $*" >&2; return 1; fi
    echo "retry: attempt $n $why, retrying in $((n * 3))s: $*" >&2
    sleep $((n * 3))
    n=$((n + 1))
  done
}
'

# AGE_GUARD — "install age only when it is missing", written ONCE and shared by
# the real sealer (step 7) and --self-test (below). Both run it inside a
# container built from $AGE_IMAGE.
#
# ⚠ IT IS ONE COPY ON PURPOSE, AND THAT IS WHAT MAKES THE SELF-TEST WORTH
# RUNNING. The guard is the whole of #633's script half: an `apk add` that runs
# unconditionally reaches the network even when age is already installed, so a
# pinned, age-bearing image would still fail at 03:00 on a host with no egress —
# the exact failure the pin exists to prevent. --self-test proves that offline
# seal works; if it exercised its own private copy of this decision, someone
# could delete the guard from step 7 and the test would stay green while
# production broke. Two vocabularies drifting apart with nothing to pin them
# together is how #623 shipped. So: change this, and both paths change.
#
# The INSTALL ITSELF is not shared, because the two callers need different
# behaviour and each defines `age_install` before running this:
#   * step 7 wraps it in `retry` (#577/#586) — a real backup must survive a DNS
#     blip at 03:00;
#   * --self-test calls apk directly — offline there is the EXPECTED outcome of
#     its negative control, and retrying a guaranteed failure five times would
#     only make the control slow.
AGE_GUARD='if command -v age >/dev/null 2>&1; then
  echo "age: preinstalled" >&2
else
  echo "age: installing" >&2
  age_install
fi
'

DSN="${MAILROOM_BACKUP_DSN:-}"
PG_CONTAINER="${MAILROOM_BACKUP_PG_CONTAINER:-}"
PG_NETWORK="${MAILROOM_BACKUP_NETWORK:-}"
BLOB_CONTAINER="${MAILROOM_BACKUP_BLOB_CONTAINER:-}"
BLOB_VOLUME="${MAILROOM_BACKUP_BLOB_VOLUME:-}"
BLOB_PATH="${MAILROOM_BACKUP_BLOB_PATH:-/var/lib/mailroom/blobs}"
RECIPIENTS="${MAILROOM_BACKUP_RECIPIENTS:-}"
OUT="${MAILROOM_BACKUP_OUT:-}"
ALLOW_EMPTY="${MAILROOM_BACKUP_ALLOW_EMPTY:-0}"
# How many DATED copies to keep beside $OUT. 0 (the default) keeps none, which
# is the behaviour every existing caller has — scripts/restore-drill.sh takes a
# throwaway fixture backup and wants no history. A scheduled backup should set
# it; see the retention section of docs/backup-restore-runbook.md.
KEEP="${MAILROOM_BACKUP_KEEP:-0}"

DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"

fail() { echo "backup: $*" >&2; exit 1; }

# Native host path for docker cp (C:/... under Git Bash, unchanged on Linux).
host_path() {
  d=$(dirname -- "$1"); b=$(basename -- "$1")
  (cd "$d" && printf '%s/%s\n' "$(pwd -W 2>/dev/null || pwd)" "$b")
}

# --- --self-test: proves the property #633 is about, and only that ----------
# Not "does age work" (upstream's problem) — "can THIS image seal AND open an
# artifact with NO network at all", which is the whole reason to pin
# MAILROOM_AGE_IMAGE instead of leaving the sealer to `apk add` at 03:00.
#
# Deliberately independent of the real sealer's PACKER container (step 7
# below): that container needs $RECIPIENTS, $WORK and a live backup run to
# exist first, none of which this mode has or wants. Testing the IMAGE
# directly — install-if-missing, generate an identity, seal, open, round-trip
# — proves the same property with less to go wrong, and runs standalone in CI
# right after the image that will BECOME MAILROOM_AGE_IMAGE is built (see
# ci.yml). It DOES embed the same install-only-when-missing guard as step 7
# (see ST_SCRIPT below) — that part is shared on purpose, not independent,
# because that guard is exactly what this mode has to prove is in effect.
#
# ⚠ EVERY docker run BELOW PASSES --entrypoint sh. Without it, an image whose
# ENTRYPOINT is /mailroom (this repo's own production image, the expected
# MAILROOM_AGE_IMAGE per #633) would run `/mailroom sh -c '...'` instead of the
# test script — docker APPENDS the command to the entrypoint, it does not
# replace it. This exact trap is called out at length in the Dockerfile's own
# header; a self-test that forgot it here would be a second, quieter copy of
# the bug it exists to prevent.
self_test() {
  IMG="${MAILROOM_AGE_IMAGE:-}"
  [ -n "$IMG" ] || fail "--self-test requires MAILROOM_AGE_IMAGE (the sealing image to prove offline-capable)"
  CONTROL_IMG="alpine:3.20"

  ST_LABEL="mailroom-backup-selftest-$$"
  st_cleanup() {
    ids="$(docker ps -aq --filter "label=$ST_LABEL" 2>/dev/null || true)"
    # shellcheck disable=SC2086 — ids is intentionally word-split
    [ -n "$ids" ] && docker rm -f $ids >/dev/null 2>&1
    return 0
  }
  # Same reasoning as the main script's trap (above): SIGTERM (a cancelled CI
  # job) skips an EXIT-only trap, and this mode creates containers same as the
  # real run does.
  trap st_cleanup EXIT INT TERM

  echo "backup --self-test: proving $IMG can seal AND open an age artifact with NO network (#633)"
  echo "backup --self-test: pulling the negative-control image ($CONTROL_IMG) FIRST, while the"
  echo "backup --self-test: network is up — so a later failure means 'age is missing', not"
  echo "backup --self-test: 'the image could not be pulled'"
  docker pull "$CONTROL_IMG" >/dev/null \
    || fail "could not pull $CONTROL_IMG — the self-test needs network for THIS step (not the offline runs below); see the header"
  # $IMG is normally freshly built by the caller (ci.yml tags it, then sets
  # MAILROOM_AGE_IMAGE to that tag) and so has no upstream copy to pull at
  # all — that is expected, not a failure, which is why this is best-effort.
  docker pull "$IMG" >/dev/null 2>&1 || true

  # ⚠ THIS EMBEDS THE SAME "INSTALL ONLY WHEN MISSING" GUARD AS THE REAL
  # SEALER (step 7, below) — deliberately, not by coincidence. An earlier
  # version of this self-test skipped straight to age-keygen, which proved
  # only "did MAILROOM_AGE_IMAGE happen to have age already", NOT the thing
  # #633 actually asks for: that a MISSING guard (an `apk add` that always
  # runs) is a regression THIS test would catch.
  #
  # It is $AGE_GUARD ITSELF, the same string step 7 runs — not a copy of it.
  # Delete the guard there and this test goes red, which is the only reason a
  # green run here says anything about the real sealer (see AGE_GUARD's own
  # comment). Only `age_install` differs, and it is defined per caller: no
  # `retry`/NET_RETRY here on purpose, because under --network none an apk
  # fetch fails immediately rather than stalling, and retrying a guaranteed
  # failure five times would only make the negative control slow.
  #
  # The whole property in one script: install if missing, make a keypair, seal
  # a KNOWN plaintext to it, open the seal, and assert the bytes came back
  # unchanged. sha256sum rather than cmp/diff — busybox ships the former in
  # every alpine image this runs against and the comparison needs nothing else.
  #
  # age-keygen's own "Public key: ..." chatter on stderr is left to flow
  # through to the caller (not redirected to a file inside the container) ON
  # PURPOSE: the caller only greps for ROUNDTRIP_OK, so that chatter is
  # harmless noise on success — but on FAILURE it is the only place "command
  # not found" or an age/apk error would show up. A file-redirect here once
  # swallowed exactly that message, so a failing case reported an empty
  # diagnostic instead of the reason.
  ST_SCRIPT='set -eu
age_install() { apk add --no-cache age >/dev/null; }
'"$AGE_GUARD"'
d="${TMPDIR:-/tmp}/mailroom-backup-selftest.$$"
mkdir -p "$d"
age-keygen -o "$d/id.txt"
age-keygen -y "$d/id.txt" > "$d/recipient.txt"
printf "mailroom backup self-test plaintext\n" > "$d/plain.txt"
age -R "$d/recipient.txt" -o "$d/sealed.age" "$d/plain.txt"
age -d -i "$d/id.txt" -o "$d/opened.txt" "$d/sealed.age"
[ "$(sha256sum < "$d/plain.txt")" = "$(sha256sum < "$d/opened.txt")" ] \
  || { echo "ROUNDTRIP MISMATCH: opened bytes differ from what was sealed" >&2; exit 1; }
echo ROUNDTRIP_OK
'

  ST_FAILS=0

  echo
  echo "--- positive: $IMG must seal+open with --network none"
  OUT1="$(docker run --network none --entrypoint sh --label "$ST_LABEL" "$IMG" -c "$ST_SCRIPT" 2>&1)" && RC1=0 || RC1=$?
  if [ "$RC1" -eq 0 ] && printf '%s' "$OUT1" | grep -q ROUNDTRIP_OK; then
    echo "ok    $IMG sealed and opened a plaintext offline"
  else
    echo "FAIL  $IMG did NOT round-trip offline (exit $RC1):"
    echo "$OUT1" | sed 's/^/      /'
    ST_FAILS=$((ST_FAILS + 1))
  fi

  echo
  echo "--- negative control: $CONTROL_IMG has no age, so this MUST fail offline"
  OUT2="$(docker run --network none --entrypoint sh --label "$ST_LABEL" "$CONTROL_IMG" -c "$ST_SCRIPT" 2>&1)" && RC2=0 || RC2=$?
  if [ "$RC2" -ne 0 ]; then
    echo "ok    $CONTROL_IMG failed offline as expected (exit $RC2) — the harness can observe a real failure"
  else
    echo "FAIL  $CONTROL_IMG (no age preinstalled) SUCCEEDED offline."
    echo "      That means this self-test proves NOTHING: without a control that can go"
    echo "      red, a pass above cannot be told apart from a check that always passes."
    ST_FAILS=$((ST_FAILS + 1))
  fi

  echo
  if [ "$ST_FAILS" != 0 ]; then
    echo "BACKUP SELF-TEST FAILED ($ST_FAILS case(s))"
    exit 1
  fi
  echo "backup self-test: $IMG seals and opens age artifacts with NO network at all,"
  echo "backup self-test: and $CONTROL_IMG (no age) proves that claim could have failed"
}

command -v docker >/dev/null 2>&1 || fail "docker is required"

if [ "${1:-}" = "--self-test" ]; then
  self_test
  exit 0
fi

[ -n "$DSN" ]        || fail "MAILROOM_BACKUP_DSN is required (a role that BYPASSES RLS — see the header)"
[ -n "$RECIPIENTS" ] || fail "MAILROOM_BACKUP_RECIPIENTS is required (a file of age public keys)"
[ -r "$RECIPIENTS" ] || fail "cannot read MAILROOM_BACKUP_RECIPIENTS=$RECIPIENTS"
[ -n "$OUT" ]        || fail "MAILROOM_BACKUP_OUT is required (where to write the artifact)"
[ -n "$PG_CONTAINER" ] || [ -n "$PG_NETWORK" ] || \
  fail "set MAILROOM_BACKUP_PG_CONTAINER (exec pg_dump in it) or MAILROOM_BACKUP_NETWORK (throwaway client)"
[ -n "$BLOB_CONTAINER" ] || [ -n "$BLOB_VOLUME" ] || \
  fail "set MAILROOM_BACKUP_BLOB_CONTAINER or MAILROOM_BACKUP_BLOB_VOLUME"
# Validated here rather than tested numerically at the point of use: `[ "$KEEP"
# -gt 0 ]` on a non-numeric value is an error the shell would swallow, so
# MAILROOM_BACKUP_KEEP=seven would quietly mean "keep nothing". A typo that
# silently disables retention is precisely the failure this issue is about.
case "$KEEP" in
  ''|*[!0-9]*) fail "MAILROOM_BACKUP_KEEP must be a whole number of copies, got '$KEEP'" ;;
esac

# --- Resolve the sealing image (#633) ----------------------------------------
# AGE_IMAGE is what steps 4 and 7 below run `age` inside — and until now it
# defaulted to plain `alpine:3.20`: neither pinned (a tag, not a digest) nor
# age-bearing, so it always fell down `apk add`'s networked path. This
# resolves it in three steps, in order of trust:
#
#   1. MAILROOM_AGE_IMAGE, if the caller set one — an explicit pin wins,
#      always.
#   2. Otherwise, the image $BLOB_CONTAINER is ALREADY RUNNING. That container
#      runs the Mailroom image, which now carries age at a digest the
#      deployment already pinned (the Dockerfile) — so this needs no new
#      configuration and no new egress: the image is already on the box.
#      Only available when MAILROOM_BACKUP_BLOB_CONTAINER (not …_BLOB_VOLUME)
#      named a live container to ask.
#   3. `alpine:3.20`, unpinned and age-less, exactly today's behaviour — the
#      safety net for a caller that set neither, so this script still runs
#      somewhere rather than refusing outright. Read the runbook (§3a) before
#      relying on this path unattended: it needs egress at backup time.
if [ -n "${MAILROOM_AGE_IMAGE:-}" ]; then
  AGE_IMAGE="$MAILROOM_AGE_IMAGE"
  echo "backup: sealing image $AGE_IMAGE (from MAILROOM_AGE_IMAGE)"
elif [ -n "$BLOB_CONTAINER" ] && \
     AGE_IMAGE="$(docker inspect -f '{{.Config.Image}}' "$BLOB_CONTAINER" 2>/dev/null)" && \
     [ -n "$AGE_IMAGE" ]; then
  echo "backup: sealing image $AGE_IMAGE (from the blob container $BLOB_CONTAINER)"
else
  AGE_IMAGE="alpine:3.20"
  echo "backup: sealing image $AGE_IMAGE (fallback — unpinned, needs egress; set MAILROOM_AGE_IMAGE to avoid this)"
fi

umask 077
WORK="$DIR/.backup-tmp.$$"
PAYLOAD="$WORK/payload"
mkdir -p "$PAYLOAD/blobs"
WORK_HOST="$(host_path "$WORK")"

LABEL="mailroom-backup=$$"          # every container this run creates carries it
CLIENT="mailroom-backup-client-$$"  # the throwaway psql/pg_dump client, by NAME

cleanup() {
  # Reap by LABEL rather than by remembered ids: helpers get created inside
  # command substitutions (subshells), so an id appended to a shell variable
  # there never reaches this function. The daemon-side label does.
  ids="$(docker ps -aq --filter "label=$LABEL" 2>/dev/null || true)"
  # shellcheck disable=SC2086 — ids is intentionally word-split
  [ -n "$ids" ] && docker rm -f $ids >/dev/null 2>&1
  rm -rf "$WORK"
  return 0
}
# ⚠ INT and TERM are NOT decoration: a CI cancellation sends SIGTERM, and a
# shell with only an EXIT trap dies WITHOUT running it. Measured in an alpine
# container — `trap cleanup EXIT` left the marker unwritten on SIGTERM, while
# `trap cleanup EXIT INT TERM` wrote it. That is how cancelled runs leaked
# long-lived side containers into the runners (mailroom#346); three were found
# aged 7h, 8h and 11h. cleanup is idempotent, so the EXIT trap firing again
# after the TERM trap is harmless.
trap cleanup EXIT INT TERM

# pg_run SCRIPT — run a /bin/sh script with psql+pg_dump against $DSN, either by
# exec'ing into the live Postgres container or by starting a throwaway client on
# the deployment's docker network. Either way the DSN never leaves the daemon.
# The script's stdout is the caller's to do with as it likes — including
# redirecting it straight to a host file (see step 2 below), which is what
# lets this script stream the dump instead of staging it in the container.
#
# The client is addressed by NAME, not by a captured id: pg_run is called inside
# a command substitution for the preflight check, and an id assigned to a global
# from in there would vanish with the subshell.
pg_run() {
  if [ -n "$PG_CONTAINER" ]; then
    docker exec -e PG_DSN="$DSN" "$PG_CONTAINER" sh -c "$1"
  else
    docker rm -f "$CLIENT" >/dev/null 2>&1 || true
    docker run --name "$CLIENT" --label "$LABEL" --network "$PG_NETWORK" \
      -e PG_DSN="$DSN" "$PG_IMAGE" sh -c "$1"
  fi
}

# --- 1. Preflight: can this role actually SEE the mail? ----------------------
# Fail here, naming the real problem, rather than partway through a dump with an
# error about row-level security on some arbitrary table.
echo "backup: preflight — does the dump role bypass RLS?"
BYPASS="$(pg_run '
set -eu
psql "$PG_DSN" -At -v ON_ERROR_STOP=1 -c \
  "SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user"
' | tr -d "\r\n ")"

if [ "$BYPASS" != "t" ]; then
  cat >&2 <<'RLS'
backup: REFUSING TO DUMP.

  The role in MAILROOM_BACKUP_DSN does NOT bypass row-level security.

  Every Mailroom table is FORCE ROW LEVEL SECURITY, and every policy keys off
  app.current_user_id() — which pg_dump never sets. This role therefore cannot
  see one row of mail. pg_dump would fail partway through with an error about
  row-level security on whichever table it reached first; this refusal just says
  so up front, before anything is written.

  This applies to mailroom_owner too: owning the tables is not enough, because
  the RLS is FORCED. Dump as a superuser (or a role with BYPASSRLS).
RLS
  exit 1
fi
echo "backup: preflight OK — the dump role bypasses RLS, so it can see the mail"

# --- 2. Dump Postgres, record the schema version and the row counts ----------
# STREAMED straight to host files, not staged under /tmp inside the container
# first (#634). The old shape (`pg_dump -f /tmp/mr.dump`, then `docker cp` it
# out and `rm` it) wrote the plaintext dump — subjects, snippets, sender
# addresses, the lot — to the container's OWN writable layer before this
# script ever touched it. That layer is not necessarily on encrypted storage:
# from Docker 29 on, the containerd image store is the default, and a writable
# layer is then a containerd snapshot under /var/lib/containerd, a completely
# different directory tree from Docker's data root (deploy/README.md §0a).
# `postgres` also cannot run read-only (needs its own scratch paths), so it
# was the one service in the stack that could still do this.
#
# `docker exec`/`docker run` stream stdout live and exit with the COMMAND's
# status, not a pipeline stage's — so redirecting pg_run's output straight to
# a host file loses no failure a pipe would have hidden, and needs no
# container-side path, no docker cp, no cleanup rm. MSYS_NO_PATHCONV (top of
# file) still matters for the blob copy below, which passes container paths as
# `docker` ARGUMENTS; plain shell redirection here is unaffected by it.
echo "backup: recording the schema version and row counts"
pg_run '
set -eu

# The schema version this backup BELONGS TO: the goose head. A restore that does
# not know which code an artifact came from is a guess (spec §16).
ver="$(psql "$PG_DSN" -At -v ON_ERROR_STOP=1 -c \
  "SELECT max(version_id) FROM public.goose_db_version WHERE is_applied")"
case "$ver" in
  ""|*[!0-9]*)
    echo "backup: no goose schema version in this database." >&2
    echo "  public.goose_db_version is missing or empty, so this database was not built by" >&2
    echo "  cmd/mailroom-migrate (the deploy path). Refusing to write an artifact that cannot" >&2
    echo "  say which code it belongs to." >&2
    exit 1;;
esac

counts="$(psql "$PG_DSN" -At -F, -v ON_ERROR_STOP=1 -c \
  "SELECT (SELECT count(*) FROM app.users),
          (SELECT count(*) FROM app.accounts),
          (SELECT count(*) FROM app.messages),
          (SELECT count(*) FROM app.blobs)")"

printf "%s\n%s\n" "$ver" "$counts"
' > "$WORK/meta"

echo "backup: dumping PostgreSQL (custom-format)"
# `exec` so the container's sh is replaced by pg_dump rather than kept around
# as a pipe-holding parent — one less process between the DSN and the stream,
# and pg_run's exit status is then pg_dump's own, not a wrapper's.
pg_run 'exec pg_dump --format=custom --no-password "$PG_DSN"' > "$PAYLOAD/postgres.dump"
# A zero-byte dump must fail the run, not sail through to be sealed and handed
# out as a "backup" — the emptiness would only be discovered at restore, by
# which point every earlier good artifact may already be pruned (retention).
[ -s "$PAYLOAD/postgres.dump" ] || fail "pg_dump produced an empty (or missing) dump at $PAYLOAD/postgres.dump"

SCHEMA_VERSION="$(sed -n 1p "$WORK/meta" | tr -d '\r')"
COUNTS="$(sed -n 2p "$WORK/meta" | tr -d '\r')"
N_USERS="$(echo "$COUNTS"    | cut -d, -f1)"
N_ACCOUNTS="$(echo "$COUNTS" | cut -d, -f2)"
N_MESSAGES="$(echo "$COUNTS" | cut -d, -f3)"
N_BLOBS="$(echo "$COUNTS"    | cut -d, -f4)"

# --- 3. The second guard: an artifact with no users is not a backup ----------
if [ "$N_USERS" -eq 0 ] && [ "$ALLOW_EMPTY" != "1" ]; then
  cat >&2 <<'EMPTY'
backup: REFUSING TO WRITE AN EMPTY ARTIFACT.

  The dump contains ZERO users. Either this really is a fresh install (set
  MAILROOM_BACKUP_ALLOW_EMPTY=1 to say so out loud), or the dump was filtered by
  row-level security and you are about to archive an empty box.
EMPTY
  exit 1
fi
echo "backup: schema_version=$SCHEMA_VERSION users=$N_USERS accounts=$N_ACCOUNTS messages=$N_MESSAGES blobs=$N_BLOBS"

# --- 4. The blob store, verbatim (it is already encrypted at rest) -----------
echo "backup: copying the blob store (still encrypted; this script holds no key)"
if [ -n "$BLOB_VOLUME" ]; then
  # ⚠ --entrypoint true, NOT JUST `true` AS THE COMMAND (#633). AGE_IMAGE can
  # now be the Mailroom production image itself (A3's whole point — see where
  # it is resolved, above) — and that image's ENTRYPOINT is /mailroom, which
  # `docker create` APPENDS a bare command to rather than replacing. Without
  # this override the container that exists only so `docker cp` can lift files
  # from its mounted volume instead starts the SERVER with "true" as a stray
  # argument. Measured, not theoretical: this exact shape (this container's sh
  # sibling below) failed with the server's own config-validation error the
  # first time a real MAILROOM_AGE_IMAGE=<mailroom image> run reached it.
  HELPER=$(docker create --label "$LABEL" -v "$BLOB_VOLUME:$BLOB_PATH:ro" --entrypoint true "$AGE_IMAGE")
  docker cp "$HELPER:$BLOB_PATH/." "$(host_path "$PAYLOAD")/blobs"
else
  docker exec "$BLOB_CONTAINER" test -d "$BLOB_PATH" \
    || fail "no blob store at $BLOB_PATH in container $BLOB_CONTAINER"
  docker cp "$BLOB_CONTAINER:$BLOB_PATH/." "$(host_path "$PAYLOAD")/blobs"
fi
N_BLOBFILES="$(find "$PAYLOAD/blobs" -type f | wc -l | tr -d ' ')"

if [ "$N_MESSAGES" -gt 0 ] && [ "$N_BLOBFILES" -eq 0 ]; then
  fail "the database has $N_MESSAGES messages but the blob store copied ZERO files.
  Postgres without the blobs is a catalogue of mail you cannot read. Check
  MAILROOM_BACKUP_BLOB_CONTAINER / MAILROOM_BACKUP_BLOB_PATH."
fi

# --- 5. Enforce the escrow boundary -----------------------------------------
# The one thing that must never be in here. This is a cheap guard against the
# classic mistake — mounting the master-key secret somewhere under the data root
# — not a cryptographic proof. The real enforcement is that this script has no
# code path that reads the master key at all.
echo "backup: enforcing the escrow boundary (no key material in the artifact)"
SUSPECT="$(find "$PAYLOAD" -type f \
  \( -iname '*master*key*' -o -iname '*.key' -o -iname '*identity*' -o -iname '*.age' \) -print)"
if [ -n "$SUSPECT" ]; then
  echo "backup: REFUSING — something that looks like key material is inside the payload:" >&2
  echo "$SUSPECT" >&2
  echo "  A backup that carries the key that opens it is not a backup, it is a copy." >&2
  echo "  The master key belongs in OFFLINE ESCROW (ADR-005), never in the artifact." >&2
  exit 1
fi

# --- 6. Manifest ------------------------------------------------------------
CREATED="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
CODE_HEAD="$(ls "$DIR/db/migrations" | sed -n 's/^0*\([0-9][0-9]*\)_.*\.sql$/\1/p' | sort -n | tail -1)"
cat > "$PAYLOAD/manifest.json" <<MANIFEST
{
  "format": "mailroom-backup-v1",
  "created_at": "$CREATED",
  "schema_version": $SCHEMA_VERSION,
  "code_migration_head_at_backup": ${CODE_HEAD:-0},
  "postgres": {
    "file": "postgres.dump",
    "format": "pg_dump custom",
    "dumped_with_rls_bypass": true
  },
  "row_counts": {
    "users": $N_USERS,
    "accounts": $N_ACCOUNTS,
    "messages": $N_MESSAGES,
    "blobs": $N_BLOBS
  },
  "blob_store": {
    "dir": "blobs/",
    "files": $N_BLOBFILES,
    "encrypted_at_rest": true
  },
  "wrapped_key_store": {
    "location": "inside postgres.dump",
    "columns": ["app.accounts.credentials_ct", "app.user_keys.private_key_ct", "app.api_keys.api_key_ct"]
  },
  "search_index": {
    "included": false,
    "reason": "disposable cache, rebuilt from Postgres by internal/search (spec 7/11)"
  },
  "master_key": {
    "included": false,
    "reason": "escrowed separately and offline (ADR-005); without it this artifact is unreadable mail"
  }
}
MANIFEST

# --- 7. Checksum, tar, encrypt to the age recipients -------------------------
# Checksumming, tarring and encrypting all happen INSIDE a container: the host
# needs nothing but docker (and on the CI runner, host tooling is not a given).
echo "backup: sealing the artifact to the age recipients"
# ⚠ --entrypoint sh IS NOT OPTIONAL (#633). AGE_IMAGE can now be the Mailroom
# production image itself (A3's whole point — see where it is resolved,
# above), and that image's ENTRYPOINT is /mailroom: `docker create IMAGE CMD`
# APPENDS CMD to the entrypoint rather than replacing it (the Dockerfile's own
# header warns about exactly this). Without the override this container would
# start the SERVER with "sh -c '...'" as stray arguments instead of running
# the sealing script — measured, not theoretical: it failed with the server's
# own "invalid configuration: MAILROOM_DATABASE_URL … is required" the first
# time a real MAILROOM_AGE_IMAGE=<mailroom image> run reached this step.
#
# ⚠ --user root, FOR THE SAME REASON. A plain alpine image (the old default)
# runs as root, so this container could always write SHA256SUMS/payload.tar.gz/
# artifact.age into /work. The Mailroom image (the new default, per A3) runs
# as `mailroom:mailroom` (non-root, by design — see the Dockerfile), and the
# files `docker cp` lands under /work before `docker start` do not belong to
# that user. Measured: without this, sealing failed with "sh: can't create
# SHA256SUMS: Permission denied" the first time this ran against the Mailroom
# image. Running this ONE throwaway container as root is not a privilege
# escalation worth avoiding — it holds the plaintext payload already, runs no
# network-facing service, and cleanup() destroys it the moment sealing ends —
# it is just what makes both kinds of AGE_IMAGE behave the same way.
# $AGE_GUARD installs age ONLY when the image does not already carry it, and
# prints which happened — the `age: preinstalled` line in a nightly log is the
# one observable saying this run needed no egress (#633). It is the SAME string
# --self-test runs, so a regression here is caught there; its own comment says
# why that sharing is load-bearing. `retry` (#577/#586) wraps the install for
# the fallback path only: a real 03:00 backup must survive a DNS blip.
PACKER=$(docker create --label "$LABEL" -w /work -e AGE_R=/recipients.txt --entrypoint sh --user root "$AGE_IMAGE" -c "$NET_RETRY"'
set -eu
age_install() { retry apk add --no-cache age >/dev/null; }
'"$AGE_GUARD"'
cd /work/payload
find . -type f ! -name SHA256SUMS | LC_ALL=C sort | xargs sha256sum > SHA256SUMS
cd /work
tar -czf payload.tar.gz -C payload .
age -R "$AGE_R" -o /work/artifact.age /work/payload.tar.gz
')
docker cp "$WORK_HOST/payload" "$PACKER:/work/payload"
docker cp "$(host_path "$RECIPIENTS")" "$PACKER:/recipients.txt"
docker start -a "$PACKER"

OUT_DIR="$(dirname -- "$OUT")"
mkdir -p "$OUT_DIR"

# ⚠ COPY TO A PARTIAL NAME AND RENAME, NEVER STRAIGHT ONTO $OUT.
#
# `docker cp` onto the final path is not atomic: it truncates the destination
# and then streams. A failure part-way — a full disk, a killed daemon, a
# cancelled run — therefore left a TRUNCATED artifact at $OUT and destroyed the
# only good one in the same stroke. The window is the whole copy, which on a
# large mailbox is a multi-GB write.
#
# The rename is atomic within a filesystem, so at every instant $OUT is either
# the previous complete artifact or the new complete one. This is also what
# makes the freshness check (scripts/backup-freshness.sh) meaningful: without
# it, a half-copied file sits at the expected path with a current timestamp,
# which an mtime check reads as healthy.
PARTIAL="$OUT.partial.$$"
rm -f "$PARTIAL"
docker cp "$PACKER:/work/artifact.age" "$(host_path "$OUT_DIR")/$(basename -- "$PARTIAL")"
[ -s "$PARTIAL" ] || fail "the sealed artifact did not arrive (or is empty) at $PARTIAL"
mv -f "$PARTIAL" "$OUT"

SIZE="$(wc -c < "$OUT" | tr -d ' ')"

# --- 7a. Dated copies and bounded retention (#539) ---------------------------
# Delegated to scripts/backup-retain.sh, which owns the dated-link scheme and
# the pruning glob. It lives in its own script for one reason: a destructive
# `rm` loop inlined here would only ever run on a production host, where no test
# can reach it. There it has a --self-test that CI runs, including the negative
# controls that matter most — that $OUT itself, a concurrent run's
# `.partial.<pid>`, and unrelated neighbours are never pruned.
#
# Exit 3 means "the artifact is complete but retention could not be applied"
# (no hard links, read-only directory). That is a WARNING, never a failed
# backup: the bytes at $OUT are good either way, and failing a good backup over
# its own bookkeeping would be the worse outcome.
if [ "$KEEP" -gt 0 ] 2>/dev/null; then
  set +e
  sh "$DIR/scripts/backup-retain.sh" --out "$OUT" --keep "$KEEP"
  RETAIN_RC=$?
  set -e
  [ "$RETAIN_RC" = 0 ] || [ "$RETAIN_RC" = 3 ] || \
    fail "retention step failed with exit $RETAIN_RC (the artifact at $OUT is complete)"
fi
echo
echo "backup: wrote $OUT ($SIZE bytes)"
echo "backup:   schema_version   $SCHEMA_VERSION"
echo "backup:   contents         postgres.dump + $N_BLOBFILES blob file(s) + manifest"
echo "backup:   NOT included     the search index (rebuildable) and the MASTER KEY"
echo "backup:"
if [ "$KEEP" -gt 0 ] 2>/dev/null; then
  echo "backup:   retention       keeping $KEEP dated copy(ies) beside $OUT"
else
  echo "backup:   retention       NONE (MAILROOM_BACKUP_KEEP=0) — this run overwrote any previous artifact"
fi
echo "backup:"
echo "backup: This artifact is USELESS without the master key from offline escrow."
echo "backup: That is the design. Verify it with scripts/restore-drill.sh —"
echo "backup: a backup that hasn't restored is a hypothesis."
echo "backup:"
echo "backup: And nothing here will tell you if this was the last run that ever"
echo "backup: succeeded. That is scripts/backup-freshness.sh's job — wire it to"
echo "backup: monitoring, or the schedule is unobserved (#539)."
