#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Proxied remote images, proven end to end in a real browser, against the REAL
# image, behind a REAL OIDC login (#667). Sibling of
# scripts/inline-images-e2e.sh and scripts/gate-e2e.sh; it exists because the gap
# BETWEEN those two is where #667 lived.
#
# With the image proxy on (ADR-026) the server parks a same-origin path,
# /jmap/proxy/{account}/{message}/{token}, in place of every remote image URL.
# That route is gated by the session cookie (HttpOnly, SameSite=Lax). The message
# body renders in a srcdoc iframe sandboxed WITHOUT allow-same-origin, so its
# origin is opaque and the cookie is withheld from every request the frame makes.
# Left for the frame to fetch, every proxied image was a 401 in Chromium and in
# Firefox on a cookie-gated deployment (other browsers were not tested).
#
# No lane saw it, and the reason is structural rather than an oversight in any
# one of them:
#
#   * the browser lanes (hostile-content, inline-images, a11y, tabs) run with
#     dev-auth, which has NO credential to withhold — the frame's request is
#     answered like anyone's;
#   * the one OIDC lane (gate-e2e.sh) drives the flow with curl, which has no
#     frame, no sandbox and no SameSite.
#
# So this lane is the conjunction: a real headless Chromium, signed in through
# the real Authorization-Code flow at the provider's own login form, holding a
# real session cookie, reading a real message in the real web client.
#
# What it proves, in ONE run, per assertion:
#
#   1. Sign-in happened IN THE BROWSER and left an HttpOnly SameSite=Lax cookie.
#   2. ONE-OFF: a message with an ordinary remote image is blocked by default;
#      after "Show remote content", INSIDE the sandboxed message frame the image
#      has decoded, its src is a data:image/ URL, and nothing in the frame
#      references /jmap/proxy/ (the app fetched it; the frame fetched nothing).
#   3. THE NEGATIVE CONTROL — the pre-fix behaviour, reproduced on purpose. A
#      frame with the same sandbox tokens is handed the same proxy path as an
#      <img src>. It must NOT decode, while the same path fetched by the parent
#      with credentials is a 200 image, and the SAME BYTES as a data: URL decode
#      in that same frame. If the frame ever does load it, the lane FAILS and
#      says its premise is gone: it could no longer tell the fix from its absence.
#      The control also has to show the request LEFT the browser and came back
#      refused (the raw status is a 401, and Chromium reports the session cookie
#      as withheld for SameSiteLax) — otherwise "did not decode" could be a
#      content policy stopping it first, which is #460 and not this.
#   4. REMEMBERED GRANT: "Always allow this sender" on one message, then a fresh
#      page on a DIFFERENT message from that sender, which nobody clicked on —
#      the same three in-frame assertions, with no click.
#   5. The sender's host was contacted by the SERVER only, never by the browser.
#
# ⚠ THE EGRESS GUARD IS NOT WEAKENED IN ANY SHIPPED BUILD. internal/egress
# refuses every private, loopback and documentation range, and the image proxy
# has no runtime knob (MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS covers IMAP/SMTP
# dials only), so an image host on an ordinary Docker network is refused with a
# 403. This lane therefore builds its OWN image with -tags e2eprivateimages
# (Dockerfile build arg MAILROOM_EXTRA_TAGS), which compiles in
# internal/api/proxy_e2e_private.go: an exemption for RFC 1918 IPv4 only, never
# loopback or link-local. The release image is built with no tags and contains
# no such switch (TestProxyEgressHasNoExemptionInShippedBuild). The network uses
# a private subnet; the proxy still does a GENUINE guarded fetch of the image.
# (It used to sit in 192.88.99.0/24, a block the guard admitted by omission;
# #676 made the guard refuse it.) If the image is built WITHOUT the tag this
# lane goes red at the control's "parent fetch is a 200 image" line, by name.
# PROXIED_E2E_SUBNET overrides the subnet; MAILROOM_IMAGE supplies a prebuilt
# image, which must have been built with the tag.
#
# PROBED (#62's rule: a suite that passes first time has proved nothing yet). The
# image was rebuilt with messageFrameDocument returning the pre-fix document —
# messageDocument(html, allowRemote, origin), unconditionally. Exactly the six
# in-frame lines went red, each reporting naturalWidth=0 and a src that was
# still the /jmap/proxy/ path, while all five control lines stayed green.
#
# Same container-only shape as the sibling suites: no published ports, no host
# toolchain, no source bind mounts (everything is `docker cp`d, because a bind
# mount does not resolve when the CI job is itself a container with a
# socket-mounted Docker — #280).
#
# Usage:  scripts/proxied-images-e2e.sh
#         MAILROOM_IMAGE=mailroom:test scripts/proxied-images-e2e.sh
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

IMAGE="${MAILROOM_IMAGE:-mailroom:proxied-e2e-$UNIQ}"
PG_IMAGE="${PG_IMAGE:-postgres:18}"
DOVECOT_IMAGE="${DOVECOT_IMAGE:-dovecot/dovecot:2.3.21}"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.10.1}"
OIDC_IMAGE="${OIDC_IMAGE:-ghcr.io/navikt/mock-oauth2-server:2.1.10}"
# A unique NAME is not enough for this lane: it also asks for a SUBNET, and two
# concurrent runs asking for the same one collide on the address range even with
# different network names (#502). So unless PROXIED_E2E_SUBNET pins one, the
# network step below picks 10.250.N.0/24 at random and tries again if Docker
# refuses it. Any RFC 1918 range satisfies the e2eprivateimages exemption.
SUBNET="${PROXIED_E2E_SUBNET:-}"
# The browser is pinned by digest in ONE shared file (#254, #447); see it for why.
. "$(dirname "$0")/e2e-chrome-image.env"

NET="mailroom-proxied-e2e-$UNIQ"
VOL="mailroom-proxied-e2e-key-$UNIQ"
PG="mailroom-proxied-e2e-pg-$UNIQ"
DV="mailroom-proxied-e2e-dovecot-$UNIQ"
OIDC="mailroom-proxied-e2e-oidc-$UNIQ"
APP="mailroom-proxied-e2e-app-$UNIQ"
IMGH="mailroom-proxied-e2e-images-$UNIQ"
CHK="mailroom-proxied-e2e-browser-$UNIQ"
CERTGEN="mailroom-proxied-e2e-certgen-$UNIQ"

TESTUSER="reader@example.com"
TESTPASS="reader-mailbox-password"
# The OIDC subject the browser signs in as. It is ALSO the declared operator, so
# the mailbox the accounts file provisions at boot belongs to the identity the
# browser ends up holding a cookie for.
SUBJECT="reader"

# How many PASS/FAIL lines the browser stage must print. A stage that stops
# early, or one an edit quietly removed an assertion from, is a failure — the
# count is checked below, not just the absence of a FAIL.
EXPECTED_CHECKS=17

cd "$(dirname "$0")/.."
# Scratch lives OUTSIDE the tree: it holds a throwaway TLS key, and nothing in
# .gitignore covers this suite's name.
WORK="$(mktemp -d "${TMPDIR:-/tmp}/mailroom-proxied-e2e.XXXXXX")"
WORK_HOST="$(cd "$WORK" && { pwd -W 2>/dev/null || pwd; })"

cleanup() {
  echo "--- cleaning up"
  docker rm -fv "$CHK" "$APP" "$DV" "$OIDC" "$PG" "$IMGH" "$CERTGEN" >/dev/null 2>&1 || true
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
# the failure surfaces later as something unrelated (#135, ported from
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

# ⚠ THE UI STRINGS AND THE SANDBOX TOKENS ARE READ OUT OF THE REPO, NOT TYPED IN
# HERE (the #308 lesson). The buttons are found by their visible text and the
# control frame is built with the client's own sandbox attribute; a copy of
# either in this file would keep "passing" against a client that had moved on —
# in the control's case, against a sandbox nobody ships. `set -e` plus the guard
# below means a moved source stops the suite instead.
SHOW_REMOTE="$(sed -n 's/^[[:space:]]*"thread\.showRemote":[[:space:]]*"\(.*\)",[[:space:]]*$/\1/p' web/src/app/i18n.ts | head -1)"
ALWAYS_ALLOW="$(sed -n 's/^[[:space:]]*"thread\.alwaysAllowSender":[[:space:]]*"\(.*\)",[[:space:]]*$/\1/p' web/src/app/i18n.ts | head -1)"
MESSAGE_SANDBOX="$(sed -n 's/^export const MESSAGE_SANDBOX = "\(.*\)";[[:space:]]*$/\1/p' web/src/app/ui/messageDocument.ts | head -1)"
if [ -z "$SHOW_REMOTE" ] || [ -z "$ALWAYS_ALLOW" ] || [ -z "$MESSAGE_SANDBOX" ]; then
  echo "FATAL: could not read the client's own strings out of web/src:" >&2
  echo "       thread.showRemote='$SHOW_REMOTE' thread.alwaysAllowSender='$ALWAYS_ALLOW'" >&2
  echo "       MESSAGE_SANDBOX='$MESSAGE_SANDBOX'" >&2
  exit 1
fi
case " $MESSAGE_SANDBOX " in
  *" allow-same-origin "*)
    echo "FATAL: MESSAGE_SANDBOX now carries allow-same-origin. The message frame is no" >&2
    echo "       longer an opaque origin, so this lane's whole premise has changed —" >&2
    echo "       and so has docs/hostile-content.md's. Do not paper over this here." >&2
    exit 1 ;;
esac
echo "client strings: showRemote='$SHOW_REMOTE' alwaysAllowSender='$ALWAYS_ALLOW'"
echo "client sandbox: '$MESSAGE_SANDBOX'"

if [ -z "${MAILROOM_IMAGE:-}" ]; then
  echo "--- building $IMAGE"
  docker build --build-arg MAILROOM_EXTRA_TAGS="${PROXIED_E2E_TAGS-e2eprivateimages}" -t "$IMAGE" .
fi

if [ -n "$SUBNET" ]; then
  docker network create --subnet "$SUBNET" "$NET" >/dev/null
else
  tries=0
  while :; do
    SUBNET="10.250.$(( $(od -An -N2 -tu2 < /dev/urandom | tr -dc '0-9') % 200 + 20 )).0/24"
    docker network create --subnet "$SUBNET" "$NET" >/dev/null 2>&1 && break
    tries=$((tries + 1))
    if [ "$tries" -ge 10 ]; then
      echo "FAIL could not create $NET on a free 10.250.N.0/24 after 10 tries" >&2
      exit 1
    fi
  done
fi
echo "--- network ($SUBNET) + master key"
docker volume create "$VOL" >/dev/null
docker run --rm -v "$VOL:/k" "$PG_IMAGE" sh -c \
  'openssl rand -hex 32 | tr -d "\r\n" > /k/master && chmod 644 /k/master' >/dev/null
SESSION_KEY="$(docker run --rm "$PG_IMAGE" sh -c 'openssl rand -hex 16' | tr -d '\r\n')"
[ -n "$SESSION_KEY" ] || { echo "FATAL: could not generate a session key" >&2; exit 1; }

echo "--- the sender's image host (a real 64x48 PNG; it records who asked)"
# A real, non-trivial PNG built in the container: 1x1 would be a tracking pixel
# by the shipped rule and would never be proxied at all. Every request is logged
# with the address it came from, so "the server fetched it, the browser never
# did" is read off the sender's own access log rather than inferred.
cat > "$WORK/image-host.cjs" <<'NODEEOF'
"use strict";
const http = require("http");
const zlib = require("zlib");
function crc32(buf) {
  let crc = 0xffffffff;
  for (let n = 0; n < buf.length; n++) {
    crc ^= buf[n];
    for (let k = 0; k < 8; k++) crc = (crc >>> 1) ^ (0xedb88320 & -(crc & 1));
  }
  return (~crc) >>> 0;
}
function chunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const td = Buffer.concat([Buffer.from(type, "ascii"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(td));
  return Buffer.concat([len, td, crc]);
}
function png(w, h, rgb) {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(w, 0);
  ihdr.writeUInt32BE(h, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // truecolour
  const row = Buffer.alloc(1 + w * 3);
  for (let x = 0; x < w; x++) {
    row[1 + x * 3] = rgb[0];
    row[2 + x * 3] = rgb[1];
    row[3 + x * 3] = rgb[2];
  }
  const raw = Buffer.concat(Array.from({ length: h }, () => row));
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", ihdr),
    chunk("IDAT", zlib.deflateSync(raw)),
    chunk("IEND", Buffer.alloc(0)),
  ]);
}
const IMG = png(64, 48, [0x2b, 0x6c, 0xb0]);
const hits = [];
http
  .createServer((req, res) => {
    // The two control paths are the harness talking to itself and are not
    // requests for an image, so they are not hits.
    if (req.url === "/__ready") {
      res.writeHead(200, { "Content-Type": "text/plain" });
      return res.end("ok");
    }
    if (req.url === "/__hits") {
      res.writeHead(200, { "Content-Type": "application/json" });
      return res.end(JSON.stringify(hits));
    }
    hits.push({
      ip: String(req.socket.remoteAddress || "").replace(/^::ffff:/, ""),
      url: req.url,
      ua: req.headers["user-agent"] || "",
    });
    if (/\.png$/.test(req.url)) {
      res.writeHead(200, { "Content-Type": "image/png", "Content-Length": IMG.length });
      return res.end(IMG);
    }
    res.writeHead(404);
    res.end();
  })
  .listen(8099);
NODEEOF
docker create --name "$IMGH" --network "$NET" --network-alias images.sender.example \
  --entrypoint node "$CHROME_IMAGE" /tmp/image-host.cjs >/dev/null
docker cp "$WORK_HOST/image-host.cjs" "$IMGH:/tmp/image-host.cjs" >/dev/null
docker start "$IMGH" >/dev/null
# Through the network alias the app will resolve, from a separate container — so
# "ready" means "reachable at the name in the mail", not "the process started".
wait_for "the sender's image host" 60 "$IMGH" \
  docker run --rm --network "$NET" "$CURL_IMAGE" -sf http://images.sender.example:8099/__ready

echo "--- postgres"
docker run -d --name "$PG" --network "$NET" --network-alias pg \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=mailroom "$PG_IMAGE" >/dev/null
# -h 127.0.0.1 forces TCP: the postgres image's first-init temp server listens on
# the Unix socket ONLY, so a socket-based check reports "ready" before
# POSTGRES_DB=mailroom exists (#135).
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

echo "--- oidc provider ($OIDC_IMAGE)"
# interactiveLogin gives a real login FORM: the username typed in becomes the
# `sub`, and the `claims` textarea is merged into the issued token. gate-e2e.sh
# posts that form with curl; here a browser fills it in and submits it.
docker run -d --name "$OIDC" --network "$NET" --network-alias oidc \
  -e JSON_CONFIG='{"interactiveLogin":true}' "$OIDC_IMAGE" >/dev/null
wait_for "oidc discovery" 60 "$OIDC" \
  docker run --rm --network "$NET" "$CURL_IMAGE" -sf \
    http://oidc:8080/default/.well-known/openid-configuration

echo "--- dovecot (implicit TLS, self-signed) + three messages with a remote image"
# The certificate is generated INSIDE a throwaway container and copied OUT with
# `docker cp` — see inline-images-e2e.sh for why a bind mount cannot do this on
# the runner (#280).
docker create --name "$CERTGEN" "$DOVECOT_IMAGE" sh -c '
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
    -keyout /tmp/dovecot.key -out /tmp/dovecot.crt \
    -subj "/CN=dovecot" -addext "subjectAltName=DNS:dovecot" >/dev/null 2>&1' >/dev/null
docker start -a "$CERTGEN" >/dev/null 2>&1 || true
docker cp "$CERTGEN:/tmp/dovecot.crt" "$WORK_HOST/dovecot.crt"
docker cp "$CERTGEN:/tmp/dovecot.key" "$WORK_HOST/dovecot.key"
docker rm -fv "$CERTGEN" >/dev/null
# A cheap absent/empty guard, NOT the proof: the handshake below is what
# establishes the trust material.
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

# Three messages, two senders, three DISTINCT image URLs.
#
#   one-off    news@sender.example       hero.png   opened, "Show remote content"
#   grant-a    digest@publisher.example  first.png  opened, "Always allow this sender"
#   grant-b    digest@publisher.example  second.png opened on a fresh page, NO click
#
# ⚠ grant-b IS A SEPARATE MESSAGE ON PURPOSE. A click on a message — either
# button — also tells the server "the user asked for THIS message", and the proxy
# honours that for half an hour (remote_request.go). Re-opening the message that
# was clicked would therefore be authorised twice over, and the run could not say
# the remembered grant did anything. grant-b has never been clicked: the only
# thing that can load its image is the grant.
#
# The image is 64x48 and declares it: a 1x1 is a tracking pixel by the shipped
# rule (sanitize_image_stats.go) and is never given a proxy reference at all.
write_message() { # $1 file, $2 from, $3 subject, $4 message-id local part, $5 image name
  cat > "$WORK/$1" <<EOF
From: $2
To: $TESTUSER
Subject: $3
Message-ID: <$4@example.com>
Date: Mon, 12 Jul 2026 10:00:00 +0000
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset=utf-8

Plain-text version of $3.
--alt
Content-Type: text/html; charset=utf-8

<html><body>
<p>$3</p>
<img src="http://images.sender.example:8099/$5" width="64" height="48" alt="remote">
</body></html>
--alt--
EOF
}
write_message one-off.eml "Newsletter <news@sender.example>" "proxied-e2e one-off" proxied-one-off hero.png
write_message grant-a.eml "Digest <digest@publisher.example>" "proxied-e2e grant-a" proxied-grant-a first.png
write_message grant-b.eml "Digest <digest@publisher.example>" "proxied-e2e grant-b" proxied-grant-b second.png

DVID=$(docker create --name "$DV" --network "$NET" --network-alias dovecot "$DOVECOT_IMAGE")
docker cp "$WORK_HOST/dovecot.conf" "$DVID:/etc/dovecot/dovecot.conf"
docker cp "$WORK_HOST/dovecot.crt" "$DVID:/etc/dovecot/dovecot.crt"
docker cp "$WORK_HOST/dovecot.key" "$DVID:/etc/dovecot/dovecot.key"
docker start "$DVID" >/dev/null
# doveadm is a precondition for `doveadm save` below and nothing more (see
# inline-images-e2e.sh: it answers before the listener is up).
wait_for "dovecot (doveadm, local)" 60 "$DV" \
  docker exec "$DV" doveadm mailbox list -u "$TESTUSER"
# The readiness that matters: a real handshake from a SEPARATE container, with
# the served certificate compared against the file the app is about to trust
# (#432).
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

for m in one-off grant-a grant-b; do
  docker cp "$WORK_HOST/$m.eml" "$DV:/tmp/$m.eml"
  docker exec "$DV" sh -c "doveadm save -u '$TESTUSER' -m INBOX < /tmp/$m.eml"
done
echo "delivered three messages into $TESTUSER INBOX"

cat > "$WORK/accounts.json" <<EOF
{"accounts":[{"name":"dovecot","imap_host":"dovecot","imap_port":993,
  "smtp_host":"dovecot","smtp_port":465,
  "username":"$TESTUSER","password":"$TESTPASS"}]}
EOF

echo "--- mailroom (OIDC — a real session cookie — syncing the dovecot account)"
# ⚠ OIDC, NOT DEV-AUTH. THAT IS THE WHOLE LANE. Under dev-auth every request is
# the operator's, the frame's included, and #667 cannot occur.
#
# The redirect URL is http://, which decides three things at once in main.go, all
# from that one value: the session cookie is not marked Secure (a browser would
# drop a Secure cookie on a cleartext origin, and there would be no session to
# test), there is no HSTS, and appCSPFor adds this origin to the app's img-src
# (#460) — which is what lets the control frame's request actually LEAVE the
# browser and be refused by the server for its missing cookie, rather than be
# stopped by a policy before it is sent.
#
# The image proxy is on by default (config.DefaultImageProxy); it is named here
# anyway so that a changed default cannot turn this lane into a test of nothing.
#
# CREATED, FED, THEN STARTED: the app must never exist without its CA bundle and
# its accounts file (#432 — see inline-images-e2e.sh for the race this removes).
APPID=$(docker create --name "$APP" --network "$NET" --network-alias mailroom \
  -v "$VOL:/k:ro" \
  -e MAILROOM_DATABASE_URL="postgres://mailroom_app:app@pg:5432/mailroom?sslmode=disable" \
  -e MAILROOM_MASTER_KEY_FILE=/k/master \
  -e MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS=1 \
  -e MAILROOM_OIDC_ISSUER="http://oidc:8080/default" \
  -e MAILROOM_OIDC_CLIENT_ID=mailroom \
  -e MAILROOM_OIDC_CLIENT_SECRET=mailroom-secret \
  -e MAILROOM_OIDC_REDIRECT_URL="http://mailroom:8080/auth/callback" \
  -e MAILROOM_SESSION_KEY="$SESSION_KEY" \
  -e MAILROOM_OPERATOR_SUBJECT="$SUBJECT" \
  -e MAILROOM_OPERATOR_EMAIL="$TESTUSER" \
  -e MAILROOM_ACCOUNTS_FILE=/tmp/accounts.json \
  -e MAILROOM_IMAGE_PROXY=1 \
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

# /readyz is not evidence that the account validated (#432): assert the positive
# onboarding line, and wait for the LOG STREAM rather than sampling it once.
_boot_i=0
while [ "$_boot_i" -lt 10 ]; do
  if docker logs "$APP" 2>&1 | grep -qE 'msg="account added"|sync disabled'; then break; fi
  _boot_i=$((_boot_i + 1)); sleep 1
done
if ! docker logs "$APP" 2>&1 | grep -q 'msg="account added"'; then
  echo "FATAL: mailroom did not validate the declared dovecot account, so no mail can sync" >&2
  echo "       and every assertion below would be about a message that never arrived." >&2
  echo "--- mailroom onboarding lines ---" >&2
  docker logs "$APP" 2>&1 | grep -iE "account|sync disabled|operator" | head -20 >&2 || true
  exit 1
fi
echo "the declared account validated against dovecot's TLS certificate"

# ⚠ THE GATE IS ASSERTED BEFORE THE BROWSER IS TRUSTED TO HAVE PASSED IT. If
# this instance answered an anonymous /jmap/session, every "signed in" below
# would be true of anyone, and the lane would be the dev-auth lanes again.
ANON="$(docker run --rm --network "$NET" "$CURL_IMAGE" -sS -o /dev/null -w '%{http_code}' \
  http://mailroom:8080/jmap/session || true)"
if [ "$ANON" != "401" ]; then
  echo "FATAL: an anonymous GET /jmap/session answered $ANON, not 401. This instance is" >&2
  echo "       not cookie-gated, so there is no credential for the frame to lack." >&2
  exit 1
fi
echo "the instance is gated: anonymous /jmap/session is 401"

echo
echo "=== a real browser: sign in, open the mail, look INSIDE the message frame ==="
cat > "$WORK/proxied-check.cjs" <<'NODEEOF'
"use strict";
const puppeteer = require("puppeteer");
const dns = require("dns").promises;

const BASE = "http://mailroom:8080";
const SENDER_HOST = "images.sender.example";
const HITS_URL = "http://" + SENDER_HOST + ":8099/__hits";
const IMG_W = 64;
const IMG_H = 48;
const SUBJECT = process.env.OIDC_SUBJECT;
const EMAIL = process.env.OIDC_EMAIL;
const SHOW_REMOTE = process.env.SHOW_REMOTE;
const ALWAYS_ALLOW = process.env.ALWAYS_ALLOW;
const SANDBOX = process.env.MESSAGE_SANDBOX;
const SUBJECTS = {
  oneOff: "proxied-e2e one-off",
  grantA: "proxied-e2e grant-a",
  grantB: "proxied-e2e grant-b",
};

let checks = 0;
let fails = 0;
const pass = (name) => {
  checks += 1;
  console.log("PASS  " + name);
};
const fail = (name, detail) => {
  checks += 1;
  fails += 1;
  console.log("FAIL  " + name + (detail ? " -- " + detail : ""));
};
const check = (ok, name, detail) => (ok ? pass(name) : fail(name, detail));
const info = (line) => console.log("INFO  " + line);
// A bail-out is ONE failure with its own name, never a row of assertions that
// all miss for a reason none of them states (#284).
const bail = (marker, why) => {
  console.log(marker + " " + why);
  process.exit(3);
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const until = async (fn, ms, step = 250) => {
  const end = Date.now() + ms;
  for (;;) {
    let v = null;
    try {
      v = await fn();
    } catch (e) {
      v = null;
    }
    if (v) return v;
    if (Date.now() > end) return null;
    await sleep(step);
  }
};
const clip = (s, n) => (s.length > n ? s.slice(0, n) + "…(" + s.length + " chars)" : s);

// Every request any page makes to the sender's host, across the whole run.
const browserToSender = [];
// What the server answered to the control frame's own request.
const controlFrameResponses = [];
// Anything else the browser said about that request (a policy refusal, a
// network failure), so a control that fails says why.
const controlFrameNotes = [];

(async () => {
  for (const [k, v] of Object.entries({ SUBJECT, EMAIL, SHOW_REMOTE, ALWAYS_ALLOW, SANDBOX })) {
    if (!v) bail("NOENV", k + " was not handed to the browser stage");
  }
  const browser = await puppeteer.launch({ args: ["--no-sandbox", "--disable-setuid-sandbox"] });
  const newPage = async () => {
    const page = await browser.newPage();
    await page.setViewport({ width: 1280, height: 900 });
    page.on("request", (req) => {
      let host = "";
      try {
        host = new URL(req.url()).hostname;
      } catch (e) {
        host = "";
      }
      if (host === SENDER_HOST) browserToSender.push(req.url());
    });
    page.on("response", (res) => {
      if (res.url().indexOf("control=frame") >= 0) controlFrameResponses.push(res.status());
    });
    page.on("requestfailed", (req) => {
      if (req.url().indexOf("control=frame") >= 0) {
        controlFrameNotes.push("requestfailed: " + ((req.failure() || {}).errorText || "?"));
      }
    });
    page.on("console", (msg) => {
      const t = msg.text();
      if (/Content Security Policy|control=frame/.test(t)) controlFrameNotes.push("console: " + t.slice(0, 300));
    });
    return page;
  };

  // ---- 1. SIGN IN, IN THE BROWSER --------------------------------------
  const login = await newPage();
  await login.goto(BASE + "/", { waitUntil: "domcontentloaded", timeout: 30000 });
  const signIn = await login.$('a[href*="/auth/start"]');
  if (!signIn) bail("NOLOGIN", "no sign-in link on " + login.url());
  await Promise.all([
    login.waitForNavigation({ waitUntil: "domcontentloaded", timeout: 30000 }),
    signIn.click(),
  ]);
  if (login.url().indexOf("http://oidc:8080/default/authorize") !== 0) {
    bail("NOLOGIN", "the sign-in link led to " + login.url() + ", not the provider");
  }
  if (!(await login.$('input[name="username"]')) || !(await login.$('textarea[name="claims"]'))) {
    bail("NOLOGIN", "the provider's login form has no username/claims fields");
  }
  await login.type('input[name="username"]', SUBJECT);
  await login.type('textarea[name="claims"]', JSON.stringify({ email: EMAIL, name: "Reader" }));
  await Promise.all([
    login.waitForNavigation({ waitUntil: "domcontentloaded", timeout: 30000 }),
    login.click('[type="submit"]'),
  ]);
  const landed = await until(
    () => login.url().indexOf(BASE + "/") === 0 && login.url().indexOf("/auth/") < 0,
    15000,
  );
  check(
    !!landed,
    "sign-in: the Authorization-Code flow completed in the browser and landed on the app",
    "ended at " + login.url(),
  );
  const cookie = (await login.cookies(BASE)).find((c) => c.name === "mailroom_session");
  check(
    !!cookie && cookie.httpOnly === true && cookie.sameSite === "Lax",
    "sign-in: the browser holds a session cookie that is HttpOnly and SameSite=Lax",
    cookie ? "httpOnly=" + cookie.httpOnly + " sameSite=" + cookie.sameSite : "no mailroom_session cookie",
  );
  if (!landed || !cookie) bail("NOLOGIN", "there is no signed-in browser to continue with");
  info("cookie: secure=" + cookie.secure + " (a cleartext origin; see the app's start-up note in the script)");

  // ---- the session, the messages, and what the server parked in them ---------
  const sess = await login.evaluate(async () => {
    const r = await fetch("/jmap/session");
    return { status: r.status, body: r.ok ? await r.json() : null };
  });
  if (sess.status !== 200 || !sess.body) bail("NOSESSION", "/jmap/session answered " + sess.status + " to the signed-in page");
  const caps = sess.body.capabilities || {};
  const USING = Object.keys(caps);
  const proxyOn = Object.values(caps).some((c) => c && c.imageProxy === true);
  if (!proxyOn) bail("NOPROXY", "the session advertises no imageProxy capability: the proxy is off and nothing here applies");
  const accountId =
    (sess.body.primaryAccounts || {})["urn:ietf:params:jmap:mail"] || Object.keys(sess.body.accounts || {})[0];
  const jmap = async (calls) => {
    const out = await login.evaluate(
      async (using, methodCalls) => {
        const r = await fetch("/jmap", {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ using, methodCalls }),
        });
        return { status: r.status, text: await r.text() };
      },
      USING,
      calls,
    );
    if (out.status !== 200) throw new Error("jmap HTTP " + out.status + ": " + out.text.slice(0, 300));
    const body = JSON.parse(out.text);
    for (const r of body.methodResponses || []) {
      if (r[0] === "error") throw new Error("jmap method error: " + JSON.stringify(r));
    }
    return body.methodResponses;
  };

  const mail = {};
  const synced = await until(
    async () => {
      const q = await jmap([["Email/query", { accountId }, "q"]]);
      const ids = q[0][1].ids || [];
      if (ids.length < 3) return null;
      const g = await jmap([
        [
          "Email/get",
          { accountId, ids, properties: ["subject", "htmlBody", "bodyValues"], fetchHTMLBodyValues: true },
          "g",
        ],
      ]);
      for (const e of g[0][1].list || []) {
        const part = (e.htmlBody || []).find((p) => p.type === "text/html");
        const html = part && e.bodyValues && e.bodyValues[part.partId] ? e.bodyValues[part.partId].value : "";
        for (const [key, subject] of Object.entries(SUBJECTS)) {
          if (e.subject === subject) mail[key] = { id: e.id, html };
        }
      }
      return mail.oneOff && mail.grantA && mail.grantB ? true : null;
    },
    90000,
    2000,
  );
  if (!synced) bail("NOSYNC", "the three messages never reached Mailroom (have: " + Object.keys(mail).join(",") + ")");
  for (const [key, m] of Object.entries(mail)) {
    const parked = /data-mailroom-remote-src="([^"]*)"/.exec(m.html);
    if (!parked) bail("NOTPARKED", key + " carries no parked remote image: " + clip(m.html, 300));
    if (!/^\/jmap\/proxy\/[0-9A-Za-z]{26}\/[0-9A-Za-z]{26}\/[0-9a-f]{32}$/.test(parked[1])) {
      bail("NOPROXY", key + " parked " + clip(parked[1], 120) + ", which is not a proxy path");
    }
    m.path = parked[1];
  }
  info("one-off parks " + mail.oneOff.path);

  // Setup complete, the way "Skip setup, open the desk" does it — otherwise a
  // first login is sent to the day-one wizard and no message is ever drawn (the
  // a11y lane has the long version). Read back, not trusted.
  const dp0 = await jmap([["DeskPreferences/get", { accountId, ids: null }, "dp"]]);
  const dpId = ((dp0[0][1].list || [])[0] || {}).id || accountId;
  const dpSet = await jmap([
    ["DeskPreferences/set", { accountId, update: { [dpId]: { setupCompletedAt: new Date().toISOString() } } }, "dps"],
    ["DeskPreferences/get", { accountId, ids: null }, "dpg"],
  ]);
  const dpAfter = (dpSet[1][1].list || [])[0];
  if (!dpAfter || !dpAfter.setupCompletedAt) {
    bail("NOSETUP", "setupCompletedAt did not stick: " + JSON.stringify(dpSet[0][1].notUpdated || null));
  }

  // ---- helpers over the real client --------------------------------------
  const senderHits = async () => (await fetch(HITS_URL)).json();
  const openMessage = async (id) => {
    // A NEW PAGE, so every open is a cold load with no React state carried over.
    const page = await newPage();
    await page.goto(BASE + "/#/all?m=" + id, { waitUntil: "domcontentloaded", timeout: 30000 });
    const frame = await until(() => page.$("iframe.message-html"), 45000);
    if (!frame) {
      const text = await page.evaluate(() => document.body.innerText.slice(0, 400)).catch(() => "");
      bail("NOFRAME", "no message frame was drawn at " + page.url() + " — the page says: " + JSON.stringify(text));
    }
    return page;
  };
  // The state of the REAL message frame, read from inside it. The frame is
  // re-resolved on every call: the client swaps its srcdoc when the images
  // arrive, which is a navigation, and a handle from before it is dead.
  const frameState = async (page) => {
    const handle = await page.$("iframe.message-html");
    if (!handle) return null;
    const frame = await handle.contentFrame();
    if (!frame) return null;
    const state = await frame.evaluate(() => {
      const attr = (e, n) => e.getAttribute(n) || "";
      const sourced = [...document.querySelectorAll("[src],[srcset]")];
      return {
        imgs: [...document.images].map((i) => ({
          alt: i.alt,
          w: i.naturalWidth,
          h: i.naturalHeight,
          complete: i.complete,
          src: attr(i, "src"),
          parked: attr(i, "data-mailroom-remote-src"),
        })),
        // The ATTRIBUTE and the RESOLVED property both: a relative proxy path
        // resolves to an absolute one, and either form is the frame being asked
        // to fetch it.
        proxyRefs: sourced.filter(
          (e) =>
            (attr(e, "src") + " " + attr(e, "srcset") + " " + (typeof e.src === "string" ? e.src : "")).indexOf(
              "/jmap/proxy/",
            ) >= 0,
        ).length,
        sourced: sourced.length,
      };
    });
    state.sandbox = await handle.evaluate((el) => el.getAttribute("sandbox") || "");
    return state;
  };
  const remoteImg = (s) => (s ? s.imgs.find((i) => i.alt === "remote") : undefined);
  // Wait for the image to SETTLE either way — loaded or failed — rather than
  // for it to succeed. A wait that only ends on success turns every failure
  // into a timeout that says nothing about what the frame actually held.
  const settled = async (page) => {
    await until(async () => {
      const img = remoteImg(await frameState(page));
      return img && img.src !== "" && img.complete;
    }, 25000);
    await sleep(500);
    return frameState(page);
  };
  const banner = (page) =>
    page.evaluate(() => ({
      buttons: [...document.querySelectorAll(".remote-banner button")].map((b) => (b.textContent || "").trim()),
      quiet: document.querySelectorAll(".remote-banner.remote-banner-quiet").length,
      text: [...document.querySelectorAll(".remote-banner")].map((b) => (b.textContent || "").trim().slice(0, 140)),
    }));
  const clickBannerButton = (page, label) =>
    page.evaluate((want) => {
      const b = [...document.querySelectorAll(".remote-banner button")].find(
        (x) => (x.textContent || "").trim() === want,
      );
      if (!b) return false;
      b.click();
      return true;
    }, label);
  // The three in-frame assertions, identical for both cases.
  const assertRendered = (label, s) => {
    const img = remoteImg(s);
    const seen = img
      ? "naturalWidth=" + img.w + " naturalHeight=" + img.h + " src=" + JSON.stringify(clip(img.src, 60))
      : s
        ? "the frame holds no <img alt=remote>"
        : "there is no message frame";
    check(
      !!img && img.w === IMG_W && img.h === IMG_H,
      label + ": inside the message frame the image DECODED (" + IMG_W + "x" + IMG_H + ")",
      seen,
    );
    check(!!img && img.src.indexOf("data:image/") === 0, label + ": its src is a data:image/ URL", seen);
    check(
      !!s && s.proxyRefs === 0,
      label + ": nothing in the frame references /jmap/proxy/",
      s ? s.proxyRefs + " of " + s.sourced + " sourced element(s) do; " + seen : seen,
    );
  };

  // ---- 2. ONE-OFF: "Show remote content" -----------------------------
  const one = await openMessage(mail.oneOff.id);
  await until(async () => (await banner(one)).buttons.includes(SHOW_REMOTE), 20000);
  {
    const b = await banner(one);
    const s = await frameState(one);
    const img = remoteImg(s);
    const hits = await senderHits();
    if (s && s.sandbox !== SANDBOX) {
      bail("NOSANDBOX", "the message frame's sandbox is " + JSON.stringify(s.sandbox) + ", not the MESSAGE_SANDBOX read from the source");
    }
    check(
      b.buttons.includes(SHOW_REMOTE) && !!img && img.src === "" && img.w === 0 &&
        img.parked.indexOf("/jmap/proxy/") === 0 && hits.length === 0,
      "one-off: blocked by default — the banner offers the opt-in, the image is parked, the sender has seen nothing",
      "buttons=" + JSON.stringify(b.buttons) + " img=" + JSON.stringify(img || null) + " senderHits=" + hits.length,
    );
  }
  if (!(await clickBannerButton(one, SHOW_REMOTE))) {
    // Still three FAILs below, each stating what the frame held: a missing
    // button is a result about the client, not a harness fault.
    info("one-off: there was no " + JSON.stringify(SHOW_REMOTE) + " button to click");
  }
  const oneState = await settled(one);
  assertRendered("one-off", oneState);
  info("one-off: banner after the click " + JSON.stringify((await banner(one)).text));

  // ---- 3. THE NEGATIVE CONTROL: the pre-fix behaviour, on purpose -----------
  //
  // The click above told the server the user asked for this message, so the
  // proxy path is one the SESSION may fetch. Hand that same path to a frame with
  // the message frame's sandbox and it must fail — not because the path is bad
  // (the parent gets a 200 image from it) and not because the frame cannot show
  // images (the same bytes as data: decode in it), but because the frame has no
  // cookie. That is #667, and it is what assertRendered would be unable to
  // distinguish from a fix if this ever stopped being true.
  const parent = await one.evaluate(async (path) => {
    const r = await fetch(path, { credentials: "include" });
    const type = (r.headers.get("content-type") || "").split(";")[0].trim();
    const u8 = new Uint8Array(await r.arrayBuffer());
    let bin = "";
    for (let i = 0; i < u8.length; i++) bin += String.fromCharCode(u8[i]);
    const anon = await fetch(path + "?control=anonymous", { credentials: "omit", redirect: "manual" });
    return {
      status: r.status,
      type,
      bytes: u8.length,
      dataUrl: "data:" + type + ";base64," + btoa(bin),
      anonStatus: anon.status,
      anonType: anon.type,
    };
  }, mail.oneOff.path);
  const parentOK = parent.status === 200 && /^image\//.test(parent.type) && parent.bytes > 0;
  check(
    parentOK,
    "control: the SAME path, fetched by the app with the session cookie, is a 200 image",
    "status=" + parent.status + " type=" + JSON.stringify(parent.type) + " bytes=" + parent.bytes +
      (parent.status === 403
        ? " — 403 is the egress guard refusing the sender's address: the image was built without -tags e2eprivateimages, or the subnet is not RFC 1918 (PROXIED_E2E_SUBNET)"
        : ""),
  );
  check(
    parent.anonStatus !== 200,
    "control: the same path WITHOUT the cookie is refused (the route is cookie-gated here)",
    "status=" + parent.anonStatus + " type=" + parent.anonType,
  );
  info("control: with cookie " + parent.status + " " + parent.type + " " + parent.bytes + "B; without cookie " + parent.anonStatus + " (" + parent.anonType + ")");

  // The frame's request, watched at the protocol level as well as through
  // puppeteer's own events. A refused image is not a "response" to puppeteer at
  // all: Chromium's opaque-response blocking discards a no-cors reply that is
  // not an image and reports the request as FAILED (net::ERR_BLOCKED_BY_ORB), so
  // the status line and the cookie decision are only visible in the raw
  // Network.*ExtraInfo events.
  const cdp = await one.createCDPSession();
  await cdp.send("Network.enable");
  const rawIds = new Set();
  const rawSent = new Map();
  const rawReceived = new Map();
  cdp.on("Network.requestWillBeSent", (e) => {
    if (e.request.url.indexOf("control=frame") >= 0) rawIds.add(e.requestId);
  });
  cdp.on("Network.requestWillBeSentExtraInfo", (e) => rawSent.set(e.requestId, e));
  cdp.on("Network.responseReceivedExtraInfo", (e) => rawReceived.set(e.requestId, e));

  let ctl = null;
  if (parentOK) {
    await one.evaluate(
      (sandbox, dataUrl, url) => {
        const f = document.createElement("iframe");
        f.id = "proxied-e2e-control";
        f.setAttribute("sandbox", sandbox);
        f.setAttribute("referrerpolicy", "no-referrer");
        f.srcdoc =
          '<!DOCTYPE html><html><body><img id="d" alt="data" src="' + dataUrl + '">' +
          '<img id="p" alt="proxied" src="' + url + '"></body></html>';
        document.body.appendChild(f);
      },
      SANDBOX,
      parent.dataUrl,
      BASE + mail.oneOff.path + "?control=frame",
    );
    const read = async () => {
      const handle = await one.$("#proxied-e2e-control");
      const frame = handle ? await handle.contentFrame() : null;
      if (!frame) return null;
      return frame.evaluate(() => {
        const d = document.getElementById("d");
        const p = document.getElementById("p");
        if (!d || !p) return null;
        return { dW: d.naturalWidth, dDone: d.complete, pW: p.naturalWidth, pDone: p.complete };
      });
    };
    await until(async () => {
      const r = await read();
      return r && r.dDone && r.pDone;
    }, 20000);
    await sleep(500);
    ctl = await read();
  }
  const ctlSeen = ctl ? JSON.stringify(ctl) : "the control frame was not built (the parent fetch failed, or the frame never loaded)";
  check(
    !!ctl && ctl.dW === IMG_W,
    "control: a frame with the message sandbox CAN show an image — the same bytes as data: decode in it",
    ctlSeen,
  );
  check(
    !!ctl && ctl.pDone === true && ctl.pW === 0,
    "control: that frame CANNOT load the proxy path itself — this is #667, reproduced",
    ctl && ctl.pW > 0
      ? "THE PREMISE OF THIS LANE IS GONE: a sandboxed, opaque-origin frame loaded a cookie-gated proxy image " +
        "(naturalWidth=" + ctl.pW + "). The assertions above can no longer tell the fix from its absence; " +
        "find out what changed (the sandbox tokens, the cookie's SameSite, the route's gate) before trusting this lane."
      : ctlSeen,
  );
  // "Did not decode" has two very different causes, and only one of them is
  // #667. If a content policy stopped the request before it was sent, the frame
  // would also show a broken image — and the control would be reproducing #460
  // (the app's own img-src) rather than the withheld cookie. So the request has
  // to be seen to LEAVE and to come back refused. Any ONE of these is that
  // evidence; a policy block (net::ERR_BLOCKED_BY_CSP) or silence is none of them.
  let rawStatus = null;
  let rawCookies = null;
  for (const id of rawIds) {
    const got = rawReceived.get(id);
    if (got && typeof got.statusCode === "number") rawStatus = got.statusCode;
    const sent = rawSent.get(id);
    if (sent && Array.isArray(sent.associatedCookies)) {
      rawCookies = sent.associatedCookies.map(
        (c) => c.cookie.name + "=" + ((c.blockedReasons || []).join("+") || "SENT"),
      );
    }
  }
  const frameStatus = controlFrameResponses.length ? controlFrameResponses[controlFrameResponses.length - 1] : null;
  const orbBlocked = controlFrameNotes.some((n) => n.indexOf("ERR_BLOCKED_BY_ORB") >= 0);
  const refusedByServer =
    (rawStatus !== null && rawStatus !== 200) || (frameStatus !== null && frameStatus !== 200) || orbBlocked;
  check(
    refusedByServer && rawStatus !== 200 && frameStatus !== 200,
    "control: the frame's request REACHED the server and came back refused — no content policy stopped it first",
    "raw status=" + rawStatus + " puppeteer status=" + frameStatus + " notes=" + JSON.stringify(controlFrameNotes) +
      " — with none of a refusal status or an opaque-response block, the request never left the browser: " +
      "the control is showing a policy block, which is not the failure this lane is about",
  );
  info(
    "control: the frame's request — raw status " + rawStatus + ", puppeteer status " + frameStatus +
      ", cookies " + (rawCookies ? JSON.stringify(rawCookies) : "(not reported)"),
  );
  for (const n of controlFrameNotes) info("control: " + n);
  await cdp.detach().catch(() => {});
  await one.evaluate(() => {
    const f = document.getElementById("proxied-e2e-control");
    if (f) f.remove();
  });
  await one.close();

  // ---- 4. A REMEMBERED GRANT, and a message nobody clicked on --------------
  const ga = await openMessage(mail.grantA.id);
  await until(async () => (await banner(ga)).buttons.includes(ALWAYS_ALLOW), 20000);
  const clickedAlways = await clickBannerButton(ga, ALWAYS_ALLOW);
  // The grant has to be on the SERVER before the next page asks for it; the
  // client shows the images optimistically, so its banner is not that evidence.
  const grant = await until(async () => {
    const r = await jmap([["RemoteAllow/get", { accountId, ids: null }, "ra"]]);
    const list = r[0][1].list || [];
    return list.length > 0 ? list : null;
  }, 15000, 500);
  info("grant: clicked=" + clickedAlways + " stored=" + JSON.stringify((grant || []).map((g) => g.kind + ":" + g.value)));
  await ga.close();

  const gb = await openMessage(mail.grantB.id);
  const gbState = await settled(gb);
  const gbBanner = await banner(gb);
  check(
    !!grant && !gbBanner.buttons.includes(SHOW_REMOTE) && gbBanner.quiet > 0,
    "grant: a different message from the allowed sender opens ALLOWED on a fresh page, with no click",
    "grantStored=" + !!grant + " buttons=" + JSON.stringify(gbBanner.buttons) + " banners=" + JSON.stringify(gbBanner.text),
  );
  assertRendered("grant", gbState);
  await gb.close();

  // ---- 5. WHO TALKED TO THE SENDER ----------------------------------------
  const appIp = (await dns.lookup("mailroom")).address;
  const hits = await senderHits();
  const strangers = hits.filter((h) => h.ip !== appIp);
  check(
    hits.some((h) => h.url === "/hero.png") && hits.some((h) => h.url === "/second.png") && strangers.length === 0,
    "sender: the images were fetched by the SERVER, and by nobody else",
    "app=" + appIp + " hits=" + JSON.stringify(hits.map((h) => h.ip + " " + h.url)),
  );
  check(
    browserToSender.length === 0,
    "sender: the browser made ZERO requests to the sender's host",
    JSON.stringify(browserToSender.slice(0, 5)),
  );
  info("sender: " + hits.length + " request(s), all from " + appIp + ": " + hits.map((h) => h.url).join(" "));

  await browser.close();
  console.log("DONE checks=" + checks + " fails=" + fails);
  process.exit(fails === 0 ? 0 : 1);
})().catch((e) => {
  console.log("ERR " + (e && e.stack ? e.stack : String(e)));
  process.exit(2);
});
NODEEOF

# --shm-size: Chromium keeps renderer shared memory in /dev/shm, which Docker
# defaults to 64 MiB; a renderer that runs out STALLS rather than erroring.
docker create --name "$CHK" --network "$NET" --shm-size=1g \
  -e NODE_PATH=/usr/src/app/node_modules \
  -e OIDC_SUBJECT="$SUBJECT" \
  -e OIDC_EMAIL="$TESTUSER" \
  -e SHOW_REMOTE="$SHOW_REMOTE" \
  -e ALWAYS_ALLOW="$ALWAYS_ALLOW" \
  -e MESSAGE_SANDBOX="$MESSAGE_SANDBOX" \
  --entrypoint node "$CHROME_IMAGE" /tmp/proxied-check.cjs >/dev/null
docker cp "$WORK_HOST/proxied-check.cjs" "$CHK:/tmp/proxied-check.cjs" >/dev/null
# ⚠ THE EXIT CODE COMES FROM `docker wait`, NOT FROM THE PIPELINE: `tee`'s status
# is 0 whatever the check found, and ${PIPESTATUS[0]} does not exist in the `sh`
# CI runs this with (a11y-e2e.sh has the long version).
set +e
docker start -a "$CHK" 2>&1 | tee "$WORK/check.out"
set -e
CHK_RC="$(docker wait "$CHK" 2>/dev/null || echo 99)"
docker rm -fv "$CHK" >/dev/null 2>&1 || true

PASSES="$(grep -c '^PASS  ' "$WORK/check.out" || true)"
FAILS="$(grep -c '^FAIL  ' "$WORK/check.out" || true)"

# ⚠ FAIL CLOSED, WITH NO LIST OF KNOWN FAILURES. Green needs ALL of: exit 0, the
# DONE line, zero FAIL lines, and exactly the expected number of PASS lines.
# Anything else is red — a bail-out marker, an ERR from the script's own catch, a
# browser that never started, or an assertion somebody deleted (#281).
echo
if [ "$CHK_RC" = "0" ] && [ "$FAILS" = "0" ] && [ "$PASSES" = "$EXPECTED_CHECKS" ] &&
   grep -q "^DONE checks=$EXPECTED_CHECKS fails=0\$" "$WORK/check.out"; then
  echo "=== PROXIED IMAGES RENDER IN THE SANDBOXED FRAME BEHIND A REAL LOGIN ($PASSES/$EXPECTED_CHECKS) ==="
  exit 0
fi

echo "=== mailroom logs (startup) ==="; docker logs "$APP" 2>&1 | head -25
echo "=== mailroom logs (latest) ===";  docker logs "$APP" 2>&1 | tail -25
echo "=== the sender's access log ==="
docker run --rm --network "$NET" "$CURL_IMAGE" -sS http://images.sender.example:8099/__hits 2>&1 || true
echo
echo
if ! grep -q '^DONE ' "$WORK/check.out"; then
  echo "PROXIED-IMAGES E2E FAILED: the browser stage did not finish (exit $CHK_RC, $PASSES pass, $FAILS fail)."
  echo "  Its last line above names where it stopped; nothing after that point was exercised,"
  echo "  so this is NOT a finding about image rendering."
else
  echo "PROXIED-IMAGES E2E FAILED: $FAILS assertion(s) failed, $PASSES of $EXPECTED_CHECKS passed (exit $CHK_RC)."
fi
exit 1
