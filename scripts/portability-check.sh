#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
#
# Portability check (CLAUDE.md's open-source portability rule, mailroom#351).
#
# The rule: nothing in this tree may identify the operator or the infrastructure
# they self-host — no personal domains, internal hostnames, VPS/homelab details,
# private repo/auth/service URLs, or personal email addresses. Neutral
# placeholders only.
#
# ⚠ IT IS A RULE THAT HAS BEEN BROKEN BEFORE BY SOMEONE WHO KNEW IT. During the
# #351 review a private repository reference was committed into a CI workflow
# while the tree was being audited for exactly that. It was caught by a manual
# sweep, at the end, by luck. That is the argument for a check that runs every
# time.
#
# THREE LAYERS, because one cannot do the job:
#
#   1. THE OPERATOR DENYLIST, supplied from OUTSIDE the repository via
#      $MAILROOM_PORTABILITY_PATTERNS. The strings that identify a particular
#      operator are exactly the strings that must not be committed, so a
#      denylist checked into the repo would BE the leak it exists to prevent.
#      In CI it comes from a repository secret; locally, export it or accept
#      that this layer is skipped.
#
#   2. THE HOST ALLOWLIST (scripts/portability-hosts.allow), which needs no
#      operator knowledge at all. A private service URL is, structurally, a URL
#      — so requiring every host this repo names to be on a short list catches
#      the general shape without anyone having to predict the specific string.
#      This is the same move #206 made when a header denylist became an
#      allowlist: enumerate what is allowed, because you cannot enumerate what
#      is not. It works by finding every `https?://HOST` in the tree.
#
#   3. THE SCHEME-LESS IMAGE-REFERENCE CHECK (#548), same allowlist, different
#      extraction. Layer 2 can only see a host that follows a URL scheme, and a
#      container image reference never has one: `registry.example.net/owner/
#      name:tag` and `docker login registry.example.net` both name a host with
#      no `http(s)://` anywhere nearby. #548 was exactly this — a private
#      registry hostname in `echo "image=…"` and `docker login …` passed layer
#      2 locally without a murmur, because a bare image reference isn't a URL.
#      Layer 3 finds the realistic places a registry host is written without a
#      scheme — YAML `image:` keys, `image=` output lines, Dockerfile `FROM`/
#      `COPY --from=`, `docker login|push|pull|tag|run|build`/`buildx build -t`
#      arguments, and `scripts/*.env` image defaults — and judges the host it
#      finds by the exact same reserved/allowlist rule as layer 2.
#
# ⚠ LAYER 2 (NOW LAYERS 2+3) CANNOT REPLACE LAYER 1, AND THAT WAS MEASURED, NOT
# ASSUMED. The violation this check exists because of was a bare
# `<org>/<repo>#<n>` cross-repository reference — no scheme, no host, nothing a
# URL or image-reference scan can see. Reproduced with a stand-in org: layers 2
# and 3 pass it without a murmur, and only the denylist finds it. That is why CI
# runs with --require-patterns and a job with no configured patterns is RED
# rather than green-with-a-note.
#
# ⚠ AND THE FIRST RUN WITH REAL PATTERNS FAILED ON THIS VERY FILE. The sentence
# above originally quoted the offending reference verbatim, org name and all, in
# the course of explaining why quoting such things is forbidden. That is the
# second time the same string was committed by someone actively working on this
# rule. It is the strongest argument the check makes for itself: describe the
# SHAPE of a leak, never an instance of one.
#
# ⚠ AND LAYER 1 REPORTS ITS OWN BOUND. Without --require-patterns (a local run)
# it prints that it checked ZERO, loudly, rather than passing quietly — "no
# operator strings found" and "nobody looked" must never render identically
# (the project has been bitten by a bounded result reading as a total: #155,
# #176). Layer 3 reports its own bound too: it prints how many image/registry
# references it examined, because a layer that silently examined zero would
# look identical to one that found nothing wrong.
#
# ⚠ AND EVEN LAYERS 2+3 TOGETHER DO NOT SEE EVERY SCHEME-LESS STRING. They cover
# the sites named above; a bare host or identifier in prose, a Go module path,
# or any other scheme-less string not shaped like one of those sites is covered
# ONLY by the operator denylist (layer 1) — which a local run skips unless
# MAILROOM_PORTABILITY_PATTERNS is set. See the NOTE printed after layer 1,
# below, which says this every run.
set -eu

SELF="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/$(basename -- "$0")"
DEFAULT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"

TAB="$(printf '\t')"

# --- shared host judgment (layers 2 and 3 both call this) --------------------
#
# $allowed must be set (the trimmed, lowercased contents of $ALLOW, one host
# per line) before this is called. Returns 0 (true) if the host is NOT allowed
# — i.e. it is a violation — and 1 (false) if it is reserved-by-RFC,
# single-label, or on the allowlist.
is_bad_host() {
  h="$1"
  case "$h" in
    *.example|*.test|*.invalid|*.localhost|localhost) return 1 ;;
    example.com|example.net|example.org|example.edu) return 1 ;;
    *.example.com|*.example.net|*.example.org|*.example.edu) return 1 ;;
    *.*) ;;                # a dotted name: must be allowlisted
    *) return 1 ;;         # single label (a compose service name) — not a domain
  esac
  ! printf '%s\n' "$allowed" | grep -qxF "$h"
}

# --- layer 3 helpers ----------------------------------------------------------
#
# Trim a value extracted from a site (YAML/.env/shell text): drop a trailing
# `# comment`, surrounding whitespace, and one layer of surrounding quotes.
trim_ref() {
  v="$1"
  v="${v%%#*}"
  v="$(printf '%s' "$v" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
  case "$v" in
    \"*\") v="${v#\"}"; v="${v%\"}" ;;
    \'*\') v="${v#\'}"; v="${v%\'}" ;;
  esac
  printf '%s\n' "$v"
}

# Resolve what can be resolved without running anything: a `${VAR:-default}`
# shell/compose expansion resolves to its default; a bare `$VAR`/`${VAR}` or a
# `${{ ... }}` Actions expression is dynamic and cannot be judged here — prints
# nothing and returns 1. Everything else (a static literal) passes through.
resolve_dynamic() {
  ref="$1"
  case "$ref" in
    \$\{\{*) return 1 ;;                 # ${{ ... }} — a Forgejo/GH Actions expr
    \$\{*:-*\})
      ref="${ref#*:-}"
      ref="${ref%\}}"
      ;;
    \$*) return 1 ;;                     # bare $VAR / ${VAR}, no default
  esac
  printf '%s\n' "$ref"
}

# Given an image reference (already trimmed), print the registry host it
# names, or nothing if it has none. Mirrors Docker's own rule: the first path
# component of a reference is a registry host only if it contains a '.' or a
# ':' (a port) or is exactly 'localhost' — otherwise it is a Docker Hub
# organisation (`clamav/clamav`, not a host) or a bare Hub image
# (`postgres:18`, no host at all).
image_ref_host() {
  ref="$(resolve_dynamic "$1")" || return 0
  ref="$(printf '%s' "$ref" | tr 'A-Z' 'a-z')"
  ref="${ref%%@sha256:*}"
  ref="${ref%%@sha512:*}"
  case "$ref" in
    */*) ;;
    *) return 0 ;;                       # no path separator: no host possible
  esac
  first="${ref%%/*}"
  case "$first" in
    *.*|*:*|localhost) ;;
    *) return 0 ;;                       # a Docker Hub org, e.g. clamav/clamav
  esac
  printf '%s\n' "${first%%:*}"
}

# `docker login ARG`: the argument itself IS the host (there is no image path
# or tag to strip), so this does not go through image_ref_host.
login_arg_host() {
  ref="$(resolve_dynamic "$1")" || return 0
  ref="$(printf '%s' "$ref" | tr 'A-Z' 'a-z')"
  printf '%s\n' "${ref%%:*}"
}

# The extraction pass over the tree. One awk invocation per candidate file
# (simpler than juggling FNR/FILENAME resets across a single multi-file
# invocation, and this repo has a few dozen matching files, not thousands).
# Prints FILE<TAB>LINE<TAB>SITE<TAB>RAWVALUE, one record per candidate found.
#
# Lines ending in a backslash are joined with the next physical line first
# (with the leading-line's line number kept), so a `docker build \` /
# `  -t IMAGE \` continuation — exactly tag-provenance.yml's shape — is
# examined as the one logical command it is.
l3_awk() {
  cat <<'AWKEOF'
{
  line = $0
  sub(/\r$/, "", line)
  if (buf == "") bufstart = FNR
  if (sub(/\\[ \t]*$/, "", line)) {
    buf = (buf == "" ? line : buf " " line)
    next
  }
  buf = (buf == "" ? line : buf " " line)
  handle(buf, bufstart)
  buf = ""
}
END {
  if (buf != "") handle(buf, bufstart)
}

function handle(line, startno,    is_yaml, is_dockerfile, is_env, is_sh, rest, rest2, valstart, m, t1, t2, trimmed) {
  # A comment line (prose describing a command, not the command itself) is
  # exempt from every site below — including THIS file's own header, which
  # explains these sites in English and would otherwise be a false positive
  # on itself the moment it names an example.
  trimmed = line
  sub(/^[ \t]+/, "", trimmed)
  if (substr(trimmed, 1, 1) == "#") return

  is_yaml = (FILENAME ~ /\.ya?ml$/)
  is_dockerfile = (FILENAME ~ /(^|\/)Dockerfile[^\/]*$/)
  is_env = (FILENAME ~ /\.env$/)
  is_sh = (FILENAME ~ /\.sh$/)

  # Site A: YAML "image:" mapping key.
  if (is_yaml && match(line, /(^|[ \t-])image:[ \t]*[^ \t]/)) {
    valstart = RSTART + RLENGTH - 1
    emit("image:", startno, substr(line, valstart))
  }

  # Site B: image= inside a quoted echo/$GITHUB_OUTPUT string (YAML only).
  if (is_yaml && match(line, /["\047]image=[^"\047 \t]/)) {
    valstart = RSTART + RLENGTH - 1
    emit("image=", startno, cutfirst(substr(line, valstart)))
  }

  # Site C: Dockerfile FROM.
  if (is_dockerfile && match(line, /^FROM[ \t]+/)) {
    rest = substr(line, RSTART + RLENGTH)
    if (match(rest, /^--platform=[^ \t]+[ \t]+/)) rest = substr(rest, RSTART + RLENGTH)
    emit("FROM", startno, firsttoken(rest))
  }

  # Site D: Dockerfile COPY --from= (a build-stage name has no '/', so it is
  # examined and simply produces no host — see image_ref_host).
  if (is_dockerfile && match(line, /COPY[ \t]+--from=[^ \t]+/)) {
    m = substr(line, RSTART, RLENGTH)
    sub(/^COPY[ \t]+--from=/, "", m)
    emit("COPY --from", startno, m)
  }

  # Site E: docker subcommand arguments, in YAML run: text or a shell script.
  if (is_yaml || is_sh) {
    if (match(line, /docker[ \t]+login[ \t]+/)) {
      rest = substr(line, RSTART + RLENGTH)
      sub(/^[ \t]+/, "", rest)
      emit("docker login", startno, firstrunarg(rest))
    }
    if (match(line, /docker[ \t]+(push|pull)[ \t]+/)) {
      rest = substr(line, RSTART + RLENGTH)
      sub(/^[ \t]+/, "", rest)
      emit("docker push/pull", startno, firstrunarg(rest))
    }
    if (match(line, /docker[ \t]+tag[ \t]+/)) {
      rest = substr(line, RSTART + RLENGTH)
      sub(/^[ \t]+/, "", rest)
      if (rest != "") {
        t1 = firstarg(rest)
        rest2 = substr(rest, g_toklen + 1)
        sub(/^[ \t]+/, "", rest2)
        emit("docker tag (src)", startno, t1)
        if (rest2 != "") {
          t2 = firstarg(rest2)
          emit("docker tag (dst)", startno, t2)
        }
      }
    }
    if ((line ~ /docker[ \t]+build/ || line ~ /buildx[ \t]+build/) &&
        match(line, /(^|[ \t])(-t|--tag)[ \t]+/)) {
      rest = substr(line, RSTART + RLENGTH)
      emit("docker build -t", startno, firstarg(rest))
    }
    if (match(line, /docker[ \t]+run[ \t]+/)) {
      rest = substr(line, RSTART + RLENGTH)
      sub(/^[ \t]+/, "", rest)
      emit("docker run", startno, firstrunarg(rest))
    }
  }

  # Site F: scripts/*.env image reference default.
  if (is_env && match(line, /^[A-Za-z_][A-Za-z0-9_]*=/)) {
    emit("env assignment", startno, substr(line, RLENGTH + 1))
  }
}

# Cut at the first space or tab (Dockerfile arguments are not quoted).
function firsttoken(s,   p1, p2, p) {
  p1 = index(s, " "); p2 = index(s, "\t")
  p = (p1 == 0) ? p2 : (p2 == 0 ? p1 : (p1 < p2 ? p1 : p2))
  if (p == 0) return s
  return substr(s, 1, p - 1)
}

# Cut at the first space, tab, or quote (an image= value sits inside a quoted
# echo string, so the value itself is never quoted).
function cutfirst(s,   p1, p2, p3, p4, p) {
  p1 = index(s, " "); p2 = index(s, "\t")
  p3 = index(s, "\""); p4 = index(s, "\047")
  p = 0
  if (p1 > 0) p = p1
  if (p2 > 0 && (p == 0 || p2 < p)) p = p2
  if (p3 > 0 && (p == 0 || p3 < p)) p = p3
  if (p4 > 0 && (p == 0 || p4 < p)) p = p4
  if (p == 0) return s
  return substr(s, 1, p - 1)
}

# Quote-aware single-argument extraction: if s starts with a quote, capture
# everything up to the matching closing quote (including internal spaces —
# needed for "${{ steps.meta.outputs.host }}"); otherwise cut at the first
# space/tab. Sets g_toklen to how many characters of s were consumed, so a
# caller can advance past this argument to look for a second one.
function firstarg(s,   q, rest, p, t) {
  q = substr(s, 1, 1)
  if (q == "\"" || q == "\047") {
    rest = substr(s, 2)
    p = index(rest, q)
    if (p > 0) { g_toklen = p + 1; return substr(rest, 1, p - 1) }
    g_toklen = length(s)
    return rest
  }
  t = firsttoken(s)
  g_toklen = length(t)
  return t
}

# `docker SUBCOMMAND [flags...] ARG ...`: walk tokens, skipping flags (and the
# following token for flags known to take a separate value — e.g. `docker pull
# -q IMAGE`, `docker run --network-alias pg $PG_IMAGE`), and return the first
# non-flag token. Used for run, login, push and pull. Best-effort: it does not
# understand quoting inside an unquoted token (`KEY="$(cmd a b)"` reads as
# three tokens, not one — a known gap, harmless in practice because the
# misattributed extra "argument" it produces has no dots and is never the
# actual image, so the real image reference on that line — always a $VAR in
# this repo — simply goes unexamined rather than misjudged) and the valflags
# list is not exhaustive for every docker flag that ever takes a value.
function firstrunarg(s,   valflags, tok, rest2) {
  valflags = " -e --env -v --volume -p --publish --name --network --network-alias --entrypoint --user -u -w --workdir --add-host --label -l --mount --env-file --username --password --platform "
  rest2 = s
  while (1) {
    sub(/^[ \t]+/, "", rest2)
    if (rest2 == "") return ""
    tok = firstarg(rest2)
    rest2 = substr(rest2, g_toklen + 1)
    if (tok ~ /^-/) {
      if (tok !~ /=/ && index(valflags, " " tok " ") > 0) {
        sub(/^[ \t]+/, "", rest2)
        firstarg(rest2)
        rest2 = substr(rest2, g_toklen + 1)
      }
      continue
    }
    return tok
  }
}

function emit(kind, lineno, val) {
  print FILENAME "\t" lineno "\t" kind "\t" val
}
AWKEOF
}

# --- self-test -----------------------------------------------------------
#
# Proves layer 3 can actually reject, and that the enumerated pass-cases do
# not false-positive. Builds temporary fixture trees under mktemp -d — never
# the repo — and runs THIS SCRIPT against them via --scan-root, so the
# self-test exercises the real check end-to-end, not a reimplementation of it.
#
# ⚠ THE MADE-UP BAD HOST IS NEVER A LITERAL TOKEN IN THIS FILE. It is assembled
# from two shell string fragments at runtime (immediately below) and every
# fixture is written from a `<<'EOF'` (fully quoted) heredoc containing the
# placeholder __BADHOST__, substituted in afterwards with `sed`. So neither
# "registry.notallowed-example-host.net" nor any fixture line naming it ever
# appears as contiguous text in this script's own source — which matters
# because this script IS a `.sh` file layer 3 itself scans (for site E, docker
# subcommands): a literal fixture line would be visible to the real check
# scanning its own source, exactly the trap #548 is about. Belt and braces:
# the split-fragment host additionally could not be reconstructed by a naive
# scan even if a placeholder leaked, since "registry.notallowed" and
# "-example-host.net" never sit adjacently in this file either.
ST_FAILS=0

st_expect_fail() {
  # $1 = label, $2 = dir, $3 = grep pattern the FAIL output must contain
  _label="$1"; _dir="$2"; _want_grep="$3"
  _out="$(sh "$SELF" --scan-root "$_dir" 2>&1)" && _rc=0 || _rc=$?
  if [ "$_rc" = "0" ]; then
    printf 'FAIL  %-46s -> wanted a reject, check PASSED\n' "$_label"
    ST_FAILS=$((ST_FAILS + 1))
    return
  fi
  # Two things must both be true, or this proves nothing about LAYER 3
  # specifically: the host must be named, AND layer 3's own FAIL line (not
  # layer 2's, which has a different remedy and would mean the reference was
  # somehow also seen as a URL) must be the one that printed it.
  if ! printf '%s\n' "$_out" | grep -qF "$_want_grep" \
     || ! printf '%s\n' "$_out" | grep -q '^FAIL: image/registry reference host'; then
    printf 'FAIL  %-46s -> rejected, but not for the expected reason\n' "$_label"
    printf '%s\n' "$_out" | sed 's/^/        /'
    ST_FAILS=$((ST_FAILS + 1))
    return
  fi
  printf 'ok    %-46s -> rejected by layer 3 (found "%s")\n' "$_label" "$_want_grep"
}

st_expect_pass() {
  _label="$1"; _dir="$2"
  _out="$(sh "$SELF" --scan-root "$_dir" 2>&1)" && _rc=0 || _rc=$?
  if [ "$_rc" != "0" ]; then
    printf 'FAIL  %-46s -> wanted a pass, check FAILED\n' "$_label"
    printf '%s\n' "$_out" | sed 's/^/        /'
    ST_FAILS=$((ST_FAILS + 1))
    return
  fi
  printf 'ok    %-46s -> passed\n' "$_label"
}

# Writes stdin (a fully-quoted heredoc in the caller) to $1, replacing the
# placeholder __BADHOST__ with the runtime-assembled bad host.
st_write() {
  sed "s/__BADHOST__/$bad_host/g" > "$1"
}

st_init_repo() {
  # $1 = dir. Seeded with the REAL scripts/portability-hosts.allow (layer 2
  # requires the file to exist at all — this is not the thing under test —
  # and reusing the real one means "mail.example.com" etc. below are judged
  # exactly as they would be in the real repo). core.autocrlf off: these
  # fixtures are read back by git grep/git ls-files as plain bytes, not
  # checked out, so CRLF conversion would only add noise.
  mkdir -p "$1/scripts"
  cp "$DEFAULT_DIR/scripts/portability-hosts.allow" "$1/scripts/portability-hosts.allow"
  ( cd "$1" && git init -q -b main >/dev/null \
    && git config core.autocrlf false \
    && git config user.email t@example.com && git config user.name t )
}

st_commit() {
  # $1 = dir. git ls-files (which layer 3 uses) reads the index, so files
  # must be `add`ed — no commit is needed.
  ( cd "$1" && git add -A >/dev/null )
}

self_test() {
  bad_part1="registry.notallowed"
  bad_part2="-example-host.net"
  bad_host="$bad_part1$bad_part2"

  st_root="$(mktemp -d 2>/dev/null || mktemp -d -t mrpc)"
  trap 'rm -rf "$st_root"' EXIT INT TERM

  echo "=== portability-check self-test ==="
  echo "made-up disallowed host for this run: $bad_host"
  echo "(assembled at runtime — see the comment above self_test in $SELF)"
  echo

  echo "--- positive cases: each MUST be rejected by layer 3 ---"

  d="$st_root/a-image-key"; st_init_repo "$d"
  st_write "$d/service.yml" <<'EOF'
services:
  app:
    image: __BADHOST__/owner/mailroom:1.0
EOF
  st_commit "$d"
  st_expect_fail "(a) YAML image: key" "$d" "$bad_host"

  d="$st_root/b-image-equals"; st_init_repo "$d"
  st_write "$d/workflow.yml" <<'EOF'
on: push
jobs:
  meta:
    runs-on: ubuntu-latest
    steps:
      - run: |
          echo "image=__BADHOST__/owner/mailroom:1.0" >> "$GITHUB_OUTPUT"
EOF
  st_commit "$d"
  st_expect_fail "(b) echo image= >> \$GITHUB_OUTPUT" "$d" "$bad_host"

  d="$st_root/c-docker-login"; st_init_repo "$d"
  st_write "$d/workflow.yml" <<'EOF'
on: push
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - run: docker login __BADHOST__ -u x --password-stdin
EOF
  st_commit "$d"
  st_expect_fail "(c) docker login ARG" "$d" "$bad_host"

  d="$st_root/d-dockerfile-from"; st_init_repo "$d"
  st_write "$d/Dockerfile" <<'EOF'
FROM __BADHOST__/base:1
CMD ["true"]
EOF
  st_commit "$d"
  st_expect_fail "(d) Dockerfile FROM" "$d" "$bad_host"

  d="$st_root/e-var-default"; st_init_repo "$d"
  st_write "$d/compose.yml" <<'EOF'
services:
  clamav:
    image: ${MAILROOM_SOME_IMAGE:-__BADHOST__/x:1}
EOF
  st_commit "$d"
  st_expect_fail "(e) \${VAR:-default} default" "$d" "$bad_host"

  echo
  echo "--- negative cases: none of these may be flagged ---"

  d="$st_root/pass"; st_init_repo "$d"
  st_write "$d/compose.yml" <<'EOF'
services:
  postgres:
    image: postgres:18@sha256:1957b2ff3137e4ef7f3bc813e74fff50b1e1ffddc85c8b9d6f14ade972be8687
  clamav:
    image: clamav/clamav:1.5.4
  local:
    image: localhost:5000/x
  mailhost:
    image: mail.example.com/x:1
EOF
  st_write "$d/publish.yml" <<'EOF'
on: push
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - run: docker login "${{ steps.meta.outputs.host }}" -u x --password-stdin
      - run: |
          docker build \
            --build-arg V="${{ steps.meta.outputs.version }}" \
            -t "${{ steps.meta.outputs.image }}:${{ steps.meta.outputs.version }}" .
      - run: docker push "${{ steps.meta.outputs.image }}:${{ steps.meta.outputs.version }}"
      - run: docker build -t mailroom:ci .
EOF
  st_write "$d/Dockerfile" <<'EOF'
FROM golang:1.26 AS builder
FROM node:24-alpine AS web
EOF
  st_write "$d/mod.go" <<'EOF'
package pass

import "github.com/emersion/go-imap/v2"
EOF
  st_commit "$d"
  st_expect_pass "postgres:18@sha256 / clamav org / localhost / mail.example.com / \${{ }} refs / Go module path" "$d"

  echo
  if [ "$ST_FAILS" != "0" ]; then
    echo "PORTABILITY-CHECK SELF-TEST FAILED ($ST_FAILS case(s))"
    exit 1
  fi
  echo "portability-check self-test: all cases passed, both directions proven"
}

# --- argument parsing ----------------------------------------------------
require_patterns=0
SCAN_ROOT=""
RUN_SELF_TEST=0
PATTERNS_FILE=""
REV=""
DENYLIST_ONLY=0
MESSAGES=0
while [ $# -gt 0 ]; do
  case "$1" in
    --require-patterns) require_patterns=1; shift ;;
    --patterns-file)
      [ $# -ge 2 ] || { echo "usage: $0 [--require-patterns] [--patterns-file F] [--rev REV] [--denylist-only] [--messages] [--scan-root DIR] [--self-test]" >&2; exit 2; }
      PATTERNS_FILE="$2"; shift 2 ;;
    --rev)
      [ $# -ge 2 ] || { echo "usage: $0 [--require-patterns] [--patterns-file F] [--rev REV] [--denylist-only] [--messages] [--scan-root DIR] [--self-test]" >&2; exit 2; }
      REV="$2"; shift 2 ;;
    --denylist-only) DENYLIST_ONLY=1; shift ;;
    --messages) MESSAGES=1; shift ;;
    --scan-root)
      [ $# -ge 2 ] || { echo "usage: $0 [--require-patterns] [--patterns-file F] [--rev REV] [--denylist-only] [--messages] [--scan-root DIR] [--self-test]" >&2; exit 2; }
      SCAN_ROOT="$2"; shift 2 ;;
    --self-test) RUN_SELF_TEST=1; shift ;;
    *) echo "usage: $0 [--require-patterns] [--patterns-file F] [--rev REV] [--denylist-only] [--messages] [--scan-root DIR] [--self-test]" >&2; exit 2 ;;
  esac
done

if [ "$RUN_SELF_TEST" -eq 1 ]; then
  self_test
  exit 0
fi

# THE DENYLIST FROM A FILE (#948). The pre-push hook (scripts/hooks/pre-push)
# keeps the operator's patterns in a file OUTSIDE the repository, for the same
# reason CI keeps them in a secret. Missing or unreadable is a hard error, not
# a skip: a guard that silently checks nothing is the failure it exists for.
if [ -n "$PATTERNS_FILE" ]; then
  if [ ! -r "$PATTERNS_FILE" ]; then
    echo "FAIL: --patterns-file $PATTERNS_FILE is not readable." >&2
    exit 1
  fi
  MAILROOM_PORTABILITY_PATTERNS="$(cat "$PATTERNS_FILE")"
  require_patterns=1
fi

# COMMIT MESSAGES, IDENTITIES AND PR TITLES (#948). On a public forge a pushed
# commit message is published the moment it lands, exactly like a file, and
# the tree check never sees it. --messages reads that text on stdin (the hook
# feeds it every pushed commit's message plus author and committer name and
# email; CI feeds it the PR title and the PR's commit messages) and checks it
# against the denylist only. As with the tree, a match is reported WITHOUT
# the matched text or the pattern.
if [ "$MESSAGES" -eq 1 ]; then
  if [ -z "${MAILROOM_PORTABILITY_PATTERNS:-}" ]; then
    echo "FAIL: --messages needs the operator denylist (MAILROOM_PORTABILITY_PATTERNS or --patterns-file);"
    echo "      without it nothing would be checked."
    exit 1
  fi
  text="$(cat)"
  hit=0
  n=0
  while IFS= read -r pat; do
    case "$pat" in ''|\#*) continue ;; esac
    n=$((n + 1))
    if printf '%s\n' "$text" | grep -q -i -E -- "$pat"; then
      hit=1
    fi
  done <<PATS
$MAILROOM_PORTABILITY_PATTERNS
PATS
  if [ "$hit" -eq 1 ]; then
    echo "FAIL: an operator-specific pattern appears in a commit message, a commit"
    echo "      author/committer identity, or a PR title. (Pattern and text withheld.)"
    exit 1
  fi
  echo "ok: $n operator pattern(s) checked against the messages, none found"
  exit 0
fi

if [ -n "$SCAN_ROOT" ]; then
  DIR="$(CDPATH= cd -- "$SCAN_ROOT" && pwd)"
else
  DIR="$DEFAULT_DIR"
fi
cd "$DIR"

ALLOW="scripts/portability-hosts.allow"
fail=0

# Files to scan: every tracked file, minus generated lockfiles (npm rewrites
# hundreds of registry URLs there and no human authors them) and binaries,
# which `git grep -I` already skips.
EXCLUDES=":(exclude)web/package-lock.json"

echo "== Layer 1: operator denylist (out-of-band) =="
if [ -n "${MAILROOM_PORTABILITY_PATTERNS:-}" ]; then
  n=0
  # One extended regex per line. Blank lines and '#' comments are ignored so the
  # secret can be written readably.
  printf '%s\n' "$MAILROOM_PORTABILITY_PATTERNS" | while IFS= read -r pat; do
    case "$pat" in ''|\#*) continue ;; esac
    n=$((n + 1))
    # Held in a variable, not a temp file. CI cancels superseded runs (#346), a
    # cancelled step gets SIGTERM, and a shell with no trap dies without
    # cleaning up — the exact mechanism that leaked containers into the CI
    # runner. Nothing to leak if nothing is written.
    if [ -n "$REV" ]; then
      # A pushed commit, not the working tree: git grep prefixes REV: to each hit.
      pathits=$(git grep -I -n -i -E -- "$pat" "$REV" -- . "$EXCLUDES" 2>/dev/null | cut -d: -f2- || true)
    else
      pathits=$(git grep -I -n -i -E -- "$pat" -- . "$EXCLUDES" 2>/dev/null || true)
    fi
    if [ -n "$pathits" ]; then
      # ⚠ The matching LINES are never printed: they are, by construction, the
      # operator-identifying text this check exists to keep out of public
      # places, and a CI log is a public place. File and line only.
      echo "FAIL: an operator-specific pattern appears in the tree."
      echo "      (pattern and matched text withheld — see the files below)"
      printf '%s\n' "$pathits" | cut -d: -f1,2 | sort -u | sed 's/^/        /'
      exit 1
    fi
  done || fail=1
  # The `while` above runs in a subshell (it is the right-hand side of a pipe),
  # so its increments are lost — count outside it rather than reporting a zero
  # that would read as "nothing to check".
  n=$(printf '%s\n' "$MAILROOM_PORTABILITY_PATTERNS" | grep -cvE '^[[:space:]]*(#|$)' || true)
  if [ "$fail" -eq 0 ]; then
    echo "ok: $n operator pattern(s) checked, none found"
  fi
elif [ "$require_patterns" -eq 1 ]; then
  echo "FAIL: MAILROOM_PORTABILITY_PATTERNS is unset, and --require-patterns was"
  echo "      given. ZERO operator patterns were checked, which is not a pass:"
  echo "      the only violation this check exists because of was a bare"
  echo "      \`owner/repo#nnn\` reference, which layer 2 cannot see at all."
  echo
  echo "      Set the repository secret MAILROOM_PORTABILITY_PATTERNS to one"
  echo "      extended regex per line covering whatever identifies this"
  echo "      operator — the forge org and host, personal domains, the deploy"
  echo "      host's name, personal email domains. Its VALUE is deliberately not"
  echo "      in this repository: writing those strings down here would be the"
  echo "      leak the check is meant to prevent."
  fail=1
else
  echo "SKIPPED: MAILROOM_PORTABILITY_PATTERNS is unset — ZERO operator patterns"
  echo "         were checked. This is not a pass; it is a local run without the"
  echo "         denylist. CI passes --require-patterns and fails here."
fi

if [ "$DENYLIST_ONLY" -eq 1 ]; then
  # The hook's mode: layers 2 and 3 judge the working tree and stay CI's job.
  [ "$fail" -eq 0 ] && exit 0 || exit 1
fi

echo
echo "NOTE: layers 2 and 3 below can only judge URLs and the image-reference /"
echo "      registry-login sites listed at the top of this file. A scheme-less"
echo "      string anywhere ELSE in the tree — prose, a Go module path, an"
echo "      arbitrary identifier — is invisible to both. Only the operator"
echo "      denylist above covers that, and this run just told you whether it"
echo "      ran at all."

echo
echo "== Layer 2: URL host allowlist =="
[ -f "$ALLOW" ] || { echo "FAIL: $ALLOW is missing"; exit 1; }

# Strip comments and whitespace; lowercase.
allowed=$(sed -e 's/#.*//' -e 's/[[:space:]]//g' "$ALLOW" | grep -v '^$' | tr 'A-Z' 'a-z' | sort -u)

# Every host named by an http(s) URL in a tracked text file, with the file it
# came from, so a failure says where to look.
hits=$(git grep -I -h -o -i -E 'https?://[A-Za-z0-9._-]+' -- . "$EXCLUDES" \
       | sed -E 's#^[Hh][Tt][Tt][Pp][Ss]?://##' | tr 'A-Z' 'a-z' | sort -u)

bad=""
for host in $hits; do
  # A trailing dot means the URL continued into something the host character
  # class does not cover — a format verb (`https://openpgpkey.%s`), a shell or
  # template placeholder, an angle-bracketed stand-in. What was captured is a
  # PREFIX, not a host; drop the dot and judge what is left, which for those
  # cases is a single label and therefore not a domain at all.
  host=${host%.}
  if is_bad_host "$host"; then
    bad="$bad $host"
  fi
done

if [ -n "$bad" ]; then
  echo "FAIL: URL host(s) not in $ALLOW:"
  for host in $bad; do
    echo "        $host"
    git grep -I -n -i -F -- "$host" -- . "$EXCLUDES" | cut -d: -f1,2 | sort -u | sed 's/^/          /' | head -5
  done
  echo
  echo "  If the host is legitimately part of this project, add it to $ALLOW with"
  echo "  a comment saying why. If it names anything the operator self-hosts, it"
  echo "  must not be in this repository at all."
  fail=1
else
  echo "ok: every URL host is reserved-by-RFC, single-label, or allowlisted"
fi

echo
echo "== Layer 3: image reference / registry-login host allowlist (scheme-less) =="
echo "   Sites: YAML image: keys and image= output lines, Dockerfile FROM /"
echo "   COPY --from=, docker login|push|pull|tag|run|build (+ buildx --tag)"
echo "   arguments in YAML run: text and shell scripts, and scripts/*.env"
echo "   image defaults (#548)."

L3_PROG="$(l3_awk)"
# ⚠ THIS SCRIPT IS EXCLUDED FROM ITS OWN LAYER-3 SCAN. It is a `.sh` file that
# legitimately contains the strings "docker login"/"docker tag"/"docker build
# -t" — in the awk program's own source (emit("docker tag (src)", ...) matches
# "docker[ \t]+tag[ \t]+" against itself), in self-test labels ("docker login
# ARG"), and in the self-test's own PASS fixture text. None of that is ever a
# false FAIL (every match there is single-label, a dynamic ${{ }}/$VAR, or the
# no-dot placeholder __BADHOST__), but it is noise with no diagnostic value —
# this file's job is to name where OTHER files may go wrong, not to review its
# own prose. This is the "confirm the real check's scope excludes the
# self-test script" mitigation named in #548's fixture-safety note, applied to
# the whole file rather than only the self-test half of it.
l3_files=$(git ls-files -- '*.yml' '*.yaml' 'Dockerfile*' ':(glob)**/Dockerfile*' 'scripts/*.env' '*.sh' ':(exclude)scripts/portability-check.sh' 2>/dev/null | sort -u)

l3_records=""
if [ -n "$l3_files" ]; then
  for f in $l3_files; do
    recs=$(awk "$L3_PROG" "$f" 2>/dev/null) || true
    if [ -n "$recs" ]; then
      if [ -n "$l3_records" ]; then
        l3_records="$l3_records
$recs"
      else
        l3_records="$recs"
      fi
    fi
  done
fi

l3_examined=0
l3_bad=""
if [ -n "$l3_records" ]; then
  while IFS="$TAB" read -r rf rl rkind rval; do
    [ -n "$rf" ] || continue
    ref=$(trim_ref "$rval")
    [ -n "$ref" ] || continue
    l3_examined=$((l3_examined + 1))
    if [ "$rkind" = "docker login" ]; then
      host=$(login_arg_host "$ref")
    else
      host=$(image_ref_host "$ref")
    fi
    [ -n "$host" ] || continue
    if is_bad_host "$host"; then
      l3_bad="$l3_bad
        $rf:$rl [$rkind]: $host"
      fail=1
    fi
  done <<L3RECORDS
$l3_records
L3RECORDS
fi

echo "   $l3_examined image/registry reference(s) examined."
if [ -n "$l3_bad" ]; then
  echo "FAIL: image/registry reference host(s) not in $ALLOW:"
  printf '%s\n' "$l3_bad"
  echo
  echo "  Same remedy as layer 2: add the host to $ALLOW with a reason, or remove"
  echo "  it if it names anything the operator self-hosts."
else
  echo "ok: every image/registry reference host is reserved-by-RFC, single-label"
  echo "    (implicit docker.io), a dynamic value, or allowlisted"
fi

echo
[ "$fail" -eq 0 ] || { echo "portability-check FAILED"; exit 1; }
echo "portability-check passed"
