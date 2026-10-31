#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Decide whether a commit can affect a RESTORE — the relevance filter behind
# .forgejo/workflows/restore-drill.yml (Forgejo #306, #273).
#
# ── why this exists as a script, and why it computes rather than lists ────────
#
# The drill workflow always runs, so the required context is always present and
# honest (#273); the "can this commit affect a restore?" question was pushed one
# level down, into the job. That filter was written as a five-entry regex
# (scripts/backup.sh, scripts/restore-drill.sh, cmd/mailroom-drill/,
# db/migrations/, the workflow file) against the ONE incident that motivated it —
# migration 00035 — and was never widened to the drill's real dependency set.
#
# It had already rotted by v1.34.4: that release changed internal/store,
# internal/writeback, internal/sync, internal/api, internal/mailsync and
# internal/onboarding, matched NOTHING in the regex, and the drill reported
# success without running. That skip happened to be correct on the merits, but
# the filter could not have told the difference — which is the same shape as a
# required check that reports success without running.
#
# `cmd/mailroom-drill` imports mailroom/db and internal/{blob,config,crypto,
# ingest,onboarding,pgpkeyd,search,search/tantivy,store,syncrunner} directly —
# 26 first-party packages once those are followed transitively — and both drill
# phases shell out to `go run ./cmd/mailroom-migrate`. A second hand-written copy
# of that list would decay exactly the way the first one did, so this script
# DERIVES it: it walks the first-party import graph out of the checked-out
# source, starting from the two commands the drill actually runs. Add an import,
# and the set widens on the same commit.
#
# ── why the walk is a grep and not `go list -deps` ────────────────────────────
#
# `go list -deps ./cmd/mailroom-drill` is the authoritative answer, and this
# script is checked against it — see --check-go-list, which CI runs inside the
# build-test job where a Go toolchain and a warm module cache already exist.
#
# It is not what runs in the drill workflow, for two reasons. The relevance step
# is the one thing that executes on EVERY push, including the docs-only pushes
# it exists to skip cheaply, and it has no Go toolchain: adding setup-go plus a
# full module download there costs a minute of runner time per commit. Worse, it
# would put proxy.golang.org on the critical path of a REQUIRED check — a module
# fetch hiccup would turn the relevance decision red and wedge merges, which is
# the same class of harm (#273) this workflow was built to remove.
#
# The grep walk needs no toolchain, no network and no module cache, and it errs
# in the safe direction: it reads every *.go file in a package regardless of
# build tags (so the `-tags tantivy` build the drill actually performs is
# covered), and any string literal that merely looks like a first-party import
# path only WIDENS the set, costing a drill rather than skipping one.
#
# ── usage ────────────────────────────────────────────────────────────────────
#
#   sh scripts/drill-relevance.sh --paths          print the relevant prefixes
#   git diff --name-only A B | sh scripts/drill-relevance.sh --decide
#                                                  print relevant=true|false
#   sh scripts/drill-relevance.sh --self-test      prove it can say BOTH
#   sh scripts/drill-relevance.sh --check-go-list  prove the walk is a superset
#                                                  of what the linker really uses
set -eu

DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$DIR"

MODULE="$(sed -n 's/^module[[:space:]][[:space:]]*\([^[:space:]]*\).*/\1/p' go.mod | head -1)"
[ -n "$MODULE" ] || { echo "drill-relevance: cannot read the module path from go.mod" >&2; exit 2; }

# The two commands the drill runs: `go build -tags tantivy ./cmd/mailroom-drill`
# for the PROVE-IT-IS-ALIVE phase (scripts/restore-drill.sh), and
# `go run ./cmd/mailroom-migrate` in BOTH the fixture and --migrate-forward
# phases. The migration RUNNER's own code was invisible to the old regex, which
# matched only the migration FILES.
ROOTS="${MODULE}/cmd/mailroom-drill ${MODULE}/cmd/mailroom-migrate"

# Inputs the import graph cannot see. Each is a file the drill reads or executes
# at run time rather than something it links:
#
#   scripts/backup.sh        the drill takes a real backup with it
#   scripts/restore-drill.sh the drill itself
#   scripts/drill-relevance.sh   this file: a change to the FILTER must be
#                            drilled, or a filter that wrongly skips everything
#                            would suppress the very run that would expose it
#   the workflow             ditto, one level up
#   go.mod / go.sum          a dependency bump changes what the drill links
#                            without touching a single first-party file
#   third_party/go-imap/     the vendored go-imap fork (#52, ADR-047): the
#                            drill links it, and a patch to it changes go.sum
#                            nowhere — it is a dependency edited in place
#
# db/ is deliberately NOT here: it arrives through the import graph, because
# cmd/mailroom-migrate imports `mailroom/db` (the embedded migrations), so
# db/migrations/ and db/schema.sql are covered by the derived set.
EXTRA_PATHS='scripts/backup.sh
scripts/restore-drill.sh
scripts/drill-relevance.sh
.forgejo/workflows/restore-drill.yml
go.mod
go.sum
third_party/go-imap/'

# pkg_dir maps a first-party import path to its directory in this checkout.
pkg_dir() {
  _p="${1#"$MODULE"}"
  _p="${_p#/}"
  [ -n "$_p" ] || _p="."
  printf '%s\n' "$_p"
}

# pkg_imports prints the first-party import paths appearing in a package's
# non-test Go files.
#
# _test.go is EXCLUDED on purpose: the drill links `go build`'s output, not a
# test binary, so a test-only dependency is not part of what a restore
# exercises. (Changes to a linked package's own _test.go files still count —
# they live under that package's directory prefix.)
#
# Every Go import is a quoted string literal containing the full package path,
# whatever the import's form (aliased, blank, grouped), so matching the quoted
# module path cannot miss one. Build-tagged files are read too, which is
# required: cmd/mailroom-drill reaches internal/search/tantivy only from a file
# behind `//go:build tantivy`, and the drill builds with that tag.
pkg_imports() {
  _dir="$1"
  [ -d "$_dir" ] || return 0
  # shellcheck disable=SC2086 — the glob must expand
  find "$_dir" -maxdepth 1 -name '*.go' ! -name '*_test.go' -type f -exec cat {} + 2>/dev/null \
    | grep -oE "\"${MODULE}(/[A-Za-z0-9_.-]+)*\"" \
    | tr -d '"' \
    | sort -u
}

# closure prints, one per line, the DIRECTORY of every first-party package
# reachable from ROOTS. Plain breadth-first search with a visited set.
closure() {
  _todo="$ROOTS"
  _seen=""
  while [ -n "$_todo" ]; do
    _next=""
    for _pkg in $_todo; do
      case " $_seen " in *" $_pkg "*) continue ;; esac
      _seen="$_seen $_pkg"
      _d="$(pkg_dir "$_pkg")"
      for _imp in $(pkg_imports "$_d"); do
        case " $_seen $_next " in *" $_imp "*) continue ;; esac
        _next="$_next $_imp"
      done
    done
    _todo="$_next"
  done
  for _pkg in $_seen; do
    pkg_dir "$_pkg"
  done | sort -u
}

# relevant_paths prints every path prefix a commit could touch that this drill
# depends on: the derived package directories (as `dir/`) plus the fixed inputs.
relevant_paths() {
  closure | sed 's|$|/|'
  printf '%s\n' "$EXTRA_PATHS"
}

# decide reads changed file paths on stdin and prints the verdict plus, on a
# SKIP, the full set it matched against.
#
# Printing the set is the point of #306's second ask: a wrong skip is otherwise
# undiagnosable after the fact — the log said "nothing here can affect a
# restore" and never said what "nothing" was measured against.
decide() {
  _changed="$(cat)"
  _paths="$(relevant_paths | sort -u)"

  # Anchored prefix match, the same shape the old regex had — a changed file is
  # relevant when its path STARTS with one of these. The sed escapes the regex
  # metacharacters that occur in real paths (`.` in .forgejo and go.mod), so
  # `go.mod` cannot also match `gohmod`. `/` is deliberately NOT escaped: it is
  # an ordinary character in an ERE, and `\/` is undefined behaviour that only
  # GNU grep happens to accept — every path here is full of them.
  _hits=""
  for _p in $_paths; do
    _re="^$(printf '%s' "$_p" | sed 's/[].[^$*\\]/\\&/g')"
    if printf '%s\n' "$_changed" | grep -qE "$_re"; then
      _hits="$_hits$_p
"
    fi
  done

  if [ -n "$_hits" ]; then
    echo "→ this commit touches the restore's dependency set — drilling"
    echo "  matched prefixes:"
    printf '%s' "$_hits" | sed 's/^/    /'
    echo "relevant=true"
    return 0
  fi

  echo "→ nothing here can affect a restore — skipping the drill, reporting success"
  echo "  ⚠ measured against the DERIVED dependency set below. If a skip here was"
  echo "    wrong, the bug is in this list, and this is the list it used:"
  printf '%s\n' "$_paths" | sed 's/^/    /'
  echo "relevant=false"
}

# --- self-test ---------------------------------------------------------------
#
# A filter that always says true is not a filter, and a filter that always says
# false is the defect #306 reported. Both directions are asserted, plus a
# negative control that proves this is a CLOSURE and not "anything under
# internal/" — without that control, `grep -q '^internal/'` would pass the
# positive case trivially.
ST_FAILS=0
st_case() {
  _label="$1"; _want="$2"; _files="$3"
  _got="$(printf '%s\n' "$_files" | decide | sed -n 's/^relevant=\(.*\)$/\1/p')"
  if [ "$_got" = "$_want" ]; then
    echo "ok    $_label → relevant=$_got"
  else
    echo "FAIL  $_label → relevant=$_got, wanted $_want"
    ST_FAILS=$((ST_FAILS + 1))
  fi
}

self_test() {
  echo "--- the derived dependency set"
  relevant_paths | sort -u | sed 's/^/    /'
  echo

  # The proven miss. v1.34.4 changed internal/store and the old regex skipped;
  # internal/crypto is the one the drill exists to exercise against the ESCROWED
  # key rather than a unit-test double.
  st_case "a commit touching only internal/crypto/" true "internal/crypto/envelope.go"
  st_case "a commit touching only internal/store/"  true "internal/store/drafts.go"
  # The migration runner, invisible to the regex that matched only db/migrations/.
  st_case "a commit touching only cmd/mailroom-migrate/" true "cmd/mailroom-migrate/main.go"
  st_case "a migration" true "db/migrations/00036_example.sql"
  st_case "the backup script" true "scripts/backup.sh"
  st_case "a dependency bump" true "go.sum"
  st_case "a patch to the vendored go-imap fork" true "third_party/go-imap/imapclient/fetch.go"

  # The control in the other direction: without it, "always true" passes every
  # case above and costs a 60-minute drill on every docs typo.
  st_case "a commit touching only docs/" false "docs/mailroom-spec.md
docs/adr/adr-005-backup.md"
  st_case "a changelog + web-only commit" false "CHANGELOG.md
web/src/app/ui/messageDocument.ts"

  # The control that proves this is a dependency CLOSURE. internal/api is a
  # first-party package the drill does not link, so a filter that merely matched
  # "internal/" would answer true here.
  if closure | grep -qx 'internal/api'; then
    echo "FAIL  the negative control is spent: internal/api is now IN the drill's"
    echo "      closure, so it no longer proves this is a closure rather than a"
    echo "      prefix match. Pick another first-party package the drill does not"
    echo "      link and update this case."
    ST_FAILS=$((ST_FAILS + 1))
  else
    st_case "a first-party package the drill does NOT link (internal/api)" false \
      "internal/api/email.go"
  fi

  # A floor on the closure, so a walk that silently degenerates to "just the two
  # roots" — a broken grep, a renamed module path — is caught rather than
  # reported as a clean, very cheap skip. This is a floor and two named members,
  # NOT a second copy of the dependency list.
  _n="$(closure | wc -l | tr -d ' ')"
  if [ "$_n" -ge 8 ]; then
    echo "ok    the closure has $_n first-party packages (floor: 8)"
  else
    echo "FAIL  the closure collapsed to $_n packages — the walk is broken"
    ST_FAILS=$((ST_FAILS + 1))
  fi
  for _must in internal/store internal/crypto internal/search/tantivy db; do
    if closure | grep -qx "$_must"; then
      echo "ok    the closure contains $_must"
    else
      echo "FAIL  the closure is missing $_must"
      ST_FAILS=$((ST_FAILS + 1))
    fi
  done

  echo
  if [ "$ST_FAILS" != "0" ]; then
    echo "DRILL RELEVANCE SELF-TEST FAILED ($ST_FAILS case(s))"
    exit 1
  fi
  echo "=== drill relevance filter says BOTH true and false, for the right reasons ==="
}

# --- cross-check against the authoritative resolver ---------------------------
#
# The walk above is a grep, so the thing that keeps it honest is this: the set it
# derives must be a SUPERSET of what the Go toolchain says the drill actually
# links. Run where a toolchain already exists (the build-test job), never on the
# relevance path.
#
# A MISSING Go toolchain is a FAILURE here, not a skip — the same rule as
# pgp-interop (#263). A check that quietly does not run is the defect this whole
# script is a response to.
check_go_list() {
  command -v go >/dev/null 2>&1 || {
    echo "drill-relevance: --check-go-list needs a Go toolchain and there is none." >&2
    echo "  This mode exists to prove the grep walk agrees with 'go list -deps';" >&2
    echo "  skipping it would leave the walk unchecked, which is the failure being" >&2
    echo "  guarded. Run it where a toolchain exists (the build-test CI job)." >&2
    exit 2
  }
  _have="$(closure)"
  # -tags tantivy because that is how scripts/restore-drill.sh builds the drill.
  _want="$(go list -deps -tags tantivy ./cmd/mailroom-drill ./cmd/mailroom-migrate \
    | grep -E "^${MODULE}(/|$)")"
  _missing=""
  for _p in $_want; do
    _d="$(pkg_dir "$_p")"
    printf '%s\n' "$_have" | grep -qx "$_d" || _missing="$_missing $_d"
  done
  echo "go list -deps: $(printf '%s\n' "$_want" | wc -l | tr -d ' ') first-party packages"
  echo "grep walk:     $(printf '%s\n' "$_have" | wc -l | tr -d ' ') package directories"
  if [ -n "$_missing" ]; then
    echo "FAIL  the walk MISSES packages the drill really links:$_missing" >&2
    echo "      A miss here means a commit to one of them would wrongly skip the" >&2
    echo "      drill — exactly the #306 defect. Fix pkg_imports/closure." >&2
    exit 1
  fi
  # Extras are expected and safe (other build tags, string literals): they widen
  # the filter, which costs a drill rather than skipping one. Report them so the
  # difference is visible rather than assumed.
  for _d in $_have; do
    printf '%s\n' "$_want" | while read -r _p; do pkg_dir "$_p"; done | grep -qx "$_d" \
      || echo "note  extra (widens the filter, harmless): $_d"
  done
  echo "ok    the derived set is a superset of go list -deps -tags tantivy"
}

case "${1:---decide}" in
  --paths) relevant_paths | sort -u ;;
  --deps) closure ;;
  --decide) decide ;;
  --self-test) self_test ;;
  --check-go-list) check_go_list ;;
  *) echo "usage: $0 [--decide|--paths|--deps|--self-test|--check-go-list]" >&2; exit 2 ;;
esac
