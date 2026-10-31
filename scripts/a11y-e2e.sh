#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# ACCESSIBILITY, PROVEN END TO END AGAINST THE REAL IMAGE (#447, spec §12).
# The automated half of #447; web/ACCESSIBILITY.md is the half it asserts.
#
# WHY IT EXISTS. web/ACCESSIBILITY.md records decisions that are invisible in a
# diff and easy to undo by accident: the desk's list is a `role="grid"` whose
# current row is named by `aria-activedescendant` (never a roving tabIndex,
# because a virtualised row can unmount while it holds focus), a row's name is an
# assembled SENTENCE with unread first, the line is a `role="slider"` whose value
# text states the split rather than the number, there is exactly ONE live region,
# a drawer's disclosure says how much is in it. Not one of those is checkable by a
# linter, and #447's own acceptance list says so: "a linter cannot observe what is
# announced; this can."
#
# WHAT THIS PROVES, end to end, against the thing we SHIP:
#   * the real image, serving the real built client, over real HTTP;
#   * a real Dovecot mailbox with thirteen messages, delivered over IMAP, whose
#     tab verdicts and area resolution are the SERVER's — so the desk under test
#     is a desk with two populated areas, a drawer with a count, an unread row, a
#     read row, a flagged row, an attachment and a two-message thread;
#   * axe-core over FIFTEEN surfaces (including a composer reached by a RELOAD,
#     mailroom#612, the day-one setup wizard,
#     `#/setup` — mailroom#447 follow-up — and the desk in the MULTI-LINE row
#     layout and at the LARGEST text size, mailroom#514) in THREE themes: zero
#     WCAG 2 A/AA violations, with the `passes` denominator printed on every run
#     so an empty result cannot read as a clean one;
#   * `color-contrast-enhanced` (AAA) measured on the same surfaces and gated
#     against scripts/a11y-contrast.allow;
#   * EIGHT STEPS of the desk's core loop, each asserting the computed role, name,
#     value and states of the active row / focused node plus the text of the live
#     region — the inputs to any announcement;
#   * the desk SCROLLED AWAY FROM ITS CURSOR ROW stays where it was put — by the
#     wheel, across a density change and under a sub-pixel row-height wobble —
#     while a cursor that MOVES is still followed (the v1.86.0 pull-back);
#   * the reading pane, after three messages opened one after another, closes
#     back to the list in ONE Escape or one press of its ← (the v1.86.0 walk
#     back through every message opened);
#   * at PHONE WIDTH (375 and 390 px, mobile, touch, 3x) every main view fits.
#     That includes the reading pane holding a real 640 px ESP newsletter,
#     filed in Archive below. The page does not overflow, nothing is off screen
#     or scrolls sideways, the action row's labels are whole, and reading
#     scrolls the document (#798). Chromium only: WebKit was measured by hand;
#   * THREE NEGATIVE CONTROLS in the same run: a glyph-only button losing its
#     name, the live region losing `role`/`aria-live`, and the grid losing
#     `aria-activedescendant`. Each must make its own check go RED, and the run
#     fails if any of them stays green.
#
# WHAT IT DOES NOT PROVE — the full list is in scripts/a11y-check.cjs's header, in
# words, and it is worth reading before quoting this suite as evidence. In short:
# NO SCREEN READER RUNS HERE. No runner in this project is Windows, so NVDA is not
# available; what is read is Chromium's own accessibility tree, which is what a
# screen reader on this platform consumes. Phrasing, verbosity and "announced
# exactly once" are the AT's business and are out of scope, stated rather than
# implied.
#
# ⚠ IT NEEDS web/node_modules/axe-core/axe.min.js AND FAILS WITHOUT IT. axe is a
# build-time devDependency, exact-pinned in web/package.json; nothing in web/src
# imports it. The CI job runs `setup-node` + `npm ci --prefix web` BEFORE this
# script (the same shape as the `web` job) and this script `docker cp`s the one
# file into the browser container. Locally:
#
#     docker run --rm -v "$PWD/web:/web" -w /web node:24-alpine npm ci
#     sh scripts/a11y-e2e.sh
#
# A MISSING axe IS A FAILURE, NEVER A SKIP (#263's rule, #159's reason): a lint
# lane that skips when its linter is absent goes green in exactly the situation it
# exists to catch.
#
# ⚠ AND UNLIKE ITS SIBLING SUITES, THIS ONE HAS NOTHING THAT SURVIVES THE
# BROWSER'S ABSENCE. hostile-content-e2e.sh still proves the sanitizer over HTTP
# with no browser at all; here the browser IS the instrument — even the fixture
# stage runs its JMAP calls with the node that ships in that image. So the image
# is checked FIRST, before anything is built, and `MAILROOM_SKIP_BROWSER=1` is
# kept only for symmetry with the siblings: in this lane it means "nothing was
# checked", it exits 0 having proven nothing, and CI never sets it.
#
# Everything is container-to-container: no published ports, no host toolchain
# beyond the one npm-installed file, no bind mounts of source. Same shape as
# scripts/hostile-content-e2e.sh and scripts/tabs-e2e.sh, so it runs identically
# on the CI runner, on Linux, and in Git Bash.
#
# EXIT CODES (one per failure kind, like hostile-content):
#     0  CLEAN
#     1  the run failed and said why on a named line
#     2  ERR …                   — the check script threw; fail closed
#     3  axe-core is not installed
#     4  the pinned Chromium image could not be obtained
#     5  NOFIXTURE …             — the mailbox did not produce the desk this lane
#                                  asserts against (an assertion made against an
#                                  empty desk is the #131 shape)
#     6  AXE <rule> on <surface>/<theme>
#     7  NONAME …                — a node's computed name is missing or wrong
#     8  NOLIVE …                — the one live region is not a live region
#     9  NOACTIVE …              — the grid names no current row, or the wrong one
#    10  CONTROLPASSED <which>   — a negative control did not go red
#    11  NOSTEP <step>           — an announcement step's own assertion failed
#    12  CONTRAST <selector>     — an AAA failure the allowlist does not name
#    13  NOTHEME <theme>         — the theme did not take
#    14  NOFIXTURE (browser side)
#    15  UNTOUCHED              — an untouched composer was saved, or a typed one
#                                 was not (mailroom#614)
#    16  SPLIT                  — with something docked, the list split is
#                                 mis-measured or cannot be dragged (mailroom#617)
#    17  SCROLLPULL             — scrolling the desk away from its cursor row was
#                                 undone without the cursor moving, or a moved
#                                 cursor was not followed
#    18  PANECLOSE              — after opening several messages, Escape or the
#                                 pane's close button did not return to the list
#    19  MOVEMENU               — the classic list's "Move to area" menu (name,
#                                 menu roles, Escape returning focus; #708)
#    20  MOVESENDER             — the sender card's "Move all from … to…" confirm
#                                 step (its buttons by role and name; #708)
#    21  SEARCHBOX              — the search box's suggestion combobox or its
#                                 options form (names, focus, fit at 360px; #724)
#    27  FOLDED                 — a folded message's button: its name lacks the
#                                 text it shows (WCAG 2.5.3, #759)
#    24  INVITE                 — the calendar invitation card: region/group
#                                 names, or an aria-pressed that is not the
#                                 server's recorded answer (#725)
#    28  PHONEWIDTH             — at 375/390 px a main view (including the wide
#                                 newsletter in the reading pane) is wider than
#                                 the phone, scrolls sideways, or reading does
#                                 not scroll the document; offenders printed (#798)
#    29  KEYS                   — Settings › Keys and its revoke dialog (#796):
#                                 control names, the dialog's focus trap, the
#                                 confirm button's announced unavailable state,
#                                 the desk's key-expiry banner (#795), or an
#                                 in-run negative control staying green
#    30  CONTACTS               — the contacts page (#816): the list's named
#                                 search and pressed filter toggles, a
#                                 contact's notes and VIP, the merge dialog's
#                                 focus trap, combobox and keyboard choice, or
#                                 an in-run negative control staying green
#    31  APIKEYS                — Settings › API keys (#49): the held send's
#                                 card not built from its own fields, the
#                                 approve/create/token dialogs' names, focus
#                                 and states, the banner's live region, or an
#                                 in-run negative control staying green
#    32  PHONEMENUS             — at 320/390 px, portrait or rotated, a pop-up
#                                 menu (Re-file with two rows selected, Move to
#                                 area, More, the sender card's Move all, the
#                                 search suggestions and options, recipient
#                                 suggestions) is off the screen, cut off,
#                                 not scrollable to its last item, or covered
#                                 (#880; axe cannot see this class of defect)
#
# Usage:  scripts/a11y-e2e.sh
#         MAILROOM_IMAGE=mailroom:test scripts/a11y-e2e.sh
#         A11Y_SHOT_DIR=/tmp/shots scripts/a11y-e2e.sh     # also save API-key screenshots
#         A11Y_CONTRAST_REPORT=1 scripts/a11y-e2e.sh    # measure AAA, gate nothing
#
# A11Y_CONTRAST_REPORT=1 is how scripts/a11y-contrast.allow is (re)written: it
# prints every `color-contrast-enhanced` failure grouped by class and colour pair
# with the worst ratio seen for each, which is the number an entry's floor has to
# sit at or below (CLAUDE.md: build the report before the fix). It marks its own
# final line REPORT-ONLY, so such a run can never be mistaken for a pass and this
# script still exits non-zero. CI never sets it.
set -eu
MSYS_NO_PATHCONV=1; export MSYS_NO_PATHCONV

# ⚠ `$$` IS NOT UNIQUE ACROSS CONCURRENT RUNS, AND CLEANUP REMOVES BY NAME — so a
# fixed (or PID-only) name is one run reaping another's network/containers
# mid-test. MEASURED on 2026-10-15: a post-merge run on main and a PR run overlapped
# on one runner host; the PR's run hit `network with name ... already exists`, its
# cleanup trap then removed the network main's run was using, and main's jobs died
# mid-run. CI's concurrency group cancels only within a REF, so overlap is by
# design. Every Docker object name (and the built image tag) carries $UNIQ so
# cleanup can only ever touch this run's own objects (#502; same idiom as
# scripts/integration-test.sh).
UNIQ="$$-$(od -An -N4 -tu4 < /dev/urandom 2>/dev/null | tr -dc '0-9' || date +%s)"
[ -n "${UNIQ#*-}" ] || UNIQ="$$-$(date +%s)"

IMAGE="${MAILROOM_IMAGE:-mailroom:a11y-e2e-$UNIQ}"
PG_IMAGE="${PG_IMAGE:-postgres:18}"
DOVECOT_IMAGE="${DOVECOT_IMAGE:-dovecot/dovecot:2.3.21}"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.10.1}"
# THE HEADLESS BROWSER IS PINNED BY DIGEST (#254) IN ONE SHARED FILE (#447) —
# scripts/e2e-chrome-image.env, which carries the whole argument. #447 adds a
# POSITIVE reason to the two older suites' negative one: a computed accessible
# name, a computed contrast ratio and an accessibility tree are the BROWSER's
# answers, not ours, so a browser bump can change every number this lane prints
# without a line of Mailroom changing.
. "$(dirname "$0")/e2e-chrome-image.env"

NET="mailroom-a11y-e2e-$UNIQ"
VOL="mailroom-a11y-e2e-key-$UNIQ"
PG="mailroom-a11y-e2e-pg-$UNIQ"
DV="mailroom-a11y-e2e-dovecot-$UNIQ"
APP="mailroom-a11y-e2e-app-$UNIQ"
# The crypto process (#796): Settings → Keys has real verbs only when one is
# wired, so this lane runs one, with its own root secret and socket volume.
CR="mailroom-a11y-e2e-crypto-$UNIQ"
PGPVOL="mailroom-a11y-e2e-pgproot-$UNIQ"
SOCKVOL="mailroom-a11y-e2e-sock-$UNIQ"

TESTUSER="reader@example.com"
TESTPASS="reader-mailbox-password"

cd "$(dirname "$0")/.."
# The HOST-side path of the checkout, in the form the docker CLI needs. `pwd -W`
# gives Git Bash the Windows form; MSYS_NO_PATHCONV (set above, and required so
# container-side paths are not rewritten) means a bare MSYS path would reach
# docker unconverted.
REPO_HOST="$(pwd -W 2>/dev/null || pwd)"
WORK="$(pwd)/.a11y-e2e-tmp.$UNIQ"
mkdir -p "$WORK"
WORK_HOST="$(cd "$WORK" && { pwd -W 2>/dev/null || pwd; })"

cleanup() {
  echo "--- cleaning up"
  docker rm -fv "$APP" "$CR" "$DV" "$PG" >/dev/null 2>&1 || true
  docker volume rm -f "$VOL" "$PGPVOL" "$SOCKVOL" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  # Only an image THIS run built (uniquely tagged); a MAILROOM_IMAGE override is the caller's.
  if [ -z "${MAILROOM_IMAGE:-}" ]; then docker rmi "$IMAGE" >/dev/null 2>&1 || true; fi
  rm -rf "$WORK"
}
# ⚠ INT and TERM are NOT decoration: a CI cancellation sends SIGTERM, and a shell
# with only an EXIT trap dies WITHOUT running it. Measured in an alpine container
# — `trap cleanup EXIT` left the marker unwritten on SIGTERM, while
# `trap cleanup EXIT INT TERM` wrote it. That is how cancelled runs leaked
# long-lived side containers into the runners (mailroom#346); three were found
# aged 7h, 8h and 11h. cleanup is idempotent, so the EXIT trap firing again after
# the TERM trap is harmless.
trap cleanup EXIT INT TERM
cleanup >/dev/null 2>&1 || true
mkdir -p "$WORK"

# wait_for retries a readiness command until it succeeds. If it never does, it
# FAILS THE SCRIPT LOUDLY and dumps the container's log and final state — the
# v1.0.0 lesson: diagnose at the point of failure, not three commands downstream,
# where it surfaces as something unrelated.
#   $1 label, $2 attempts (~seconds), $3 container to dump ("" none), $4.. command
wait_for() {
  _label="$1"; _attempts="$2"; _dump="$3"; shift 3
  _i=0
  while [ "$_i" -lt "$_attempts" ]; do
    if "$@" >/dev/null 2>&1; then return 0; fi
    _i=$((_i + 1))
    sleep 1
  done
  echo "FATAL: $_label did not become ready after ${_attempts}s" >&2
  if [ -n "$_dump" ]; then
    echo "--- docker inspect $_dump ---" >&2
    docker inspect -f 'status={{.State.Status}} exit={{.State.ExitCode}} oomkilled={{.State.OOMKilled}} error={{.State.Error}}' \
      "$_dump" >&2 2>&1 || true
    echo "--- docker logs $_dump (last 50) ---" >&2
    docker logs --tail 50 "$_dump" >&2 2>&1 || true
  fi
  exit 1
}

# ---------------------------------------------------------------------------
# 0. The two instruments, checked BEFORE anything is built.
# ---------------------------------------------------------------------------
#
# Both are hard dependencies of every assertion this lane makes, and a lane whose
# instrument is missing must say so in five seconds rather than after a three
# minute image build and a stack.
AXE_JS="web/node_modules/axe-core/axe.min.js"
if [ ! -s "$AXE_JS" ]; then
  echo "FAIL  $AXE_JS is missing, so the accessibility LINT cannot run at all." >&2
  echo "      axe-core is an exact-pinned devDependency of web/package.json; install it:" >&2
  echo "        docker run --rm -v \"\$PWD/web:/web\" -w /web node:24-alpine npm ci" >&2
  echo "      (CI does this with setup-node + \`npm ci --prefix web\` before this script.)" >&2
  echo "      This is a FAILURE and not a skip: a lint lane that skips when its linter" >&2
  echo "      is absent goes green in exactly the situation it exists to catch (#263)." >&2
  exit 3
fi
AXE_VERSION="$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' web/node_modules/axe-core/package.json | head -1)"
echo "--- axe-core $AXE_VERSION ($AXE_JS)"

if ! docker image inspect "$CHROME_IMAGE" >/dev/null 2>&1 \
   && ! docker pull -q "$CHROME_IMAGE" >/dev/null 2>&1; then
  if [ "${MAILROOM_SKIP_BROWSER:-0}" = "1" ]; then
    echo "skip  $CHROME_IMAGE unobtainable, MAILROOM_SKIP_BROWSER=1."
    echo "skip  ⚠ NOTHING WAS CHECKED. Every assertion in this lane is a browser"
    echo "skip    measurement — the accessibility tree, the computed names, the"
    echo "skip    contrast ratios — and even the fixture's JMAP calls run in that"
    echo "skip    image. A green exit here is not evidence of anything. CI never"
    echo "skip    sets this variable."
    exit 0
  fi
  echo "FAIL  $CHROME_IMAGE could not be obtained, so no accessibility assertion" >&2
  echo "      in this lane can run — the browser IS the instrument here, unlike in" >&2
  echo "      hostile-content-e2e.sh, where the sanitizer stage still runs without one." >&2
  echo "      Set MAILROOM_SKIP_BROWSER=1 to skip this deliberately (and prove nothing)." >&2
  exit 4
fi

if [ -z "${MAILROOM_IMAGE:-}" ]; then
  echo "--- building $IMAGE"
  docker build -t "$IMAGE" .
fi

echo "--- network + master key"
docker network create "$NET" >/dev/null
docker volume create "$VOL" >/dev/null
docker run --rm -v "$VOL:/k" "$PG_IMAGE" sh -c \
  'openssl rand -hex 32 | tr -d "\r\n" > /k/master && chmod 644 /k/master' >/dev/null

echo "--- postgres"
docker run -d --name "$PG" --network "$NET" --network-alias pg \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=mailroom "$PG_IMAGE" >/dev/null
# -h 127.0.0.1 forces TCP: the postgres image's first-init temp server listens on
# the Unix socket ONLY, so a socket-based check reports "ready" before
# POSTGRES_DB=mailroom exists. TCP comes up only with the real post-init server.
wait_for postgres 60 "$PG" \
  docker exec "$PG" pg_isready -h 127.0.0.1 -U postgres -d mailroom -q

echo "--- roles + migrations"
docker exec "$PG" psql -U postgres -d mailroom -v ON_ERROR_STOP=1 \
  -c "CREATE ROLE mailroom_owner LOGIN PASSWORD 'owner' CREATEROLE;" \
  -c "GRANT ALL ON SCHEMA public TO mailroom_owner;" \
  -c "GRANT ALL ON DATABASE mailroom TO mailroom_owner;" >/dev/null
docker run --rm --network "$NET" --entrypoint /mailroom-migrate \
  -e MAILROOM_MIGRATE_DATABASE_URL="postgres://mailroom_owner:owner@pg:5432/mailroom?sslmode=disable" \
  "$IMAGE" up >/dev/null
docker exec "$PG" psql -U postgres -d mailroom -v ON_ERROR_STOP=1 \
  -c "ALTER ROLE mailroom_app LOGIN PASSWORD 'app';" >/dev/null

echo "--- the crypto process (own-key management, #796)"
# ITS OWN ROOT SECRET IN ITS OWN VOLUME, as in deploy/compose.yml: the app
# container never mounts it. The socket is a named volume both mount; a fresh
# named volume inherits the image's /run/mailroom-crypto (owned by the shared
# `mailroom` user, mode 700), which is what lets the two talk. Without this
# process the capability says keyManagement:false and the Keys page has no
# Make / Revoke / Back up to lint — a surface that cannot appear is not tested.
docker volume create "$PGPVOL" >/dev/null
docker volume create "$SOCKVOL" >/dev/null
docker run --rm -v "$PGPVOL:/p" "$PG_IMAGE" sh -c \
  'openssl rand -hex 32 | tr -d "\r\n" > /p/root && chmod 644 /p/root' >/dev/null
docker run -d --name "$CR" --network "$NET" \
  -v "$PGPVOL:/p:ro" -v "$SOCKVOL:/run/mailroom-crypto" \
  -e MAILROOM_PGP_ROOT_KEY_FILE=/p/root \
  -e MAILROOM_PGP_SOCKET=/run/mailroom-crypto/crypto.sock \
  -e MAILROOM_DATABASE_URL="postgres://mailroom_app:app@pg:5432/mailroom?sslmode=disable" \
  -e MAILROOM_LOG_FORMAT=text \
  --entrypoint /mailroom-crypto "$IMAGE" >/dev/null
wait_for "mailroom-crypto" 60 "$CR" docker exec "$CR" /mailroom-crypto -healthcheck

echo "--- dovecot (implicit TLS, self-signed)"
# A self-signed cert for the alias "dovecot", generated INSIDE a throwaway
# container and copied OUT with `docker cp` — a bind mount of a host path does not
# resolve when the CI job is itself a container with a socket-mounted Docker (the
# -v source is read by the host daemon, so $WORK mounts an empty ghost dir and the
# certs never reach the job filesystem). Same reasoning, verbatim, as
# scripts/hostile-content-e2e.sh.
CERTGEN=$(docker create "$DOVECOT_IMAGE" sh -c '
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
    -keyout /tmp/dovecot.key -out /tmp/dovecot.crt \
    -subj "/CN=dovecot" -addext "subjectAltName=DNS:dovecot" >/dev/null 2>&1')
docker start -a "$CERTGEN" >/dev/null 2>&1 || true
docker cp "$CERTGEN:/tmp/dovecot.crt" "$WORK_HOST/dovecot.crt"
docker cp "$CERTGEN:/tmp/dovecot.key" "$WORK_HOST/dovecot.key"
docker rm -fv "$CERTGEN" >/dev/null
# A cheap absent/empty guard, NOT the proof: what establishes the trust material
# is the handshake check below, which compares the bytes dovecot serves against
# this exact file.
{ [ -s "$WORK/dovecot.crt" ] && [ -s "$WORK/dovecot.key" ] &&
  grep -q "BEGIN CERTIFICATE" "$WORK/dovecot.crt"; } || {
  echo "FATAL: certgen produced no usable certificate" >&2; exit 1; }

# ⚠ THE SPECIAL-USE FOLDERS BELOW ARE NOT DECORATION, AND THE FIRST RUN WITHOUT
# THEM PROVED IT. Dovecot's default mailbox set is INBOX and nothing else, and on
# that mailbox the desk's ARCHIVE key does nothing at all: core/actions.ts
# archive() looks for a mailbox whose role is "archive", finds none, surfaces
# "This account has no Archive folder, so there is nothing to archive into" and
# returns false — so no row moves, no undo is registered and no toast is raised.
# The archive step of the announcement script sat for twenty seconds waiting for a
# sentence the client was never going to write, while the answer was on screen in
# an error toast the assertion was not reading. (Both halves are fixed: the
# folders are here, and a11y-check.cjs now prints the toast it DID get.)
#
# RFC 6154 SPECIAL-USE is how mailroom learns a folder's role (internal/sync
# MailboxAttrArchive and friends), so declaring them is what makes this fixture a
# mailbox the client's verbs can act on rather than one that merely holds mail.
#
# NOTE the comment lives HERE and not inside the heredoc: this is an UNQUOTED
# heredoc (it interpolates $TESTPASS), so a `backtick` in prose inside it becomes
# a command substitution — which is exactly how this block first failed, with
# `e: command not found` from the word `e` in a sentence.
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

ssl = required
ssl_cert = </etc/dovecot/dovecot.crt
ssl_key = </etc/dovecot/dovecot.key
disable_plaintext_auth = yes

namespace inbox {
  inbox = yes
  separator = /

  mailbox Archive {
    auto = create
    special_use = \Archive
  }
  mailbox Trash {
    auto = create
    special_use = \Trash
  }
  mailbox Sent {
    auto = create
    special_use = \Sent
  }
  mailbox Drafts {
    auto = create
    special_use = \Drafts
  }
  mailbox Junk {
    auto = create
    special_use = \Junk
  }
}

listen = *

log_path=/dev/stdout
info_log_path=/dev/stdout
debug_log_path=/dev/stdout
EOF

DVID=$(docker create --name "$DV" --network "$NET" --network-alias dovecot "$DOVECOT_IMAGE")
docker cp "$WORK_HOST/dovecot.conf" "$DVID:/etc/dovecot/dovecot.conf"
docker cp "$WORK_HOST/dovecot.crt" "$DVID:/etc/dovecot/dovecot.crt"
docker cp "$WORK_HOST/dovecot.key" "$DVID:/etc/dovecot/dovecot.key"
docker start "$DVID" >/dev/null
# doveadm is a precondition for `doveadm save` and NOTHING MORE: it reads the
# config and the mail store directly, so it answers before the master process has
# finished starting and it never opens a socket.
wait_for "dovecot (doveadm, local)" 60 "$DV" \
  docker exec "$DV" doveadm mailbox list -u "$TESTUSER"

# ⚠ THIS is the readiness that matters. The app's job here is to dial 993, verify
# the certificate against the CA bundle we hand it, and sync; `doveadm mailbox
# list` proves none of that. So this is a real handshake from a SEPARATE container
# on this network — the app's exact path — comparing the served certificate
# byte-for-byte against $WORK/dovecot.crt rather than merely observing that a
# handshake happened. A port probe would go green against a server holding any
# certificate at all, and a mismatch there surfaces ten steps later, inside the
# app, dressed up as a rendering result (#432).
dovecot_serves_our_cert() {
  docker run --rm -i --network "$NET" --entrypoint sh "$DOVECOT_IMAGE" -c '
    cat > /tmp/ours.crt
    ours=$(openssl x509 -noout -fingerprint -sha256 -in /tmp/ours.crt 2>/dev/null) || exit 1
    served=$(openssl s_client -connect dovecot:993 </dev/null 2>/dev/null |
             openssl x509 -noout -fingerprint -sha256 2>/dev/null) || exit 1
    [ -n "$ours" ] && [ "$ours" = "$served" ]
  ' < "$WORK/dovecot.crt"
}
wait_for "dovecot TLS on 993 serving OUR certificate" 60 "$DV" dovecot_serves_our_cert

# ---------------------------------------------------------------------------
# The fixture mailbox
# ---------------------------------------------------------------------------
#
# THIRTEEN MESSAGES, CHOSEN SO THE SERVER PUTS THEM WHERE THIS LANE NEEDS THEM.
# The desk is a partition into AREAS fed by category tabs (internal/areas), and
# the tab is decided by internal/tabs from header facts alone — so the fixture has
# to be written against that rule list rather than against a hope:
#
#   * PROMOTIONS needs the promotional LEXICON (internal/kind promotionalSubject:
#     "40% off", "sale", "deal", "save", "clearance", "last chance", "ends
#     today/tonight", "exclusive"). List-Unsubscribe alone is NOT enough and the
#     comment in classify.go says why at length: RFC 8058 one-click means
#     REGULATED BULK SENDER and nothing more, so `bulk_apparatus` files it under
#     Updates. Reading "is bulk" as "is marketing" is the exact mistake that once
#     had promotional leading most of a real mailbox on apparatus alone.
#   * These subjects therefore carry no TRANSACTIONAL word either
#     (order/receipt/invoice/shipping/confirm/statement/…), because
#     `transactional_subject` sits ABOVE the promotional rule: "free shipping" is
#     filed as a dispatch notice, which is correct and not what we want here.
#   * PRIMARY needs NO bulk apparatus at all — no List-Unsubscribe, no List-Id, no
#     Precedence, no Feedback-ID, no recognised ESP — which is the POSITIVE
#     Primary verdict (`no_bulk_markers`), not a fallback.
#   * UPDATES is the transactional lexicon, and it exists here so a third area is
#     populated: the ≥2-areas requirement then does not rest on one rule firing.
#   * NOTHING carries List-Id, which would be Forums by a protocol fact and would
#     take the message out of whichever area we meant.
#
# All of it is VERIFIED over Area/getSummary before the browser starts, and the
# lane FAILS if the verdicts did not land — see the fixture stage below.
mkdir -p "$WORK/mail"

mkmail() {
  # $1 file, $2 From, $3 Subject, $4 extra headers (may be empty), $5 body
  {
    printf 'From: %s\n' "$2"
    printf 'To: Reader <%s>\n' "$TESTUSER"
    printf 'Subject: %s\n' "$3"
    printf 'Message-ID: <%s@example.test>\n' "$1"
    printf 'Date: Tue, 08 Sep 2026 09:00:00 +0000\n'
    printf 'MIME-Version: 1.0\n'
    if [ -n "$4" ]; then printf '%s\n' "$4"; fi
    printf 'Content-Type: text/plain; charset=utf-8\n'
    printf '\n'
    printf '%s\n' "$5"
  } > "$WORK/mail/$1.eml"
}

UNSUB='List-Unsubscribe: <mailto:stop@lists.example.com>
List-Unsubscribe-Post: List-Unsubscribe=One-Click'

# --- Promotions (five) -----------------------------------------------------
mkmail d1 'Shop Deals <deals@shop.example.com>'   '40% off everything this weekend'      "$UNSUB" 'Forty per cent, everything, until Sunday.'
mkmail d2 'Shop Deals <deals@shop.example.com>'   'Our summer sale starts now'           "$UNSUB" 'The summer sale is live.'
mkmail d3 'Market News <news@market.example.com>' 'Last chance: clearance on garden tools' "$UNSUB" 'Everything must go.'
mkmail d4 'Market News <news@market.example.com>' 'Exclusive member deals inside'        "$UNSUB" 'Members only.'
mkmail d5 'Shop Deals <deals@shop.example.com>'   'Save on winter coats, ends tonight'   "$UNSUB" 'Ends at midnight.'

# --- Primary (five, one of them a two-message thread) ----------------------
mkmail p1 'Ada Lovelace <ada@example.com>'   'Lunch on Thursday?' '' 'Are you free at one?'
mkmail p2 'Ada Lovelace <ada@example.com>'   'Re: Lunch on Thursday?' \
  'In-Reply-To: <p1@example.test>
References: <p1@example.test>' 'Actually, half past one is easier.'
mkmail p3 'Grace Hopper <grace@example.com>' 'The garden plan' '' 'I have drawn the beds out.'
mkmail p5 'Ada Lovelace <ada@example.com>'   'Holiday photographs' '' 'The ones from the coast.'

# --- Updates (three) -------------------------------------------------------
mkmail u1 'Utility Billing <billing@utility.example.com>' 'Your invoice is ready'      "$UNSUB" 'Available in your account.'
mkmail u2 'Utility Billing <billing@utility.example.com>' 'Your monthly statement'     "$UNSUB" 'For the period ending last week.'
mkmail u3 'Forum Digest <digest@forum.example.com>'       'Weekly digest of new questions' "$UNSUB" 'Twelve new questions this week.'

# --- Primary, with an attachment (the reading pane, the viewer) ------------
# text/plain is the cheapest thing the attachment VIEWER can actually render
# (ui/attachmentPreviewPolicy.ts PREVIEW_TEXT_TYPES), which is what makes the
# viewer surface reachable at all. A PDF would need bytes; an image would need a
# codec; this needs neither and still exercises the same dialog, focus trap and
# top bar.
cat > "$WORK/mail/p4.eml" <<EOF
From: Alan Turing <alan@example.com>
To: Reader <$TESTUSER>
Subject: Notes from the meeting
Message-ID: <p4@example.test>
Date: Tue, 08 Sep 2026 09:00:00 +0000
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="a11yb"

--a11yb
Content-Type: text/plain; charset=utf-8

The notes are attached.

--a11yb
Content-Type: text/plain; charset=utf-8; name="notes.txt"
Content-Disposition: attachment; filename="notes.txt"

Three things we agreed:
one, two, three.

--a11yb--
EOF

docker cp "$WORK_HOST/mail" "$DV:/tmp/mail" >/dev/null
docker exec "$DV" sh -c "for f in /tmp/mail/*.eml; do doveadm save -u '$TESTUSER' -m INBOX < \"\$f\" || exit 1; done"
DELIVERED=$(ls "$WORK/mail" | wc -l | tr -d ' \r\n')
echo "delivered $DELIVERED messages into $TESTUSER INBOX"

# --- A HELD INLINE IMAGE (#675), filed in ARCHIVE --------------------------
# The withheld chip as a held INLINE image produces it: an HTML body
# referencing a cid: image that the quarantine holds (flagged below, after the
# fixture stage, because the verdict is written to the row sync creates).
# ARCHIVE, NOT INBOX, ON PURPOSE: the desk counts INBOX (deskLiveSQL), and every
# per-area count and row this lane asserts was computed for the thirteen
# messages above. A message off the desk changes none of them; it is opened
# directly by id. Its own directory, so DELIVERED stays the desk's count.
mkdir -p "$WORK/held"
cat > "$WORK/held/held.eml" <<EOF
From: Ada Lovelace <ada@example.com>
To: Reader <$TESTUSER>
Subject: Held inline image
Message-ID: <held-inline@example.test>
Date: Mon, 07 Sep 2026 09:00:00 +0000
MIME-Version: 1.0
Content-Type: multipart/related; boundary="a11yh"

--a11yh
Content-Type: text/html; charset=utf-8

<p>The banner is below.</p><img src="cid:held@example.test" alt="banner">
--a11yh
Content-Type: image/png; name="banner.png"
Content-ID: <held@example.test>
Content-Transfer-Encoding: base64

iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4IScHAAK2AQU0pnWqAAAAAElFTkSuQmCC
--a11yh--
EOF
docker cp "$WORK_HOST/held/held.eml" "$DV:/tmp/held.eml" >/dev/null
docker exec "$DV" sh -c "doveadm save -u '$TESTUSER' -m Archive < /tmp/held.eml"
echo "delivered the held-inline message into $TESTUSER Archive"

# --- A FOLLOW-UP THAT IS DUE (#58), filed in SENT --------------------------
# The user's own message to a person, never answered. "No one answered your
# mail" is due seven days after it was sent, and a fixture cannot wait a week,
# so the row is BACKDATED after sync (below, after the fixture stage). SENT,
# NOT INBOX, for the held message's reason: the desk counts INBOX, so nothing
# this lane asserts about the desk's rows moves.
mkdir -p "$WORK/sent"
cat > "$WORK/sent/followup.eml" <<EOF
From: Reader <$TESTUSER>
To: Grace Hopper <grace@example.com>
Subject: Seed order for the spring beds
Message-ID: <followup-sent@example.test>
Date: Tue, 01 Sep 2026 09:00:00 +0000
MIME-Version: 1.0
Content-Type: text/plain; charset=utf-8

Shall I order the seeds this week?
EOF
docker cp "$WORK_HOST/sent/followup.eml" "$DV:/tmp/followup.eml" >/dev/null
docker exec "$DV" sh -c "doveadm save -u '$TESTUSER' -m Sent < /tmp/followup.eml"
echo "delivered the unanswered follow-up message into $TESTUSER Sent"

# --- A CALENDAR INVITATION (#725), filed in ARCHIVE --------------------------
# The invitation card and its Accept / Maybe / Decline group. Archive for the
# held message's reason: off the desk, so no count above changes. Addressed to
# the login address, which the boot reconcile makes a verified identity, so the
# card offers its buttons. The organiser is a reserved name nothing delivers
# to; the step that presses Accept undoes it inside the Undo Send window, so
# nothing is ever submitted to SMTP.
mkdir -p "$WORK/invite"
cat > "$WORK/invite/invite.eml" <<EOF
From: Organiser <organiser@example.org>
To: Reader <$TESTUSER>
Subject: Invitation: Accessibility review
Message-ID: <a11y-invite@example.test>
Date: Mon, 07 Sep 2026 10:00:00 +0000
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="a11yi"

--a11yi
Content-Type: text/plain; charset=utf-8

You are invited to the accessibility review.
--a11yi
Content-Type: text/calendar; charset=utf-8; method=REQUEST

BEGIN:VCALENDAR
PRODID:-//Example//Fixture//EN
VERSION:2.0
METHOD:REQUEST
BEGIN:VEVENT
UID:a11y-invite-1@example.test
SEQUENCE:0
DTSTAMP:20260907T100000Z
DTSTART:20301015T150000Z
DTEND:20301015T160000Z
SUMMARY:Accessibility review
LOCATION:Room 3
ORGANIZER;CN=Organiser:mailto:organiser@example.org
ATTENDEE;PARTSTAT=NEEDS-ACTION;CN=Reader:mailto:$TESTUSER
END:VEVENT
END:VCALENDAR
--a11yi--
EOF
docker cp "$WORK_HOST/invite/invite.eml" "$DV:/tmp/invite.eml" >/dev/null
docker exec "$DV" sh -c "doveadm save -u '$TESTUSER' -m Archive < /tmp/invite.eml"
echo "delivered the calendar invitation into $TESTUSER Archive"

# --- A THREAD WITH A FOLDED MESSAGE (#759), filed in ARCHIVE -----------------
# The pane folds every READ message that is not the one opened, so the fixture
# below marks the first of these two read and the check opens the second. It
# exists so the folded button is linted ON PURPOSE: until #759 a folded message
# reached the lane only by accident, behind another step's overlay.
# Archive for the held message's reason: off the desk, so no count changes.
mkdir -p "$WORK/fold"
cat > "$WORK/fold/f1.eml" <<EOF
From: Grace Hopper <grace@example.com>
To: Reader <$TESTUSER>
Subject: Compiler notes
Message-ID: <a11y-fold-1@example.test>
Date: Mon, 07 Sep 2026 11:00:00 +0000
MIME-Version: 1.0
Content-Type: text/plain; charset=utf-8

The first draft of the notes is ready for review.
EOF
cat > "$WORK/fold/f2.eml" <<EOF
From: Grace Hopper <grace@example.com>
To: Reader <$TESTUSER>
Subject: Re: Compiler notes
Message-ID: <a11y-fold-2@example.test>
In-Reply-To: <a11y-fold-1@example.test>
References: <a11y-fold-1@example.test>
Date: Mon, 07 Sep 2026 12:00:00 +0000
MIME-Version: 1.0
Content-Type: text/plain; charset=utf-8

And the second section is done too.
EOF
docker cp "$WORK_HOST/fold" "$DV:/tmp/fold" >/dev/null
docker exec "$DV" sh -c "doveadm save -u '$TESTUSER' -m Archive < /tmp/fold/f1.eml && doveadm save -u '$TESTUSER' -m Archive < /tmp/fold/f2.eml"
echo "delivered the two-message folded thread into $TESTUSER Archive"

# --- A WIDE MARKETING NEWSLETTER (#798), filed in ARCHIVE ---------------------
# The phone-width step's subject. Real bulk mail is not a paragraph: an ESP
# template is a centred FIXED-WIDTH table (600-700 px; this one is 640) inside a
# 100%-wide background table, with a "view online" row, a logo, a 2x hero image
# sized down by attribute, a row of two fixed-width cells, a bulletproof button
# table, a footer with a postal address and a long tracking URL, and a 1x1 open
# pixel. That structure is what a narrow reader has to contain; a hand-written
# <p> would encode the belief under test (CLAUDE.md, fixture realism). The hero
# is a real 1280x640 PNG carried inline (cid:) so an image actually RENDERS at
# its attribute width (remote images are blocked here); the logo and the pixel
# stay remote, as they are in the wild. Archive for the held message's reason:
# off the desk, so no count above changes.
mkdir -p "$WORK/newsletter"
{
cat <<EOF
From: The Grand Theatre <updates@news.theatre.example.com>
To: Reader <$TESTUSER>
Subject: Tickets on sale now for the autumn season at the Grand
Message-ID: <a11y-newsletter@news.theatre.example.com>
Date: Mon, 07 Sep 2026 13:00:00 +0000
MIME-Version: 1.0
List-Unsubscribe: <https://news.theatre.example.com/u/?id=8f3a2c91d7&list=autumn-season-2026>, <mailto:unsubscribe@news.theatre.example.com?subject=unsubscribe>
List-Unsubscribe-Post: List-Unsubscribe=One-Click
Content-Type: multipart/alternative; boundary="a11ynla"

--a11ynla
Content-Type: text/plain; charset=utf-8

The autumn season is on sale now. View online:
https://news.theatre.example.com/view/?id=8f3a2c91d7&utm_source=newsletter&utm_medium=email&utm_campaign=autumn-season-2026

--a11ynla
Content-Type: multipart/related; boundary="a11ynlr"

--a11ynlr
Content-Type: text/html; charset=utf-8

<!DOCTYPE html>
<html lang="en"><head>
<meta http-equiv="Content-Type" content="text/html; charset=UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>The autumn season</title>
<style type="text/css">
body{margin:0;padding:0;-webkit-text-size-adjust:100%}
table,td{border-collapse:collapse;mso-table-lspace:0pt;mso-table-rspace:0pt}
img{border:0;outline:none;text-decoration:none;-ms-interpolation-mode:bicubic}
.btn a{color:#ffffff;text-decoration:none}
</style>
</head>
<body style="margin:0;padding:0;background-color:#e6e6e6;">
<div style="display:none;font-size:1px;line-height:1px;max-height:0;max-width:0;opacity:0;overflow:hidden;">Six productions, one season ticket, and the best seats go first.</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" bgcolor="#e6e6e6" style="background-color:#e6e6e6;">
<tr><td align="center" valign="top">
<table role="presentation" width="640" cellpadding="0" cellspacing="0" border="0" style="width:640px;">
<tr><td align="center" style="padding:10px 20px;font-family:Arial,Helvetica,sans-serif;font-size:12px;color:#555555;">If you are having trouble reading this email, <a href="https://news.theatre.example.com/view/?id=8f3a2c91d7" style="color:#555555;">read the online version</a>.</td></tr>
<tr><td align="center" bgcolor="#ffffff" style="padding:20px 0;"><img src="https://news.theatre.example.com/img/logo-640.png" width="300" height="80" alt="The Grand Theatre" style="display:block;width:300px;height:80px;"></td></tr>
<tr><td align="center"><img src="cid:hero-autumn@news.theatre.example.com" width="640" height="320" alt="The autumn season" style="display:block;width:640px;height:320px;"></td></tr>
<tr><td bgcolor="#ffffff" style="padding:30px 40px 10px;font-family:Georgia,serif;font-size:26px;line-height:32px;color:#222222;">Six productions. One season ticket.</td></tr>
<tr><td bgcolor="#ffffff" style="padding:0 40px 20px;font-family:Arial,Helvetica,sans-serif;font-size:16px;line-height:24px;color:#333333;">Season ticket holders choose their seats before single tickets go on sale, and keep them for every production.</td></tr>
<tr><td bgcolor="#ffffff">
<table role="presentation" width="640" cellpadding="0" cellspacing="0" border="0" style="width:640px;"><tr>
<td width="320" valign="top" style="width:320px;padding:0 20px 20px 40px;font-family:Arial,Helvetica,sans-serif;font-size:14px;line-height:20px;color:#333333;"><b>October</b><br>A new production of a classic comedy, eight weeks only.</td>
<td width="320" valign="top" style="width:320px;padding:0 40px 20px 20px;font-family:Arial,Helvetica,sans-serif;font-size:14px;line-height:20px;color:#333333;"><b>November</b><br>The touring musical returns for a strictly limited run.</td>
</tr></table>
</td></tr>
<tr><td align="center" bgcolor="#ffffff" style="padding:10px 40px 40px;">
<table role="presentation" cellpadding="0" cellspacing="0" border="0" class="btn"><tr><td align="center" bgcolor="#b0122b" width="260" height="48" style="width:260px;height:48px;border-radius:4px;font-family:Arial,Helvetica,sans-serif;font-size:16px;font-weight:bold;"><a href="https://news.theatre.example.com/season/?utm_source=newsletter&amp;utm_medium=email&amp;utm_campaign=autumn-season-2026" style="display:inline-block;line-height:48px;width:260px;color:#ffffff;text-decoration:none;">Choose your seats</a></td></tr></table>
</td></tr>
<tr><td align="center" style="padding:20px 40px;font-family:Arial,Helvetica,sans-serif;font-size:11px;line-height:16px;color:#777777;">The Grand Theatre, 1 Example Street, Example City EX1 2MP<br>You are receiving this because you bought tickets from us. <a href="https://news.theatre.example.com/u/?id=8f3a2c91d7&amp;list=autumn-season-2026" style="color:#777777;">Unsubscribe</a> | <a href="https://news.theatre.example.com/preferences/?id=8f3a2c91d7" style="color:#777777;">Update your preferences</a><br>https://news.theatre.example.com/track/click/?u=8f3a2c91d7e04b6aa1c3&amp;id=5e7d0b2f19&amp;e=a7c9d1e3f5</td></tr>
</table>
</td></tr>
</table>
<img src="https://t.news.theatre.example.com/open/8f3a2c91d7e04b6aa1c3.gif" width="1" height="1" alt="" style="display:block;width:1px;height:1px;border:0;">
</body></html>
--a11ynlr
Content-Type: image/png; name="hero-autumn.png"
Content-ID: <hero-autumn@news.theatre.example.com>
Content-Disposition: inline; filename="hero-autumn.png"
Content-Transfer-Encoding: base64

EOF
cat <<'EOF'
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
ny8AAAAASUVORK5CYII=
--a11ynlr--

--a11ynla--
EOF
} > "$WORK/newsletter/newsletter.eml"
docker cp "$WORK_HOST/newsletter/newsletter.eml" "$DV:/tmp/newsletter.eml" >/dev/null
docker exec "$DV" sh -c "doveadm save -u '$TESTUSER' -m Archive < /tmp/newsletter.eml"
echo "delivered the wide marketing newsletter into $TESTUSER Archive"

echo "--- accounts file (the app connects to dovecot over implicit TLS, verifying the cert)"
cat > "$WORK/accounts.json" <<EOF
{"accounts":[{"name":"dovecot","imap_host":"dovecot","imap_port":993,
  "smtp_host":"dovecot","smtp_port":465,
  "username":"$TESTUSER","password":"$TESTPASS"}]}
EOF

echo "--- mailroom (dev-auth, syncing the dovecot account)"
# ⚠ CREATED, FED, THEN STARTED — the app process must never exist without its CA
# bundle. The app reconciles the declared accounts file ~30ms into boot and dials
# IMAP to validate it; a container started before the cert is in place reads
# accounts.json while SSL_CERT_FILE names a file that does not exist yet, and
# validation dies with "certificate signed by unknown authority". The result
# LATCHES in postgres (reconcileAccounts leaves a known account alone), so no
# restart re-validates it and even replacing the container does not clear it. That
# arrangement lost a required check on CI twice on byte-identical trees (#432).
# SEARCH IS ON, as it is in production: the sender card's "Move all from…" counts
# through a `from:` search (#708), and with MAILROOM_SEARCH_DISABLED=1 it can only
# fail, so its confirm step could never be reached here.
APPID=$(docker create --name "$APP" --network "$NET" --network-alias mailroom \
  -v "$VOL:/k:ro" \
  -v "$SOCKVOL:/run/mailroom-crypto" \
  -e MAILROOM_DATABASE_URL="postgres://mailroom_app:app@pg:5432/mailroom?sslmode=disable" \
  -e MAILROOM_MASTER_KEY_FILE=/k/master \
  -e MAILROOM_PGP_SOCKET=/run/mailroom-crypto/crypto.sock \
  -e MAILROOM_WKD_DOMAIN=example.com \
  -e MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS=1 \
  -e MAILROOM_DEV_AUTH_SUBJECT="operator" \
  -e MAILROOM_DEV_AUTH_I_UNDERSTAND="this-instance-has-no-authentication" \
  -e MAILROOM_DEV_AUTH_EMAIL="$TESTUSER" \
  -e MAILROOM_ACCOUNTS_FILE=/tmp/accounts.json \
  -e MAILROOM_SYNC_INTERVAL=2s \
  -e SSL_CERT_FILE=/tmp/dovecot.crt \
  -e MAILROOM_LOG_FORMAT=text \
  "$IMAGE")
docker cp "$WORK_HOST/accounts.json" "$APPID:/tmp/accounts.json"
docker cp "$WORK_HOST/dovecot.crt" "$APPID:/tmp/dovecot.crt"
docker start "$APPID" >/dev/null

wait_for "mailroom /readyz" 90 "$APP" \
  docker run --rm --network "$NET" "$CURL_IMAGE" -sf http://mailroom:8080/readyz

# ⚠ /readyz IS NOT EVIDENCE THAT THE ACCOUNT VALIDATED. The app serves every read
# path perfectly well with its one declared account stored sync-disabled, and this
# lane would then lint a desk with no mail on it. Boot onboarding logs one of two
# lines and never neither, so assert the positive one at the point the fault would
# occur.
_boot_i=0
while [ "$_boot_i" -lt 15 ]; do
  if docker logs "$APP" 2>&1 | grep -qE 'msg="account added"|sync disabled'; then break; fi
  _boot_i=$((_boot_i + 1)); sleep 1
done
if ! docker logs "$APP" 2>&1 | grep -q 'msg="account added"'; then
  echo "FATAL: mailroom did not validate the declared dovecot account, so no mail can sync" >&2
  echo "       and every surface below would be linted empty." >&2
  docker logs "$APP" 2>&1 | grep -iE "account|sync disabled" | head -20 >&2 || true
  exit 1
fi
echo "the declared account validated against dovecot's TLS certificate"

# ---------------------------------------------------------------------------
# The fixture stage: build the desk over REAL JMAP, then PROVE it exists.
# ---------------------------------------------------------------------------
#
# WHY IN NODE, AND WHY IN THE BROWSER IMAGE. This stage reads and writes JSON —
# resolve the account id from the session, wait for thirteen messages to sync,
# create three areas, mark one message read and one flagged, cross two rows to the
# other side of the line, then read Area/getSummary back — and doing that with
# `grep -o` over curl output is how a fixture comes to believe it did something it
# did not. The pinned Chromium image already ships node with a global `fetch` (the
# same reason scripts/hostile-content-e2e.sh builds its tracker on that image: no
# package install, works offline), and it is a hard dependency of this lane
# anyway, checked at the top.
#
# ⚠ AND IT REPORTS PER-AREA COUNTS, NOT ONE TOTAL. "13 messages synced" cannot
# distinguish a desk with two populated areas from thirteen messages that all
# landed in Needs sorting — and the second one would lint perfectly while proving
# nothing about a grid with rows in it. The stage FAILS unless at least two areas
# hold at least two surface rows each AND one drawer holds at least two.
cat > "$WORK/a11y-fixture.cjs" <<'FIXEOF'
"use strict";
const fs = require("fs");
const BASE = "http://mailroom:8080";

const NEED = [
  "urn:ietf:params:jmap:core",
  "urn:ietf:params:jmap:mail",
  "urn:mailroom:desk",
  "urn:mailroom:triage",
];

let USING = NEED;

async function jmap(calls) {
  const res = await fetch(BASE + "/jmap", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ using: USING, methodCalls: calls }),
  });
  const text = await res.text();
  if (!res.ok) throw new Error("jmap HTTP " + res.status + ": " + text.slice(0, 400));
  const body = JSON.parse(text);
  if (body.type) throw new Error("jmap request error: " + text.slice(0, 400));
  for (const r of body.methodResponses) {
    if (r[0] === "error") throw new Error("jmap method error: " + JSON.stringify(r));
  }
  return body.methodResponses;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

(async () => {
  // ---- the session, and the CAPABILITIES this fixture needs ---------------
  // ⚠ ASSERTED, NOT ASSUMED. RFC 8620 §3.2's `using` gate is fail-closed: a
  // capability this server does not advertise makes the whole request a 400, and
  // a capability we forget to declare makes every method in it `unknownMethod`
  // while the read path still looks perfect. That is #159 by name, and it cost a
  // release once.
  const sess = await (await fetch(BASE + "/jmap/session")).json();
  const advertised = Object.keys(sess.capabilities || {});
  const missing = NEED.filter((u) => !advertised.includes(u));
  if (missing.length) {
    console.log("NOFIXTURE the session does not advertise " + missing.join(","));
    process.exit(5);
  }
  const accountId = Object.values(sess.primaryAccounts || {})[0];
  if (!accountId) {
    console.log("NOFIXTURE the session names no primary account");
    process.exit(5);
  }
  console.log("fixture accountId=" + accountId);

  // ---- wait for the mail to sync -----------------------------------------
  const WANT = Number(process.env["A11Y_WANT"] || "13");
  let ids = [];
  const started = Date.now();
  for (;;) {
    const [q] = await jmap([
      ["Email/query", { accountId, filter: {}, calculateTotal: true, limit: 60 }, "q"],
    ]);
    ids = q[1].ids || [];
    if (ids.length >= WANT) break;
    if (Date.now() - started > 180000) {
      console.log("NOFIXTURE only " + ids.length + " of " + WANT + " messages synced in 180s");
      process.exit(5);
    }
    await sleep(2000);
  }
  console.log("fixture synced=" + ids.length + " in " + (Date.now() - started) + "ms");

  const [g] = await jmap([
    [
      "Email/get",
      {
        accountId,
        ids,
        properties: ["subject", "from", "threadId", "hasAttachment", "keywords", "receivedAt"],
      },
      "g",
    ],
  ]);
  const mail = g[1].list;
  const bySubject = new Map(mail.map((m) => [m.subject, m]));
  const need = (subject) => {
    const m = bySubject.get(subject);
    if (!m) {
      console.log("NOFIXTURE no message with subject " + JSON.stringify(subject) + " synced");
      process.exit(5);
    }
    return m;
  };

  // ---- the two-message thread --------------------------------------------
  const threads = new Map();
  for (const m of mail) threads.set(m.threadId, (threads.get(m.threadId) || 0) + 1);
  const biggest = Math.max(...threads.values());
  if (biggest < 2) {
    console.log("NOFIXTURE no thread holds two messages (largest=" + biggest + ")");
    process.exit(5);
  }

  // ---- the held inline image (#675), in Archive ----------------------------
  // Waited for on its own: it is not one of the WANT desk messages, so the loop
  // above can finish before it lands.
  let held = null;
  for (const t0 = Date.now(); !held; ) {
    const [hq] = await jmap([["Email/query", { accountId, filter: {}, limit: 60 }, "hq"]]);
    const [hg] = await jmap([["Email/get", { accountId, ids: hq[1].ids || [], properties: ["subject"] }, "hg"]]);
    held = (hg[1].list || []).find((m) => m.subject === "Held inline image") || null;
    if (held) break;
    if (Date.now() - t0 > 120000) {
      console.log("NOFIXTURE the held-inline message never synced from Archive");
      process.exit(5);
    }
    await sleep(2000);
  }

  // ---- the calendar invitation (#725), in Archive ---------------------------
  // Waited for on its own, like the held message, and REQUIRED to arrive as an
  // answerable invitation: a card with no buttons would make the step below
  // assert over a group that is not there.
  let invite = null;
  for (const t0 = Date.now(); !invite; ) {
    const [iq] = await jmap([["Email/query", { accountId, filter: {}, limit: 60 }, "iq"]]);
    const [ig] = await jmap([
      ["Email/get", { accountId, ids: iq[1].ids || [], properties: ["subject", "calendarInvite"] }, "ig"],
    ]);
    invite = (ig[1].list || []).find((m) => m.subject === "Invitation: Accessibility review") || null;
    if (invite) break;
    if (Date.now() - t0 > 120000) {
      console.log("NOFIXTURE the calendar invitation never synced from Archive");
      process.exit(5);
    }
    await sleep(2000);
  }
  if (!invite.calendarInvite || invite.calendarInvite.canReply !== true) {
    console.log("NOFIXTURE the invitation is not answerable: " + JSON.stringify(invite.calendarInvite));
    process.exit(5);
  }

  // ---- the folded thread (#759), in Archive -------------------------------
  // Waited for like the held message. The FIRST message is marked read through
  // Email/set, so the pane opened on the second folds it; the check asserts
  // the folded button exists before it lints anything.
  let fold1 = null;
  let fold2 = null;
  for (const t0 = Date.now(); !(fold1 && fold2); ) {
    const [fq] = await jmap([["Email/query", { accountId, filter: {}, limit: 80 }, "fq"]]);
    const [fg] = await jmap([
      ["Email/get", { accountId, ids: fq[1].ids || [], properties: ["subject", "threadId"] }, "fg"],
    ]);
    const list = fg[1].list || [];
    fold1 = list.find((m) => m.subject === "Compiler notes") || null;
    fold2 = list.find((m) => m.subject === "Re: Compiler notes") || null;
    if (fold1 && fold2) break;
    if (Date.now() - t0 > 120000) {
      console.log("NOFIXTURE the folded-thread messages never synced from Archive");
      process.exit(5);
    }
    await sleep(2000);
  }
  if (fold1.threadId !== fold2.threadId) {
    console.log("NOFIXTURE the two folded-thread messages are not one conversation");
    process.exit(5);
  }
  const [fs1] = await jmap([
    ["Email/set", { accountId, update: { [fold1.id]: { "keywords/$seen": true } } }, "fs1"],
  ]);
  if (Object.keys(fs1[1].notUpdated || {}).length) {
    console.log("NOFIXTURE could not mark the folded message read: " + JSON.stringify(fs1[1].notUpdated));
    process.exit(5);
  }

  // ---- the wide newsletter (#798), in Archive ------------------------------
  // Waited for like the held message. hasAttachment is NOT asserted: the hero
  // is an inline part, and whether it counts is not this lane's question.
  let newsletter = null;
  for (const t0 = Date.now(); !newsletter; ) {
    const [nq] = await jmap([["Email/query", { accountId, filter: {}, limit: 80 }, "nq"]]);
    const [ng] = await jmap([["Email/get", { accountId, ids: nq[1].ids || [], properties: ["subject"] }, "ng"]]);
    newsletter =
      (ng[1].list || []).find((m) => m.subject === "Tickets on sale now for the autumn season at the Grand") || null;
    if (newsletter) break;
    if (Date.now() - t0 > 120000) {
      console.log("NOFIXTURE the wide newsletter never synced from Archive");
      process.exit(5);
    }
    await sleep(2000);
  }

  // ---- the attachment ----------------------------------------------------
  const attach = need("Notes from the meeting");
  if (attach.hasAttachment !== true) {
    console.log("NOFIXTURE the attachment message reports hasAttachment=" + attach.hasAttachment);
    process.exit(5);
  }

  // ---- the areas, WHICH THE SERVER ALREADY MADE ---------------------------
  //
  // ⚠ THIS STAGE USED TO CREATE THEM, AND Area/set REFUSED — correctly. The store
  // materialises the DEFAULT PLAN the first time a user has zero areas (store
  // materialiseDefaultAreas, from internal/areas.Default): one area per category
  // tab, named Primary / Social / Promotions / Updates / Forums and fed by that
  // tab alone. A tab may feed AT MOST ONE area, so "create an area fed by
  // promotions" is invalidProperties on an instance that already has Promotions.
  //
  // Reading them instead of inventing them is the better fixture anyway: the desk
  // this lane lints is then the desk a first-run user actually gets, not an
  // arrangement no deployment has.
  const [ag] = await jmap([["Area/get", { accountId, ids: null }, "ag"]]);
  const areas = (ag[1].list || []).map((a) => ({
    id: a.id,
    name: a.name,
    tabs: a.tabs,
    position: a.position,
  }));
  console.log(
    "fixture areas=" +
      areas.map((a) => a.name + "[" + (a.tabs || []).join("|") + "]").join(" "),
  );
  // The three the fixture's mail is aimed at. Named rather than counted: if the
  // default plan is ever changed, this says WHICH area went missing instead of
  // failing later with an empty section.
  const areaForTab = (tab) => areas.find((a) => (a.tabs || []).indexOf(tab) >= 0);
  const missingArea = ["promotions", "primary", "updates"].filter((x) => !areaForTab(x));
  if (missingArea.length) {
    console.log(
      "NOFIXTURE no area is fed by " +
        missingArea.join(",") +
        " — the default area plan is not what this fixture's mail was written for",
    );
    process.exit(5);
  }

  // ---- one read row, one flagged row (the tray) --------------------------
  // Through the client's own verb — Email/set on `keywords` — rather than through
  // `doveadm flags`, so the state under test is the one JMAP reports and there is
  // one fewer tool's search syntax between the fixture and the assertion.
  const seenOne = need("Weekly digest of new questions");
  const flagOne = need("The garden plan");
  const [es] = await jmap([
    [
      "Email/set",
      {
        accountId,
        update: {
          [seenOne.id]: { "keywords/$seen": true },
          [flagOne.id]: { "keywords/$flagged": true },
        },
      },
      "es",
    ],
  ]);
  if (Object.keys(es[1].notUpdated || {}).length) {
    console.log("NOFIXTURE Email/set refused: " + JSON.stringify(es[1].notUpdated));
    process.exit(5);
  }

  // ---- THE LINE, MOVED TO THE FLOOR --------------------------------------
  //
  // ⚠ MEASURED, NOT ASSUMED, AND THE MEASUREMENT CHANGED THIS STAGE. The first run
  // of this fixture expected every message to be `unscored` ("no triage model is
  // trained in this stack") and to sit on the surface. Area/getSummary said
  // otherwise: at the DEFAULT line of 0.5, `unscored` was 0 everywhere and the
  // split was Primary surface=5, Promotions drawer=5, Updates drawer=3 — every
  // promotional and machine message already BELOW the line, on scores this stack
  // produces at ingest. So Review first had rows in exactly ONE area, and a lane
  // that had not checked would have linted a one-section desk while claiming two.
  //
  // The line is the desk's one setting, so the fixture sets it: threshold 0 puts
  // everything the model scored on the surface (lineSideSQL: `score >= threshold`
  // is 'surface'), which is the desk the announcement script needs — several
  // populated areas, and a drawer whose contents are there because the USER put
  // them there.
  const [dp] = await jmap([
    ["DeskPreferences/set", { accountId, update: { [accountId]: { lineThreshold: 0 } } }, "dp"],
  ]);
  if (Object.keys(dp[1].notUpdated || {}).length) {
    console.log("NOFIXTURE DeskPreferences/set refused: " + JSON.stringify(dp[1].notUpdated));
    process.exit(5);
  }

  // ---- two rows on the OTHER side of the line ----------------------------
  // `Triage/correct` is the call the client makes for Shift+↓ (core/actions.ts
  // setImportant), and since #464 the server writes it as a PLACEMENT
  // (messages.line_side) which OUTRANKS the score — so these two rows are in the
  // Promotions drawer because the user put them there, and the drawer has a count
  // to announce.
  const crossed = [need("40% off everything this weekend").id, need("Our summer sale starts now").id];
  const corrections = {};
  for (const id of crossed) corrections[id] = "notImportant";
  const [tc] = await jmap([["Triage/correct", { accountId, corrections }, "tc"]]);
  const refused = Object.keys(tc[1].notCorrected || {});
  if (refused.length) {
    console.log("NOFIXTURE Triage/correct refused: " + JSON.stringify(tc[1].notCorrected));
    process.exit(5);
  }

  // ---- READ THE DESK BACK, and report per area ---------------------------
  const [sum] = await jmap([["Area/getSummary", { accountId }, "sum"]]);
  const rows = sum[1].list || [];
  const nameOf = (id) => {
    if (id === null) return null;
    const a = areas.find((x) => x.id === id);
    return a ? a.name : id;
  };
  const summary = rows.map((r) => ({
    areaId: r.areaId,
    areaName: nameOf(r.areaId),
    surface: r.surface,
    drawer: r.drawer,
    unscored: r.unscored,
  }));
  for (const r of summary) {
    console.log(
      "fixture summary " +
        (r.areaName || "(needs sorting)") +
        " surface=" +
        r.surface +
        " drawer=" +
        r.drawer +
        " unscored=" +
        r.unscored,
    );
  }
  console.log("fixture threshold=" + sum[1].threshold);

  // ⚠ THE GATE. Two areas with two rows each, and one drawer with two — the exact
  // shape every assertion downstream depends on. Reported per area above, so a
  // failure says WHICH area came up short instead of "the fixture is wrong".
  const populated = summary.filter(
    (r) => r.areaId !== null && r.surface + r.unscored >= 2,
  );
  const drawers = summary.filter((r) => r.areaId !== null && r.drawer >= 2);
  if (populated.length < 2) {
    console.log(
      "NOFIXTURE only " +
        populated.length +
        " area(s) hold >= 2 surface rows; the tab classifier did not put the mail " +
        "where this fixture assumed (see the fixture comment in scripts/a11y-e2e.sh)",
    );
    process.exit(5);
  }
  if (drawers.length < 1) {
    console.log("NOFIXTURE no drawer holds >= 2 rows, so there is no drawer count to announce");
    process.exit(5);
  }

  // ---- MARK SETUP COMPLETE, the way "Skip setup, open the desk" does --------
  //
  // ⚠ WITHOUT THIS THE BROWSER STAGE NEVER SEES A DESK AT ALL. Main grew the
  // day-one wizard (web/src/app/ui/setup/, openSetup.ts) after this lane was
  // written: a fresh login's DeskPreferences row is materialised with
  // `setupCompletedAt` null, and `shouldOpenSetup()` sends the client to
  // `#/setup` once an account exists and the mailboxes have loaded — which this
  // fixture's account and mailboxes now have. Every `#desk-grid` wait in
  // a11y-check.cjs then times out, because the grid is never drawn while the
  // wizard has the screen: `ERR Waiting for selector #desk-grid[role=grid]
  // failed: Waiting failed: 60000ms exceeded`, with nothing in that line saying
  // WHY (mailroom#447 follow-up; see also the timeout diagnostics added to
  // a11y-check.cjs's `waitForSelectorD` for the general form of this mistake).
  //
  // This is the EXACT call the client's own exit makes
  // (app/ui/setup/SetupRoute.tsx `finish`, the handler behind both "Skip setup"
  // and step 4's "Open the desk"): `DeskPreferences/set` with
  // `setupCompletedAt` set to `new Date().toISOString()` — an RFC 3339 UTC
  // timestamp — on the one record this login has (internal/api/desk_preferences.go:
  // the wire `id` is the account id in this single-account dev-auth fixture, the
  // same key the `lineThreshold` write above already uses).
  //
  // ⚠ READ BACK, NOT TRUSTED FROM THE /set RESPONSE. A patch the server silently
  // coerced, ignored or rejected by a path this fixture did not check would
  // otherwise look identical to one it applied, and the failure would surface as
  // the SAME bare grid timeout this block exists to stop.
  //
  // ORDERING NOTE for whoever edits the surface list next: the setup WIZARD
  // itself is still linted (a11y-check.cjs's "setup-wizard" surface) — by
  // navigating to `#/setup` explicitly once the browser stage starts, which
  // renders the wizard regardless of `setupCompletedAt` (App.tsx: `const setup =
  // route.page === "setup"`, not gated by `shouldOpenSetup`). Marking setup
  // complete HERE, before any browser exists, and reaching the wizard by hand
  // THERE is simpler than sequencing one browser through "wizard first, then
  // mark complete, then the desk": it needed no change to how a11y-check.cjs
  // hands the fixture manifest to the browser, and it is exactly how a real user
  // reaches the wizard after day one — "Run setup again", from Settings.
  const setupAt = new Date().toISOString();
  console.log("fixture setupCompletedAt=" + setupAt);
  const [dpSet] = await jmap([
    [
      "DeskPreferences/set",
      { accountId, update: { [accountId]: { setupCompletedAt: setupAt } } },
      "dpset",
    ],
  ]);
  if (Object.keys(dpSet[1].notUpdated || {}).length) {
    console.log(
      "NOFIXTURE DeskPreferences/set (setupCompletedAt) refused: " +
        JSON.stringify(dpSet[1].notUpdated),
    );
    process.exit(5);
  }
  const [dpGet] = await jmap([["DeskPreferences/get", { accountId, ids: null }, "dpget"]]);
  const dpAfter = (dpGet[1].list || [])[0];
  if (!dpAfter || dpAfter.setupCompletedAt === null || dpAfter.setupCompletedAt === undefined) {
    console.log(
      "NOFIXTURE setupCompletedAt read back as " +
        JSON.stringify(dpAfter && dpAfter.setupCompletedAt) +
        " after DeskPreferences/set — the wizard would still open and every desk " +
        "surface below would wait forever for #desk-grid",
    );
    process.exit(5);
  }
  console.log("fixture setupCompletedAt confirmed by DeskPreferences/get: " + dpAfter.setupCompletedAt);

  const out = {
    accountId,
    areas,
    summary,
    crossed,
    threshold: sum[1].threshold,
    biggestThread: biggest,
    seen: { id: seenOne.id, subject: seenOne.subject },
    flagged: { id: flagOne.id, subject: flagOne.subject },
    attachment: { id: attach.id, subject: attach.subject, name: "notes.txt" },
    heldInline: { id: held.id, subject: held.subject, name: "banner.png" },
    invite: { id: invite.id, subject: invite.subject, title: "Accessibility review" },
    folded: { openId: fold2.id, foldedId: fold1.id, subject: fold2.subject },
    newsletter: { id: newsletter.id, subject: newsletter.subject },
    drawerArea: { name: drawers[0].areaName, count: drawers[0].drawer },
    messages: mail.map((m) => ({
      id: m.id,
      subject: m.subject,
      sender: (m.from && m.from[0] && (m.from[0].name || m.from[0].email)) || "",
    })),
  };
  fs.writeFileSync("/tmp/fixture.json", JSON.stringify(out, null, 1));
  console.log(
    "fixture OK areas_with_rows=" +
      populated.map((p) => p.areaName + ":" + (p.surface + p.unscored)).join(",") +
      " drawers=" +
      drawers.map((d) => d.areaName + ":" + d.drawer).join(",") +
      " thread=" +
      biggest +
      " attachment=" +
      attach.id,
  );
})().catch((e) => {
  console.log("ERR fixture " + (e && e.message ? e.message : String(e)));
  process.exit(2);
});
FIXEOF

echo
echo "=== building the fixture desk over real JMAP ================================"
FIXC=$(docker create --network "$NET" -e A11Y_WANT="$DELIVERED" \
  --entrypoint node "$CHROME_IMAGE" /tmp/a11y-fixture.cjs)
docker cp "$WORK_HOST/a11y-fixture.cjs" "$FIXC:/tmp/a11y-fixture.cjs" >/dev/null
set +e
docker start -a "$FIXC" 2>&1
FIX_RC=$?
set -e
if [ "$FIX_RC" != "0" ]; then
  echo
  echo "=== mailroom logs ==="; docker logs "$APP" 2>&1 | tail -30
  echo
  echo "A11Y E2E FAILED: the fixture desk was not built (exit $FIX_RC)"
  docker rm -fv "$FIXC" >/dev/null 2>&1 || true
  exit 5
fi
docker cp "$FIXC:/tmp/fixture.json" "$WORK_HOST/fixture.json" >/dev/null
docker rm -fv "$FIXC" >/dev/null 2>&1 || true
[ -s "$WORK/fixture.json" ] || { echo "A11Y E2E FAILED: the fixture wrote no manifest" >&2; exit 5; }

# THE VERDICT for the held inline image (#675). A real detection needs clamd and
# a matching signature (scripts/clamav-test.sh); the chip renders from the
# STORED verdict, so the lane writes that. Part 2 is the image (1 is the HTML).
# The browser half reads the name the chip announces, so a wrong part fails
# there (no chip) rather than passing for the wrong reason.
HELD_ROW=$(docker exec "$PG" psql -U postgres -d mailroom -qtA -v ON_ERROR_STOP=1 -c "
  UPDATE app.attachments a SET scan_status = 'flagged'
    FROM app.messages m
   WHERE m.id = a.message_id AND m.subject = 'Held inline image' AND a.part_path = '2'
  RETURNING a.part_path" | head -1)
[ "$HELD_ROW" = "2" ] || { echo "A11Y E2E FAILED: the held-inline image part was not projected to flag (got '$HELD_ROW')" >&2; exit 5; }
echo "flagged part $HELD_ROW of the held-inline message"

# THE FOLLOW-UP'S AGE (#58): the sent message arrived seconds ago as far as
# INTERNALDATE is concerned; ten days back makes it due (seven) and keeps it
# inside the thirty-day window. Waited for, since Sent may sync a pass after
# INBOX; a row that never appears fails HERE rather than as an empty strip.
FU_ROW=""
_fu_i=0
while [ "$_fu_i" -lt 30 ]; do
  FU_ROW=$(docker exec "$PG" psql -U postgres -d mailroom -qtA -v ON_ERROR_STOP=1 -c "
    UPDATE app.messages SET received_at = now() - interval '10 days'
     WHERE subject = 'Seed order for the spring beds'
    RETURNING 'backdated'" | head -1)
  [ "$FU_ROW" = "backdated" ] && break
  _fu_i=$((_fu_i + 1)); sleep 2
done
[ "$FU_ROW" = "backdated" ] || { echo "A11Y E2E FAILED: the follow-up sent message never synced" >&2; exit 5; }
echo "backdated the follow-up sent message ten days"

echo
echo "=== axe + the accessibility tree, in the pinned Chromium ===================="
# docker cp the script and the linter IN rather than bind-mounting a host path (see
# the cert note above — a bind mount does not resolve when the job is itself a
# container with a socket-mounted Docker).
#
# --shm-size: Chromium keeps renderer shared memory in /dev/shm, which Docker
# defaults to 64 MiB. This run drives one browser through forty-two surface
# loads and three reloads; when shm runs out a renderer STALLS rather than
# erroring, which would surface here as a selector that never appears.
#
# A11Y_PREFS_SET_DELAY_MS: every ClientPreferences/set is held IN THE PAGE this
# long before it is sent, ON PURPOSE and by default (#658, #463's method). The
# Appearance steps apply on screen at once and write upward afterwards, and the
# step after them reloads; a harness that waited for the screen instead of the
# write lost the write to that reload whenever the runner was slow, and the
# server put the old value back. Held 1.5 s, that race is lost EVERY run, so a
# step that stops waiting for its write goes red here rather than once a month
# on main. Set 0 to measure without it.
CHKC=$(docker create --network "$NET" --shm-size=1g \
  -e NODE_PATH=/usr/src/app/node_modules \
  -e A11Y_CONTRAST_REPORT="${A11Y_CONTRAST_REPORT:-0}" \
  -e A11Y_PREFS_SET_DELAY_MS="${A11Y_PREFS_SET_DELAY_MS:-1500}" \
  -e A11Y_SHOTS="$([ -n "${A11Y_SHOT_DIR:-}" ] && echo 1 || echo 0)" \
  --entrypoint node "$CHROME_IMAGE" /tmp/a11y-check.cjs)
docker cp "$REPO_HOST/scripts/a11y-check.cjs" "$CHKC:/tmp/a11y-check.cjs" >/dev/null
docker cp "$REPO_HOST/scripts/a11y-contrast.allow" "$CHKC:/tmp/a11y-contrast.allow" >/dev/null
docker cp "$REPO_HOST/$AXE_JS" "$CHKC:/tmp/axe.min.js" >/dev/null
docker cp "$WORK_HOST/fixture.json" "$CHKC:/tmp/fixture.json" >/dev/null
# ⚠ THE EXIT CODE COMES FROM `docker wait`, NOT FROM THE PIPELINE. Piping
# `docker start -a` into `tee` (which is what streams a multi-minute run rather
# than holding it all back to the end) throws the container's status away and
# leaves `tee`'s, which is 0 whatever the check found — a lane that is green
# however red it went. `${PIPESTATUS[0]}` would fix it in bash and NOT in the
# `sh` that CI invokes this with, which is the worse of the two failures: it would
# work locally. `docker wait` prints the code the container actually exited with
# and needs no shell feature at all.
set +e
docker start -a "$CHKC" 2>&1 | tee "$WORK/check.out"
set -e
CHK_RC="$(docker wait "$CHKC" 2>/dev/null || echo 99)"
# Screenshots of the API keys screens (#49), only when asked for: a directory
# on the host to copy them into. CI never sets it.
if [ -n "${A11Y_SHOT_DIR:-}" ]; then
  mkdir -p "$A11Y_SHOT_DIR"
  docker cp "$CHKC:/tmp/shots/." "$A11Y_SHOT_DIR/" >/dev/null 2>&1 || echo "note: no screenshots to copy"
fi
docker rm -fv "$CHKC" >/dev/null 2>&1 || true

echo
if [ "$CHK_RC" != "0" ] || ! grep -q '^CLEAN a11y ' "$WORK/check.out"; then
  echo "=== mailroom logs ==="
  docker logs "$APP" 2>&1 | tail -30
  echo
  # NO ALLOWLIST OF KNOWN FAILURES. Anything that is not a CLEAN line is a
  # failure — the enumerated codes, `ERR …` from the check script's own catch, and
  # any output nobody has seen before. An allowlist of already-known tokens is an
  # allowlist that no NEW failure can redden the build through (#281).
  echo "A11Y E2E FAILED (exit $CHK_RC)"
  if [ "$CHK_RC" = "0" ]; then
    echo "  ⚠ the check script exited 0 without printing a CLEAN line, which means it"
    echo "    stopped somewhere it does not know about. That is a failure, not a pass."
    exit 1
  fi
  exit "$CHK_RC"
fi

echo "=== THE CLIENT IS OPERABLE BY THE ACCESSIBILITY TREE ==="
grep '^CLEAN a11y ' "$WORK/check.out"
