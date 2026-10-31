#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Mailroom backup retention — bounded local history beside the latest artifact
# (#539).
#
# scripts/backup.sh writes ONE path, $MAILROOM_BACKUP_OUT, and every previous
# caller wants exactly that: restore-drill.sh takes a throwaway fixture backup
# and the runbook names a single file. So $OUT stays the canonical "latest" and
# history lives beside it as DATED HARD LINKS.
#
# WHY HISTORY AT ALL, when a nightly run rewrites $OUT every night: one artifact
# is one chance. A backup taken while the database was already damaged
# overwrites the last good one, and damage is normally noticed after that. What
# retention buys is not "a backup" — it is "a backup from before the thing that
# went wrong".
#
# WHY HARD LINKS: the current artifact's bytes are stored once however many
# dated names point at them, so keeping 7 nightlies of a multi-GB artifact costs
# one artifact plus the nightly deltas, not seven. Unlinking an old name frees
# nothing until the last name for those bytes is gone, which is exactly the
# semantics wanted. A filesystem without hard links falls back to a copy.
#
# ⚠ THIS LIVES IN ITS OWN SCRIPT SO IT CAN BE TESTED WITHOUT A DATABASE.
# Inline in backup.sh it would only ever execute on a production host — a
# destructive loop (`rm` in a glob) that no test could reach, which is the
# shape this project has been bitten by. `--self-test` runs in CI.
#
# Usage:
#
#   sh scripts/backup-retain.sh --out /backups/mailroom.age --keep 7
#   sh scripts/backup-retain.sh --self-test
#
# Exit codes:
#
#   0  retention applied (or --keep 0: nothing to do)
#   2  usage error
#   3  THE ARTIFACT IS INTACT BUT RETENTION IS NOT IN EFFECT — could not create
#      the dated copy. Callers must treat this as a warning, never as a failed
#      backup: the artifact at --out is complete either way, and failing a good
#      backup over its bookkeeping would be the worse outcome.
set -eu

OUT=""
KEEP=""
MODE=apply

usage() {
  echo "usage: $0 --out FILE --keep N | --self-test" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --out)       [ $# -ge 2 ] || usage; OUT="$2"; shift 2 ;;
    --keep)      [ $# -ge 2 ] || usage; KEEP="$2"; shift 2 ;;
    --self-test) MODE=selftest; shift ;;
    -h|--help)   usage ;;
    *)           echo "backup-retain: unknown argument '$1'" >&2; usage ;;
  esac
done

# The dated-name shape. UTC and fixed width, so lexicographic order IS
# chronological order and pruning needs no date parsing. The glob deliberately
# cannot match $OUT itself, nor backup.sh's `.partial.<pid>` from a concurrent
# run — both would be catastrophic things to prune.
stamp_now() { date -u +%Y%m%dT%H%M%SZ; }
dated_glob() { printf '%s.????????T??????Z\n' "$1"; }

# --- the operation -----------------------------------------------------------
# Sets N_PRUNED and DATED for the caller to report.
N_PRUNED=0
DATED=""
retain() {
  _out="$1"; _keep="$2"

  case "$_keep" in
    ''|*[!0-9]*) echo "backup-retain: --keep must be a whole number, got '$_keep'" >&2; return 2 ;;
  esac
  [ -n "$_out" ] || { echo "backup-retain: --out is required" >&2; return 2; }
  # Refuse to date-stamp something that is not there. A dated link to a missing
  # artifact would be a lie the freshness check cannot see.
  [ -f "$_out" ] || { echo "backup-retain: no artifact at $_out" >&2; return 2; }

  [ "$_keep" -eq 0 ] && return 0

  DATED="$_out.$(stamp_now)"
  if ! ln -f -- "$_out" "$DATED" 2>/dev/null; then
    if ! cp -p -- "$_out" "$DATED" 2>/dev/null; then
      return 3
    fi
  fi

  # Prune by name, newest-first, dropping everything past the bound.
  # shellcheck disable=SC2046 — the glob is intentionally word-split
  for _old in $(ls -1 $(dated_glob "$_out") 2>/dev/null | LC_ALL=C sort -r | tail -n +$((_keep + 1))); do
    # Belt and braces: never unlink the canonical path, whatever the glob did.
    [ "$_old" = "$_out" ] && continue
    rm -f -- "$_old" && N_PRUNED=$((N_PRUNED + 1))
  done
  return 0
}

# --- self-test ---------------------------------------------------------------
ST_FAILS=0
st_expect() {
  _label="$1"; _want="$2"; _got="$3"
  if [ "$_got" = "$_want" ]; then
    printf 'ok    %-56s %s\n' "$_label" "$_got"
  else
    printf 'FAIL  %-56s got %s, wanted %s\n' "$_label" "$_got" "$_want"
    ST_FAILS=$((ST_FAILS + 1))
  fi
}

# Make an artifact plus N dated siblings with distinct, ordered stamps.
seed() {
  _dir="$1"; _n="$2"
  _out="$_dir/mailroom.age"
  printf 'age-encryption.org/v1\npayload\n' > "$_out"
  _i=1
  while [ "$_i" -le "$_n" ]; do
    # 2026010100000{i}Z style stamps: fixed width, strictly increasing.
    printf 'age-encryption.org/v1\nold %s\n' "$_i" > "$_out.2026010${_i}T000000Z"
    _i=$((_i + 1))
  done
  printf '%s\n' "$_out"
}

n_dated() { ls -1 "$1".????????T??????Z 2>/dev/null | wc -l | tr -d ' \t\n'; }

# inode NAME -> its inode number, or "unknown" if this host's `stat` supports
# neither the GNU nor the BSD flag. Used to tell two ARTIFACTS apart (below),
# where nlink alone cannot: nlink counts names on the SAME bytes, not whether
# two files are the same bytes at all.
inode() { stat -c %i -- "$1" 2>/dev/null || stat -f %i -- "$1" 2>/dev/null || echo unknown; }

self_test() {
  _tmp="$(mktemp -d 2>/dev/null || mktemp -d -t mrbr)"
  trap 'rm -rf "$_tmp"' EXIT INT TERM

  echo "--- pruning to the bound"
  mkdir -p "$_tmp/a"; _out="$(seed "$_tmp/a" 5)"
  N_PRUNED=0; retain "$_out" 3 || true
  # 5 seeded + 1 created now = 6; keeping 3 means 3 remain.
  st_expect "5 old + 1 new, --keep 3 -> dated copies remaining" 3 "$(n_dated "$_out")"
  st_expect "the canonical artifact still exists"               yes "$([ -f "$_out" ] && echo yes || echo no)"
  # The NEWEST must survive: today's stamp sorts above the 2026-01-0x fixtures.
  st_expect "the newest dated copy is the one just created"     yes \
    "$([ -f "$DATED" ] && echo yes || echo no)"
  st_expect "the OLDEST fixture was pruned"                     no \
    "$([ -f "$_out.20260101T000000Z" ] && echo yes || echo no)"

  echo
  echo "--- the negative controls: things that must NEVER be pruned"
  mkdir -p "$_tmp/b"; _out="$(seed "$_tmp/b" 2)"
  # A concurrent run's partial, and an unrelated neighbour.
  printf 'partial\n' > "$_out.partial.99999"
  printf 'unrelated\n' > "$_tmp/b/notes.txt"
  N_PRUNED=0; retain "$_out" 1 || true
  st_expect "a concurrent run's .partial is untouched"          yes \
    "$([ -f "$_out.partial.99999" ] && echo yes || echo no)"
  st_expect "an unrelated neighbouring file is untouched"       yes \
    "$([ -f "$_tmp/b/notes.txt" ] && echo yes || echo no)"
  st_expect "the canonical artifact is untouched"               yes \
    "$([ -f "$_out" ] && echo yes || echo no)"
  st_expect "--keep 1 leaves exactly 1 dated copy"              1 "$(n_dated "$_out")"

  echo
  echo "--- --keep 0 is the default, and must do nothing at all"
  mkdir -p "$_tmp/c"; _out="$(seed "$_tmp/c" 2)"
  N_PRUNED=0; DATED=""; retain "$_out" 0 || true
  st_expect "--keep 0 creates no dated copy"                    2 "$(n_dated "$_out")"
  st_expect "--keep 0 prunes nothing"                           0 "$N_PRUNED"

  echo
  echo "--- refusals (a broken invocation must not look like success)"
  mkdir -p "$_tmp/d"
  set +e; retain "$_tmp/d/absent.age" 3 >/dev/null 2>&1; _rc=$?; set -e
  st_expect "no artifact at --out -> exit 2"                    2 "$_rc"
  set +e; retain "$_tmp/a/mailroom.age" "seven" >/dev/null 2>&1; _rc=$?; set -e
  st_expect "--keep 'seven' -> exit 2"                          2 "$_rc"

  echo
  echo "--- THE POSITIVE CONTROL: TONIGHT's dated copy is a hard link to \$OUT"
  # This proves only that making tonight's dated name does not store tonight's
  # bytes a second time (nlink=2: \$OUT and the dated name share one inode). It
  # does NOT prove "history is nearly free" — that claim is false; see the
  # two-night case below for what KEEP actually costs.
  mkdir -p "$_tmp/e"; _out="$(seed "$_tmp/e" 0)"
  N_PRUNED=0; retain "$_out" 5 || true
  _links="$(stat -c %h -- "$_out" 2>/dev/null || stat -f %l -- "$_out" 2>/dev/null || echo unknown)"
  if [ "$_links" = unknown ]; then
    echo "FAIL  cannot read a link count on this host, so the hard-link claim"
    echo "      CANNOT be proven here. That is a broken self-test, not a pass."
    ST_FAILS=$((ST_FAILS + 1))
  else
    st_expect "the dated copy is a LINK to \$OUT's bytes TONIGHT (nlink)"   2 "$_links"
  fi

  echo
  echo "--- TWO NIGHTS ARE TWO ARTIFACTS: KEEP does not make history free"
  # Simulate backup.sh for real: each night writes a PARTIAL file and mv -f's
  # it onto \$OUT (a new inode every time — backup.sh's own atomic-write
  # comment explains why), then backup-retain.sh hard-links tonight's dated
  # name to whatever inode \$OUT holds right now. So the link only ever avoids
  # storing TONIGHT's bytes twice; it can never make last night's dated copy
  # alias tonight's, because last night's artifact is already a different
  # inode by the time tonight's link is made.
  mkdir -p "$_tmp/f"; _out="$_tmp/f/mailroom.age"

  printf 'age-encryption.org/v1\nnight one\n' > "$_out.partial.1"
  mv -f "$_out.partial.1" "$_out"
  N_PRUNED=0; DATED=""; retain "$_out" 2 || true
  _night1_dated="$DATED"
  _night1_inode="$(inode "$_night1_dated")"

  # A stamp is second-resolution (stamp_now): without this, a fast run could
  # mint the SAME dated name for both nights, and `ln -f` would silently
  # replace night one's dated copy with night two's — reading as "the first
  # night's dated copy still exists" while actually holding night two's bytes.
  sleep 1

  printf 'age-encryption.org/v1\nnight two\n' > "$_out.partial.2"
  mv -f "$_out.partial.2" "$_out"
  N_PRUNED=0; DATED=""; retain "$_out" 2 || true
  _night2_dated="$DATED"
  _night2_inode="$(inode "$_night2_dated")"

  st_expect "the two nights got DIFFERENT dated names"           yes \
    "$([ "$_night1_dated" != "$_night2_dated" ] && echo yes || echo no)"
  st_expect "the first night's dated copy still exists"          yes \
    "$([ -f "$_night1_dated" ] && echo yes || echo no)"
  st_expect "the second night's dated copy still exists"         yes \
    "$([ -f "$_night2_dated" ] && echo yes || echo no)"
  if [ "$_night1_inode" = unknown ] || [ "$_night2_inode" = unknown ]; then
    echo "FAIL  cannot read an inode number on this host, so the two-artifact"
    echo "      claim CANNOT be proven here. That is a broken self-test, not a pass."
    ST_FAILS=$((ST_FAILS + 1))
  else
    st_expect "the two nights are DIFFERENT inodes (two real artifacts)" no \
      "$([ "$_night1_inode" = "$_night2_inode" ] && echo yes || echo no)"
  fi
  st_expect "--keep 2 kept BOTH nights' dated copies"             2 "$(n_dated "$_out")"

  echo
  if [ "$ST_FAILS" != "0" ]; then
    echo "BACKUP RETENTION SELF-TEST FAILED ($ST_FAILS case(s))"
    exit 1
  fi
  echo "backup retention self-test: all cases passed"
}

# --- main --------------------------------------------------------------------
if [ "$MODE" = selftest ]; then
  self_test
  exit 0
fi

[ -n "$OUT" ] || usage
[ -n "$KEEP" ] || usage

set +e
retain "$OUT" "$KEEP"
RC=$?
set -e
case "$RC" in
  0)
    [ -n "$DATED" ] && echo "backup-retain: dated copy $DATED"
    [ "$N_PRUNED" -gt 0 ] && echo "backup-retain: pruned $N_PRUNED dated copy(ies), keeping $KEEP"
    exit 0 ;;
  3)
    echo "backup-retain: WARNING could not create a dated copy beside $OUT." >&2
    echo "backup-retain: the artifact itself is complete; retention is NOT in effect." >&2
    exit 3 ;;
  *) exit "$RC" ;;
esac
