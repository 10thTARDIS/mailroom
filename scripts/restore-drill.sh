#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# THE RESTORE DRILL (spec §11, ADR-005, #40).
#
#   "A backup that hasn't restored is a hypothesis."
#
# Takes an encrypted backup artifact, restores it into a scratch container, and
# then proves the restore is ALIVE rather than merely present:
#
#   1. OPEN      the artifact with the age identity          (catches corruption)
#   2. CHECKSUM  every file in the payload
#   3. SCHEMA    the artifact's goose version == the code's   (catches skew)
#   4. RESTORE   pg_restore into a throwaway PostgreSQL 18
#   5. COUNT     restored rows == the counts the backup recorded
#   6. PROVE     per user: decrypt one message with the ESCROWED master key, open
#                the wrapped credentials, REBUILD the search index from Postgres,
#                run one search that must return a hit, and check the SECOND
#                escrowed root is present and is not the master key again.
#
# Step 6 is the drill. Steps 1-5 only prove that bytes came back; step 6 proves
# the MAIL did — and it cannot pass without the master key, which is escrowed
# SEPARATELY and OFFLINE and is never in the artifact (ADR-005). That separation
# is the point: a real restore needs secrets from more than one place, and this
# script is where that stops being a claim.
#
# Since ADR-024 there are TWO escrowed roots, and the second one's loss is
# invisible to every check above it: the mailbox restores, opens, searches and
# reads perfectly, and only the mail encrypted to the user's PGP keys is gone.
# So the drill demands `--pgp-root` as well, and a run without it must say
# `--no-pgp-root` out loud and is then reported as having proved nothing about
# encrypted mail. A drill that green-lit that backup would certify a hypothesis,
# which is the one thing this script exists to stop.
#
# The search index is not restored, because it is never backed up (spec §7/§11:
# a disposable cache; a restored-but-stale index is worse than none). It is
# REBUILT here, from Postgres, which is the only honest way to know that it can be.
#
# Modes
#   sh scripts/restore-drill.sh
#       Fixture (the CI default). Builds its own world — Postgres, migrations,
#       master key, age keypair, real envelope-encrypted mail — takes a real
#       backup with scripts/backup.sh, destroys the source, and drills the
#       artifact. Proves the whole chain with no production secrets in CI.
#
#   sh scripts/restore-drill.sh --artifact mailroom.age \
#        --identity backup-identity.txt --master-key /media/escrow/master.key \
#        --pgp-root /media/escrow/pgp-root.key
#       The real thing.
#
# Proving the drill can FAIL — a drill that cannot fail is not a drill:
#   --inject corrupt      truncate the artifact
#   --inject no-key       restore without the escrowed master key
#   --inject wrong-key    restore with the WRONG master key
#   --inject no-pgp-root  restore without the escrowed PGP root (ADR-024)
#   --inject same-root    point both secrets at the SAME file — the collapse
#                         that leaves everything working with one root, and
#                         which nothing else here would notice
#   --inject missing-blobs  delete half the restored blob files: Postgres comes
#                         back whole, the blob store does not
#   --inject wrong-pgp-root  the PGP root is present, well-formed, and
#                         genuinely distinct from the master key — but it is
#                         the root from a DIFFERENT box, so it does not match
#                         the check value the database recorded
#                         (app.pgp_root_check, #551). This proves the
#                         check-value comparison, which runs BEFORE any unseal,
#                         so it never reaches the unseal (see the next one)
#   --inject wrong-pgp-root-no-check  the same wrong root, against a database
#                         with NO recorded check value (a pre-00055 database).
#                         The comparison has nothing to compare, so the drill
#                         falls through to unsealing a stored key and must fail
#                         there. This is the only injection that exercises the
#                         drill's unseal check at all (#654): no-pgp-root and
#                         same-root fail before any unseal, and wrong-pgp-root
#                         fails at the check value
#   --inject schema       restore into code at a different schema version
#   --inject no-hit       search for a canary the backup does not contain
# Each MUST exit non-zero. `--self-test` runs all ten, then a clean run.
#
# Everything goes through the Docker API only — docker cp + docker exec, no bind
# mounts, no published ports — because the CI runner executes jobs inside
# containers with a socket-mounted Docker, where host paths and host ports do not
# resolve. Same shape as scripts/integration-test.sh and scripts/tantivy-test.sh.
set -eu

# Without this, Git Bash rewrites CONTAINER-side paths (/blobs -> C:/Program
# Files/Git/blobs) and containers die before they start. No-op on Linux/CI.
MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
HOSTDIR="$(cd "$DIR" && { pwd -W 2>/dev/null || pwd; })"

# The SOURCE and DESTINATION clusters are named separately, and that is the whole
# point of having two variables rather than one.
#
# ⚠ WITH ONE VARIABLE THIS DRILL COULD NOT TEST THE SCENARIO IT EXISTS FOR.
# Setting a single MAILROOM_PG_IMAGE=postgres:18 made BOTH clusters 18, which
# exercises an 18 -> 18 restore. A real disaster restore is very often exactly
# the other thing: the dump on disk was written by the major version you were
# running BEFORE, and you are restoring it onto whatever you have now. That is
# also precisely what a version upgrade is (ADR-025), and when the PostgreSQL 16
# -> 18 upgrade was done the drill could not verify it — the cross-version
# restore had to be proved by hand instead, which is the kind of one-off proof
# that is true once and then rots.
#
# So: MAILROOM_PG_SRC_IMAGE builds the cluster the backup is TAKEN from, and
# MAILROOM_PG_DST_IMAGE is the one it is RESTORED into. Both default to
# MAILROOM_PG_IMAGE (and then to the supported major), so the ordinary
# same-version run is unchanged and needs no new configuration.
#
#   # rehearse the next major upgrade before doing it for real:
#   MAILROOM_PG_SRC_IMAGE=postgres:18 MAILROOM_PG_DST_IMAGE=postgres:19 \
#     sh scripts/restore-drill.sh
PG_IMAGE="${MAILROOM_PG_IMAGE:-postgres:18}"
PG_SRC_IMAGE="${MAILROOM_PG_SRC_IMAGE:-$PG_IMAGE}"
PG_DST_IMAGE="${MAILROOM_PG_DST_IMAGE:-$PG_IMAGE}"
GO_IMAGE="${MAILROOM_GO_IMAGE:-golang:1.27}"
GO_ALPINE="${MAILROOM_GO_ALPINE_IMAGE:-golang:1.27-alpine}"
AGE_IMAGE="${MAILROOM_AGE_IMAGE:-alpine:3.20}"
TANTIVY_VERSION="${TANTIVY_VERSION:-v1.0.6}"

# ⚠ EVERY PACKAGE FETCH INSIDE A CONTAINER GOES THROUGH `retry` (#577).
#
# One --self-test job makes roughly fifty of them — `apk add` and the Tantivy
# download, each in a fresh container, across eleven drill invocations — and a
# single transient resolver failure used to fail the whole required check:
# `DNS: transient error` → apk exits → DRILL_EXIT=4 → "failed, but NOT for its
# own reason". That last line is the #348 guard doing its job (a run that never
# reached its phase proves nothing); the weakness was upstream of it. It happened
# twice on 2026-09-15, on two different injections.
#
# BOUNDED ON PURPOSE, IN TWO DIRECTIONS. 5 attempts, 3/6/9/12 s apart; and since
# #586, each ATTEMPT is bounded too (RETRY_TIMEOUT, 180 s by default). The second
# bound is not a refinement of the first — it is the failure mode the first one
# cannot see. A fetch that stalls never exits, so the retry loop never runs: the
# drill sat inside `apk add` for 18+ minutes, log byte-identical, without ever
# printing a retry line. Raise RETRY_TIMEOUT before a call that legitimately
# needs longer. A resolver that stays down still fails the run, with the tool's
# OWN error printed above the give-up line — a real outage must stay red and stay
# attributable, not be waited out. It is a string rather than a host function
# because every fetch happens inside some container's `sh -c`: prepend it to that
# script. scripts/backup.sh and scripts/tantivy-test.sh carry identical copies,
# because they run standalone — keep the three in step.
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

ARTIFACT=""; IDENTITY=""; MASTER_KEY=""; PGP_ROOT=""; EXPECT_SCHEMA=""; QUERY=""; INJECT=""
SELF_TEST=0; MIGRATE_FWD=0; NO_PGP_ROOT=0; ONLY_USERS=""
while [ $# -gt 0 ]; do
  case "$1" in
    --artifact)      ARTIFACT="$2"; shift 2 ;;
    --identity)      IDENTITY="$2"; shift 2 ;;
    --master-key)    MASTER_KEY="$2"; shift 2 ;;
    # The SECOND escrowed root (ADR-024). It covers the PGP private keys, and
    # its loss is invisible to every other check here.
    --pgp-root)      PGP_ROOT="$2"; shift 2 ;;
    --no-pgp-root)   NO_PGP_ROOT=1; shift ;;
    # Repeatable. Scopes WHICH users must prove alive; see phase 5/6.
    --user-id)       ONLY_USERS="$ONLY_USERS $2"; shift 2 ;;
    --expect-schema) EXPECT_SCHEMA="$2"; shift 2 ;;
    --query)         QUERY="$2"; shift 2 ;;
    --inject)        INJECT="$2"; shift 2 ;;
    --fixture)       shift ;;
    # Build the fixture at HEAD-1, back it up, restore it, and migrate it FORWARD
    # to HEAD before proving it — the DR case a real restore always hits (your
    # backup is older than your code; spec §16). Fixture-only.
    --migrate-forward) MIGRATE_FWD=1; shift ;;
    --self-test)     SELF_TEST=1; shift ;;
    *) echo "restore-drill: unknown argument $1" >&2; exit 2 ;;
  esac
done

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

# --- network-event diagnostics (#502) ----------------------------------------
# One main push run lost its Docker network mid-iteration ("network
# mailroom-drill-<pid>-<random> not found" at step 3/6, after steps 1-2 had used
# it). Nothing in scripts/ removes networks by prefix, so either a stray early
# cleanup (a nested sh re-running the trap), a parallel run, or the daemon did
# it. It cannot be reproduced on demand, so each run records the daemon's
# network events and a failure prints them: DESTROY/DISCONNECT timing names the
# remover. This is EVIDENCE ONLY and must never fail or slow a drill.
#
# drill_network_report [LOGFILE] — print the recorded events for every network
# name found in LOGFILE (if given), then every destroy/disconnect/remove seen by
# any run that recorded into $DRILL_EVENTS_DIR (or this run's own file).
drill_network_report() {
  _rlog="${1:-}"
  _rdir="${DRILL_EVENTS_DIR:-}"
  if [ -n "$_rdir" ]; then _rfiles="$_rdir"/drill-net-events.*; else _rfiles="${EVFILE:-}"; fi
  echo "DRILL NETWORK EVENTS: (epoch-nanoseconds action network-name network-id container)"
  _rany=0
  for _f in $_rfiles; do
    [ -s "$_f" ] || continue
    _rany=1
    echo "  -- $(basename -- "$_f")"
    if [ -n "$_rlog" ]; then
      for _n in $(grep -o 'mailroom-drill-[0-9][0-9]*-[0-9][0-9]*' "$_rlog" 2>/dev/null | sort -u); do
        grep -F " $_n " "$_f" | sed 's/^/  this-run-network: /'
      done
    fi
    grep -E ' (destroy|disconnect|remove|prune) ' "$_f" | sed 's/^/  REMOVAL: /'
  done
  [ "$_rany" -eq 1 ] || echo "  (no events were recorded: docker events was unavailable or saw nothing)"
  echo "DRILL NETWORK EVENTS: end"
}

# --- --self-test -------------------------------------------------------------
# Prove the drill can fail BEFORE trusting it when it passes.
#
# ⚠ "THE DRILL EXITED NON-ZERO" IS NOT EVIDENCE THE INJECTION WAS DETECTED (#348).
# It used to be the whole assertion, and the consequence was observed: the
# Docker-in-Docker daemon backing a runner died mid-job, every step after that
# failed with "Cannot connect to the Docker daemon", and THREE injections in a
# row printed "--- self-test OK". No injected fault was exercised and nothing
# about the safety property was tested, while the log said otherwise.
#
# The self-test is strongest exactly when nothing is wrong, and silently
# degenerates into a rubber stamp precisely when the environment is broken —
# which is when you would most want to know. So each injection must now fail
# FOR ITS OWN REASON: its distinctive marker has to appear in the output.
expected_marker() {
  # ⚠ EVERY PATTERN HERE WAS CAPTURED FROM A REAL --self-test RUN, not read off
  # the code. Two would have been wrong if inferred: `missing-blobs` fails at
  # [3/5] REINDEX, not the [1/5] DECRYPT the injection's own comment suggests,
  # and `no-key` never reaches a numbered phase at all.
  #
  # The three PGP-root injections deliberately match their SUB-reason rather
  # than a shared "[5/5] PGP ROOT FAILED", so each proves its own fault was
  # detected instead of merely something about roots.
  #
  # wrong-pgp-root-no-check is the one marker that is the UNSEAL failure's own
  # text (unsealPGPKeys, cmd/mailroom-drill), not the check-value text (#654).
  #
  # ⚠ wrong-pgp-root's marker CHANGED under #551. Before it, the drill's only
  # proof the escrowed root was wrong was unsealing an actual stored key, so
  # this injection failed at "key .* did NOT unseal". Since #551 the
  # database-wide check value (app.pgp_root_check, migration 00055) is
  # compared FIRST — deliberately, so a wrong root is caught even with zero
  # stored keys — and a mismatch there is now a hard failure before
  # unsealing is ever attempted. Re-captured from a real --self-test run
  # after #551 landed; the old "did NOT unseal" text no longer appears here
  # at all (unsealPGPKeys is never reached).
  case "$1" in
    corrupt)        echo 'THE ARTIFACT WILL NOT OPEN\.' ;;
    no-key)         echo 'THE ESCROWED MASTER KEY IS NOT HERE\.' ;;
    wrong-key)      echo '\[1/5\] DECRYPT FAILED' ;;
    no-pgp-root)    echo 'PGP ROOT FAILED: crypto: read PGP root key' ;;
    same-root)      echo 'PGP ROOT FAILED: the escrowed PGP root is BYTE-IDENTICAL to the master key' ;;
    wrong-pgp-root) echo 'PGP ROOT FAILED: this root does NOT match app\.pgp_root_check' ;;
    wrong-pgp-root-no-check) echo 'PGP ROOT FAILED: key .* did NOT unseal' ;;
    schema)         echo 'SCHEMA VERSION MISMATCH\.' ;;
    no-hit)         echo '\[4/5\] SEARCH FAILED' ;;
    missing-blobs)  echo '\[3/5\] REINDEX FAILED' ;;
    # ⚠ NO DEFAULT PATTERN. An unknown injection must fail loudly rather than
    # fall through to an empty one, because `grep -E ''` matches everything and
    # would recreate exactly the bug this function exists to fix.
    *)              echo '' ;;
  esac
}

if [ "$SELF_TEST" -eq 1 ]; then
  # ⚠ `set -e` OFF FOR THIS BLOCK, AND IT IS LOAD-BEARING. Every run below is
  # EXPECTED to fail; the whole point is to inspect HOW. With `set -e` (line 74)
  # a subshell inherits it, so `( drill; echo "DRILL_EXIT=$?" )` aborts on the
  # failing drill and never reaches the echo — and the parent then dies on the
  # failing subshell.
  #
  # That bit the first version of this very fix: the injection loop survived
  # only because a pipeline's status is its LAST command (`tee`), so the missing
  # DRILL_EXIT line happened to route to the right branch, while the negative
  # control — not a pipeline — killed the script outright at exit 2. A guard
  # whose status recording silently never runs is precisely the shape of #348.
  #
  # The block always ends in an explicit exit, so nothing after it is affected.
  set +e
  fails=0
  SELFTEST_LOGS=$(mktemp -d)
  # Child drills record their network events here and leave them for the report (#502).
  DRILL_EVENTS_DIR="$SELFTEST_LOGS"; export DRILL_EVENTS_DIR
  for inj in corrupt no-key wrong-key no-pgp-root same-root wrong-pgp-root wrong-pgp-root-no-check schema no-hit missing-blobs; do
    echo
    echo "###############################################################################"
    echo "# SELF-TEST: inject '$inj' — the drill MUST fail, and fail for ITS OWN reason"
    echo "###############################################################################"
    want=$(expected_marker "$inj")
    if [ -z "$want" ]; then
      echo "!!! SELF-TEST FAILED: no expected marker is defined for injection '$inj'."
      echo "    Add one to expected_marker(); an injection whose detection is not"
      echo "    asserted is an injection that proves nothing."
      fails=$((fails + 1))
      continue
    fi
    log="$SELFTEST_LOGS/$inj.log"
    reported=0
    # Streamed AND captured: the exit status rides in the stream because a POSIX
    # pipeline reports the LAST command's status, not the drill's.
    ( sh "$0" --inject "$inj" 2>&1; echo "DRILL_EXIT=$?" ) | tee "$log"
    if grep -q '^DRILL_EXIT=0$' "$log"; then
      echo "!!! SELF-TEST FAILED: injection '$inj' did NOT fail the drill"
      fails=$((fails + 1))
      drill_network_report "$log"; reported=1
    elif grep -qE "$want" "$log"; then
      echo "--- self-test OK: '$inj' failed with ITS OWN marker: /$want/"
    else
      echo "!!! SELF-TEST FAILED: injection '$inj' failed, but NOT for its own reason."
      echo "    wanted output matching: /$want/"
      echo "    ⚠ The drill exiting non-zero is not enough. A dead Docker daemon, a"
      echo "      DNS failure or a full disk all satisfy that, and all three used to"
      echo "      print 'self-test OK' (#348). Read the output above: if the drill"
      echo "      never reached the phase this injection targets, the environment"
      echo "      broke and this run tested nothing."
      fails=$((fails + 1))
      drill_network_report "$log"; reported=1
    fi
    if grep -qE 'network .* not found' "$log" && [ "$reported" -eq 0 ]; then
      echo "--- note: '$inj' logged a vanished network (#502)"
      drill_network_report "$log"
    fi
  done

  # ⚠ THE NEGATIVE CONTROL FOR THE CHECK ABOVE (#348). Without it the marker
  # check is itself unverified — the same shape as the bug it fixes. Break the
  # environment the way the reported incident did (nothing on PATH, so docker
  # and age cannot be found) and confirm the marker check REFUSES the run
  # instead of accepting it.
  #
  # /bin/sh by absolute path: a `PATH=... sh` assignment applies to the command
  # LOOKUP too, so a relative `sh` would not be found and this would test the
  # wrong thing.
  echo
  echo "###############################################################################"
  echo "# SELF-TEST: environment collapse — the marker check MUST refuse it"
  echo "###############################################################################"
  nclog="$SELFTEST_LOGS/env-collapse.log"
  ( PATH=/nonexistent /bin/sh "$0" --inject same-root 2>&1; echo "DRILL_EXIT=$?" ) > "$nclog" 2>&1
  tail -12 "$nclog"
  if grep -q '^DRILL_EXIT=0$' "$nclog"; then
    echo "!!! SELF-TEST FAILED: the drill PASSED with nothing on PATH, which cannot be right"
    fails=$((fails + 1))
  elif grep -qE "$(expected_marker same-root)" "$nclog"; then
    echo "!!! SELF-TEST FAILED: the marker check ACCEPTED a run that died of a broken"
    echo "    environment. That is the #348 bug itself, and it means every"
    echo "    'self-test OK' above is worth nothing."
    fails=$((fails + 1))
  else
    echo "--- self-test OK: a broken environment is NOT mistaken for a real detection"
  fi
  echo
  echo "###############################################################################"
  echo "# SELF-TEST: no injection — the drill MUST pass"
  echo "###############################################################################"
  if sh "$0"; then
    echo "--- self-test OK: a healthy backup passes"
  else
    echo "!!! SELF-TEST FAILED: a healthy backup did not pass"
    fails=$((fails + 1))
    drill_network_report
  fi
  echo
  echo "###############################################################################"
  echo "# SELF-TEST: restore an OLDER snapshot then migrate forward — the drill MUST pass"
  echo "###############################################################################"
  rc=0
  sh "$0" --migrate-forward || rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "--- self-test OK: an older backup restores and migrates forward to HEAD"
  elif [ "$rc" -eq 3 ]; then
    echo "::warning::migrate-forward phase SKIPPED: the head migration is in the root commit, so there is no older revision to restore from"
    echo "--- self-test SKIPPED (not passed): migrate-forward has no older revision in this history"
  else
    echo "!!! SELF-TEST FAILED: restore-then-migrate-forward did not pass"
    fails=$((fails + 1))
    drill_network_report
  fi
  echo
  [ "$fails" -eq 0 ] || { echo "SELF-TEST FAILED ($fails)"; exit 1; }
  echo "SELF-TEST PASSED: the drill fails when it must, and passes when it must."
  exit 0
fi

FIXTURE=0
[ -z "$ARTIFACT" ] && FIXTURE=1

if [ "$FIXTURE" -eq 0 ]; then
  [ -r "$ARTIFACT" ]   || { echo "restore-drill: cannot read --artifact $ARTIFACT" >&2; exit 2; }
  [ -n "$IDENTITY" ]   || { echo "restore-drill: --identity is required with --artifact" >&2; exit 2; }
  [ -r "$IDENTITY" ]   || { echo "restore-drill: cannot read --identity $IDENTITY" >&2; exit 2; }
  [ -n "$MASTER_KEY" ] || { echo "restore-drill: --master-key is required (get it from OFFLINE ESCROW)" >&2; exit 2; }
  if [ -z "$PGP_ROOT" ] && [ "$NO_PGP_ROOT" -eq 0 ]; then
    echo "restore-drill: --pgp-root is required (the crypto process's OWN escrowed secret," >&2
    echo "  ADR-024 — a DIFFERENT secret from the master key). Without it this drill cannot" >&2
    echo "  tell you whether mail encrypted to your PGP keys is recoverable, and every other" >&2
    echo "  check here passes without it." >&2
    echo "  If this instance genuinely has no PGP root, pass --no-pgp-root and accept that." >&2
    exit 2
  fi
  [ -z "$PGP_ROOT" ] || [ -r "$PGP_ROOT" ] || { echo "restore-drill: cannot read --pgp-root $PGP_ROOT" >&2; exit 2; }
fi
case "$QUERY" in *[[:space:]]*) echo "restore-drill: --query must be a single word" >&2; exit 2 ;; esac

umask 077

# ⚠ `$$` IS NOT UNIQUE ACROSS CONCURRENT RUNS, AND THIS SCRIPT DELETES BY NAME.
#
# Every docker object below used to be named `…-$$`. The startup cleanup further
# down removes any object with those names before creating them — a guard added
# so a cancelled run does not leave a stale network behind (#346). Put those two
# together and a PID collision is not a nuisance, it is one run REAPING
# ANOTHER'S CONTAINERS AND NETWORK mid-restore.
#
# ⚠ MEASURED, NOT SUPPOSED. Two drill runs on 2026-09-05 — one for `main`, one
# for a PR branch — each spawn eleven sub-invocations for the self-test suite,
# so eleven PIDs each. Their PID sets intersected at 5055, and the victim died
# with `network mailroom-drill-5055 not found` three minutes later.
#
# ⚠ AND CONCURRENT DRILLS ARE BY DESIGN. restore-drill.yml keys its concurrency
# group on `github.ref` with `cancel-in-progress: false` — deliberately, because
# cancelling a half-finished restore drill teaches you nothing. So two refs run
# at once, for ten minutes each, and the collision window is the whole run.
#
# The suffix therefore carries randomness as well as the PID. /dev/urandom is
# present in every container this runs in and in Git Bash; the `|| date` arm
# keeps the script working rather than silently un-suffixed if it ever is not.
UNIQ="$$-$(od -An -N4 -tu4 < /dev/urandom 2>/dev/null | tr -dc '0-9' || date +%s)"
[ -n "${UNIQ#*-}" ] || UNIQ="$$-$(date +%s)"

WORK="$DIR/.drill-tmp.$UNIQ"
mkdir -p "$WORK"
host_path() { d=$(dirname -- "$1"); b=$(basename -- "$1"); (cd "$d" && printf '%s/%s\n' "$(pwd -W 2>/dev/null || pwd)" "$b"); }
WORK_HOST="$(host_path "$WORK")"

NET="mailroom-drill-$UNIQ"
VOL="mailroom-drill-blobs-$UNIQ"
SRCPG="mailroom-drill-srcpg-$UNIQ"
DSTPG="mailroom-drill-dstpg-$UNIQ"
PW="drill-$UNIQ"
CANARY="zarquon"             # the word planted in the fixture's mail
LABEL="mailroom-drill=$UNIQ" # every container this run creates carries it

# Network-event recorder (#502). The file lives OUTSIDE $WORK so a cleanup that
# runs early (which removes $WORK) cannot take the evidence with it.
if [ -n "${DRILL_EVENTS_DIR:-}" ] && [ -d "$DRILL_EVENTS_DIR" ]; then
  EVFILE="$DRILL_EVENTS_DIR/drill-net-events.$UNIQ"   # kept: the self-test parent reports it
  EVKEEP=1
else
  EVFILE="$(mktemp 2>/dev/null || echo "${TMPDIR:-/tmp}/drill-net-events.$UNIQ")"
  EVKEEP=0
fi
EVPID=""

cleanup() {
  # $1 is the trap that fired (EXIT, INT or TERM). Logged so that a cleanup
  # running MID-ITERATION (a nested sh re-running the trap, #502) is visible:
  # compare pid/ppid with the run's own, and see whether steps follow it.
  _rc=$?
  echo "DRILL CLEANUP: trap=${1:-?} pid=$$ ppid=$PPID status=$_rc at=$(date +%s) net=$NET" >&2
  # A standalone drill (not under --self-test, whose parent reports instead)
  # prints the recorded events when it is failing.
  if [ "${1:-}" = EXIT ] && [ "$_rc" -ne 0 ] && [ "$EVKEEP" -eq 0 ] && [ -z "${DRILL_EVENTS_DIR:-}" ]; then
    drill_network_report >&2 || true
  fi
  # Stop the recorder only at EXIT, so an early cleanup does not blind it.
  if [ "${1:-}" = EXIT ] && [ -n "$EVPID" ]; then
    kill "$EVPID" >/dev/null 2>&1 || true
    wait "$EVPID" >/dev/null 2>&1 || true
    EVPID=""
  fi
  if [ "${1:-}" = EXIT ] && [ "$EVKEEP" -eq 0 ]; then rm -f "$EVFILE" >/dev/null 2>&1 || true; fi
  # Reap by LABEL, not by a list of ids. mkcid is called as `X=$(mkcid ...)`,
  # i.e. inside a command substitution, i.e. in a SUBSHELL — so any id it
  # appended to a variable would be lost the moment the subshell exited, and the
  # containers would leak (a weekly CI job that leaks 30 containers a run fills
  # the runner's disk). The daemon-side label survives that; a shell variable
  # does not.
  ids="$(docker ps -aq --filter "label=$LABEL" 2>/dev/null || true)"
  # shellcheck disable=SC2086 — ids is intentionally word-split
  [ -n "$ids" ] && docker rm -fv $ids >/dev/null 2>&1
  docker rm -fv "$SRCPG" "$DSTPG" >/dev/null 2>&1
  docker volume rm -f "$VOL" >/dev/null 2>&1
  docker network rm "$NET" >/dev/null 2>&1
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
trap 'cleanup EXIT' EXIT
trap 'cleanup INT' INT
trap 'cleanup TERM' TERM

# Start the recorder. --since now closes the gap between start and the network
# being created. It must never fail the drill: unavailable => one warning.
docker events --since "$(date +%s)" --filter type=network \
  --format '{{.TimeNano}} {{.Action}} {{.Actor.Attributes.name}} {{.Actor.ID}} {{.Actor.Attributes.container}}' \
  >"$EVFILE" 2>/dev/null &
EVPID=$!
sleep 1   # let it subscribe before the network is created
if ! kill -0 "$EVPID" 2>/dev/null; then
  echo "restore-drill: warning: docker events is unavailable; no network-event diagnostics this run (#502)" >&2
  EVPID=""
fi

# mkcid ARGS... — create a labelled throwaway container and print its id.
mkcid() { docker create --label "$LABEL" "$@"; }

wait_pg() {
  i=0
  until docker exec "$1" pg_isready -h 127.0.0.1 -U postgres -q 2>/dev/null; do
    i=$((i + 1)); [ "$i" -gt 60 ] && { echo "postgres $1 did not start" >&2; exit 1; }
    sleep 1
  done
}

# The schema version the CODE in this checkout is at: what a restore is being
# restored INTO (spec §16).
CODE_HEAD="$(ls "$DIR/db/migrations" | sed -n 's/^0*\([0-9][0-9]*\)_.*\.sql$/\1/p' | sort -n | tail -1)"
[ -n "$EXPECT_SCHEMA" ] || EXPECT_SCHEMA="$CODE_HEAD"

# --migrate-forward: build the fixture one migration BEHIND head, so the artifact
# is genuinely older than the code and must be carried forward after restore.
#
# ⚠ THE FIXTURE IS BUILT FROM THE OLD REVISION'S CODE, NOT THIS CHECKOUT'S (#236).
# It used to seed the older schema by running TODAY'S cmd/mailroom-drill, which
# rests on an assumption nothing guarantees: that current code can write to the
# previous schema. Expand-and-contract (ADR-008) promises no such thing — the
# deploy order is migrate-then-start, so head code only ever meets head schema.
# Migration 00029 made that concrete: `internal/store` began writing
# `classified_tab`, the column does not exist at 28, and this phase died with
# SQLSTATE 42703 while building its own fixture — before a backup was even taken.
# It stayed red from 00027 through 00029 because this workflow is separate from
# CI and never appears in a PR's checks.
#
# So the fixture now runs the code that ACTUALLY WROTE that schema. Which is also
# the honest simulation, and exactly what this script's own `schema` injection
# message already tells a human to do: an old artifact comes from old code.
# Everything AFTER the backup — restore, migrate forward, verify — stays on this
# checkout, because that is the half under test.
OLD_HEAD=""; FIXTURE_MIGRATE="up"
FIXTURE_SRC="$HOSTDIR"
if [ "$MIGRATE_FWD" -eq 1 ]; then
  [ "$FIXTURE" -eq 1 ] || { echo "restore-drill: --migrate-forward is fixture-only (do not pass --artifact)" >&2; exit 2; }
  OLD_HEAD=$((CODE_HEAD - 1))
  [ "$OLD_HEAD" -ge 1 ] || { echo "restore-drill: --migrate-forward needs at least two migrations (head=$CODE_HEAD)" >&2; exit 2; }
  FIXTURE_MIGRATE="up-to $OLD_HEAD"      # the fixture is captured at head $OLD_HEAD
  EXPECT_SCHEMA="$OLD_HEAD"              # so the older artifact passes the equality guard
  echo "=== --migrate-forward: fixture at schema $OLD_HEAD, restore, then migrate -> $CODE_HEAD"

  # The revision to build the fixture from: the parent of whatever commit ADDED
  # the head migration. Derived from the migration itself rather than from a
  # release tag — tags are a release-cadence artifact and a migration can land
  # between two of them, whereas "the commit before this migration existed" is
  # exactly the definition wanted.
  HEAD_MIG="$(ls "$DIR/db/migrations" | grep -E "^0*${CODE_HEAD}_.*\.sql$" | head -1)"
  [ -n "$HEAD_MIG" ] || { echo "restore-drill: cannot find the migration file for head $CODE_HEAD" >&2; exit 1; }

  # git_at runs git IN $DIR via a subshell rather than passing `-C "$DIR"`.
  # This script exports MSYS_NO_PATHCONV=1 for docker's sake, and that also stops
  # Git Bash rewriting an MSYS path into the Windows form git.exe needs — so
  # `git -C /c/Users/...` fails with "cannot change to". It failed CLAIMING THIS
  # WAS NOT A GIT CHECKOUT, which was a confident wrong diagnosis of a working
  # repository. A `cd` needs no path argument and cannot be mangled.
  git_at() { (cd "$DIR" && git "$@"); }

  git_at rev-parse --git-dir >/dev/null 2>&1 || {
    echo "restore-drill: --migrate-forward needs git history to build the fixture from the" >&2
    echo "  revision that predates $HEAD_MIG, and git failed here. Either this is not a" >&2
    echo "  checkout, or git is unusable in this environment:" >&2
    git_at rev-parse --git-dir >&2 2>&1 || true
    exit 1
  }
  ADD_COMMIT="$(git_at log --diff-filter=A --format=%H -1 -- "db/migrations/$HEAD_MIG" 2>/dev/null || true)"
  [ -n "$ADD_COMMIT" ] || {
    echo "restore-drill: cannot find the commit that added db/migrations/$HEAD_MIG." >&2
    echo "  A SHALLOW CLONE is the usual cause — CI must check out with fetch-depth: 0." >&2
    echo "  (An uncommitted new migration would also do it; commit it first.)" >&2
    exit 1
  }
  OLD_REV="$(git_at rev-parse --verify "${ADD_COMMIT}^" 2>/dev/null || true)"
  [ -n "$OLD_REV" ] || {
    # A full-history clone whose ROOT commit already contains the head migration
    # (the public repository starts from a single squashed commit) has no older
    # revision to build the fixture from. That is a property of the history, not
    # a defect: say so loudly and exit 3, which --self-test reports as SKIPPED.
    # A shallow clone is told apart so it still fails.
    if [ "$(git_at rev-parse --is-shallow-repository 2>/dev/null || echo true)" = "false" ]; then
      echo "restore-drill: $HEAD_MIG was added by the root commit $ADD_COMMIT; there is no older" >&2
      echo "  revision to build the migrate-forward fixture from. SKIPPING this phase until a" >&2
      echo "  migration lands after the root commit." >&2
      exit 3
    fi
    echo "restore-drill: $HEAD_MIG was added by $ADD_COMMIT, which has no parent in this" >&2
    echo "  clone. Check out with full history (fetch-depth: 0)." >&2
    exit 1
  }

  mkdir -p "$WORK/oldsrc"
  git_at archive "$OLD_REV" | (cd "$WORK/oldsrc" && tar -x) || {
    echo "restore-drill: could not export $OLD_REV into a working tree" >&2; exit 1; }

  # VERIFY, do not assume. If the exported tree is not actually at $OLD_HEAD then
  # the fixture is not older than the code and this phase would be proving
  # nothing while looking green — the failure mode this whole issue was about.
  OLD_SRC_HEAD="$(ls "$WORK/oldsrc/db/migrations" | sed -n 's/^0*\([0-9][0-9]*\)_.*\.sql$/\1/p' | sort -n | tail -1)"
  [ "$OLD_SRC_HEAD" = "$OLD_HEAD" ] || {
    echo "restore-drill: the fixture revision $OLD_REV has migration head $OLD_SRC_HEAD," >&2
    echo "  but this phase needs $OLD_HEAD. Migrations were probably not added one per" >&2
    echo "  commit; build the fixture from a revision whose head really is $OLD_HEAD." >&2
    exit 1
  }
  [ -d "$WORK/oldsrc/cmd/mailroom-drill" ] || {
    echo "restore-drill: revision $OLD_REV has no cmd/mailroom-drill, so it cannot seed" >&2
    echo "  its own fixture. That tool must predate the migration being tested." >&2
    exit 1
  }
  FIXTURE_SRC="$(host_path "$WORK/oldsrc")"
  echo "=== fixture will be built from $OLD_REV (migration head $OLD_SRC_HEAD), not this checkout"
fi

# $$ is not unique across runner invocations, and a run cancelled mid-flight can
# leave its network behind — clear any stale one (and its containers) first.
docker ps -aq --filter "network=$NET" 2>/dev/null | xargs -r docker rm -fv >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null

# =============================================================================
# FIXTURE — build a world worth backing up.
# =============================================================================
if [ "$FIXTURE" -eq 1 ]; then
  echo "=== fixture: building a real Mailroom to back up =============================="
  docker volume create "$VOL" >/dev/null

  # The master key and — independently — the age keypair the artifact is sealed
  # to. TWO secrets from TWO generators: that IS the escrow model.
  # ⚠ --entrypoint sh AND --user root, for the same measured reasons as the
  # sealer in scripts/backup.sh (see its PACKER). AGE_IMAGE reads the SAME
  # MAILROOM_AGE_IMAGE the runbook now tells operators to point at the Mailroom
  # image (#633) — and that image has ENTRYPOINT /mailroom, which docker APPENDS
  # a command to rather than replacing, and runs as non-root, which cannot
  # mkdir /out. Without both overrides this step starts the SERVER instead of
  # generating keys, so the drill would break for exactly the operators who
  # followed the new advice — and the drill is what proves a backup can be
  # opened at all.
  KEYGEN=$(mkcid --entrypoint sh --user root "$AGE_IMAGE" -c "$NET_RETRY"'
    set -eu
    # Install ONLY when missing, and say which happened (#633): a
    # MAILROOM_AGE_IMAGE that already carries age (the Mailroom image does,
    # see the Dockerfile) must need no network here, and `apk add` fetches the
    # index even for an already-installed package, so skipping it is the whole
    # point, not an optimisation.
    if command -v age >/dev/null 2>&1; then
      echo "age: preinstalled" >&2
    else
      echo "age: installing" >&2
      retry apk add --no-cache age >/dev/null
    fi
    mkdir -p /out
    head -c 32 /dev/urandom | od -An -tx1 | tr -d " \n" > /out/master.key
    head -c 32 /dev/urandom | od -An -tx1 | tr -d " \n" > /out/wrong-master.key
    head -c 32 /dev/urandom | od -An -tx1 | tr -d " \n" > /out/pgp-root.key
    head -c 32 /dev/urandom | od -An -tx1 | tr -d " \n" > /out/wrong-pgp-root.key
    age-keygen -o /out/backup-identity.txt 2>/dev/null
    age-keygen -y /out/backup-identity.txt > /out/backup-recipients.txt
  ')
  docker start -a "$KEYGEN" >/dev/null
  for f in master.key wrong-master.key pgp-root.key wrong-pgp-root.key backup-identity.txt backup-recipients.txt; do
    docker cp "$KEYGEN:/out/$f" "$WORK_HOST/$f"
  done
  echo "--- fixture: master key + PGP root (+ their wrong-box decoys) + age keypair generated"

  # Source Postgres, built the way production is: cmd/mailroom-migrate as
  # mailroom_owner. That is what leaves a goose_db_version head for the backup to
  # record — and a database built any other way has no schema version to record.
  docker run -d --name "$SRCPG" --label "$LABEL" --network "$NET" --network-alias srcpg \
    -e POSTGRES_PASSWORD="$PW" -e POSTGRES_DB=mailroom "$PG_SRC_IMAGE" >/dev/null
  wait_pg "$SRCPG"
  docker exec "$SRCPG" psql -q -v ON_ERROR_STOP=1 -U postgres -d mailroom \
    -c "CREATE ROLE mailroom_owner LOGIN PASSWORD '$PW' CREATEROLE;" \
    -c "GRANT ALL ON SCHEMA public TO mailroom_owner;" \
    -c "GRANT ALL ON DATABASE mailroom TO mailroom_owner;" >/dev/null

  MIG=$(mkcid --network "$NET" -w /src \
    -v mailroom-gomod:/go/pkg/mod -v mailroom-gobuild:/root/.cache/go-build \
    -e MAILROOM_MIGRATE_DATABASE_URL="postgres://mailroom_owner:$PW@srcpg:5432/mailroom?sslmode=disable" \
    "$GO_IMAGE" go run ./cmd/mailroom-migrate $FIXTURE_MIGRATE)
  # $FIXTURE_SRC, not $HOSTDIR: under --migrate-forward this is the OLD revision's
  # tree (#236). Identical to the checkout in every other mode.
  docker cp "$FIXTURE_SRC/." "$MIG:/src" >/dev/null 2>&1
  docker start -a "$MIG" >/dev/null
  docker exec "$SRCPG" psql -q -v ON_ERROR_STOP=1 -U postgres -d mailroom \
    -c "ALTER ROLE mailroom_app LOGIN PASSWORD '$PW';" >/dev/null
  echo "--- fixture: db/migrations applied via cmd/mailroom-migrate (the deploy path)"

  # Real mail: real envelope encryption, real blob store, real sealed account
  # credentials — through internal/ingest, the same path the sync engine uses.
  #
  # Also: a real PGP key, sealed under the PGP root, so `verify` step 5 has
  # something to unseal — a fixture with zero stored keys can only report that
  # leg UNPROVEN, which would mean the drill's OWN self-test never exercises
  # its unseal code. --pgp-root-key-file is checked for in $FIXTURE_SRC rather
  # than assumed, because under --migrate-forward $FIXTURE_SRC is an OLD
  # revision (#236) that may predate this flag; passing an unknown flag to
  # that binary would fail seed outright, which is worse than seeding zero keys.
  SEED_PGP_ARGS=""
  if grep -q -- '--pgp-root-key-file' "$FIXTURE_SRC/cmd/mailroom-drill/main.go" 2>/dev/null; then
    SEED_PGP_ARGS="--pgp-root-key-file /pgp-root.key"
  fi
  # shellcheck disable=SC2086 — SEED_PGP_ARGS is intentionally word-split (0 or 2 words)
  SEED=$(mkcid --network "$NET" -w /src -v "$VOL:/blobs" \
    -v mailroom-gomod:/go/pkg/mod -v mailroom-gobuild:/root/.cache/go-build \
    -e MAILROOM_DATABASE_URL="postgres://mailroom_app:$PW@srcpg:5432/mailroom?sslmode=disable" \
    -e MAILROOM_MASTER_KEY_FILE=/master.key \
    -e MAILROOM_BLOB_ROOT=/blobs \
    "$GO_IMAGE" go run ./cmd/mailroom-drill seed --canary "$CANARY" --messages 3 \
    $SEED_PGP_ARGS)
  # THE LINE #236 WAS ABOUT. Seeding an older schema with today's internal/store
  # writes columns that schema does not have; the fixture is seeded by the code
  # that shipped with it.
  docker cp "$FIXTURE_SRC/." "$SEED:/src" >/dev/null 2>&1
  docker cp "$WORK_HOST/master.key" "$SEED:/master.key"
  [ -z "$SEED_PGP_ARGS" ] || docker cp "$WORK_HOST/pgp-root.key" "$SEED:/pgp-root.key"
  docker start -a "$SEED"
  if [ -n "$SEED_PGP_ARGS" ]; then
    echo "--- fixture: mail ingested + one real PGP key seeded, both encrypted at rest"
  else
    echo "--- fixture: mail ingested, encrypted at rest under a key DERIVED from the master"
    echo "--- fixture: NO PGP KEY seeded ($FIXTURE_SRC predates --pgp-root-key-file) —"
    echo "    verify step 5 will report the escrow leg UNPROVEN for this run"
  fi

  # #654: wrong-pgp-root-no-check needs a database with NO recorded check value,
  # so the drill's check-value comparison has nothing to object to and the
  # unseal is reached. The seed always records the row (it must, for every other
  # injection), so remove it here, as the superuser: mailroom_app has no DELETE
  # on it by design. Fail loudly if the row was not there to remove, so this can
  # never silently degrade into a plain wrong-pgp-root run.
  if [ "$INJECT" = "wrong-pgp-root-no-check" ]; then
    if [ -z "$SEED_PGP_ARGS" ]; then
      echo "restore-drill: wrong-pgp-root-no-check needs a fixture that seeds a PGP key" >&2
      exit 2
    fi
    DELETED=$(docker exec "$SRCPG" psql -qAt -v ON_ERROR_STOP=1 -U postgres -d mailroom \
      -c "WITH d AS (DELETE FROM app.pgp_root_check RETURNING 1) SELECT count(*) FROM d;")
    if [ "$DELETED" != "1" ]; then
      echo "restore-drill: expected to delete exactly 1 app.pgp_root_check row, deleted '$DELETED'" >&2
      exit 2
    fi
    echo "--- fixture: app.pgp_root_check row removed from the source (injection: no recorded check value)"
  fi

  # Search for the canary, which the fixture plants in the message BODY and
  # nowhere else. Subject/from/snippet are plaintext columns in Postgres, so a
  # search that matched one of those would still pass on a restore whose blob
  # store was entirely missing. A body word can only be found if a blob was
  # really restored, really decrypted with the escrowed key, and really indexed.
  [ -n "$QUERY" ] || QUERY="$CANARY"

  echo
  echo "=== taking a real backup with scripts/backup.sh ==============================="
  ARTIFACT="$WORK/artifact.age"
  IDENTITY="$WORK/backup-identity.txt"
  MASTER_KEY="$WORK/master.key"
  PGP_ROOT="$WORK/pgp-root.key"
  MAILROOM_BACKUP_DSN="postgres://postgres:$PW@127.0.0.1:5432/mailroom?sslmode=disable" \
  MAILROOM_BACKUP_PG_CONTAINER="$SRCPG" \
  MAILROOM_BACKUP_BLOB_VOLUME="$VOL" \
  MAILROOM_BACKUP_RECIPIENTS="$WORK/backup-recipients.txt" \
  MAILROOM_BACKUP_OUT="$ARTIFACT" \
  MAILROOM_BACKUP_KEEP=2 \
    sh "$DIR/scripts/backup.sh"
  # KEEP=2 is set for coverage, not because the drill wants history (#539).
  # backup-retain.sh has its own --self-test, but that tests retention in
  # isolation against fixture files; nothing else exercises backup.sh actually
  # DELEGATING to it, against a real sealed artifact, on the runner's
  # filesystem. This is the only place that join runs. The dated hard link lands
  # beside $ARTIFACT and the drill restores $ARTIFACT as before, so it changes
  # nothing else about this run — and if retention ever breaks the artifact
  # itself, the restore below is what will say so.
  # Deliberately THIS checkout's backup.sh even when the fixture is older (#236).
  # In the field the artifact would have been written by the old release's script,
  # but the more useful question is whether TODAY's backup script copes with an
  # older schema — a real operator re-running backups during a staged upgrade hits
  # exactly that, and the old script is not the one that can still be fixed.

  # Destroy the source. Everything below must come out of the artifact, and the
  # only way to be sure of that is for the original to be gone.
  docker rm -fv "$SRCPG" >/dev/null 2>&1 || true
  docker volume rm -f "$VOL" >/dev/null 2>&1 || true
  echo "--- fixture: SOURCE DESTROYED. Everything below comes out of the artifact."
fi

# =============================================================================
# INJECTIONS — the failures the drill must be able to see.
# =============================================================================
case "$INJECT" in
  "") ;;
  corrupt)
    echo
    echo "!!! INJECTING 'corrupt': truncating the artifact (a half-copied upload, a full disk)"
    SZ=$(wc -c < "$ARTIFACT" | tr -d ' ')
    dd if="$ARTIFACT" of="$WORK/corrupt.age" bs=1 count=$((SZ - 64)) 2>/dev/null
    ARTIFACT="$WORK/corrupt.age"
    ;;
  no-key)
    echo
    echo "!!! INJECTING 'no-key': nobody could find the escrow envelope"
    MASTER_KEY="$WORK/absent.key"
    rm -f "$MASTER_KEY"
    ;;
  wrong-key)
    echo
    echo "!!! INJECTING 'wrong-key': the escrow envelope from the OTHER box"
    [ -r "$WORK/wrong-master.key" ] || \
      printf '%s' "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff" > "$WORK/wrong-master.key"
    MASTER_KEY="$WORK/wrong-master.key"
    ;;
  no-pgp-root)
    echo
    echo "!!! INJECTING 'no-pgp-root': the SECOND escrow envelope was never retrieved"
    PGP_ROOT="$WORK/absent-pgp-root.key"
    rm -f "$PGP_ROOT"
    ;;
  same-root)
    echo
    echo "!!! INJECTING 'same-root': both secrets point at the SAME file"
    echo "    (the collapse that leaves a stack booting, serving and drilling perfectly"
    echo "     while having exactly one root — ADR-024's whole point, silently deleted)"
    cp "$WORK/master.key" "$WORK/collapsed-root.key"
    PGP_ROOT="$WORK/collapsed-root.key"
    ;;
  wrong-pgp-root)
    echo
    echo "!!! INJECTING 'wrong-pgp-root': the PGP root escrow envelope from the OTHER box"
    echo "    (present, well-formed, genuinely distinct from the master key — but it is"
    echo "     not the root the stored PGP keys were sealed under, so it must not open"
    echo "     them. This is the disaster case ADR-024 exists to catch. It is caught by"
    echo "     the recorded check value (#551), BEFORE any unseal: see"
    echo "     wrong-pgp-root-no-check for the injection that reaches the unseal)"
    [ -r "$WORK/wrong-pgp-root.key" ] || \
      printf '%s' "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100" > "$WORK/wrong-pgp-root.key"
    PGP_ROOT="$WORK/wrong-pgp-root.key"
    ;;
  wrong-pgp-root-no-check)
    echo
    echo "!!! INJECTING 'wrong-pgp-root-no-check': the wrong box's PGP root, against a"
    echo "    database that never recorded app.pgp_root_check (the row was deleted from"
    echo "    the source before the backup). With no recorded value the check-value"
    echo "    comparison cannot object, so this is the only injection that reaches the"
    echo "    drill's unseal check (#654)"
    [ -r "$WORK/wrong-pgp-root.key" ] || \
      printf '%s' "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100" > "$WORK/wrong-pgp-root.key"
    PGP_ROOT="$WORK/wrong-pgp-root.key"
    ;;
  schema)
    echo
    echo "!!! INJECTING 'schema': restoring an old artifact into newer code"
    EXPECT_SCHEMA=$((CODE_HEAD + 92))
    ;;
  no-hit)
    echo
    echo "!!! INJECTING 'no-hit': searching for a canary the backup does not contain"
    QUERY="antidisestablishmentarianism"
    ;;
  missing-blobs)
    # Applied later, against the restored payload — see the block before the
    # blob docker cp. Accepted here so the unknown-injection guard does not
    # reject it.
    ;;
  *) echo "restore-drill: unknown --inject $INJECT" >&2; exit 2 ;;
esac

# =============================================================================
# 1/6 OPEN the artifact + 2/6 checksum the payload.
# =============================================================================
echo
echo "=== 1/6 open the artifact (age) =============================================="
# --entrypoint sh + --user root: see the keygen step's comment (#633). This one
# also writes /work/payload.tar.gz, which a non-root user could not create in a
# root-owned working directory.
OPENER=$(mkcid -w /work --entrypoint sh --user root "$AGE_IMAGE" -c "$NET_RETRY"'
set -eu
# Install ONLY when missing, and say which happened (#633) — see the keygen
# step above for why this is not an optimisation: a pinned MAILROOM_AGE_IMAGE
# is supposed to make this whole step network-free.
if command -v age >/dev/null 2>&1; then
  echo "age: preinstalled" >&2
else
  echo "age: installing" >&2
  retry apk add --no-cache age >/dev/null
fi
if ! age -d -i /identity.txt -o /work/payload.tar.gz /work/artifact.age 2>/tmp/age.err; then
  cat /tmp/age.err >&2
  echo "" >&2
  echo "restore-drill: THE ARTIFACT WILL NOT OPEN." >&2
  echo "  age could not decrypt-and-authenticate it. Either the file is corrupt or" >&2
  echo "  truncated (age authenticates every chunk AND the end of the stream, so a" >&2
  echo "  half-copied file cannot pass), or this is not the identity it was sealed to." >&2
  exit 1
fi
mkdir -p /work/payload
tar -xzf /work/payload.tar.gz -C /work/payload
cd /work/payload
if ! sha256sum -c SHA256SUMS >/dev/null 2>&1; then
  echo "restore-drill: THE PAYLOAD CHECKSUMS DO NOT MATCH." >&2
  sha256sum -c SHA256SUMS >&2 || true
  exit 1
fi
echo "--- artifact opened; every payload file checksums clean"
')
docker cp "$(host_path "$ARTIFACT")" "$OPENER:/work/artifact.age"
docker cp "$(host_path "$IDENTITY")" "$OPENER:/identity.txt"
docker start -a "$OPENER"
docker cp "$OPENER:/work/payload" "$WORK_HOST/payload"

# =============================================================================
# 3/6 SCHEMA VERSION — does this artifact belong to this code?
# =============================================================================
echo
echo "=== 2/6 schema version ======================================================="
MANIFEST="$WORK/payload/manifest.json"
[ -r "$MANIFEST" ] || { echo "restore-drill: the artifact carries no manifest.json" >&2; exit 1; }
jnum() { sed -n "s/.*\"$1\": *\([0-9][0-9]*\).*/\1/p" "$MANIFEST" | head -1; }

ART_SCHEMA="$(jnum schema_version)"
M_USERS="$(jnum users)";       M_ACCOUNTS="$(jnum accounts)"
M_MESSAGES="$(jnum messages)"; M_BLOBS="$(jnum blobs)"
[ -n "$ART_SCHEMA" ] || { echo "restore-drill: the manifest records no schema_version" >&2; exit 1; }

echo "--- artifact schema_version = $ART_SCHEMA"
echo "--- this code expects       = $EXPECT_SCHEMA"
if [ "$ART_SCHEMA" != "$EXPECT_SCHEMA" ]; then
  cat >&2 <<SKEW

restore-drill: SCHEMA VERSION MISMATCH.

  This artifact was taken at goose schema version $ART_SCHEMA.
  The code you are restoring it with is at version $EXPECT_SCHEMA.

  Restoring mail into the wrong schema is how a backup becomes a data-loss event.
  Migrations are forward-only (ADR-008), so the supported moves are:

    * artifact OLDER than the code: restore it as-is (re-run with
      --expect-schema $ART_SCHEMA), then carry it forward with
      \`mailroom-migrate up\`, then re-drill. Spec §16 requires drills to exercise
      exactly this.
    * artifact NEWER than the code: you are restoring with old code. Check out the
      revision whose migration head is $ART_SCHEMA first. Never downgrade a database.
SKEW
  exit 1
fi
echo "--- schema versions agree"

# =============================================================================
# 4/6 RESTORE into a scratch container.
# =============================================================================
echo
# Names the ACTUAL image rather than a hard-coded version: this step can now run
# cross-version, and a banner that always said "18" would be wrong exactly when
# the run was interesting.
echo "=== 3/6 pg_restore into a scratch $PG_DST_IMAGE (dumped from $PG_SRC_IMAGE) ==="
docker run -d --name "$DSTPG" --label "$LABEL" --network "$NET" --network-alias dstpg \
  -e POSTGRES_PASSWORD="$PW" -e POSTGRES_DB=postgres "$PG_DST_IMAGE" >/dev/null
wait_pg "$DSTPG"

# The roles must exist BEFORE the restore: the dump preserves ownership and the
# RLS grants, and pg_restore cannot invent the roles they name. On a real DR box
# this is the step people forget, so the drill does it exactly as the runbook
# tells a human to.
#
# ⚠ CREATEROLE, because production's mailroom_owner HAS it (measured on the
# instance: rolcreaterole = t) and step 4b below migrates the restored snapshot
# forward as that role. Without it the drill models a WEAKER owner than the one
# that ships, and any migration touching roles fails here and only here —
# which is exactly what happened when 00035 added mailroom_wkd (#266): steps 1
# through 4 passed, and 4b stopped on `permission denied to create role`. The
# drill was right to fail; what it had wrong was the role it was failing as.
#
# mailroom_wkd is listed for the same reason, one release early: from schema 35
# it OWNS app.wkd_public_key (#266), so a dump taken at 35 or later names it and
# pg_restore cannot invent it either. The artifact restored here is deliberately
# one version behind, so it does not need the role yet — which is precisely how
# this would have become someone's outage instead of a comment.
docker exec "$DSTPG" psql -q -v ON_ERROR_STOP=1 -U postgres -d postgres \
  -c "CREATE ROLE mailroom_owner NOLOGIN CREATEROLE;" \
  -c "CREATE ROLE mailroom_app   NOLOGIN;" \
  -c "CREATE ROLE mailroom_admin NOLOGIN;" \
  -c "CREATE ROLE mailroom_wkd   NOLOGIN;" \
  -c "GRANT mailroom_wkd TO mailroom_owner;" \
  -c "CREATE DATABASE mailroom OWNER mailroom_owner;" >/dev/null

docker cp "$WORK_HOST/payload/postgres.dump" "$DSTPG:/mr.dump"
# --exit-on-error is NOT optional: without it pg_restore prints its errors, keeps
# going, and exits 0 — one more way to be told that a broken restore is fine.
docker exec "$DSTPG" pg_restore -U postgres -d mailroom --exit-on-error /mr.dump
docker exec "$DSTPG" psql -q -v ON_ERROR_STOP=1 -U postgres -d mailroom \
  -c "ALTER ROLE mailroom_app LOGIN PASSWORD '$PW';" >/dev/null
echo "--- restored"

# =============================================================================
# 5/6 COUNT — did the rows come back?
# =============================================================================
echo
echo "=== 4/6 row counts vs the manifest ==========================================="
R="$(docker exec "$DSTPG" psql -At -F, -U postgres -d mailroom -c \
  "SELECT (SELECT count(*) FROM app.users),
          (SELECT count(*) FROM app.accounts),
          (SELECT count(*) FROM app.messages),
          (SELECT count(*) FROM app.blobs)" | tr -d '\r')"
R_USERS="$(echo "$R" | cut -d, -f1)";    R_ACCOUNTS="$(echo "$R" | cut -d, -f2)"
R_MESSAGES="$(echo "$R" | cut -d, -f3)"; R_BLOBS="$(echo "$R" | cut -d, -f4)"
printf '    %-10s %9s %9s\n' TABLE manifest restored
printf '    %-10s %9s %9s\n' users    "$M_USERS"    "$R_USERS"
printf '    %-10s %9s %9s\n' accounts "$M_ACCOUNTS" "$R_ACCOUNTS"
printf '    %-10s %9s %9s\n' messages "$M_MESSAGES" "$R_MESSAGES"
printf '    %-10s %9s %9s\n' blobs    "$M_BLOBS"    "$R_BLOBS"
if [ "$M_USERS" != "$R_USERS" ] || [ "$M_ACCOUNTS" != "$R_ACCOUNTS" ] ||
   [ "$M_MESSAGES" != "$R_MESSAGES" ] || [ "$M_BLOBS" != "$R_BLOBS" ]; then
  echo "restore-drill: ROW COUNTS DO NOT MATCH the manifest. The restore is incomplete." >&2
  exit 1
fi
echo "--- every row the backup recorded came back"

# --migrate-forward: the restore is validated at its OWN (older) schema above;
# now carry it forward with the forward-only migrations, exactly as a real DR
# runbook does after restoring a backup taken by an earlier release (spec §16).
# Everything below then proves the migrated-forward data on HEAD schema.
if [ "$MIGRATE_FWD" -eq 1 ]; then
  echo
  echo "=== 4b/6 migrate the restored OLD snapshot forward ($ART_SCHEMA -> $CODE_HEAD) ==="
  # The owner role restored NOLOGIN; migrations must run AS the owner so new
  # objects match the ownership of the restored ones (the RLS grants depend on it).
  docker exec "$DSTPG" psql -q -v ON_ERROR_STOP=1 -U postgres -d mailroom \
    -c "ALTER ROLE mailroom_owner LOGIN PASSWORD '$PW';" >/dev/null
  FWD=$(mkcid --network "$NET" -w /src \
    -v mailroom-gomod:/go/pkg/mod -v mailroom-gobuild:/root/.cache/go-build \
    -e MAILROOM_MIGRATE_DATABASE_URL="postgres://mailroom_owner:$PW@dstpg:5432/mailroom?sslmode=disable" \
    "$GO_IMAGE" go run ./cmd/mailroom-migrate up)
  docker cp "$HOSTDIR/." "$FWD:/src" >/dev/null 2>&1
  docker start -a "$FWD"
  docker exec "$DSTPG" psql -q -v ON_ERROR_STOP=1 -U postgres -d mailroom \
    -c "ALTER ROLE mailroom_owner NOLOGIN;" >/dev/null
  NEW_HEAD="$(docker exec "$DSTPG" psql -At -U postgres -d mailroom \
    -c "SELECT max(version_id) FROM public.goose_db_version WHERE is_applied" | tr -d '\r')"
  if [ "$NEW_HEAD" != "$CODE_HEAD" ]; then
    echo "restore-drill: migrate-forward reached schema $NEW_HEAD, expected HEAD $CODE_HEAD" >&2
    exit 1
  fi
  echo "--- migrated $ART_SCHEMA -> $NEW_HEAD; the restored old snapshot is now at HEAD"
  ART_SCHEMA="$NEW_HEAD"   # the drill now proves migrated-forward data at HEAD
fi

# =============================================================================
# 6/6 PROVE IT IS ALIVE. This is the drill.
# =============================================================================
echo
echo "=== 5/6 the users to prove ==================================================="
USER_IDS="$(docker exec "$DSTPG" psql -At -U postgres -d mailroom \
  -c "SELECT id FROM app.users WHERE deleted_at IS NULL ORDER BY id" | tr -d '\r' | tr '\n' ' ')"
[ -n "$(echo "$USER_IDS" | tr -d ' ')" ] || {
  echo "restore-drill: the restored database has NO USERS. There is nothing to prove." >&2; exit 1; }

# ⚠ EVERY USER MUST PASS, AND ON A REAL INSTANCE THAT IS NOT ALWAYS RIGHT (#540).
#
# The rule below — "a user with no messages is a failed restore" — is exactly
# right for the mailbox you are trying to get back, and it is what stops a
# restore that silently produces nothing from reporting success. But the first
# real-artifact drill failed on a SECOND user that legitimately had no mail: this
# instance auto-provisions any identity that completes the OIDC flow (#542), so
# an empty account is a normal thing for a real database to contain and an
# impossible thing for a one-user fixture to contain.
#
# --user-id scopes the proof rather than weakening it. Passing "at least one user
# is alive" instead would have let a restore where four of five mailboxes came
# back empty report success, which is the opposite of what this drill is for.
if [ -n "$ONLY_USERS" ]; then
  for u in $ONLY_USERS; do
    case " $USER_IDS " in
      *" $u "*) ;;
      # A typo'd id that quietly proves nothing is the worst outcome here: the
      # drill would print a confident verdict about a user the artifact does not
      # contain.
      *) echo "restore-drill: --user-id $u is not a live user in the RESTORED database." >&2
         echo "  The artifact holds: $USER_IDS" >&2
         exit 2 ;;
    esac
  done
  USER_IDS="$ONLY_USERS"
  echo "--- scoped by --user-id to: $USER_IDS"
else
  echo "--- $USER_IDS"
fi

echo
echo "=== 6/6 decrypt + reindex + search + BOTH escrowed roots ====================="
SHOW="--show-content=false"
[ "$FIXTURE" -eq 1 ] && SHOW="--show-content=true"

VERIFY=$(mkcid --network "$NET" -w /src \
  -v mailroom-gomod-alpine:/go/pkg/mod -v mailroom-gobuild-alpine:/root/.cache/go-build \
  -e MAILROOM_DATABASE_URL="postgres://mailroom_app:$PW@dstpg:5432/mailroom?sslmode=disable" \
  -e MAILROOM_MASTER_KEY_FILE=/escrow/master.key \
  -e MAILROOM_PGP_ROOT_KEY_FILE=/escrow/pgp-root.key \
  -e MAILROOM_BLOB_ROOT=/blobs \
  -e MAILROOM_SEARCH_ROOT=/scratch-index \
  -e USER_IDS="$USER_IDS" -e DRILL_QUERY="$QUERY" -e SHOW="$SHOW" \
  -e NO_PGP_ROOT="$NO_PGP_ROOT" \
  -e TANTIVY_VERSION="$TANTIVY_VERSION" \
  "$GO_ALPINE" sh -c "$NET_RETRY"'
set -eu
if [ ! -r /escrow/master.key ]; then
  echo "" >&2
  echo "restore-drill: THE ESCROWED MASTER KEY IS NOT HERE." >&2
  echo "  The artifact restored, and what came back is unreadable mail. The master key is" >&2
  echo "  escrowed separately and offline (ADR-005) precisely so that holding the backup is" >&2
  echo "  not the same as holding the mail. Retrieve it from escrow and run this again." >&2
  exit 1
fi
retry apk add --no-cache gcc musl-dev curl tar >/dev/null
# To a FILE, then untar: retrying a `curl | tar` pipe would hand tar the
# concatenation of every partial attempt.
retry curl -fsSL -o /tmp/tantivy.tar.gz \
  "https://github.com/anyproto/tantivy-go/releases/download/${TANTIVY_VERSION}/linux-amd64-musl.tar.gz"
tar -xzf /tmp/tantivy.tar.gz -C /usr/local/lib
mkdir -p /scratch-index

# The REAL engine (CGO/Tantivy, ADR-006), not the in-memory fallback: a drill that
# proved an engine the image does not ship would be proving nothing.
CGO_ENABLED=1 go build -tags tantivy -o /drill ./cmd/mailroom-drill

args=""
[ -n "${DRILL_QUERY:-}" ] && args="--query $DRILL_QUERY"
# The caller explicitly accepted that this instance has no PGP root. The
# drill prints what it therefore did NOT prove; it does not pass in silence.
[ "${NO_PGP_ROOT:-0}" = "1" ] && args="$args --no-pgp-root"
rc=0
for u in $USER_IDS; do
  # shellcheck disable=SC2086 — $args is intentionally word-split
  /drill verify --user-id "$u" "$SHOW" $args || rc=1
done
exit "$rc"
')
# ⚠ COPY THE SOURCE, NOT THE REPOSITORY — AND DO NOT SWALLOW THE ERROR (#540).
#
# This was `docker cp "$HOSTDIR/." "$VERIFY:/src" >/dev/null 2>&1`, and the
# FIRST EVER real-artifact drill died right here with no message at all: `set -e`
# aborted on a failed copy whose stderr had been discarded, after phases 1-5 had
# every one of them passed. On a disaster-recovery day that is the worst output
# this script could produce — it looks like the restore worked and then stops.
#
# The cause generalises beyond the host it was found on. `.git` was 58 MB of a
# 73 MB tree that the build does not read (the container runs `go build`, never
# `git`), and **a single unreadable file anywhere in the copied tree corrupts the
# whole tar stream**: docker writes the member header, cannot read the body, and
# every subsequent file then fails with `missed writing N bytes` until the copy
# ends in `unexpected EOF`. It is not a partial copy, it is a total loss. Here
# the unreadable files were three root-owned `.git/objects/bitmap-ref-tips_*`
# left by a `git gc` someone ran as root months earlier — invisible to a CI
# checkout, which is precisely why this only surfaced on a real host.
#
# Staged through a tarball rather than piped, so that each step's own exit status
# is checked instead of a pipeline's — the same reason `--exit-on-error` is not
# optional two phases up, and the same failure mode this drill exists to catch.
mkdir -p "$WORK/src"
tar -cf "$WORK/src.tar" \
    --exclude=./.git --exclude='./.drill-tmp.*' --exclude='./.backup-tmp.*' \
    -C "$DIR" . \
  || { echo "restore-drill: could not archive the source tree at $DIR — if a file in it" >&2
       echo "  is unreadable to you, tar says which; one such file breaks the whole copy." >&2
       exit 1; }
tar -xf "$WORK/src.tar" -C "$WORK/src" \
  || { echo "restore-drill: could not unpack the staged source tree" >&2; exit 1; }
rm -f "$WORK/src.tar"
docker cp "$WORK_HOST/src/." "$VERIFY:/src" \
  || { echo "restore-drill: could not copy the staged source into the verify container" >&2; exit 1; }
# ⚠ 'missing-blobs' acts HERE, not in the case block above, because it has to
# damage the RESTORED PAYLOAD rather than the artifact: the point is a restore
# where Postgres came back whole and the blob store did not. That is the shape
# of a partial object-store copy, and it is precisely what the drill could not
# see before #351 B6 — an unreadable body degraded to a headers-only document,
# was still counted as indexed, and the drill printed THE RESTORE IS ALIVE.
if [ "$INJECT" = "missing-blobs" ]; then
  echo
  echo "!!! INJECTING 'missing-blobs': deleting restored blob files"
  echo "    (Postgres intact, blob store partially copied — the rows still exist and"
  echo "     are searchable by subject, but their content is gone. The drill must"
  echo "     refuse to call that a live restore.)"
  # ⚠ PRESERVE THE BLOB STEP 1 NEEDS, OR THIS TESTS THE WRONG THING. The drill
  # decrypts each user's OLDEST message directly in step 1; if that blob goes,
  # the drill fails there and the new whole-corpus check in step 3 is never
  # reached. Measured the first time this was written: it failed with
  # "[1/5] DECRYPT FAILED: blob: not found", which is the PRE-EXISTING check,
  # so the injection proved nothing about the one it was added for.
  #
  # Blob filenames are the hex content hash, and hashes have no relation to
  # message order — so "keep the first file" does not keep the oldest message.
  # Ask Postgres instead.
  # ⚠ while-read over a HEREDOC, not `for f in $(find ...)`. The first version of
  # this used word-splitting and silently mis-fired: this repo's checkout path
  # contains a space, so each path split into fragments, `rm -f` ignored the
  # non-existent ones, and the counter still advanced — it reported "deleted 5 of
  # 3" and the drill PASSED. A heredoc also keeps the loop out of a subshell, so
  # the counters survive it.
  KEEP=$(docker exec "$DSTPG" psql -At -U postgres -d mailroom -c \
    "SELECT DISTINCT ON (user_id) encode(content_hash,'hex') FROM app.messages ORDER BY user_id, id")
  _n=0; _deleted=0; _kept=0
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    _n=$((_n + 1))
    if printf '%s\n' "$KEEP" | grep -qx "$(basename "$f")"; then
      _kept=$((_kept + 1)); continue
    fi
    rm -f "$f" && _deleted=$((_deleted + 1))
  done <<INNER_EOF
$(find "$WORK/payload/blobs" -type f | sort)
INNER_EOF
  echo "    $_n blob file(s): deleted $_deleted, kept $_kept that step 1 decrypts"
  echo "    directly — so a failure here can ONLY come from step 3"
  if [ "$_deleted" -eq 0 ]; then
    echo "!!! nothing was deleted — this injection cannot exercise its check" >&2
    exit 2
  fi
fi
docker cp "$WORK_HOST/payload/blobs" "$VERIFY:/blobs"
# The escrow directory is assembled from whatever was actually supplied. A key
# that is missing stays missing — the drill's job is to notice, not to paper
# over it, and the `no-key` / `no-pgp-root` injections prove it does.
mkdir -p "$WORK/escrow"
if [ -r "$MASTER_KEY" ]; then
  cp "$MASTER_KEY" "$WORK/escrow/master.key"
fi
if [ -n "$PGP_ROOT" ] && [ -r "$PGP_ROOT" ]; then
  cp "$PGP_ROOT" "$WORK/escrow/pgp-root.key"
fi
docker cp "$WORK_HOST/escrow" "$VERIFY:/escrow"
docker start -a "$VERIFY"

echo
echo "=============================================================================="
echo " THE RESTORE IS ALIVE."
echo "   artifact opened, checksums clean, schema $ART_SCHEMA matched,"
echo "   $R_MESSAGES message(s) restored and decrypted with the ESCROWED master key,"
echo "   the search index REBUILT from Postgres, and a search returned a hit."
if [ "$NO_PGP_ROOT" -eq 1 ]; then
  echo ""
  echo " EXCEPT the PGP root, which you asked to skip. This run says NOTHING about"
  echo " whether mail encrypted to your PGP keys is recoverable — see docs/key-escrow.md."
else
  echo "   The SECOND escrowed root is present and distinct from the master key."
fi
echo " This backup is no longer a hypothesis."
echo "=============================================================================="
