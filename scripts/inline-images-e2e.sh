#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Inline (cid:) images, proven end to end against the REAL image (Forgejo #99),
# WITHOUT re-opening the remote-content hole (#39, spec §11). Sibling of
# scripts/hostile-content-e2e.sh and companion to docs/hostile-content.md §2.8.
#
# The one thing this proves, in a single run, in a real headless Chromium:
#
#   1. A multipart/related message with a cid-referenced inline image renders that
#      image INLINE — resolved to a data: URL the same way web/ does — under the
#      message document's own CSP (img-src data:), with NO gate.
#   2. A remote tracking pixel in the SAME message is STILL blocked: the tracker
#      container receives ZERO bytes and the browser makes ZERO requests to it.
#   3. The "Show remote content" affordance still works: with allowRemote, the
#      browser DOES attempt the remote fetch (we abort it so the tracker stays
#      silent) — proving inline-by-default did not disable the opt-in.
#   4. A QUARANTINED inline image (#675), in the REAL reading pane: it gets the
#      same withheld chip and release action as an attachment, its bytes are
#      never requested, and it does not render — while the displayed image in
#      the same message IS fetched and DOES render. An image part whose
#      Content-ID the HTML never references is listed as a file instead of
#      appearing nowhere.
#
# Same container-only shape as hostile-content-e2e.sh: postgres + dovecot (real
# implicit TLS) + the real mailroom image + a tracker that must stay empty +
# alpine-chrome. No published ports, no host toolchain, no source bind mounts.
#
# Usage:  scripts/inline-images-e2e.sh
#         MAILROOM_IMAGE=mailroom:test scripts/inline-images-e2e.sh
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

IMAGE="${MAILROOM_IMAGE:-mailroom:inline-e2e-$UNIQ}"
PG_IMAGE="${PG_IMAGE:-postgres:18}"
DOVECOT_IMAGE="${DOVECOT_IMAGE:-dovecot/dovecot:2.3.21}"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.10.1}"
# PINNED BY DIGEST (#254) IN ONE SHARED FILE (#447). This suite's assertions are
# negative ones (nothing was fetched, no remote reference resolved), and a browser
# that moves under a floating tag can turn those green for the wrong reason with
# no commit to point at. "Keep the suites on the SAME digest" used to be a
# sentence in this comment; scripts/e2e-chrome-image.env is that sentence made
# structural, and it carries the full argument.
. "$(dirname "$0")/e2e-chrome-image.env"

NET="mailroom-inline-e2e-$UNIQ"
VOL="mailroom-inline-e2e-key-$UNIQ"
PG="mailroom-inline-e2e-pg-$UNIQ"
DV="mailroom-inline-e2e-dovecot-$UNIQ"
APP="mailroom-inline-e2e-app-$UNIQ"
TRK="mailroom-inline-e2e-tracker-$UNIQ"

TESTUSER="victim@example.com"
TESTPASS="victim-mailbox-password"

cd "$(dirname "$0")/.."
WORK="$(pwd)/.inline-e2e-tmp.$UNIQ"
mkdir -p "$WORK"
WORK_HOST="$(cd "$WORK" && { pwd -W 2>/dev/null || pwd; })"

cleanup() {
  echo "--- cleaning up"
  docker rm -fv "$APP" "$DV" "$PG" "$TRK" >/dev/null 2>&1 || true
  docker volume rm -f "$VOL" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  # Only an image THIS run built (uniquely tagged); a MAILROOM_IMAGE override is the caller's.
  if [ -z "${MAILROOM_IMAGE:-}" ]; then docker rmi "$IMAGE" >/dev/null 2>&1 || true; fi
  rm -rf "$WORK"
}
# ⚠ INT and TERM are NOT decoration: a CI cancellation sends SIGTERM, and a
# shell with only an EXIT trap dies WITHOUT running it. Measured in an alpine
# container — `trap cleanup EXIT` left the marker unwritten on SIGTERM, while
# `trap cleanup EXIT INT TERM` wrote it. That is how cancelled runs leaked
# long-lived side containers into the runners (mailroom#346); three were found
# aged 7h, 8h and 11h. cleanup is idempotent, so the EXIT trap firing again
# after the TERM trap is harmless.
trap cleanup EXIT INT TERM
cleanup >/dev/null 2>&1 || true
mkdir -p "$WORK"

# wait_for retries a readiness command until it succeeds. If it never does, it
# FAILS THE SCRIPT LOUDLY and dumps the container's log and final state.
#
# Without this a bare `for ... do <check> && break; done` falls through on
# exhaustion and the script carries on against a service that never came up, so
# the failure surfaces later as something unrelated — a real CI run died with
# "container ... is not running" from the NEXT docker exec, which says nothing
# about postgres having failed to start (#135, ported from
# hostile-content-e2e.sh).
#
#   $1 label, $2 attempts (~seconds), $3 container to dump on failure ("" none),
#   $4.. the command to run.
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

if [ -z "${MAILROOM_IMAGE:-}" ]; then
  echo "--- building $IMAGE"
  docker build -t "$IMAGE" .
fi

echo "--- network + master key"
docker network create "$NET" >/dev/null
docker volume create "$VOL" >/dev/null
docker run --rm -v "$VOL:/k" "$PG_IMAGE" sh -c \
  'openssl rand -hex 32 | tr -d "\r\n" > /k/master && chmod 644 /k/master' >/dev/null

echo "--- tracker (the thing that must stay silent)"
docker run -d --name "$TRK" --network "$NET" --network-alias tracker.evil.example \
  --entrypoint node "$CHROME_IMAGE" -e '
    const fs = require("fs"); fs.writeFileSync("/tmp/hits","");
    require("http").createServer((req,res) => {
      fs.appendFileSync("/tmp/hits", req.method+" "+req.url+"\n");
      res.writeHead(200,{"Content-Type":"image/gif"}); res.end();
    }).listen(8099);
  ' >/dev/null

echo "--- postgres"
docker run -d --name "$PG" --network "$NET" --network-alias pg \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=mailroom "$PG_IMAGE" >/dev/null
# -h 127.0.0.1 forces TCP: the postgres image's first-init temp server listens on
# the Unix socket ONLY, so a socket-based check reports "ready" before
# POSTGRES_DB=mailroom exists. TCP comes up only with the real post-init server.
# This check was socket-based and raced that init (#135).
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

echo "--- dovecot (implicit TLS, self-signed) + an inline-image message"
# Generate the self-signed cert INSIDE a throwaway container and copy the files
# OUT with `docker cp` — a bind mount of a host path does not resolve when the CI
# job is itself a container with a socket-mounted Docker (the -v source is read by
# the host daemon, so $WORK — a path inside the job container — mounts an empty
# ghost dir and the certs never reach the job filesystem).
#
# ⚠ THIS IS WHY THE SUITE PASSED LOCALLY AND FAILED THE MOMENT IT REACHED CI
# (#280). The bind-mount form works fine against a host Docker in Git Bash, so a
# green local run said nothing about the runner. hostile-content-e2e.sh already
# carries this fix and this comment; this script never got it, because it had
# never run anywhere that could expose the difference.
CERTGEN=$(docker create "$DOVECOT_IMAGE" sh -c '
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
    -keyout /tmp/dovecot.key -out /tmp/dovecot.crt \
    -subj "/CN=dovecot" -addext "subjectAltName=DNS:dovecot" >/dev/null 2>&1')
docker start -a "$CERTGEN" >/dev/null 2>&1 || true
docker cp "$CERTGEN:/tmp/dovecot.crt" "$WORK_HOST/dovecot.crt"
docker cp "$CERTGEN:/tmp/dovecot.key" "$WORK_HOST/dovecot.key"
docker rm -fv "$CERTGEN" >/dev/null
# ⚠ `docker start -a … || true` above swallows a certgen failure, and an empty or
# truncated dovecot.crt would then be copied into BOTH dovecot and the app with
# nothing said. This is a cheap absent/empty guard, NOT the proof: what actually
# establishes the trust material is the handshake in the readiness check below,
# which compares the bytes dovecot serves against this exact file.
{ [ -s "$WORK/dovecot.crt" ] && [ -s "$WORK/dovecot.key" ] &&
  grep -q "BEGIN CERTIFICATE" "$WORK/dovecot.crt"; } || {
  echo "FATAL: certgen produced no usable certificate (crt $(wc -c < "$WORK/dovecot.crt" 2>/dev/null || echo 0) bytes)" >&2
  exit 1; }

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
}
listen = *
log_path=/dev/stdout
info_log_path=/dev/stdout
debug_log_path=/dev/stdout
EOF

# A multipart/related message: an alternative (text + HTML), and an inline PNG the
# HTML references by Content-ID. The HTML ALSO carries a remote tracking pixel, so
# one message exercises both paths. The plain-text half exists on purpose — the
# reading pane must prefer the HTML (#99), and here we render the HTML directly.
# The PNG is a real 1x1 image, so a decode either succeeds (naturalWidth>0) or it
# does not.
cat > "$WORK/inline.eml" <<EOF
From: Newsletter <news@example.com>
To: $TESTUSER
Subject: Your weekly digest
Message-ID: <inline-1@example.com>
Date: Mon, 12 Jul 2026 10:00:00 +0000
MIME-Version: 1.0
Content-Type: multipart/related; boundary="rel"

--rel
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset=utf-8

Plain-text version. If you can read THIS in the reading pane, the HTML lost.
--alt
Content-Type: text/html; charset=utf-8

<html><body>
<p>Hello, here is our logo:</p>
<img src="cid:logo@example.com" width="1" height="1" alt="logo">
<img src="https://tracker.evil.example:8099/hero.png" width="600" height="200" alt="remote">
<img src="https://tracker.evil.example:8099/pixel.gif?id=victim" width="1" height="1" alt="beacon">
</body></html>
--alt--
--rel
Content-Type: image/png
Content-ID: <logo@example.com>
Content-Transfer-Encoding: base64

iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==
--rel--
EOF

# #675's message: the HTML references two inline images, one of which the
# harness quarantines below, and the message carries a third image part with a
# Content-ID that the HTML never references. Three DIFFERENT 1x1 PNGs, so no
# two parts share a content hash — the quarantine's release identity is the
# bytes (ADR-027 §8), and identical parts would make "this one is held" a claim
# about all three.
cat > "$WORK/held.eml" <<EOF
From: Newsletter <news@example.com>
To: $TESTUSER
Subject: Held inline image
Message-ID: <inline-held-675@example.com>
Date: Sun, 11 Jul 2026 10:00:00 +0000
MIME-Version: 1.0
Content-Type: multipart/related; boundary="rel"

--rel
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset=utf-8

Plain-text version.
--alt
Content-Type: text/html; charset=utf-8

<html><body>
<p>Two pictures:</p>
<img src="cid:shown@example.com" width="1" height="1" alt="shown">
<img src="cid:held@example.com" width="1" height="1" alt="held">
</body></html>
--alt--
--rel
Content-Type: image/png; name="logo.png"
Content-ID: <shown@example.com>
Content-Transfer-Encoding: base64

iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGPgqjgBAAHaAUsQ+/EAAAAAAElFTkSuQmCC
--rel
Content-Type: image/png; name="banner.png"
Content-ID: <held@example.com>
Content-Transfer-Encoding: base64

iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4IScHAAK2AQU0pnWqAAAAAElFTkSuQmCC
--rel
Content-Type: image/png; name="spacer.png"
Content-ID: <unref@example.com>
Content-Transfer-Encoding: base64

iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGOQW2ADAAHaAPtcEaIXAAAAAElFTkSuQmCC
--rel--
EOF

DVID=$(docker create --name "$DV" --network "$NET" --network-alias dovecot "$DOVECOT_IMAGE")
docker cp "$WORK_HOST/dovecot.conf" "$DVID:/etc/dovecot/dovecot.conf"
docker cp "$WORK_HOST/dovecot.crt" "$DVID:/etc/dovecot/dovecot.crt"
docker cp "$WORK_HOST/dovecot.key" "$DVID:/etc/dovecot/dovecot.key"
docker start "$DVID" >/dev/null
# doveadm is a precondition for the `doveadm save` two lines down and NOTHING
# MORE: it is a local binary that reads the config and the mail store directly,
# so it answers before the master process has finished starting and it never
# opens a socket. Measured: it returned on its FIRST attempt 655ms after
# `docker start`, while dovecot's own "starting up for imap" log line had not
# been written yet.
wait_for "dovecot (doveadm, local)" 60 "$DV" \
  docker exec "$DV" doveadm mailbox list -u "$TESTUSER"

# ⚠ THIS is the readiness that matters, and the old check did not imply it.
#
# The app's ONE job here is to dial 993, verify the certificate against the CA
# bundle we hand it, and sync. Three separate things have to hold for that, and
# `doveadm mailbox list` proved none of them: that the TLS listener on 993 is
# accepting, that it is reachable at the network alias the app dials, and that
# the certificate it serves is the one we are about to trust. So this is a real
# handshake from a SEPARATE container on this network — the app's exact path —
# and it compares the served certificate byte-for-byte against $WORK/dovecot.crt
# rather than merely observing that a handshake happened.
#
# Written this way because a port probe would go green against a server holding
# any certificate at all, and a mismatch there surfaces as an "unknown
# authority" failure ten steps later, in the app, dressed up as a rendering
# result (#432). The check needs no openssl on the host — it runs in the dovecot
# image, which has one — and it reads our cert from stdin.
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

docker cp "$WORK_HOST/inline.eml" "$DV:/tmp/inline.eml"
docker exec "$DV" sh -c "doveadm save -u '$TESTUSER' -m INBOX < /tmp/inline.eml"
docker cp "$WORK_HOST/held.eml" "$DV:/tmp/held.eml"
docker exec "$DV" sh -c "doveadm save -u '$TESTUSER' -m INBOX < /tmp/held.eml"
echo "delivered the inline-image message and the held-inline message into $TESTUSER INBOX"

cat > "$WORK/accounts.json" <<EOF
{"accounts":[{"name":"dovecot","imap_host":"dovecot","imap_port":993,
  "smtp_host":"dovecot","smtp_port":465,
  "username":"$TESTUSER","password":"$TESTPASS"}]}
EOF

echo "--- mailroom (dev-auth, syncing the dovecot account)"
# ⚠ CREATED, FED, THEN STARTED — the app process must never exist without its CA
# bundle, and this used to `docker run -d` first and `docker cp` the two files in
# afterwards, then restart to pick them up.
#
# That lost a required check on CI twice on byte-identical trees (#432). The app
# reconciles the declared accounts file ~30ms into boot (main.go reconcileAccounts,
# right after the operator is provisioned) and dials IMAP to validate. If it got
# there in the window BETWEEN the two docker cps, it read accounts.json while
# SSL_CERT_FILE named a file that did not exist yet, so the root pool it built did
# not contain our CA and validation died with:
#
#   account stored but sync disabled: IMAP validation failed
#   err="… tls: failed to verify certificate: x509: certificate signed by unknown authority"
#
# And that LATCHES. reconcileAccounts is a reconcile, not a create: an account
# whose name already exists is left alone and its sync_enabled flag is not
# touched, so the restart that finally had the certificate never re-validated it.
# Every later pass logged accounts=0, nothing synced, nothing rendered, and the
# browser half never ran — which the suite then reported, correctly, as the
# tracker's silence being UNPROVEN.
#
# Whether the app won that race was pure timing: locally it reaches reconcile
# ~250ms BEFORE the first cp lands (measured: `docker run -d` returned 17.258,
# "http server listening" 17.292, cp#1 landed 17.581), so the file was simply
# absent and the later restart did the real work. A loaded runner only has to be
# a few hundred ms slower to boot for the same script to fail. Feeding a created
# container removes the window rather than making it smaller — there is no
# instant at which a mailroom process is running without both files.
APPID=$(docker create --name "$APP" --network "$NET" --network-alias mailroom \
  -v "$VOL:/k:ro" \
  -e MAILROOM_DATABASE_URL="postgres://mailroom_app:app@pg:5432/mailroom?sslmode=disable" \
  -e MAILROOM_MASTER_KEY_FILE=/k/master \
  -e MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS=1 \
  -e MAILROOM_DEV_AUTH_SUBJECT="operator" \
  -e MAILROOM_DEV_AUTH_I_UNDERSTAND="this-instance-has-no-authentication" \
  -e MAILROOM_DEV_AUTH_EMAIL="$TESTUSER" \
  -e MAILROOM_ACCOUNTS_FILE=/tmp/accounts.json \
  -e MAILROOM_SYNC_INTERVAL=2s \
  -e MAILROOM_SEARCH_DISABLED=1 \
  -e SSL_CERT_FILE=/tmp/dovecot.crt \
  -e MAILROOM_LOG_FORMAT=text \
  "$IMAGE")
docker cp "$WORK_HOST/accounts.json" "$APPID:/tmp/accounts.json"
docker cp "$WORK_HOST/dovecot.crt" "$APPID:/tmp/dovecot.crt"
docker start "$APPID" >/dev/null

wait_for "mailroom /readyz" 60 "$APP" \
  docker run --rm --network "$NET" "$CURL_IMAGE" -sf http://mailroom:8080/readyz

# ⚠ /readyz IS NOT EVIDENCE THAT THE ACCOUNT VALIDATED. The app serves every read
# path perfectly well with its one declared account stored sync-disabled — that is
# exactly the state #432 shipped into the browser stage, where it read as a
# rendering result. Boot onboarding logs one of two lines and never neither, so
# assert the positive one: a failure here is a harness/trust fault, and saying so
# at the point it happened costs one grep and saves reading the whole suite's
# output backwards.
#
# The loop waits for the LOG STREAM, not for validation to succeed — it ends on
# whichever of the two lines appears, so a genuine failure still falls straight
# through to the diagnosis below. It exists because `docker logs` reads a pipe the
# daemon drains asynchronously, and because reconciliation happening strictly
# before the listener starts is today's ordering, not a promise. A check that can
# be sampled a millisecond too early is a flake of exactly the kind this file is
# fixing.
_boot_i=0
while [ "$_boot_i" -lt 10 ]; do
  if docker logs "$APP" 2>&1 | grep -qE 'msg="account added"|sync disabled'; then break; fi
  _boot_i=$((_boot_i + 1)); sleep 1
done
if ! docker logs "$APP" 2>&1 | grep -q 'msg="account added"'; then
  echo "FATAL: mailroom did not validate the declared dovecot account, so no mail can sync" >&2
  echo "       and every assertion below would be about a message that never arrived." >&2
  echo "--- mailroom onboarding lines ---" >&2
  docker logs "$APP" 2>&1 | grep -iE "account|sync disabled" | head -20 >&2 || true
  echo "--- the certificate the app was given as SSL_CERT_FILE ---" >&2
  docker exec "$APP" sh -c 'wc -c < /tmp/dovecot.crt' >&2 2>&1 || echo "  (absent)" >&2
  exit 1
fi
echo "the declared account validated against dovecot's TLS certificate"

echo "--- quarantining the held inline part (#675)"
# A real detection needs clamd and a signature that matches (scripts/clamav-test.sh's
# territory). What the reading pane renders from is the STORED verdict, so the
# harness writes that verdict directly: `flagged` on part 3, the banner.png part
# (1 is the alternative, 2 the displayed logo, 4 the unreferenced spacer). The
# browser stage then checks, from the server's own Email/get, that THIS part is
# in `quarantined` before it believes anything it sees — so a wrong part path
# bails there rather than passing for the wrong reason. No scanner runs in this
# suite, so nothing re-scans the row behind our back.
HELD_ROW=""
_held_i=0
while [ "$_held_i" -lt 60 ]; do
  HELD_ROW=$(docker exec "$PG" psql -U postgres -d mailroom -qtA -v ON_ERROR_STOP=1 -c "
    UPDATE app.attachments a SET scan_status = 'flagged'
      FROM app.messages m
     WHERE m.id = a.message_id AND m.subject = 'Held inline image' AND a.part_path = '3'
    RETURNING a.part_path" 2>/dev/null | head -1)
  [ -n "$HELD_ROW" ] && break
  _held_i=$((_held_i + 1)); sleep 1
done
if [ -z "$HELD_ROW" ]; then
  echo "FATAL: the held-inline message never had its part 3 projected into app.attachments," >&2
  echo "       so the #675 stage would be asserting about a part nothing holds" >&2
  exit 1
fi
echo "part $HELD_ROW of the held-inline message is now flagged"

echo
echo "=== driving inline resolution + render from a real browser ================="
# ⚠ THE MESSAGE CSP IS READ OUT OF THE REPO, NOT TYPED IN HERE (#308 item 4).
#
# This suite's `csp = (remote) => […]` helper omitted `script-src '<reporter
# hash>'` — it was the PRE-#100 policy, so every conclusion here was drawn about
# a document this app has not rendered since the height reporter landed. The
# inline/remote findings still held, but "the document under test is the one we
# ship" did not, and nothing compared the two.
#
# scripts/e2e-policy.cjs now evaluates the REAL messageCSP() out of
# messageDocument.ts. Staged into a directory and copied into the container
# because it has no checkout (same reason as the certs above); `set -e` means a
# moved source stops the suite rather than silently testing a policy nobody
# ships.
mkdir -p "$WORK/policy-src"
cp scripts/e2e-policy.cjs "$WORK/policy-src/e2e-policy.cjs"
cp web/src/app/ui/messageDocument.ts "$WORK/policy-src/messageDocument.ts"
cp cmd/mailroom/main.go "$WORK/policy-src/main.go"
cp internal/api/blob.go "$WORK/policy-src/blob.go"

# node has global fetch (Node 18+) and puppeteer in this image; it is on the same
# private network, so it talks to mailroom:8080 exactly as the web app would. The
# script is docker cp'd in rather than passed with `node -e`, because it now
# requires the extractor and its sources from the container filesystem.
FAILFILE="$WORK/fails"; : > "$FAILFILE"
cat > "$WORK/inline-check.cjs" <<'NODEEOF'
  const puppeteer = require("puppeteer");
  const BASE = "http://mailroom:8080";
  const jmap = async (calls) => {
    const r = await fetch(BASE + "/jmap", { method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ using:["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"], methodCalls: calls }) });
    return r.json();
  };
  const expand = (tpl, vars) => tpl.replace(/\{(\w+)\}/g, (_,k)=>encodeURIComponent(vars[k]??""));
  // The REAL messageCSP() and the REAL height reporter, extracted from
  // web/src/app/ui/messageDocument.ts. A failure to read them throws, lands in
  // the catch below as ERR …, and reddens all four tokens — it cannot degrade
  // into "rendered under no policy at all", which would pass every assertion.
  const P = require("/tmp/policy-src/e2e-policy.cjs").extract("/tmp/policy-src");
  const csp = P.messageCSP;
  console.log("POLICY msg_csp=" + JSON.stringify(csp(false)));
  // The document the client actually builds: the CSP, plus the ONE hash-pinned
  // script the policy admits. Rendering without the reporter would test a
  // strictly more permissive document than we ship — `script-src` present but
  // nothing exercising it.
  const doc = (html, remote) => "<!DOCTYPE html><html><head><meta charset=utf-8>"
    + "<meta http-equiv=\"Content-Security-Policy\" content=\"" + csp(remote) + "\">"
    + "<meta name=\"referrer\" content=\"no-referrer\"></head><body>" + html
    + "<script>" + P.REPORTER_JS + "</script></body></html>";
  // The two client-side rewrites, verbatim from web/.
  const rewriteCid = (html, url) => html.replace(/ src="cid:([^"]*)"/gi, () => url ? " src=\"" + url + "\"" : "");
  const restoreRemote = (html) => html.replaceAll("data-mailroom-remote-src=\"", "src=\"").replaceAll("data-mailroom-remote-srcset=\"", "srcset=\"");

  (async () => {
    // 1. session: the download template + the real accountId.
    const sess = await (await fetch(BASE + "/jmap/session")).json();
    const acct = sess.primaryAccounts["urn:ietf:params:jmap:mail"] || Object.keys(sess.accounts)[0];
    const dtpl = sess.downloadUrl;

    // 2. wait for the message to sync, then fetch its body the way the reading pane does.
    // BY SUBJECT, not ids[0]: the mailbox holds #675's message too, and which
    // one sorts first is a fact about dates, not about this assertion.
    let email = null;
    for (let i = 0; i < 40 && !email; i++) {
      const q = await jmap([["Email/query", { accountId: acct }, "0"]]);
      const ids = q.methodResponses?.[0]?.[1]?.ids || [];
      if (ids.length) {
        const g = await jmap([["Email/get", { accountId: acct, ids,
          properties:["subject","htmlBody","attachments","bodyValues"], fetchHTMLBodyValues: true }, "1"]]);
        email = (g.methodResponses?.[0]?.[1]?.list || []).find(e => e.subject === "Your weekly digest") || null;
      }
      if (!email) await new Promise(r=>setTimeout(r,2000));
    }
    if (!email) { console.log("NOSYNC"); process.exit(3); }

    const htmlPart = (email.htmlBody||[]).find(p=>p.type==="text/html");
    const html = email.bodyValues[htmlPart.partId].value;
    if (!/ src="cid:logo@example.com"/.test(html)) { console.log("NOCID " + html); process.exit(4); }
    if (!/data-mailroom-blocked/.test(html)) { console.log("NOTPARKED " + html); process.exit(4); }

    // 3. resolve cid -> data: URL by fetching the inline part from OUR download endpoint.
    const img = (email.htmlBody||[]).concat(email.attachments||[])
      .find(p=>p.cid && /^image\//.test(p.type||"") && p.blobId);
    if (!img) { console.log("NOIMGPART"); process.exit(5); }
    const path = expand(dtpl, { accountId: acct, blobId: img.blobId, name: "inline", type: "" });
    const dl = /^https?:\/\//.test(path) ? path : BASE + path;
    const res = await fetch(dl);
    const ct = (res.headers.get("content-type")||"").split(";")[0].trim();
    if (!/^image\/(png|jpeg|gif|webp|bmp|avif|tiff|heic)$/.test(ct)) { console.log("BADTYPE " + ct); process.exit(6); }
    const b64 = Buffer.from(await res.arrayBuffer()).toString("base64");
    const dataUrl = "data:" + ct + ";base64," + b64;

    const resolvedHtml = rewriteCid(html, dataUrl);

    const browser = await puppeteer.launch({ args:["--no-sandbox","--disable-setuid-sandbox"] });
    const render = async (documentHtml) => {
      const page = await browser.newPage();
      let tracker = [], dialogs = 0;
      page.on("dialog", async d => { dialogs++; await d.dismiss(); });
      await page.setRequestInterception(true);
      page.on("request", req => {
        // Never actually let a tracker request out: record the attempt and abort,
        // so the tracker container stays at zero bytes even in the allowRemote run.
        if (/tracker\.evil\.example|evil\.example/.test(req.url())) { tracker.push(req.url()); req.abort(); }
        else req.continue();
      });
      await page.setContent(documentHtml, { waitUntil:"networkidle0", timeout:15000 });
      await new Promise(r=>setTimeout(r,800));
      // `src` is the ATTRIBUTE, not the resolved property: this document has no
      // base URL, so a same-origin proxy reference cannot actually load here and
      // naturalWidth alone could not tell "the opt-in produced a loadable
      // reference" from "the opt-in produced nothing" (ADR-026).
      const imgs = await page.evaluate(() => [...document.images].map(i => ({
        alt: i.alt, w: i.naturalWidth, src: i.getAttribute("src") || ""
      })));
      await page.close();
      return { imgs, tracker, dialogs };
    };

    // Default render: inline must decode; remote must not even be requested.
    const def = await render(doc(resolvedHtml, false));
    const logo = def.imgs.find(i=>i.alt==="logo");
    const inlineOK = logo && logo.w > 0;
    const remoteBlocked = def.tracker.length === 0 && def.dialogs === 0;

    // allowRemote render: the opt-in must STILL attempt the ordinary remote
    // image — and must NOT attempt the tracking pixel (#55 increment 2).
    //
    // ⚠ THE MESSAGE NOW CARRIES BOTH ON PURPOSE. It used to carry only a 1x1,
    // which meant "the opt-in still fetches" and "the beacon is suppressed" could
    // not both be tested: suppressing the only remote image would have looked
    // identical to the opt-in being broken. With a 600x200 image beside it, the
    // two are independently observable, and this is the only place either claim
    // is checked in a real browser rather than by a string assertion.
    const opened = await render(doc(restoreRemote(resolvedHtml), true));
    const logo2 = opened.imgs.find(i=>i.alt==="logo");
    // ⚠ WITH THE IMAGE PROXY ON (ADR-026) THE BROWSER MUST NOT REACH THE SENDER
    // AT ALL. The server parks a same-origin /jmap/proxy/... reference INSTEAD of
    // the sender's URL, so "the opt-in works" is now "the browser attempted the
    // PROXY", not "the browser attempted hero.png".
    //
    // This assertion used to be `tracker.some(hero.png)` and it went red on the
    // very commit that made the proxy the fetcher — correctly, because the
    // behaviour it described had changed. Asserting the OLD shape would now be
    // asserting that the proxy does not work.
    const proxied = /data-mailroom-remote-src="\/jmap\/proxy\//.test(resolvedHtml);
    // ⚠ alt="remote", NOT "hero". Only the URL says hero.png; the element is
    // alt="remote" (line ~219). The first version of this looked for "hero" and
    // could therefore NEVER have passed — an assertion that cannot observe its
    // subject, which is the vacuous-test problem inverted and just as useless.
    const hero = opened.imgs.find(i => i.alt === "remote");
    // ⚠ THE POSITIVE HAS TO REMAIN OBSERVABLE, or the beacon assertion below
    // goes vacuous — as this script's own matched-pair comment warns. With the
    // proxy on, this document has no base URL, so a same-origin proxy reference
    // cannot actually load here; what IS checkable is that the opt-in produced a
    // loadable proxy reference rather than nothing at all.
    //
    // This used to assert hero.src started with /jmap/proxy/. Since v1.81.0 the
    // app hands the frame a data: URL, so that assertion was stale; whether the
    // proxied image actually decodes in the frame is the proxied-images lane's
    // job (scripts/proxied-images-e2e.sh). Here we only require the element.
    const remoteAttempted = proxied
      ? !!hero
      : opened.tracker.some(u => /hero\.png/.test(u));
    // Either way the SENDER must never be contacted by the browser.
    const senderContactedByBrowser = opened.tracker.some(u => /hero\.png/.test(u));
    const beaconAttempted = opened.tracker.some(u => /pixel\.gif/.test(u));

    // ---- 4. #675: a HELD inline image, in the REAL reading pane --------------
    // Everything above renders a document this script assembled. This stage
    // opens the app itself, because what is under test is the app's own list
    // of parts and its own fetch decisions.
    const USING_ALL = Object.keys(sess.capabilities || {});
    const jmapAll = async (calls) => (await fetch(BASE + "/jmap", { method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ using: USING_ALL, methodCalls: calls }) })).json();
    const q2 = await jmapAll([["Email/query", { accountId: acct }, "q"]]);
    const g2 = await jmapAll([["Email/get", { accountId: acct, ids: q2.methodResponses?.[0]?.[1]?.ids || [],
      properties: ["subject", "htmlBody", "attachments", "attachmentScan"] }, "g"]]);
    const heldMsg = (g2.methodResponses?.[0]?.[1]?.list || []).find(e => e.subject === "Held inline image");
    if (!heldMsg) { console.log("NOHELDMSG"); process.exit(7); }
    const byCid = (c) => (heldMsg.htmlBody || []).find(p => (p.cid || "").toLowerCase().indexOf(c) >= 0);
    const heldPart = byCid("held@"), shownPart = byCid("shown@"), unrefPart = byCid("unref@");
    // PREMISES. The fixture's shape is the whole test: the held and unreferenced
    // images must be BODY parts (in htmlBody, absent from attachments) — the
    // exact place #675 lost them — and the held one must really be held.
    if (!heldPart || !shownPart || !unrefPart) { console.log("NOHELDSHAPE " + JSON.stringify(heldMsg.htmlBody)); process.exit(7); }
    if ((heldMsg.attachments || []).some(p => p.partId === heldPart.partId || p.partId === unrefPart.partId)) {
      console.log("NOHELDSHAPE the parts are already attachments: " + JSON.stringify(heldMsg.attachments)); process.exit(7);
    }
    if (!(heldMsg.attachmentScan && (heldMsg.attachmentScan.quarantined || []).indexOf(heldPart.partId) >= 0)) {
      console.log("NOTHELD " + heldPart.partId + " " + JSON.stringify(heldMsg.attachmentScan)); process.exit(7);
    }
    // Setup complete, as "Skip setup" does it, or a first open lands on the
    // day-one wizard and no message is drawn (proxied-images-e2e.sh does the same).
    const dp0 = await jmapAll([["DeskPreferences/get", { accountId: acct, ids: null }, "dp"]]);
    const dpId = ((dp0.methodResponses?.[0]?.[1]?.list || [])[0] || {}).id || acct;
    await jmapAll([["DeskPreferences/set", { accountId: acct,
      update: { [dpId]: { setupCompletedAt: new Date().toISOString() } } }, "dps"]]);

    const pane = await browser.newPage();
    const asked = [];
    pane.on("request", (req) => asked.push(req.url()));
    await pane.goto(BASE + "/#/all?m=" + heldMsg.id, { waitUntil: "domcontentloaded", timeout: 30000 });
    await pane.waitForSelector("iframe.message-html", { timeout: 45000 }).catch(() => null);
    // The positive control, and the clock for everything else: wait until the
    // DISPLAYED inline image has decoded inside the real frame. Until then a
    // missing chip or an unasked-for blob proves nothing.
    let shownRendered = false, heldImg = null;
    for (let i = 0; i < 60 && !shownRendered; i++) {
      const h = await pane.$("iframe.message-html");
      const f = h ? await h.contentFrame() : null;
      const st = f ? await f.evaluate(() => [...document.images].map(im => ({
        alt: im.alt, w: im.naturalWidth, src: im.getAttribute("src") || "" }))).catch(() => null) : null;
      if (st) {
        const s = st.find(im => im.alt === "shown");
        heldImg = st.find(im => im.alt === "held") || null;
        shownRendered = !!(s && s.w > 0);
      }
      if (!shownRendered) await new Promise(r => setTimeout(r, 500));
    }
    await pane.waitForSelector(".attachment-withheld", { timeout: 10000 }).catch(() => null);
    const chips = await pane.$$eval(".attachment-withheld", els => els.map(e => ({
      label: e.getAttribute("aria-label") || "",
      buttons: [...e.querySelectorAll("button")].map(b => (b.textContent || "").trim()),
      links: e.querySelectorAll("a").length })));
    const files = await pane.$$eval(".attachments .attachment-name", els => els.map(e => (e.textContent || "").trim()));
    const askedFor = (blobId) => asked.some(u => u.indexOf(blobId) >= 0 || u.indexOf(encodeURIComponent(blobId)) >= 0);
    const heldChip = chips.find(c => c.label.indexOf("banner.png") >= 0);
    await pane.close();

    await browser.close();
    console.log("HELD_SHOWN_RENDERED=" + shownRendered);
    console.log("HELD_SHOWN_FETCHED=" + askedFor(shownPart.blobId));
    console.log("HELD_CHIP=" + !!heldChip);
    console.log("HELD_RELEASE_OFFERED=" + !!(heldChip && heldChip.buttons.indexOf("Release") >= 0 && heldChip.links === 0));
    console.log("HELD_NEVER_FETCHED=" + !askedFor(heldPart.blobId));
    console.log("HELD_NOT_RENDERED=" + !!(heldImg && heldImg.w === 0 && heldImg.src === ""));
    console.log("UNREF_LISTED=" + (files.indexOf("spacer.png") >= 0));
    console.log("INLINE_OK=" + !!inlineOK);
    console.log("REMOTE_BLOCKED_DEFAULT=" + !!remoteBlocked);
    console.log("INLINE_OK_WITH_REMOTE=" + !!(logo2 && logo2.w > 0));
    console.log("REMOTE_ATTEMPTED_WHEN_ALLOWED=" + !!remoteAttempted);
    console.log("PROXIED=" + !!proxied);
    console.log("SENDER_CONTACTED_BY_BROWSER=" + !!senderContactedByBrowser);
    console.log("BEACON_ATTEMPTED_WHEN_ALLOWED=" + !!beaconAttempted);
    console.log("DONE");
  })().catch(e => { console.log("ERR " + e.message); process.exit(2); });
NODEEOF

inline_check() {
  cid=$(docker create --network "$NET" \
    -e NODE_PATH=/usr/src/app/node_modules --entrypoint node "$CHROME_IMAGE" /tmp/inline-check.cjs)
  docker cp "$WORK_HOST/inline-check.cjs" "$cid:/tmp/inline-check.cjs" >/dev/null
  docker cp "$WORK_HOST/policy-src" "$cid:/tmp/policy-src" >/dev/null
  docker start -a "$cid" 2>&1
  docker rm -fv "$cid" >/dev/null 2>&1
}
BROWSER_OUT=$(inline_check || true)

echo "$BROWSER_OUT"
pass() { echo "ok    $1"; }
fail() { echo "FAIL  $1"; echo "1" >> "$FAILFILE"; }
# ⚠ A BAIL-OUT IS ONE FAILURE, NOT FOUR — AND ONE OF THE FOUR READS AS A
# SECURITY REGRESSION THAT DID NOT HAPPEN.
#
# The render script exits early at its own guards (NOSYNC / NOCID / NOTPARKED)
# when the message never arrived or never had the shape this suite is about. The
# four assertions below then all miss their markers and print four diagnoses,
# every one of them wrong — including "remote content leaked by default", which
# is the most alarming sentence this suite can emit. A real run produced exactly
# that: the message failed to sync in CI, and the log said the tracker had been
# let through when nothing had rendered at all.
#
# This is the same conflation #284 fixed for the tracker check below, one block
# higher up: an assertion that cannot distinguish "the thing under test failed"
# from "the thing under test never ran" is not reporting a result. The run still
# FAILS — bailing out is never a pass — it now says why once, accurately.
if printf '%s' "$BROWSER_OUT" | grep -q "^DONE"; then
  printf '%s' "$BROWSER_OUT" | grep -q "INLINE_OK=true"                    && pass "the cid inline image decoded and rendered" || fail "the inline image did not render"
  printf '%s' "$BROWSER_OUT" | grep -q "REMOTE_BLOCKED_DEFAULT=true"       && pass "no remote request and no script by default"  || fail "remote content leaked by default"
  printf '%s' "$BROWSER_OUT" | grep -q "INLINE_OK_WITH_REMOTE=true"        && pass "inline still renders once remote is allowed"  || fail "inline broke under allowRemote"
  printf '%s' "$BROWSER_OUT" | grep -q "REMOTE_ATTEMPTED_WHEN_ALLOWED=true" && pass "Show-remote-content still loads the picture" || fail "the remote opt-in no longer works"
  # ⚠ THE PROXY'S OWN CLAIM (ADR-026), and the reason the assertion above changed
  # shape. With the proxy on, allowing remote content must load the picture
  # WITHOUT the browser ever touching the sender: the server fetches it. This line
  # is what makes "the opt-in works" and "the sender still cannot see you" two
  # separate, independently observable facts rather than one.
  printf '%s' "$BROWSER_OUT" | grep -q "SENDER_CONTACTED_BY_BROWSER=false" && pass "the browser never contacted the sender, even with remote content allowed" || fail "the browser fetched straight from the sender — the proxy was bypassed"
  # ⚠ THE TWO ABOVE AND BELOW ARE A MATCHED PAIR, and neither is worth anything
  # alone. Together they say: allowing remote content loads the picture and does
  # NOT load the beacon (#55 increment 2). Without the line above, this one would
  # also pass if the opt-in were simply broken and nothing loaded at all.
  printf '%s' "$BROWSER_OUT" | grep -q "BEACON_ATTEMPTED_WHEN_ALLOWED=false" && pass "the 1x1 tracking pixel was NOT fetched even with remote content allowed" || fail "a tracking pixel was fetched once remote content was allowed — the suppression is not working"
  # #675. The first two are the positive control: the app really opened the
  # message, really fetched and decoded its displayed inline image. Without
  # them, "never fetched" and "no chip" could both be a page that never loaded.
  printf '%s' "$BROWSER_OUT" | grep -q "HELD_SHOWN_RENDERED=true" && pass "the app rendered the displayed inline image beside the held one" || fail "the displayed inline image never rendered in the app — the #675 checks below observed nothing"
  printf '%s' "$BROWSER_OUT" | grep -q "HELD_SHOWN_FETCHED=true"  && pass "the app fetched the displayed inline image" || fail "the app never asked for the displayed inline image"
  printf '%s' "$BROWSER_OUT" | grep -q "HELD_CHIP=true"           && pass "the held inline image has a withheld chip (#675)" || fail "the held inline image has NO withheld chip — it vanished without notice (#675)"
  printf '%s' "$BROWSER_OUT" | grep -q "HELD_RELEASE_OFFERED=true" && pass "the chip offers Release and no download link" || fail "the held inline image's chip has no release action, or links to its bytes"
  printf '%s' "$BROWSER_OUT" | grep -q "HELD_NEVER_FETCHED=true"  && pass "the held inline image's bytes were never requested" || fail "the app requested the held inline image's bytes"
  printf '%s' "$BROWSER_OUT" | grep -q "HELD_NOT_RENDERED=true"   && pass "the held inline image did not render in the frame" || fail "the held inline image rendered, or kept a src, in the message frame"
  printf '%s' "$BROWSER_OUT" | grep -q "UNREF_LISTED=true"        && pass "an image part nothing references is listed as a file (#675)" || fail "an image part whose Content-ID the HTML never references appears nowhere"
else
  case "$BROWSER_OUT" in
    *NOSYNC*)
      fail "the message never reached Mailroom, so NOTHING below was exercised — this is a
      harness/sync failure, not a rendering or remote-content result. Check the
      mailroom log for account provisioning: 'accounts=0' in every sync pass means
      the configured account was never registered" ;;
    *NOCID*)
      fail "the synced message carried no cid: reference, so inline resolution was never
      exercised — the fixture or the delivery is wrong, not the renderer" ;;
    *NOTPARKED*)
      fail "the synced message carried no data-mailroom-blocked marker, so the browser
      stage bailed before rendering — remote parking is what broke, and every
      inline finding below would have been vacuous" ;;
    *NOHELDMSG*|*NOHELDSHAPE*|*NOTHELD*)
      fail "the #675 stage bailed on its premises (the held-inline message is missing,
      has the wrong shape, or its part is not in quarantined) — nothing about the
      reading pane was established; the marker line above says which" ;;
    *)
      fail "the browser stage did not reach DONE and printed no known bail-out marker;
      its output is above and nothing here has been established" ;;
  esac
fi

echo
echo "--- THE TRACKER: it must never have been contacted"
# ⚠ AN EMPTY HIT LOG HAS TWO CAUSES, AND THIS USED TO CONFLATE THEM (#284).
#
# Either the browser rendered a message carrying a live tracking pixel and
# correctly refused to fetch it — which is what this suite exists to prove — or
# NO RENDER EVER HAPPENED, so nothing could have leaked and the silence means
# nothing. The second is not hypothetical: #280's probe produced exactly it.
# With remote parking disabled the browser check bails at its NOTPARKED guard
# before rendering, and this line printed "ok" inside a FAILING run.
#
# Note this differs from hostile-content's equivalent, which runs BEFORE its
# browser stage and claims only "the server fetched nothing remote" — a scoped
# claim it fully establishes. This check sits AFTER the render, so it speaks for
# the server and the browser both, and it may only say so once the browser has
# actually finished. The render script prints DONE as its last line and never
# reaches it if it throws.
HITS=$(docker exec "$TRK" sh -c 'wc -c < /tmp/hits' 2>/dev/null | tr -d ' \r\n' || echo "?")
if [ "$HITS" != "0" ]; then
  echo "FAIL  the tracker was contacted ($HITS bytes):"; docker exec "$TRK" sh -c 'cat /tmp/hits' | head -20
  echo "1" >> "$FAILFILE"
elif printf '%s' "$BROWSER_OUT" | grep -q "^DONE"; then
  pass "the tracker received ZERO bytes — neither the server nor the browser fetched it"
else
  fail "the tracker's silence is UNPROVEN: the server fetched nothing, but no render completed, so the browser half was never exercised"
fi

FAILS=$(wc -l < "$FAILFILE" 2>/dev/null | tr -d ' \r\n'); FAILS=${FAILS:-0}
echo
if [ "$FAILS" != "0" ]; then
  # ⚠ THE HEAD AS WELL AS THE TAIL. This dumped only the last 30 lines, which on
  # a sync failure are 30 identical "sync pass complete accounts=0" records — the
  # symptom repeated, with the startup lines that say WHY (account provisioning,
  # OIDC/dev-auth, the accounts file) already scrolled off. Triaging a real CI
  # failure meant reading a log that had thrown away its own explanation.
  echo "=== mailroom logs (startup) ==="; docker logs "$APP" 2>&1 | head -25
  echo "=== mailroom logs (latest) ==="; docker logs "$APP" 2>&1 | tail -20
  echo; echo "INLINE-IMAGE E2E FAILED ($FAILS assertion(s))"; exit 1
fi
echo "=== INLINE IMAGES RENDER; REMOTE STAYS BLOCKED — END TO END ==="
