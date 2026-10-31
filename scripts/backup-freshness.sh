#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Mailroom backup freshness — THE SIGNAL THAT IS ALLOWED TO GO RED (Forgejo #539).
#
# scripts/backup.sh produces a sound, self-verifying artifact. Nothing watches
# whether it ever runs again. This is that watcher: one exit code, pollable by
# anything, with no Mailroom process involved — the artifact is a host file, so
# the check is a host script.
#
# THE RULE THIS ENCODES, already written in the runbook about the restore drill
# and equally true here: "a scheduled job nobody reads is a scheduled job that
# is failing." A backup you cannot see stopping is not a backup.
#
# ⚠ "FRESH" MUST NOT MEAN "A FILE WAS TOUCHED", AND THAT IS THE WHOLE REASON
# THIS DOES MORE THAN CHECK AN MTIME. A partial `docker cp`, a full disk, or a
# rename that lost a race all leave a file at the expected path with a current
# timestamp. An mtime-only check calls that healthy, which is the precise failure
# this project keeps paying for: a green signal that reports on the wrong thing.
# So the artifact must also be non-empty AND carry the age header. That turns the
# green into "a plausible artifact exists", which is the claim worth making.
#
# It deliberately does NOT verify the artifact restores. Only a restore proves
# that (scripts/restore-drill.sh), and pretending otherwise here would be the
# same overclaim in a new place.
#
# Usage:
#
#   MAILROOM_BACKUP_OUT=/backups/mailroom.age sh scripts/backup-freshness.sh
#   sh scripts/backup-freshness.sh --path /backups/mailroom.age --max-age-hours 36
#   sh scripts/backup-freshness.sh --self-test
#
# --path defaults to $MAILROOM_BACKUP_OUT so the check and the backup are
# configured from the same variable, and a timer that sets one cannot silently
# watch a different file than it writes.
#
# --max-age-hours defaults to 36: a nightly backup has missed at least one full
# run before this fires, so a single transient failure does not page anyone,
# while two consecutive ones do. Set it from the schedule, not from taste —
# roughly 1.5x the interval.
#
# Exit codes (the interface; the text is for humans):
#
#   0  OK       an artifact exists, looks like age, and is within the bound
#   1  NOT OK   missing, empty, not an age artifact, or older than the bound
#   2  usage error — a broken invocation must NOT look like a healthy backup,
#                    which is why it is not 0, nor 1 (it is not a backup verdict)
set -eu

AGE_MAGIC='age-encryption.org/v1'
PATH_ARG="${MAILROOM_BACKUP_OUT:-}"
MAX_AGE_HOURS="${MAILROOM_BACKUP_MAX_AGE_HOURS:-36}"
PROM_OUT=""
MODE=check

usage() {
  echo "usage: $0 [--path FILE] [--max-age-hours N] [--prom-out FILE] | --self-test" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --path)           [ $# -ge 2 ] || usage; PATH_ARG="$2"; shift 2 ;;
    --max-age-hours)  [ $# -ge 2 ] || usage; MAX_AGE_HOURS="$2"; shift 2 ;;
    --prom-out)       [ $# -ge 2 ] || usage; PROM_OUT="$2"; shift 2 ;;
    --self-test)      MODE=selftest; shift ;;
    -h|--help)        usage ;;
    *)                echo "backup-freshness: unknown argument '$1'" >&2; usage ;;
  esac
done

# mtime in epoch seconds. GNU first, BSD second. A host with neither is not a
# reason to report a healthy backup, so the caller treats failure here as unknown
# rather than as fresh.
mtime_of() {
  stat -c %Y -- "$1" 2>/dev/null && return 0
  stat -f %m -- "$1" 2>/dev/null && return 0
  return 1
}

size_of() { wc -c < "$1" 2>/dev/null | tr -d ' \t\n'; }

# --- the check ---------------------------------------------------------------
# Returns 0 (OK) or 1 (not OK), and sets REASON / AGE_SECS / SIZE for the
# caller to print and to export. Split from the printing so the self-test can
# assert the VERDICT without parsing prose.
REASON=""
AGE_SECS=-1
SIZE=0
check_artifact() {
  _f="$1"; _max_h="$2"

  if [ -z "$_f" ]; then
    REASON="no path given: set MAILROOM_BACKUP_OUT or pass --path"
    return 1
  fi
  if [ ! -e "$_f" ]; then
    REASON="MISSING: no artifact at $_f — either a backup has never run, or its output moved"
    return 1
  fi
  if [ ! -f "$_f" ]; then
    REASON="NOT A FILE: $_f exists but is not a regular file (a directory here usually means a bind mount created it)"
    return 1
  fi

  SIZE="$(size_of "$_f")"
  SIZE="${SIZE:-0}"
  if [ "$SIZE" -eq 0 ]; then
    REASON="EMPTY: $_f is 0 bytes — a run started and produced nothing"
    return 1
  fi

  # The header check. `head -c` on a large file reads only the first bytes.
  if ! head -c 21 -- "$_f" 2>/dev/null | grep -q "$AGE_MAGIC"; then
    REASON="NOT AN AGE ARTIFACT: $_f does not begin with '$AGE_MAGIC' ($SIZE bytes) — truncated, partially copied, or not the file you think"
    return 1
  fi

  if ! _mt="$(mtime_of "$_f")"; then
    REASON="UNKNOWN AGE: cannot read the mtime of $_f (no usable stat) — refusing to report healthy on a measurement I could not take"
    return 1
  fi
  _now="$(date -u +%s)"
  AGE_SECS=$((_now - _mt))
  # A future mtime is a clock or filesystem problem, not freshness. Reporting it
  # as healthy would hide exactly the sort of host fault worth knowing about.
  if [ "$AGE_SECS" -lt -300 ]; then
    REASON="FUTURE MTIME: $_f is dated $(( -AGE_SECS )) s in the future — check the host clock"
    return 1
  fi
  [ "$AGE_SECS" -lt 0 ] && AGE_SECS=0

  _max_s=$((_max_h * 3600))
  if [ "$AGE_SECS" -gt "$_max_s" ]; then
    REASON="STALE: $_f is $((AGE_SECS / 3600))h old, bound is ${_max_h}h — backups have stopped"
    return 1
  fi

  REASON="OK: $_f is $((AGE_SECS / 3600))h old ($SIZE bytes), within ${_max_h}h"
  return 0
}

# Optional Prometheus textfile output, for a node_exporter textfile collector.
# Written even when the check fails — a metrics file that disappears on failure
# teaches the dashboard to show "no data" instead of "broken".
write_prom() {
  _ok="$1"; _out="$2"
  _tmp="$_out.tmp.$$"
  {
    echo "# HELP mailroom_backup_ok Whether a plausible, in-date Mailroom backup artifact exists (1) or not (0)."
    echo "# TYPE mailroom_backup_ok gauge"
    echo "mailroom_backup_ok $_ok"
    echo "# HELP mailroom_backup_age_seconds Age of the newest Mailroom backup artifact, -1 if unknown."
    echo "# TYPE mailroom_backup_age_seconds gauge"
    echo "mailroom_backup_age_seconds $AGE_SECS"
    echo "# HELP mailroom_backup_size_bytes Size of the newest Mailroom backup artifact."
    echo "# TYPE mailroom_backup_size_bytes gauge"
    echo "mailroom_backup_size_bytes $SIZE"
  } > "$_tmp"
  mv -f "$_tmp" "$_out"
}

# --- self-test ---------------------------------------------------------------
# CI runs this (see .forgejo/workflows/ci.yml), because "the alert fires when
# backups stop" is a claim that has to be demonstrated, not asserted. #539's
# acceptance criteria say so explicitly.
ST_FAILS=0
st_case() {
  _label="$1"; _want="$2"; _file="$3"; _max="$4"
  if check_artifact "$_file" "$_max"; then _got=0; else _got=1; fi
  if [ "$_got" = "$_want" ]; then
    printf 'ok    %-52s -> %s\n' "$_label" "$REASON"
  else
    printf 'FAIL  %-52s -> wanted exit %s, got %s (%s)\n' "$_label" "$_want" "$_got" "$REASON"
    ST_FAILS=$((ST_FAILS + 1))
  fi
}

# Set an mtime N hours in the past. GNU `-d` first, BSD `-t` second.
age_file() {
  _f="$1"; _hours="$2"
  touch -d "$_hours hours ago" -- "$_f" 2>/dev/null && return 0
  _stamp="$(date -u -v-"${_hours}"H +%Y%m%d%H%M 2>/dev/null)" || return 1
  touch -t "$_stamp" -- "$_f" 2>/dev/null
}

self_test() {
  _tmp="$(mktemp -d 2>/dev/null || mktemp -d -t mrbf)"
  trap 'rm -rf "$_tmp"' EXIT INT TERM

  # A real-shaped artifact: the age header plus filler, so the size check is not
  # what carries the positive case.
  _good="$_tmp/good.age"
  { printf '%s\n' "$AGE_MAGIC"; printf 'x%.0s' $(seq 1 200); } > "$_good"

  echo "--- the alerting conditions (each must go RED)"
  st_case "no path given"                          1 ""                  36
  st_case "MISSING: no artifact at all"            1 "$_tmp/absent.age"   36
  : > "$_tmp/empty.age"
  st_case "EMPTY: 0 bytes"                         1 "$_tmp/empty.age"    36
  printf 'not an age file at all\n' > "$_tmp/plain.txt"
  st_case "NOT AGE: fresh but wrong content"       1 "$_tmp/plain.txt"    36
  # The truncation case, which is the one an mtime-only check gets wrong: a
  # current timestamp on bytes that are not an artifact.
  head -c 8 -- "$_good" > "$_tmp/truncated.age"
  st_case "TRUNCATED: fresh, non-zero, cut short"  1 "$_tmp/truncated.age" 36
  mkdir -p "$_tmp/adir.age"
  st_case "NOT A FILE: a directory at the path"    1 "$_tmp/adir.age"     36

  echo
  echo "--- staleness, against the same artifact (only the mtime differs)"
  _stale="$_tmp/stale.age"; cp -- "$_good" "$_stale"
  if age_file "$_stale" 100; then
    st_case "STALE: 100h old, 36h bound"           1 "$_stale"            36
  else
    echo "FAIL  cannot back-date a file on this host (no usable touch -d/-t), so"
    echo "      the staleness case CANNOT be proven here. That is a broken"
    echo "      self-test, not a passing one."
    ST_FAILS=$((ST_FAILS + 1))
  fi

  echo
  echo "--- THE POSITIVE CONTROL (must go GREEN, or every case above is vacuous)"
  st_case "OK: fresh, non-empty, age header"       0 "$_good"             36
  # The same artifact with a bound that contains its age — proves the bound is
  # read, not ignored. Without this, a check hard-coded to 0 would pass the
  # staleness case above and look correct.
  _recent="$_tmp/recent.age"; cp -- "$_good" "$_recent"
  if age_file "$_recent" 10; then
    st_case "OK: 10h old under a 36h bound"        0 "$_recent"           36
    st_case "STALE: the SAME 10h file, 2h bound"   1 "$_recent"           2
  fi

  echo
  if [ "$ST_FAILS" != "0" ]; then
    echo "BACKUP FRESHNESS SELF-TEST FAILED ($ST_FAILS case(s))"
    exit 1
  fi
  echo "backup freshness self-test: all cases passed, both directions proven"
}

# --- main --------------------------------------------------------------------
if [ "$MODE" = selftest ]; then
  self_test
  exit 0
fi

case "$MAX_AGE_HOURS" in
  ''|*[!0-9]*) echo "backup-freshness: --max-age-hours must be a whole number of hours, got '$MAX_AGE_HOURS'" >&2; exit 2 ;;
esac

if check_artifact "$PATH_ARG" "$MAX_AGE_HOURS"; then
  echo "backup-freshness: $REASON"
  [ -n "$PROM_OUT" ] && write_prom 1 "$PROM_OUT"
  exit 0
fi
echo "backup-freshness: $REASON" >&2
[ -n "$PROM_OUT" ] && write_prom 0 "$PROM_OUT"
exit 1
