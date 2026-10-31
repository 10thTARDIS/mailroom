# Deploying Mailroom

This directory is a self-contained Docker Compose stack for running Mailroom:
Postgres, schema migrations, the crypto process, malware scanning and the
server, wired together with non-root, least-privilege containers per spec §11.
The same stack serves a trial instance and a real one; the
[guide to connecting your first real mailbox](../docs/production-cutover-runbook.md)
covers what changes when real mail is involved.

Placeholders below (`mail.example.com`, hostnames, ports) are neutral —
substitute your own values; nothing in this repo should encode a real
operator hostname (see the project's open-source portability rule).

## What this stack proves today, and what it doesn't

**Testable now:**
- The image builds and runs as a static, non-root binary.
- The server boots, connects to Postgres, and serves `/healthz` (liveness),
  `/readyz` (readiness — a real DB round-trip via `SystemTx`), and
  `/metrics` (Prometheus). Those three are always unauthenticated.
- Schema migrations (`db/migrations/*.sql`) apply cleanly to a fresh
  database via `mailroom-migrate`, and the RLS-scoped `mailroom_app` role is
  correctly bootstrapped.
- Any identity that completes the OIDC flow is auto-provisioned on first login
  (app-binding is the access gate), and the JMAP API (`/jmap`, `/jmap/session`,
  `/.well-known/jmap`) serves their own isolated mail behind that session. A
  member of the `mailroom-admin` group gets the admin role, re-evaluated each
  login.
- Declared IMAP accounts (`secrets/accounts.json`) are onboarded at boot for the
  optional declared-accounts operator, and per-user background loops sync every
  provisioned user's accounts every `MAILROOM_SYNC_INTERVAL`.

- An anonymous visitor reaches the **sign-in page** and the health probes, and
  nothing else — see the route table in §4. A login with no email claim, or one
  resolving to a disabled/deleted user, is refused and told why.

- **Outbound mail is real.** `cmd/mailroom` wires a send worker over
  `provider.SMTPSender`, so a submission is queued as a durable job and then
  actually delivered over SMTP with the account's sealed credentials — including
  compose-time PGP signing where the identity has a bound key. Bounces come back
  as DSNs and land on the submission.

  ⚠ This line said *"no outbound submission beyond the local queue"* until the
  v1.34.6 wave. That stopped being true when send landed and nobody updated the
  deployment doc — which matters more here than in a code comment, because this
  file is what someone reads when deciding what the stack is safe to point at.

- **Attachments are scanned at delivery** (ADR-027), by a sealed `clamd` sidecar
  with **no network at all**, reached over a Unix socket, with signature updates
  fetched by a second container that holds no mail. Each attachment gets a stored
  verdict — clean / flagged / unknown / unscanned.

  **The verdict is now ENFORCED** (v1.61.0, ADR-027 §8). A part whose verdict is
  `flagged` — **or `unknown`**, which gets the same door for a different reason —
  is refused at the part-download and preview routes and cannot be attached to an
  outgoing message, until the user releases its content. The release is keyed on
  the CONTENT, so it covers every copy of those bytes in the mailbox.

  ⚠ This paragraph said *"recorded but not yet ENFORCED — nothing is blocked"*
  for three releases, and it was the honest thing to say for all three. It is
  kept here rather than deleted because the direction of the error matters: a
  deployment document that overstates a guard is worse than one that has none,
  and this file is what someone reads when deciding what the stack is safe to
  point at.

  ⚠ **What is still NOT blocked, deliberately:** the raw message download. A
  quarantined part sits inside those bytes base64-encoded, and blocking it would
  make a message with one bad attachment unreadable, unforwardable and
  unexportable. It carries an `X-Mailroom-Attachment-Scan` header instead, and
  the export sidecar records the verdict beside the bytes. `unscanned` does not
  block either — an instance running no scanner has every row in that state.

  ⚠ **It is fail-closed, so a dead scanner stops mail.** If `clamd` does not
  answer, every account's sync pauses: mail stays on the provider, cursors are
  withheld, the expunge reaper is skipped, and account health says the scanner is
  unreachable. Nothing is lost and it recovers by itself. Unset
  `MAILROOM_CLAMD_SOCKET` to turn scanning off entirely.

  ⚠ **A cold start pauses sync for about a minute** — not the "minutes, waiting
  on a ~300 MB download" this document claimed before the first real deploy
  measured it. The `clamav/clamav` image *ships* a signature database, so clamd
  loads it and starts serving in roughly 70 seconds; freshclam fetches only an
  incremental update afterwards, and does not gate clamd at all.

  ⚠ **A signature database can stop updating without either container ever
  looking unhealthy for it** — `clamd` keeps scanning against whatever
  it last loaded and reports success every time, since a stale verdict is not
  a wrong verdict from its point of view. Two independent signals catch this
  instead of one: `clamav-signatures` carries its own healthcheck (freshclam
  running AND the daily database written within the last 3 days by mtime) —
  scraped, never `depends_on`'d, because a CDN outage must not stop mail — and
  `/metrics` exposes `mailroom_scanner_signature_version` and
  `mailroom_scanner_signature_published_timestamp_seconds`, read from clamd
  within seconds of boot and every 10 minutes after (from v1.79.1; v1.79.0 read
  them only on the re-scan worker's daily pass, so they sat at 0 for up to a
  day after every restart). Alert on
  `time() - mailroom_scanner_signature_published_timestamp_seconds > 259200`
  (3 days, the age at which Mailroom itself calls the database stale). It is a
  timestamp rather than an age because an age would stand still between reads,
  while `time() - ts` keeps growing even if clamd stops answering. It reads zero, and so alerts, when clamd
  reports no publication date: an unknown age is treated as stale.

  ⚠ **A LARGE MAILBOX HAS A LONG FIRST SWEEP, and outbound mail shares its
  queue.** Attachments are re-checked in chunks of 50 messages with a 30-second
  pause between them, and the pass that gives each file its content fingerprint
  is deliberately *not* rationed by the age of the mail. On ~50,000
  attachment-bearing messages that is roughly **1,000 chunks — eight hours or
  more**.

  Nothing is at risk: each chunk is bounded to two minutes and an interrupted
  sweep resumes by itself. But the job dispatcher is **sequential**, so for those
  hours a send can wait behind a chunk — **up to about two minutes**.

  So if you are importing a large archive, do it in this order:

  1. import,
  2. let the sweep finish,
  3. *then* start relying on the instance to send.

  `mailroom-admin attachment-report` shows the remaining work under *"the re-scan
  queue, by the job's own predicate"* — `due now` reaching 0 is the signal.

  ⚠ **And there is a real choice at cutover, which is not obvious.** `unscanned`
  does **not** withhold anything; `unknown` does. So importing a large old
  mailbox with `MAILROOM_CLAMD_SOCKET` **unset** leaves that mail visible and
  unchecked, to be swept later — whereas importing with the scanner on can leave
  a file that hit a *momentary* failure withheld until the signature database
  next moves. Neither is wrong; they trade "unchecked for a while" against
  "withheld for a while", and the second is the one people are surprised by.

- **`MAILROOM_AUTHSERV_IDS` — whose mail authentication you believe.**
  Comma-separated RFC 8601 authserv-ids. It gates two things: whether a passing
  DKIM/DMARC result may suppress the "someone is claiming your own domain"
  warning, and — in current releases — whether the reading pane's sender-authentication
  notice (`authResults`) has anything to show at all. **Leave it unset and
  neither one reads anything.**

  ⚠ **An `Authentication-Results` header is attacker-writable unless you check
  who wrote it.** RFC 8601 §5 makes a mail server strip only the headers bearing
  *its own* id, so a line the sender adds claiming some other id travels through
  untouched. Without this setting, a stranger spoofing your domain turns off the
  warning about it by adding one header — and, for the same reason, could add a
  forged `dmarc=pass` header of their own to cancel your provider's genuine
  `dmarc=fail` in what the notice shows. Both features trust ONLY headers whose
  authserv-id is in this list; everything else is treated as if it were not
  there.

  The id belongs to **the server that actually did the authentication**, which is
  normally your mail provider rather than Mailroom — Mailroom reads mail over
  IMAP from a provider that has already run SPF, DKIM and DMARC. Read it off a
  real, already-delivered message: it is the first thing in the
  `Authentication-Results` value, before the first `;` — for example, a header
  reading `Authentication-Results: mx.example.com; dmarc=pass …` means the value
  is `mx.example.com`. Do not guess it; different providers use different ids.

  ⚠ **Leaving it unset is safe, but its two consumers fail safe in OPPOSITE
  directions — both toward "say nothing false", neither toward "trust an
  unverified header".** The spoofing warning turns NOISIER: no result is
  believed, so it fires on own-domain mail it might otherwise have suppressed.
  The sender-authentication notice turns SILENT: with nothing trusted there is
  no way to tell a genuine failure from a forged one, so it reports no
  authentication information rather than risk showing a forged verdict as real.
  Setting the value *wrong* does the same to both. There is no value of this
  setting that makes either one show something false — which is the point, and
  the reason it has no default.

  **A provider that changed its id: `id<YYYY-MM-DD`.** An entry may carry a
  received-time bound, for example
  `MAILROOM_AUTHSERV_IDS=mx.provider.example,old.provider.example<2026-07-01`.
  The bounded id is trusted only for mail whose INTERNALDATE (when the provider
  delivered it, never the sender's `Date` header) is strictly before 00:00 UTC
  on that date. RFC 8601 §5 strips only the *current* id, so after the switch a
  forged header bearing the old id is no longer stripped; the bound keeps it
  untrusted for new mail while older mail keeps its verdicts. A malformed bound
  stops the server and `mailroom-admin` at startup. Choose the date with
  `mailroom-admin auth-report --authserv`, which prints each id's first and last
  topmost date and the notice counts under the rule you configured (ADR-035).

  **What the notice shows.** With an id configured: a header from a trusted id
  shows its verdicts; any other inbound mail reads "unauthenticated", whether it
  carries no `Authentication-Results` or only ones from ids you did not name (a
  header the sender can write must not turn a warning off). Mail that is locally
  authored or in Sent or Drafts shows nothing; a From of your own address does
  not count, because the sender writes it. Bounded entries never apply to mail
  imported from an archive.

- **`MAILROOM_LOCAL_DKIM` — Mailroom's own DKIM check (default on).** Mail that
  arrives with no `Authentication-Results` header from a trusted id (see above)
  has its DKIM signatures verified by Mailroom before the message is stored,
  using DNS (each lookup bounded to 3 seconds, at most 4 lookups and 8 seconds
  per message; a DNS failure stores nothing and is not retried). Only a
  signature that passes with a signing domain aligned to the From domain (the
  same domain, or its organisational parent under the Public Suffix List) is
  shown: the reading pane reads "Verified by Mailroom (DKIM, example.com)"
  instead of "unauthenticated". A failure is never shown. Mail already stored,
  archive imports and a provider's own verdict are untouched. It needs outbound
  DNS from the app container. `0`/`false`/`no`/`off` disables it entirely. The
  `mailroom_local_dkim_total{result}` metric counts outcomes (ADR-036).

**Still not here:** this is a working single-operator mail backend plus web
client, not a finished product. The project's production-readiness tracker lists what has
to be true before an instance carries anyone's primary mail — read it before
deciding this one is ready, and deploy behind TLS and on encrypted volumes
regardless — §0 states both as prerequisites and gives a check for each.
Cookies are marked `Secure` automatically when `MAILROOM_OIDC_REDIRECT_URL`
uses `https://` — there is no `MAILROOM_COOKIE_SECURE` variable, and setting
one does nothing (it was documented here in earlier versions and never existed in the
code; the redirect URL's scheme is the single signal, shared with HSTS).

## 0. Host prerequisites

Two things this stack **cannot do for itself**, and both are invisible when
wrong: nothing fails, nothing logs, and the instance looks identical either way.
They are the host's job — the host provisioning's, for example an Ansible role — so each is
stated here as a prerequisite with a way to check it on a running machine.

### 0a. At-rest volume encryption — `search` is a plaintext copy of the mailbox

Mailroom encrypts the **blob store** itself (spec §9/ADR-004): `blobs` holds
ciphertext, under per-user data keys wrapped by a master the server never sees
in the clear. It does not, and cannot, cover the rest:

| volume | what is in it in the clear | needed |
|---|---|---|
| `search` | The **plaintext of every message**: Tantivy stores the subject and the first 256 KiB of the extracted body verbatim, because highlights are computed over stored fields. Tantivy has no native encryption, and the index directories are protected by nothing but mode `0700` inside one process. | **Required.** |
| `pgdata` | `app.messages` stores `subject`, `snippet` (a body extract), `from_name` and `from_address` as plain columns, plus every address the mailbox has corresponded with. | **Required.** |
| `blobs` | Ciphertext — but also the wrapped key material and the file layout. | Defence in depth. |

Spec §11 is explicit that this is *"a stated deployment requirement"*:
volume/filesystem-level encryption (LUKS/dm-crypt or equivalent) covering the
search index and model files, *"since they contain the same plaintext (incl.
decrypted PGP mail)"*. `internal/search/doc.go` says the same thing from the
code's side — *"THE INDEX CONTAINS THE PLAINTEXT OF ENCRYPTED MAIL … Nothing in
this package can compensate for that, so nothing in this package pretends to."*
That gets sharper when PGP decryption lands: mail the sender encrypted
end-to-end is indexed decrypted, like all the rest.

⚠ `pgdata` is listed as **required** here, not as defence in depth. It was
originally scoped it as the softer of the two; the columns above are why it
isn't. Subjects and a body snippet for the whole mailbox are not metadata.

**Check it — on the host.** `lsblk -s` inverts the tree, printing the device and
everything *beneath* it:

```sh
# Substitute the Compose project name (default `mailroom-deploy`).
src=$(df --output=source /var/lib/docker/volumes/mailroom-deploy_search/_data | tail -1)
lsblk -s -o NAME,TYPE,FSTYPE "$src"
```

The stack is encrypted **iff at least one row has `TYPE=crypt`**. No `crypt`
row anywhere beneath the volume means the plaintext index is on a bare disk.

**Check it — from inside the container**, where there is no `lsblk` and no
`/dev` to speak of. This is the constrained case, and it works:

```sh
docker compose -f deploy/compose.yml exec mailroom \
  grep ' /var/lib/mailroom/search ' /proc/self/mountinfo
#   ... - ext4 /dev/mapper/ubuntu--vg-ubuntu--lv rw

docker compose -f deploy/compose.yml exec mailroom cat /sys/block/dm-0/dm/uuid
#   LVM-<32-char-vg-id><32-char-lv-id>   -> a logical volume:   NOT encrypted
#   CRYPT-LUKS2-<luks-uuid>-<name>       -> a dm-crypt mapping: ENCRYPTED

docker compose -f deploy/compose.yml exec mailroom ls /sys/block/dm-0/slaves/
#   sda3                       -> one layer down; repeat for each dm-* you find
```

The container's `/sys/block/*/dm/uuid` is byte-identical to the host's, so this
is the same measurement, not an approximation of it.

⚠ **`/dev/mapper/` proves nothing.** LVM logical volumes and dm-crypt mappings
live in the same directory. A stock Ubuntu LVM root is
`/dev/mapper/ubuntu--vg-ubuntu--lv` and is **not** encrypted — a check written
as *"is the backing device under `/dev/mapper`?"*, or an eyeball over `df`
output, calls that host encrypted. The discriminator is the `dm/uuid` prefix,
`CRYPT-` versus `LVM-`, and nothing about the path.

⚠ **And the converse: LVM on top of LUKS reports `LVM-` at the top** while being
perfectly well encrypted. So a correct check walks `slaves/` **recursively** and
treats the stack as encrypted if **any** ancestor is a `CRYPT-` mapping.
Reading only the top device is a false negative on a good setup — which is how
a real warning gets dismissed as noisy and then switched off.

⚠ **Nothing enforces this at boot today.** It is a runbook check, and the
commands above are the runbook. A boot-time warning is *feasible* — the
measurement above is exactly the one the server would make, from exactly the
place it would make it — and is tracked separately rather than bundled into a
documentation change.

⚠ **The encrypted set above is `pgdata`, `search`, and `blobs` as VOLUMES — it
is not the whole story once a container writes to disk outside a volume.** Every check above measures a *named volume*, which does live under
Docker's data root (`/var/lib/docker` by default). A container's **writable
layer** — anything a process inside it writes to an ordinary, non-volume path
— is a different thing entirely, and where it lives depends on which storage
driver is in play:

- **Classic overlay2 driver:** the writable layer is also under Docker's data
  root, alongside the volumes, so the checks above happen to cover it too.
- **The containerd image store**, the default on **fresh installs from
  Docker 29 on**: writable layers are containerd **snapshots**, stored under
  **`/var/lib/containerd`** — a separate directory tree that the volume checks
  above never look at. `docker info` reports which one is active:

  ```sh
  docker info --format '{{.DriverStatus}}{{.Driver}}'   # overlay2 build
  docker info | grep -i 'driver-type'                    # containerd build:
  #   driver-type: io.containerd.snapshotter.v1
  ```

  With the containerd image store, check `/var/lib/containerd` the same way
  §0a already checks the volumes:

  ```sh
  lsblk -s -o NAME,TYPE,FSTYPE "$(df --output=source /var/lib/containerd | tail -1)"
  ```

  Encrypted **iff** at least one row has `TYPE=crypt` — same rule as above,
  same recursive-ancestor caveat: check `/var/lib/docker` too, since the two
  roots can sit on different disks.

This was found on a real host: `docker exec <postgres> sh -c 'echo x >
/tmp/probe'` landed at
`/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/<n>/fs/tmp/probe`
— the OS disk, unencrypted, while `/var/lib/docker` sat correctly on LUKS. In
this stack, `mailroom`, `crypto` and `clamd` all run `read_only: true`, so none
of them can write to a writable layer at all: `mailroom` and `clamd` take a
`tmpfs` `/tmp` for their scratch files (a tmpfs never touches storage of either
kind), and `crypto` has no writable path whatsoever. `postgres` cannot run
read-only — it needs its own scratch paths — and is the one service that
still can, which is why it gets the `tmpfs` `/tmp` added in this release.

As of the backup changes below, **Mailroom's own `scripts/backup.sh` no longer
writes a plaintext dump anywhere inside a container** — it streams `pg_dump`
straight to a host file — so this is no longer about Mailroom's backup
path. It is still about **any other tool** that shells into `postgres` and
writes a scratch file: a manual `pg_dump -f /tmp/...`, a debugging session, a
future script that forgets this note. Check both roots before trusting that a
host is "encrypted" on the strength of the volumes alone.

⚠ **A by-hand snapshot must be STREAMED, not written to `/tmp` and copied out.**
`postgres` now mounts a `tmpfs` at `/tmp` (v1.77.0), and `docker cp` cannot read
a file from a tmpfs mount — the dump reports success and the copy then fails
with `Could not find the file`. Use:

```sh
docker exec <postgres> pg_dump -U postgres -d mailroom -Fc > pre-upgrade.dump
docker exec -i <postgres> pg_restore --list < pre-upgrade.dump | grep -c 'TABLE DATA'
```

The second line is the check, not decoration: it is what distinguishes a real
dump from an empty file, and `pg_restore` lives in the container rather than on
the host. Dump as the **superuser** — every table forces row-level security, so
an owner-role dump fails loudly (see §0a of `backup-restore-runbook.md`).

⚠ **Swap is part of the at-rest set, and nothing above checks it.** The
decrypted text of messages lives in process memory — the indexer's input, a
rendered body, a PGP plaintext — and the kernel may page any of
it to swap. An unencrypted swap device quietly undoes the volume encryption for
whatever it held. Either run **without swap** or put it on an encrypted device:

```sh
swapon --show          # no output: no swap, nothing to check
lsblk -s -o NAME,TYPE,FSTYPE "$(swapon --show=NAME --noheadings | head -1)"  # else: want TYPE=crypt
```

### 0b. TLS is external, and three behaviours depend on it silently

The stack terminates **no TLS** and publishes exactly one port, on `127.0.0.1`
by default. That is deliberate: a fleet that already runs a reverse proxy should
not have a second TLS implementation in it. But four things key off the proxy
being there, and each fails quietly:

1. **Cookie `Secure` and HSTS come solely from `MAILROOM_OIDC_REDIRECT_URL`'s
   scheme** — see the note at the end of the section above. An `http://`
   redirect URL produces non-`Secure` session cookies **even behind a working
   TLS proxy**, because the app is never told about the proxy.
2. **`MAILROOM_HTTP_BIND=0.0.0.0` bypasses the proxy** unless the host firewall
   stops it (see the `MAILROOM_HTTP_BIND` note further down). Reaching the
   instance from another machine needs it; production needs the firewall
   alongside it.
3. **The address a signed-in device shows is the proxy's, unless told
   otherwise.** Settings > Security lists each device with the address it last
   connected from. Behind a proxy that is the proxy's address, because the app
   sees only the connection. Set `MAILROOM_TRUSTED_PROXIES` to the proxy's IP or
   CIDR (comma-separated; empty trusts none) and Mailroom reads
   `X-Forwarded-For` from those proxies only: right to left, showing the first
   address that is not itself a listed proxy. A request from any other address
   keeps its own address and its header is ignored, so a client cannot choose
   what it shows. A malformed entry stops startup. The address is display only;
   nothing is allowed or refused because of it.
4. **`/metrics` must not be publicly reachable.** It is unauthenticated by
   design, like `/healthz` and `/readyz`, and the proxy — or the firewall — is
   the only thing that scopes it.

⚠ **What `/metrics` gives away.** The route table in §4 said it *"exposes no
user id, subject or address"* in earlier versions. The middle and the last are true; the
first was not. `mailroom_sync_disabled_accounts{user="…"}` and
`mailroom_sync_lag_seconds{account="…"}` carry **ULIDs** — internal ids, never
addresses — and a ULID's leading bits are its creation timestamp. So an
anonymous scrape yields the number of users and accounts, when each was created,
how far behind sync is, how much writeback is owed, and how many messages each
account sent today. No mail and no addresses; a complete operational profile.

The JMAP timing histograms (`mailroom_jmap_method_duration_seconds{method="…"}`
and `mailroom_jmap_request_duration_seconds`) add no ids: the `method` label is a
JMAP method name from the server's own table, or `unknown`, never a client string.

Search latency is `mailroom_search_duration_seconds{kind="search"|"snippet"}`:
the time one search-index read took — a query's matches, or the highlighting
for one page — and nothing else; the label is one of those two words, never
query text. The web client searches as you type after a fixed pause
(`SEARCH_DEBOUNCE_MS`, 250 ms); read this histogram's p95, for example
`histogram_quantile(0.95, sum by (le) (rate(mailroom_search_duration_seconds_bucket{kind="search"}[1d])))`,
before changing that pause.

The image proxy counts what it declines to serve:
`mailroom_proxy_refusals_total{reason="…"}` and
`mailroom_proxy_fetches_total{result="served"|"cached"}`. The `reason` label is
one of a fixed set of server-side names (for example `egress_refused`,
`upstream_status`, `content_type`, `too_large`, `rate_limited`), never a URL, host
or message id. Each refusal also writes one INFO log line, `image proxy refused`,
carrying that reason and, where relevant, the sender's HTTP status or declared
media type. When the refusal is a failed fetch (the sender answered an error,
timed out, sent something that is not an image or too large, or the egress
guard refused the address), the line also names the sender's `host` — the
registrable domain only (`tracker.example` for `u-123.img.tracker.example`; the
provider's domain, such as `github.io`, under a hosting provider's shared suffix;
`ip-literal` for an address), never the full hostname, path or query, any of which
can carry the recipient's token.
Refusals about who may see an image never name a host. A refusal rate is
`sum(rate(mailroom_proxy_refusals_total[1h])) / (sum(rate(mailroom_proxy_refusals_total[1h])) + sum(rate(mailroom_proxy_fetches_total[1h])))`.
The counters reset on restart, so `mailroom-admin proxy-report` (which reads the
database) does not show them; read them from `/metrics`.

With `MAILROOM_IMAGE_PROXY_PREFETCH` on (the default), images in mail from senders
you allow are fetched when the mail arrives and kept while the message is kept. `mailroom_proxy_prefetch_total{result="fetched"|"already_cached"|"refused"|"empty"}`
counts those images apart from reads (a refused one also writes the log line
above, with `prefetch=true`), and `result="cap_reached"` counts prefetch runs
skipped because the user's kept images reached `MAILROOM_IMAGE_PROXY_PREFETCH_MAX_BYTES`
(16 GiB by default; a WARN says so at most hourly, and nothing kept is deleted). Images kept for a message are evicted by nothing
but the deletion of the last message that shows them, so their size is a gauge:
`mailroom_proxy_cache_bytes{user,kind="referenced"|"unreferenced"}` and
`mailroom_proxy_cache_entries{…}`, refreshed after each cache write and once per
user at start. Only the `unreferenced` half is bounded by the cache cap; the `referenced` half stops growing at the prefetch ceiling.
`mailroom-admin proxy-report` prints the same split and, before anything runs,
how many images the prefetch would fetch for recent mail.

**Verify it with a negative control, not a successful load.** That the site
answers over HTTPS says nothing about whether it *also* answers beside the
proxy. From a second machine:

```sh
curl -sS --max-time 5 http://<host>:8080/healthz    # MUST fail: refused or timed out
curl -sS --max-time 5 http://<host>:8080/metrics    # MUST fail, the same way
curl -sSI https://<public-name>/auth/login | grep -i strict-transport-security
```

and in the browser's devtools, the session cookie must show `Secure`. A
verification that only loads the site over HTTPS and stops there passes
unchanged on a host whose app port is answering the whole LAN beside the proxy
— which is the condition this check exists to catch.

## 1. Get the image

### Released versions are published by CI — prefer these

Pushing a `vX.Y.Z` tag builds the release image and publishes it to the GitHub
Container Registry as `ghcr.io/10thtardis/mailroom:X.Y.Z`. The package is
public, so pulling it needs no registry login. Pull that tag, pin it by digest
(`ghcr.io/10thtardis/mailroom:X.Y.Z@sha256:...`, printed by the publish job and
shown on the package page), and set `MAILROOM_IMAGE` to it.

Three properties worth knowing, because they are what makes a published image
better than one built on the host:

- **It is the same bytes CI tested.** Building on the deployment host produces a
  *different artifact* from the one the test matrix ran against.
- **It only exists if the tag's provenance was proven.** The publish job runs
  `needs: [tag-provenance]`, so an image exists only for a tag whose commit is
  reachable from `main` and already had a green CI run.
- **The version stamp is verified before the push**, by running `--version` on
  the built image and matching it against the tag. An image reporting
  `0.0.0-dev` can never be published.

Publishing uses the workflow's own `GITHUB_TOKEN` (`packages: write` on the
publish job only); there is no registry secret to create.

A final release is also tagged `:latest` as a convenience; a pre-release never
moves it. **Never use `:latest` as a deployment pin.** Deploy an exact version
(ideally by digest) so the running version is a reviewable line in
configuration and a rollback names a specific artifact.

### Building locally

From the repo root (the Dockerfile needs the whole module as build context):

```sh
docker build -t mailroom:test .
```

⚠ A local build for anything but throwaway testing must pass
`--build-arg MAILROOM_VERSION=X.Y.Z`, or the binary reports `0.0.0-dev` and a
running instance cannot say which release it is.

This produces one image containing both `/mailroom` (the server, default
entrypoint) and `/mailroom-migrate` (the schema migrator, run as a one-shot
job below). See the Dockerfile's header comment for the non-root/Alpine
trade-off (a shell is kept deliberately so Docker healthchecks have
something to exec).

## 2. Generate secrets

> ### ⚠ Rebuilding after a disaster? Do not generate these.
>
> The two `openssl rand` lines below mint **new** crypto roots. That is right for
> a first install and wrong for a restore, and the two roots fail differently:
>
> - a wrong **master key** announces itself — `blob: not found`, because blob
>   paths derive from it;
> - a wrong **PGP root** is caught only if the database recorded one. Since
>   migration 00055 the `crypto` service records a check value of its root
>   at its first boot and refuses to start on a different one — it restarts,
>   never turns healthy, and logs `PGP ROOT MISMATCH`. A database that never ran
>   00055 has no record and adopts whatever root it is given. There, a wrong root
>   still announces nothing until somebody signs or decrypts — potentially months
>   later, by which point every stored private key is unopenable and
>   `pgp-export-wrapped` cannot rescue them either.
>
> If you are restoring, install both from escrow instead:
> [`docs/backup-restore-runbook.md`](../docs/backup-restore-runbook.md) step 5,
> and [`docs/key-escrow.md`](../docs/key-escrow.md) → Recovery.

Docker secrets are file-based here (`deploy/secrets/*`), gitignored by
`deploy/secrets/.gitignore` — **never commit real values**. Generate them:

```sh
cd deploy/secrets
openssl rand -hex 24 | tr -d '\r\n' > postgres_password.txt
openssl rand -hex 24 | tr -d '\r\n' > mailroom_owner_password.txt
openssl rand -hex 24 | tr -d '\r\n' > mailroom_app_password.txt
openssl rand -hex 32 | tr -d '\r\n' > mailroom_master_key.txt   # envelope-encryption master key, spec §9/§11
openssl rand -hex 32 | tr -d '\r\n' > mailroom_pgp_root_key.txt # crypto process root, ADR-024 — a SECOND root
openssl rand -hex 32 | tr -d '\r\n' > mailroom_session_key.txt  # signs session cookies — see below
printf '{"accounts": []}' > accounts.json                       # your IMAP/SMTP accounts — see §3
: > mailroom_oidc_client_secret.txt                             # paste your provider's secret in §4; empty for now
```

The `tr -d '\r\n'` matters on Windows/Git Bash, where openssl emits CRLF; a
stray `\r` in a password breaks `psql` auth and DSN parsing downstream (the two
root keys survive it — `crypto.ReadKeyFile` trims — but the passwords do not:
`internal/pgdsn.WithPasswordFile`, which injects a password file's contents
into a connection URL, deliberately strips only trailing `\n` — matching how
the `role-bootstrap-*` steps read the same file with `$(cat ...)` to SET the
role's password — so a trailing `\r` is treated as part of the password on
both ends, and a trailing space would be too). Confirm with `wc -c *.txt` —
you want exactly 48 bytes for the three password files, 64 for each root key,
and 64 for the session key, no more.

**Every file in that list must exist before the first `docker compose up`,
including the ones you have no use for yet** — `mailroom_pgp_root_key.txt` if
you never use PGP, `mailroom_oidc_client_secret.txt` if you are not using OIDC.
An **empty** file is fine and resolves exactly as an unset variable would; an
**absent** one is not.

> ⚠ **A missing secret file is not the clean error you would expect, and this
> section claimed otherwise until it was measured** (Docker 29.5.3, Compose
> 5.1.4). Compose does *not* refuse to start. `docker compose config` exits **0**,
> and the daemon **creates the missing path as a DIRECTORY** — on the host, inside
> `deploy/secrets/`, and bind-mounted as a directory at `/run/secrets/<name>`.
>
> Two things follow. First, the failure lands later and reads oddly: the service
> that wanted the secret fails at boot with *"is a directory"* rather than
> *"missing secret"*. It is still fail-closed — nothing starts with an empty or
> guessed key — but you will be reading the wrong error.
>
> Second, and more annoying: you are now left with a **directory** named
> `mailroom_session_key.txt` (or whatever you forgot), so the obvious fix —
> `openssl rand -hex 32 > secrets/mailroom_session_key.txt` — fails with
> `Is a directory` until you `rmdir` it first.
>
> Create all of them up front and neither happens.

### The session key and the client secret are files, not `.env` variables

`MAILROOM_SESSION_KEY` and `MAILROOM_OIDC_CLIENT_SECRET` were environment
variables in earlier versions, and they are the only two values in the auth block that
are genuinely secret — the issuer, client id and redirect URL are not, and stay
in `.env`. An environment variable is readable by anything that can run `docker
inspect`, read `/proc/<pid>/environ`, or land inside the container; the session
key is the **sole HMAC key for session cookies**, so reading it is enough to
mint a valid session for any user ULID, with no password, no login flow and no
database access.

Rotating the session key (write a new one, restart) invalidates every live
session. That is the intended emergency response and costs nothing worse than a
re-login.

> **Upgrading an instance that had these in `.env`:** move the values into
> `secrets/mailroom_session_key.txt` and `secrets/mailroom_oidc_client_secret.txt`.
> The variables are no longer read. You cannot get this wrong quietly — a
> configured OIDC block with an empty client secret or session key is a hard
> config error at boot (*"OIDC is partially configured"*), never a fall-through
> to the open dev-auth path.

### The two root secrets, and why losing them costs different things

There are now **two** independent roots, and this is deliberate — see ADR-024.
The crypto process holds PGP private keys under its own secret precisely so
that an attacker who has taken the master key still cannot derive them.

| secret | held by | what its loss costs |
|---|---|---|
| `mailroom_master_key.txt` | the `mailroom` server | **the entire mailbox.** It encrypts the stored account credentials and every mail blob, and it seeds the blob *paths*, so without it the blob store is not merely unreadable, it is **unaddressable** — the files cannot even be located. |
| `mailroom_pgp_root_key.txt` | the `crypto` service | **every message ever encrypted to your PGP keys, permanently** — and *quietly*: the mailbox restores, opens and works normally, and only the encrypted mail is gone. The one thing that does object is the `crypto` service itself, which refuses a substitute root once the database has recorded the original. |

**Neither is recoverable, and they are not copies of each other.** Do not point
both variables at the same file "to simplify" — that collapses two roots into
one and deletes the property the separate process exists for, with no error and
nothing visibly different.

Escrow **both**, separately and off this host, before you rely on the instance:
[`docs/key-escrow.md`](../docs/key-escrow.md).

## 3. Declare your mail accounts

`secrets/accounts.json` is the declarative account list. The server reconciles
it at boot: an account whose `name` already exists is **left exactly as it
is** (credentials are not rewritten, `sync_enabled` is not touched), and only
new names are onboarded — so restarting never duplicates or clobbers
anything. Removing an entry does *not* delete the account; deletion stays a
deliberate operator action.

```json
{
  "accounts": [
    {
      "name": "personal",
      "provider_profile": "generic",
      "imap_host": "imap.example.com",
      "imap_port": 993,
      "smtp_host": "smtp.example.com",
      "smtp_port": 465,
      "username": "you@example.com",
      "password": "<app password>"
    }
  ]
}
```

Only `name`, `imap_host`, `smtp_host`, `username`, and `password` are
required; ports default to implicit TLS (993/465), `provider_profile` to
`generic`, and the optional `smtp_username`/`smtp_password` override the IMAP
credentials for providers that issue separate submission logins. Parsing is
strict — an unknown key is a boot error, not a silent no-op.

Onboarding dials IMAP to validate each new account. A failure does **not** stop
the boot, and what it does to the account depends on who failed:

- **The server rejected it** (bad password, refused configuration): the account
  is stored with sync **disabled**. Retrying cannot help, and retrying a wrong
  password can lock the account at the provider. Fix the credentials, then run
  `/mailroom-admin enable-account --user-id <id> --account-id <id>`, which
  re-validates before switching sync back on. This is the only supported way
  out of a disabled account: the boot reconcile matches declared accounts by
  **name** and never re-validates one it already has, so a restart, a container
  replacement or a deploy will not do it.
- **Anything else** (connection, DNS, TLS, timeout): the account stays
  **enabled** and the sync loop retries it with backoff. A blip while the stack
  is coming up costs one pass, not the account.

While any account is disabled, every sync pass names it at WARN, and
`mailroom_sync_disabled_accounts` on `/metrics` is above zero, so the state is
visible without scrolling back to the boot log.

### Sending as aliases is a permission at the PROVIDER

If the mailbox receives at a catchall or derived addresses and you intend to
reply **from** them, the provider must allow this mailbox to send as those
addresses. Receiving at an address and being authorised to send as it are
separate permissions, and a catchall grants only the first. Without the second,
every send from such an address is refused at the envelope sender:

```
mail from: 550 "5.7.1 sender address rejected: not allowed"
```

Mailroom cannot detect this in advance: the identity looks valid, compose accepts
it, and the refusal arrives from the provider after the undo window, where the
Outbox reports it. Grant the permission **per mailbox** — a second mailbox
on the same domain does not inherit it — and prove it with one send from a
derived address to an external recipient that actually arrives.

### The accounts file is additive, not declarative

The paragraphs above already said the load-bearing part — an existing name is
left alone — but it is easy to read past, and an automation that edits this
file to "fix" an account will not get the fix it expects. This is what boot
actually does with each kind of edit:

| you change in the file | what boot does |
|---|---|
| add a whole new account entry | onboarded (IMAP-validated) |
| add `identities` / `wildcard_domains` to an **existing** account | added (never removed) |
| change `password` / `username` on an existing account | **nothing** |
| change `imap_host` / `imap_port` / `smtp_host` / `smtp_port` / `provider_profile` on an existing account | **nothing** |
| change `writeback_enabled` on an existing account | **nothing** |
| remove an entry entirely | **nothing** — the account keeps running |

An Ansible role (or any other deploy automation) driving this file **may
assume**: writing a brand-new account entry and redeploying onboards it, and
adding an identity/wildcard domain to an existing entry and redeploying grants
it. It **must not assume**: that rewriting an existing entry's credentials,
endpoints, or `writeback_enabled` changes anything live, or that deleting an
entry removes the account. All four are silent no-ops by design (a redeploy
must never re-dial a working account or rotate a password out from under a
running sync loop) — the risk is an operator believing a file edit took effect
when it did not.

For each of those no-ops, here is what actually applies it today:

- **Credential rotation** (`username`/`password`): through the web client's
  account settings — JMAP `Account/set` as a connection change, which
  re-validates against IMAP and re-seals the credentials — or from the command
  line with:

  ```sh
  mailroom-admin set-account-credentials --user-id <id> --account-id <id> \
    --from-accounts-file --account <name>   # reads the rotated entry straight
                                             # out of the accounts file
  # or:
  mailroom-admin set-account-credentials --user-id <id> --account-id <id> \
    --password-file /run/secrets/new_password [--smtp-too]
  ```

  Both validate the candidate credential FIRST — IMAP always, and SMTP
  (an AUTH-only dial of the account's SMTP host, nothing is sent) whenever an
  SMTP credential is being written — through the same egress guard as
  `Account/set`'s connection change, and re-seal it only on success; a rejected
  credential leaves the stored one byte-identical and the command exits
  non-zero. Never pass the password on the command line. The command prints,
  before it writes, which of IMAP and SMTP will change (never a username or
  password).

  ⚠ `--password-file` carries one password and no SMTP fields. When the
  account's stored SMTP login equals its IMAP login (the common case) both are
  updated, as `Account/set` does. When the stored SMTP login **differs**
  (its own `smtp_username`/`smtp_password`), `--password-file` **keeps the
  stored SMTP pair unchanged** and says so (`SMTP login KEPT`); add
  `--smtp-too` to write the new password to the SMTP login as well (the SMTP
  username is still kept). `Account/set` has no such protection: it seals the
  IMAP login as the SMTP login too, so rotate such an account with this
  command. `--from-accounts-file` writes the file's own SMTP login (applying
  the smtp-defaults-to-imap fallback) and so already preserves it;
  `--smtp-too` is refused there.
- **Endpoint changes** (`imap_host`/`imap_port`/`smtp_host`/`smtp_port`/
  `provider_profile`): the same `Account/set` connection-change path as
  credential rotation (it re-supplies the whole endpoint block together).
- **Re-enabling a disabled account**: `mailroom-admin enable-account
  --user-id <id> --account-id <id>` (§3 above).
- **`writeback_enabled`**: `Account/set` with `writebackEnabled` — there is no
  `mailroom-admin` equivalent for a *live* account; `add-account --ingest-only`
  only sets it at creation.

  ⚠ **Ingest-only also refuses SENDING, not just IMAP writeback** (ADR-028).
  An account with `writeback_enabled = false`, or the whole instance under
  `MAILROOM_WRITEBACK_DISABLED`, cannot send mail: `Email/set` refuses to
  create a submission for it (`internal/api/submission.go`) and, as a second,
  independent gate, the dispatcher refuses to send it even if one got queued
  another way (`internal/compose/send.go`'s `errIngestOnlyCannotSend`). This
  is deliberately a stricter, separate check from the writeback switch's
  IMAP-side enforcement, not an inherited one — see §2 of
  `docs/production-cutover-runbook.md` for the shadow-period consequences.
- **Removing an account**: also `Account/set` (destroy) — there is no
  `mailroom-admin` command for this either. Deleting the file entry alone does
  nothing.

**`mailroom-admin accounts-report`** is the read-only tool for seeing exactly
where a given file and the running database disagree, instead of having to
hold this table in your head:

```sh
docker compose -f deploy/compose.yml exec mailroom \
  /mailroom-admin accounts-report --user-id <id> --fail-on-drift
```

It reports, per declared/stored account: any of the fields above that differ
(never printing a password either way — only `matches` or `DIFFERS`, with the
remediation above named), an account stored with sync disabled, and identities
the file now declares that have not been added yet. `--fail-on-drift` exits
non-zero when any of that needs action, so it can gate a deploy pipeline; run
without it, it is purely informational.

**Verifying after a deploy that changed the accounts file:** do not rely on
the absence of an ERROR line — a rejected account logs at WARN, not ERROR, and
never stops the boot. Check the actual sync pass instead:

```sh
docker compose -f deploy/compose.yml logs mailroom | grep 'sync pass complete'
# {"...","msg":"sync pass complete","accounts":N,"disabled":0,"synced":N,"failed":0,...}
```

`disabled`/`failed` above zero names a real problem (§3's rejection path,
or a still-drifting rotation this deploy did not actually apply); and then
run `accounts-report --fail-on-drift` as above to see exactly which account
and which field.

## 4. Authentication — OIDC (the default)

The server refuses to start unless exactly one auth path is usable, so it can
never come up with no way to authenticate. **OIDC is the path. Use it for any
instance that will hold real mail.** Copy [`.env.example`](.env.example) to
`.env` next to `compose.yml` (Compose reads it automatically; `.env` is
gitignored) and fill in these — or export them:

```sh
MAILROOM_OIDC_ISSUER=https://auth.example.com/application/o/mailroom/
MAILROOM_OIDC_CLIENT_ID=mailroom
MAILROOM_OIDC_REDIRECT_URL=https://mail.example.com/auth/callback
```

The other two — the **client secret** and the **session key** — do not go in
`.env`; they are files under `deploy/secrets/`, for the reasons in §2:

```sh
cd deploy/secrets
printf '%s' 'the-secret-your-provider-issued' > mailroom_oidc_client_secret.txt
openssl rand -hex 32 | tr -d '\r\n' > mailroom_session_key.txt   # >= 32 bytes
```

`MAILROOM_OPERATOR_SUBJECT` / `MAILROOM_OPERATOR_EMAIL` are **no longer required
under OIDC** — users are provisioned from their login now (see *Who may log in*
below), not from config. They remain honored for two narrow purposes: they still
provision a boot-time user whose **declared-accounts file** (§3) is reconciled at
startup, and they are the fixed identity the dev-auth hatch (§4b) injects. Leave
them unset for a pure multi-user OIDC instance.

⚠ **If you DO set `MAILROOM_OPERATOR_SUBJECT`, it must match your provider's
`sub` claim byte-for-byte, and nothing here can check that for you.**
Getting it wrong is worse than leaving it unset: the boot-time user is created
under the guessed subject and **your declared accounts start syncing mail into
it**, while your real login takes the insert path, collides with the
`email UNIQUE` constraint, and is refused — so you are locked out of an instance
that is busily filling a mailbox you cannot reach. The sign-in page says the
account could not be set up (not that your password was wrong), and the log
names both subjects: `msg="login refused: this email already belongs to another
user" token_subject=… operator_subject=…`. If your provider's email for you
differs from `MAILROOM_OPERATOR_EMAIL`, the login instead **succeeds into a
second, empty user**, and the log says so at WARN (`login created a NEW user`).
Either way, see [*A wrong operator subject*](#a-wrong-operator-subject-recovering-without-losing-the-mail-574)
below.

Authentik's `sub` is an opaque UUID, not your username or email. To discover it
without guessing:

1. Leave both operator variables **unset** and start the stack.
2. Log in once through OIDC. You will be auto-provisioned (see *Who may log in*).
3. Read the subject straight out of the log line that login writes:

   ```sh
   docker compose -f deploy/compose.yml logs mailroom | grep 'login provisioned'
   # time=... msg="login provisioned" subject=<the value you want> user_id=... role=...
   ```

4. Only then set the variables, if you need the declared-accounts reconciliation
   at all, and restart.

### A wrong operator subject: recovering without losing the mail

The obvious fix, setting `MAILROOM_OPERATOR_SUBJECT` to the right value and
restarting, **stops the server from booting.** Boot provisions the operator
under the new subject, and that is a second user whose email the first one
still holds, so it hits the same unique constraint. The user that owns your
mail has to be moved to the real subject first. It keeps its id, and with it
every account and message.

1. **Read both subjects from the log line** of the refused login (or of the
   `login created a NEW user` warning):

   ```sh
   docker compose -f deploy/compose.yml logs mailroom | grep -E 'login refused|login created a NEW user'
   ```

   `operator_subject` is the wrong value you configured; `token_subject` is the
   subject your provider actually issued.

2. **Confirm `token_subject` is YOUR account** in the identity provider's own
   admin view. ⚠ Do not skip this. Anyone who can put your email address on an
   account at the provider can produce that log line, and step 3 hands
   everything the operator owns to whoever holds `token_subject`.

3. **Stop the server and move the user**, with
   [`rebind-operator-subject.sql`](rebind-operator-subject.sql):

   ```sh
   docker compose -f deploy/compose.yml stop mailroom
   docker compose -f deploy/compose.yml exec -T postgres \
     psql -U postgres -d mailroom -v ON_ERROR_STOP=1 \
       -v old='<operator_subject>' -v new='<token_subject>' \
     < deploy/rebind-operator-subject.sql
   ```

   It prints `NOTICE: moved user <id> …` and `COMMIT`. On any refusal it prints
   an `ERROR` saying why, and changes nothing. If your real login had already
   created a second, **empty** user, the script retires that one first
   (disabled, with its subject and email renamed out of the way). It refuses if
   that user owns any accounts or messages, because then there are two real
   users and choosing between them is your decision.

4. **Now** set `MAILROOM_OPERATOR_SUBJECT` to `token_subject` in `.env` and
   start the server (`docker compose -f deploy/compose.yml up -d mailroom`).
   The order matters in this direction too: once the user has moved, a boot
   that still carries the old value provisions a new user under it, collides
   on the email, and refuses to start.

5. **Sign out, then sign in.** A browser that got into the empty second user
   keeps that session until it expires. The `operator ready` line at boot and
   the `login provisioned` line at sign-in now show the same `user_id`.

This exact script is exercised end to end, both shapes and every refusal, by
`internal/store/users_rebind_integration_test.go`.

Register `MAILROOM_OIDC_REDIRECT_URL` with your provider as the client's
redirect URI. The server serves **the path of that URL** as its callback route,
so the route it listens on is by construction the one you registered — they
cannot silently disagree.

A **partially** configured OIDC block (one typo'd variable) is a hard config
error and the server refuses to boot. It is never a fall-through to the open
dev-auth hatch — a typo must not be able to open the instance.

### Signing out now ends the provider session too

From v1.63.0, `/auth/logout` does more than clear Mailroom's cookie. When your
provider publishes an `end_session_endpoint`, Mailroom redirects there carrying
`id_token_hint`, `client_id` and `post_logout_redirect_uri`, so the SSO session
ends at the provider as well. Before this, "sign out" on a shared machine left
the next person able to walk straight back in through the IdP with no password
prompt.

**⚠ One provider-side step, or this quietly degrades.** Most providers reject an
unregistered `post_logout_redirect_uri`. Register this exact URL against the
Mailroom client — it is the origin of `MAILROOM_OIDC_REDIRECT_URL` plus
`/auth/login?notice=signed_out`:

```
https://mail.example.com/auth/login?notice=signed_out
```

(so, for a redirect URL of `https://mail.example.com/auth/callback`.) In
Authentik this goes in the provider's **Redirect URIs** list, which covers both
directions.

There is nothing to configure in Mailroom itself. The URL is derived from
`MAILROOM_OIDC_REDIRECT_URL`'s own origin — the one origin already registered
with your provider — so a deployment whose login works has the best chance of
its logout being accepted too.

**If you skip the registration:** the provider refuses the logout request and
the user lands on a provider error page rather than the sign-in page. Their
Mailroom session is gone either way — the local cookies are cleared *before* any
provider redirect, so no failure downstream can leave the browser signed in.

**If your provider publishes no `end_session_endpoint`**, which is allowed
(RP-initiated logout is an optional part of OIDC), logout stays purely local and
there is nothing to configure. The same fallback applies if the session no
longer holds an ID token (for instance one revoked or expired before logout):
Mailroom will not send you to the provider without `id_token_hint`, because a
hint-less logout request is ignored or downgraded to an "are you sure?" prompt
by most providers — it would look like single sign-out while ending nothing.

**Sessions are server-side.** Each signed-in browser is a record in Mailroom's
database; the cookie holds only a random secret. That means:

- Signing out revokes the record, not just the browser's cookie.
- **Disabling a user in Mailroom ends their sessions on the next request.**
- A session is refused 12 hours after sign-in (`auth.DefaultOutageAllowance`;
  there is no setting for it yet). Renewal against the provider is a later
  release, so until then this is the real upper bound on "how long after the
  provider revokes someone can they still read mail".
- A database error while checking a session answers **503**, not 401, so a brief
  outage does not bounce every open tab to the sign-in page.
- Deploying this release signs everyone out once (old signed cookies are not
  accepted). Rotating `deploy/secrets/mailroom_session_key.txt` no longer ends
  sessions; it only invalidates sign-ins in flight.
- The default OIDC scopes now include `offline_access`, so that a provider which
  honours it issues a refresh token, which Mailroom stores sealed for later
  renewal. Nothing uses it yet.

### The login gate

With OIDC configured, this is the complete unauthenticated surface. There is
nothing else:

| Route | Anonymous request gets | Why |
| --- | --- | --- |
| `/auth/login` | **200 — the sign-in page** | The front door. A real page, not a redirect. |
| `/auth/start` | 302 to your provider | What the "Sign in" button hits (PKCE + state + nonce). |
| *(path of `MAILROOM_OIDC_REDIRECT_URL`)* | 302 back to `/auth/login` | The callback. On failure it lands back on the sign-in page **with the reason** and no session cookie. |
| `/auth/logout` | 302 to `/auth/login?notice=signed_out` | Clearing a cookie must never itself need a session. An *anonymous* request carries no ID token, so it never goes to the provider — see above. |
| `/healthz`, `/readyz`, `/metrics` | 200 | Probes carry no session. `/metrics` exposes no mail, no subject and no address — but it **does** carry user and account **ULIDs** as label values, so it must not be publicly reachable (§0b). |
| `/jmap`, `/jmap/session`, `/.well-known/jmap` | **401** | A 302 is meaningless to a programmatic client (spec §10). |
| `/jmap/download/…`, `/jmap/upload/…` | **401** | These carry mail content and attachments. |
| `/jmap/eventsource/…` | **401** | |
| `/`, `/index.html`, `/assets/…`, **anything else** | 302 to `/auth/login` | The web client is never served to an anonymous visitor. |

This table is not documentation of intent — it is asserted, route by route,
against the real mux, for both auth paths, in `cmd/mailroom/gate_test.go`. A
route added without choosing a side of the gate fails that test.

### Who may log in

**App-binding is the access gate.** Any identity that completes the OIDC flow
for this application is authorized, and is **auto-provisioned on first login**
from the verified token's claims (`email`, and `name`/`preferred_username` for
the display name). Control who reaches Mailroom at your provider — bind the
application to the right users/groups in Authentik — and Mailroom trusts any
successfully-authenticated identity it receives. There is no longer an
allowlist-of-one in Mailroom's own config.

**Admins come from a group.** A member of the **`mailroom-admin`** group (the
`groups` claim) gets the admin role; everyone else is an ordinary user. This is
re-evaluated on **every** login, so adding or removing someone from the group in
your provider takes effect the next time they sign in — no restart, no DB edit.
For this to work the provider must emit a `groups` claim: Mailroom requests the
`groups` scope, and in Authentik you expose group membership by adding a groups
scope mapping to the application. A provider that emits no groups simply yields
no admins.

A login is still refused (no session cookie; the browser lands back on the
sign-in page with the reason) when the token carries **no email** to provision
from, or resolves to a user an admin has **disabled or deleted** — a disabled
user cannot log back in even though your provider still authenticates them.
It is also refused when the token's **email already belongs to a different
user** here (emails are unique): a wrong `MAILROOM_OPERATOR_SUBJECT` (above), a
provider that re-issued a new subject for an existing person, or one user's
email changed at the provider to another user's. The page tells the user it is
a server-side problem, and the log names the subjects involved (never the
email).

Every user gets their own fully-isolated mailbox (per-user RLS, per-user
encryption keys, per-user blob paths and search index). The per-user sync,
indexing and Trash-purge loops fan out over all provisioned users automatically;
a user who signs in for the first time starts syncing within about a minute, no
restart required.

Sign in at `https://mail.example.com/auth/login` (or just open `/` — you will be
sent there).

### Signing in to Gmail and Outlook (OAuth, optional)

Users can connect a Gmail / Google Workspace or Outlook.com / Microsoft 365
mailbox by signing in at the provider, instead of typing an app password
(ADR-046). Outlook.com no longer accepts passwords over IMAP at all, so for
those users this is the only way in. The feature is off until you register
**your own** OAuth client at the provider and give Mailroom its id and secret;
any user of the instance may then connect a mailbox.

**The full, step-by-step guide is
[docs/oauth-providers.md](../docs/oauth-providers.md)** (registering the Google
and Microsoft apps, every variable, the redirect URIs, troubleshooting). In
short: set `MAILROOM_OAUTH_GOOGLE_CLIENT_ID` / `_SECRET` and/or
`MAILROOM_OAUTH_MICROSOFT_CLIENT_ID` / `_SECRET` (all honor `_FILE`; keep the
secrets in files), register `https://mail.example.com/oauth/callback/google`
and `/oauth/callback/microsoft` at the provider (the boot log prints the exact
values as `redirect_uri`), and restart. A provider is offered only when both
its id and secret are set; one without the other refuses to boot.

> In this release a connected Gmail account is **held with sync off** (it
> connects, its health says `gmail_sync_pending`) until Mailroom's Gmail sync
> model is switched on in a later update. Outlook / Microsoft 365 accounts sync
> straight away.

## 4b. Dev-auth — the INSECURE escape hatch

> **Setting `MAILROOM_DEV_AUTH_SUBJECT` turns authentication OFF.** Not
> "weakens" — off. There is no login, no session, no cookie: every request that
> reaches the port, from anyone, is the operator, and reads and writes their
> mail. **Never put real mail on an instance running this, and never expose one
> to an untrusted network.**

Honored **only** when the OIDC block is entirely absent:

```sh
MAILROOM_DEV_AUTH_SUBJECT=dev-operator
MAILROOM_DEV_AUTH_EMAIL=you@example.com
```

The server then warns on every boot (`auth.DevAuthWarning`, greppable), and
`/auth/login` renders a page that says the instance has no authentication rather
than pretending to offer a sign-in. It exists to exercise the stack before you
have a provider wired up. Switching to OIDC is the gate: do it before any real
mail arrives.

## 5. Bring the stack up

```sh
docker compose -f deploy/compose.yml up -d
```

This runs, in order (see `deploy/compose.yml`'s header comment for the full
rationale):

1. `postgres` — Postgres 18, empty `mailroom` database (no schema.sql
   mount: **migrations are the schema source of truth** for this
   deployment, not `db/schema.sql`'s init-mount path used by the repo-root
   dev `docker-compose.yml`).
2. `role-bootstrap-owner` — creates the LOGIN `mailroom_owner` role
   `mailroom-migrate` needs (schema.sql/migrations create it `NOLOGIN` by
   default).
3. `migrate` — runs `mailroom-migrate up`, applying every migration under
   `db/migrations/` as `mailroom_owner`. This is what actually creates
   `mailroom_app`/`mailroom_admin` and the whole schema + RLS policies.
4. `role-bootstrap-app` — `ALTER ROLE mailroom_app LOGIN PASSWORD ...` (only
   possible now that migrate has created the role).
5. `mailroom` — the server, once the app role exists and Postgres is
   healthy.

Watch it come up:

```sh
docker compose -f deploy/compose.yml ps
docker compose -f deploy/compose.yml logs -f mailroom
```

Every command in this guide names a **service** (`postgres`, `mailroom`,
`crypto`, `clamd`, …) through `docker compose exec`/`logs`/`ps`, which works no
matter what the Compose **project** name is. If you ever need the actual
**container** name instead — a raw `docker exec`/`docker cp` outside Compose,
as [`docs/backup-restore-runbook.md`](../docs/backup-restore-runbook.md) does
to take a backup — it follows `<project>-<service>-1`. The default project is
`mailroom-deploy` (`compose.yml`'s `name:` key); `-p <name>` overrides it.
`docker compose -f deploy/compose.yml ps` shows the real names for whichever
project you actually used, so confirm rather than assume.

## 6. Verify

⚠ **The port below is `MAILROOM_HTTP_PORT`, not always 8080.** `compose.yml`
publishes on `${MAILROOM_HTTP_PORT:-8080}` (8080 if unset), but
`deploy/.env.example` sets it to `8090`, so an instance deployed from an
unmodified copy of that file answers on 8090, not 8080. The examples below use
the shell fallback form so they work either way in a shell where `.env` has
been sourced, or fall back to 8080 when it has not — check which port your
`.env` actually set rather than trusting either number blindly.

```sh
PORT="${MAILROOM_HTTP_PORT:-8080}"
curl -s -o /dev/null -w '%{http_code}\n' "http://localhost:$PORT/healthz"   # -> 200
curl -s -o /dev/null -w '%{http_code}\n' "http://localhost:$PORT/readyz"    # -> 200 once migrate + Postgres are up
curl -s "http://localhost:$PORT/metrics" | head                             # Prometheus text format
```

`/readyz` returning 200 means the server did a real round-trip against
Postgres through `Store.SystemTx` — i.e. the whole chain (roles, migration,
network) worked, not just that the process started. All three probes are
unauthenticated by design (a health checker carries no session).

The crypto process (ADR-024) is a separate service with a separate probe. It
has no port — it speaks only over its Unix socket — so the check runs inside
the container:

```sh
docker compose -f deploy/compose.yml exec crypto /mailroom-crypto -healthcheck   # -> ok
docker compose -f deploy/compose.yml ps crypto                                   # -> healthy
```

A socket that answers also means the PGP root passed its boot check: the
process compares its root with a check value the database recorded, and exits
before listening if they differ. Its log says which of the three states it
reached:

```sh
docker compose -f deploy/compose.yml logs crypto | grep -o '"outcome":"[a-z]*"'
#   "outcome":"recorded"  first boot against this database: the root is now pinned; nothing was verified
#   "outcome":"verified"  the root matches the one this database recorded
#   "outcome":"mismatch"  the wrong root: the container restarts and never turns healthy
#                         (docs/backup-restore-runbook.md, step 5)
```

The scanner (ADR-027) is the same shape — no port, a Unix socket, its own probe
— and is worth checking separately because a `clamd` that is merely still
downloading signatures looks identical to a broken one from the app's side:

```sh
docker compose -f deploy/compose.yml ps clamd                    # -> healthy (~70 s from a cold start)
docker compose -f deploy/compose.yml exec clamd clamdscan --ping 1   # -> PONG

# ⚠ The isolation that makes the sidecar worth having. Both must fail.
docker compose -f deploy/compose.yml exec clamd wget -qO- -T3 https://example.com   # -> Network unreachable
docker compose -f deploy/compose.yml exec clamd ip route                            # -> no default route

# And the `unknown` state must actually be producible — it is not, by default.
docker compose -f deploy/compose.yml exec clamd sh -c 'clamconf | grep -iE "^AlertEncrypted =|^AlertExceedsMax|^StreamMaxLength"'
#   AlertEncrypted = "yes"   AlertExceedsMax = "yes"   StreamMaxLength = "104857600"
```

⚠ **After a deploy that recreated both clam containers, compare the loaded
signatures with the ones on disk, and reload if they differ.** clamd takes
about 70 seconds to load its database. If freshclam publishes an update during
that load, clamd finishes loading the older file, and its periodic SelfCheck then
treats the newer one as already seen: clamd scans one update behind until the
next update changes the directory again, typically within a day. Seen in practice, when daily 28132 landed during the load of 28131.

```sh
docker compose -f deploy/compose.yml exec clamd clamdscan --version                       # what clamd has LOADED
docker compose -f deploy/compose.yml exec clamav-signatures sh -c 'sigtool --info /var/lib/clamav/daily.c?d | grep Version'   # what is ON DISK (.cld or .cvd)
# If they differ:
docker compose -f deploy/compose.yml exec clamd clamdscan --reload
```

A reload briefly doubles clamd's anonymous memory, which is what the sizing rule
below provides for. `mailroom_scanner_signature_published_timestamp_seconds` reports
the database clamd has actually loaded, so a stale load also shows there.

⚠ With those two alerts off — which is stock — an encrypted archive and an
archive too deep to open both return plain `OK`: a clean verdict on a file
nothing looked inside. That is not a tuning preference, it is the difference
between four verdict states and three.

Then confirm the app is actually using it, from its own boot log:

```sh
docker compose -f deploy/compose.yml logs mailroom | grep "attachment scanning"
#   -> "attachment scanning is ON; a scanner outage will PAUSE sync ..."
```

Two more things worth confirming once, because both are the kind of mistake that
leaves everything working while quietly removing the point of the service:

```sh
# The crypto process must NOT have the master key. It DOES also carry
# mailroom_app_password — that is the database login (mailroom_app), not the
# master key, and it is expected (see below): the crypto process reads/writes
# app.user_keys under the same RLS as the server, so it needs a DB credential
# of its own.
docker compose -f deploy/compose.yml exec crypto ls /run/secrets/   # -> mailroom_pgp_root_key, mailroom_app_password

# The server must NOT have the PGP root.
docker compose -f deploy/compose.yml exec mailroom ls /run/secrets/ # -> no mailroom_pgp_root_key
```

Keys are stored in `app.user_keys` under the same row-level security as your
mail, so the crypto service needs the database as well as its own secret. It
connects as `mailroom_app`, exactly like the server — **the database is not the
boundary.** The server can read the sealed key bytes and gets ciphertext it
cannot open, because it does not have the root that seals them.

⚠ There is no in-memory fallback if the DSN is missing: the service refuses to
start. That is deliberate. A crypto process running without its database would
report itself healthy, create keys that vanish on restart, and lose the
database's half of account isolation at the same time — all silently.

`mailroom-admin pgp-generate` and friends run inside the `mailroom` container
and talk to the crypto process over the shared socket, and — in current releases — so does an ordinary send: signing happens at Submit
(`internal/compose/compose.go`) for any identity with a bound signing key,
rewriting the stored message before it is queued. So a `crypto` container that
is down now DOES cost you mail for those identities: signing (and, once it
ships, decryption) fails rather than silently sending unsigned — there is no
fallback that reads the key in the server, because a fallback like that is the
boundary deleting itself under load. An identity with no bound key is
unaffected either way; mail delivery and reading in general are unaffected.

## 6b. Publishing a key (WKD)

Publishing makes a key findable by anyone who knows the address — which is the
point, and also the reason it is an explicit act rather than a default.

```sh
C=deploy/compose.yml
docker compose -f $C exec mailroom /mailroom-admin pgp-generate   --user-id <ULID> --email you@example.com
docker compose -f $C exec mailroom /mailroom-admin pgp-bind   --user-id <ULID> --key-id <KEY> --identity-id <IDENTITY>
docker compose -f $C exec mailroom /mailroom-admin pgp-list --user-id <ULID>

# Nothing is public until this step. Until it runs, every lookup 404s.
docker compose -f $C exec mailroom /mailroom-admin pgp-publish   --user-id <ULID> --key-id <KEY>
```

`pgp-publish` prints the exact URLs a correspondent's client will request —
**advanced first** — so you can check them against the DNS you control. Use
`pgp-unpublish` to withdraw.

Then set `MAILROOM_WKD_DOMAIN` in your `.env` to the mail domain and restart.
A published key is served at the hashed path a client asks for, and the same
hash addresses a human-readable page:

```sh
PORT="${MAILROOM_HTTP_PORT:-8080}"   # see §6 — deploy/.env.example sets 8090
curl -sI "http://localhost:$PORT/.well-known/openpgpkey/policy"      # -> 200
curl -s  "http://localhost:$PORT/.well-known/openpgpkey/hu/<hash>" | file -
#   -> "OpenPGP Public Key" / "data" — NOT ASCII text
```

⚠ **If that last command says "ASCII text", the endpoint is broken** even
though it returned 200. WKD serves the raw key packets, and ASCII armor is the
common mistake — every server-side signal stays green while no mail client
accepts the key.

### Backing up and restoring a key (`pgp-export-wrapped`, `pgp-import`)

A **generated** key exists nowhere but in Mailroom. Its private half is stored
sealed under `mailroom_pgp_root_key.txt` (§2), so the backup of the database and
that root is one way to keep it (restore runbook). This is the other way: a
portable copy that **neither** of those is needed to open. Take one for every key
you generate, before you rely on it. Once decryption ships, the only thing
the key protects is mail sent to it, so losing it loses that mail.

**What it exports.** One ASCII-armored OpenPGP private key (primary and
subkeys) for one key id, with its secret packets encrypted under a passphrase
**you** choose. Mailroom's own roots do not open it, and Mailroom does not store
or recover the passphrase. `gpg --import` reads it too.

**Where the wrapping key comes from.** From a file you write: the first line is
the passphrase, at least 12 characters (trailing newlines are stripped, leading
spaces are kept). It is never an argument or an environment variable, and the
output is never printed.

```sh
C=deploy/compose.yml
umask 077
# 1. Choose a passphrase (a password manager's generator is fine) and put it in a file.
printf '%s' '<a passphrase of 12+ characters>' > /tmp/pgp-pass.txt

# 2. Export. --out must not exist yet; the command refuses to overwrite.
docker compose -f $C cp /tmp/pgp-pass.txt mailroom:/tmp/pgp-pass.txt
docker compose -f $C exec mailroom /mailroom-admin pgp-export-wrapped \
  --user-id <ULID> --key-id <KEY> \
  --passphrase-file /tmp/pgp-pass.txt --out /tmp/pgp-key-<KEY>.asc

# 3. Bring the file out, then delete both copies inside the container.
docker compose -f $C cp mailroom:/tmp/pgp-key-<KEY>.asc ./pgp-key-<KEY>.asc
docker compose -f $C exec mailroom rm -f /tmp/pgp-pass.txt /tmp/pgp-key-<KEY>.asc
shred -u /tmp/pgp-pass.txt 2>/dev/null || rm -f /tmp/pgp-pass.txt

# 4. Check it opens BEFORE relying on it (asks for the passphrase).
gpg --import ./pgp-key-<KEY>.asc
```

**How to store it.** Keep the exported file and the passphrase in **different
places** (for example the file in off-host backup storage and the passphrase in
a password manager). Either alone is useless to a thief, and both together are
the private key. Do not keep either on this host.

**Restoring.** On a fresh instance (or after the key was deleted), put the file
and a passphrase file where the container can read them, then:

```sh
docker compose -f $C cp ./pgp-key-<KEY>.asc mailroom:/tmp/pgp-key.asc
docker compose -f $C cp /tmp/pgp-pass.txt mailroom:/tmp/pgp-pass.txt
docker compose -f $C exec mailroom /mailroom-admin pgp-import \
  --user-id <ULID> --file /tmp/pgp-key.asc --passphrase-file /tmp/pgp-pass.txt
docker compose -f $C exec mailroom rm -f /tmp/pgp-key.asc /tmp/pgp-pass.txt

# Then re-attach it to the address it signs for, and re-publish if it was published.
docker compose -f $C exec mailroom /mailroom-admin pgp-list --user-id <ULID>
docker compose -f $C exec mailroom /mailroom-admin pgp-bind --user-id <ULID> --key-id <NEW-KEY> --identity-id <IDENTITY>
docker compose -f $C exec mailroom /mailroom-admin pgp-publish --user-id <ULID> --key-id <NEW-KEY>
```

`pgp-import` unlocks the key once and stores it like any other import, sealed
under the instance's PGP root. The fingerprint is unchanged, and the key id is
new (use the one `pgp-import` prints). A wrong passphrase gives one error,
"could not unlock the key"; without `--passphrase-file` a protected file is
refused. Importing a key that is already held changes nothing.

**What is lost without a backup.** If the database or the PGP root is lost and
you have no exported file, the key is gone permanently: nothing can sign as it,
and mail already encrypted to it cannot be opened, now or later. A published
key must be replaced and re-published, and correspondents must fetch the new
one. Keys you imported yourself are different: you still hold the original.

### ⚠ The DNS half, which this container cannot do for you

A correspondent's client tries the **advanced** method first:

```
https://openpgpkey.example.com/.well-known/openpgpkey/example.com/hu/<hash>
```

and only then the **direct** one:

```
https://example.com/.well-known/openpgpkey/hu/<hash>
```

So an `openpgpkey.` subdomain that exists but does not serve keys correctly
**breaks every lookup**, including the ones the direct method would have
answered. It must be **absent or fully correct** — there is no working middle,
and a half-configured subdomain fails in the direction where you believe your
key is published and nobody can fetch it.

Whichever method you choose, the proof is a real client, not a `curl`:

```sh
gpg --auto-key-locate clear,wkd --locate-keys you@example.com
```

That is the only check that exercises the path, the encoding and the content
type the way a correspondent's software will.

### Key discovery, in both directions (v1.64.0)

Publishing is only half of it. From v1.64.0 Mailroom also **exchanges keys in
band**, which needs no configuration and no DNS:

- **Outbound** — mail sent from an identity with a bound key carries an
  `Autocrypt:` header holding that key's public half. ⚠ Only **designated**
  identities do: the key is looked up through the identity's `default_key_id`,
  so a catch-all's derived aliases advertise nothing, structurally.
- **Inbound** — a correspondent's `Autocrypt:` header is read at ingest and
  their key filed against their contact. ⚠ The header's claimed address must
  equal the message's `From`, or the key is discarded: without that check a
  sender could plant a key under somebody else's address.

⚠ **Mailroom does not advertise `prefer-encrypt=mutual`,** and that is
deliberate rather than an oversight. It would tell every Autocrypt-aware
correspondent to encrypt by default, and Mailroom cannot decrypt yet — the body
would be unreadable and every attachment withheld as `unknown`. Turning it on
belongs with the decryption work.

To go looking rather than waiting, sweep your contacts' own domains:

```sh
/mailroom-admin key-discover --user <ULID>            # report only
/mailroom-admin key-discover --user <ULID> --apply    # store what it finds
```

Every lookup goes through the guarded egress client, because a WKD URL's host
comes from an email address a stranger chose. Expect a near-empty result on most
mailboxes; the report prints how many addresses it *tried*, so a zero can be
read as "nobody publishes" rather than "the fetcher is broken".

### What publishing does NOT do

Withdrawing a key (`wkd_published = false`) stops new lookups. It cannot recall
a key someone already fetched and cached — revocation is the mechanism for
"stop trusting this", and it is a different act.

The mail surface is behind the gate, so an anonymous request gets nothing:

```sh
BASE="http://localhost:${MAILROOM_HTTP_PORT:-8080}"   # see §6 — deploy/.env.example sets 8090
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

code $BASE/jmap/session                         # -> 401
code $BASE/.well-known/jmap                     # -> 401
code $BASE/jmap/download/acct/blob/name.pdf     # -> 401  (mail content)
code -X POST $BASE/jmap/upload/acct/            # -> 401
code $BASE/jmap/eventsource/                    # -> 401
code $BASE/                                     # -> 302  -> /auth/login
code $BASE/auth/login                           # -> 200  (the sign-in page)
```

`scripts/gate-e2e.sh` runs exactly this — plus full Authorization-Code logins
against a real OIDC provider in a container — against the built image, and also
proves the multi-user model: **a first-time subject is auto-provisioned and
reaches `/jmap/session`; a subject in the `mailroom-admin` group gets
`role=admin`; two different subjects each get their own isolated user; and the
dev-auth hatch still works.**

Sign in at `http://localhost:${MAILROOM_HTTP_PORT:-8080}/auth/login` (§6);
afterwards `/jmap/session` returns the JMAP Session object listing your
account and capabilities.

The boot log tells you the rest: `login provisioned` with the user id and role
on each sign-in, one line per declared account (`declared account onboarded` /
`account already configured`), and then `user fan-out: starting per-user loops`
and `sync pass starting` / `account synced` from the per-user background loops.

```sh
docker compose -f deploy/compose.yml logs mailroom | grep -E 'login provisioned|account|sync pass|fan-out'
```

Out-of-band account surgery (fix a password, pause an account, force a sync):

```sh
docker compose -f deploy/compose.yml exec mailroom /mailroom-admin list-accounts --user-id <id>
docker compose -f deploy/compose.yml exec mailroom /mailroom-admin sync-once --user-id <id>
```

Change the published port with `MAILROOM_HTTP_PORT` (default `8080`), e.g.
`MAILROOM_HTTP_PORT=9090 docker compose -f deploy/compose.yml up -d`.

The port is published on **`127.0.0.1` by default** (`MAILROOM_HTTP_BIND`).
Earlier versions bound every interface, which meant that adding a TLS terminator
in front did not stop the app being reachable directly beside it — the proxy,
and anything it enforces, was bypassable. A reverse proxy on the same host
still works unchanged. **To reach the instance from another machine, set
`MAILROOM_HTTP_BIND=0.0.0.0`** (or a specific interface) in `.env`.

## 6c. Memory limits

Every service in `compose.yml` carries an explicit `mem_limit` (`postgres` also
gets `mem_reservation`, a floor rather than only a ceiling), sized from cgroup
v2 `memory.peak` measured on a running instance rather than guessed. Seven are
environment-overridable — set them in `.env` rather than editing
`compose.yml`, same as everything else in that file:

| variable | default | protects |
|---|---|---|
| `MAILROOM_POSTGRES_MEM_LIMIT` | `1g` | `postgres` — its peak is mostly reclaimable page cache. |
| `MAILROOM_POSTGRES_MEM_RESERVATION` | `256m` | A floor for `postgres` so it isn't the first thing squeezed when other containers compete for host memory. |
| `MAILROOM_CRYPTO_MEM_LIMIT` | `384m` | `crypto` — key operations and signing need little memory. Raised from 128m with decrypt on open (ADR-037): one decrypt holds the request, ciphertext, plaintext (capped at 32 MiB) and response at once, and up to two run together. Not yet measured on real encrypted mail. |
| `MAILROOM_CLAMAV_SIGNATURES_MEM_LIMIT` | `2g` | `clamav-signatures` — measured peak 910 MiB, because freshclam's default `TestDatabases` loads the new signature DB to verify it before publishing, a clamd-sized load rather than freshclam's own footprint. |
| `MAILROOM_CLAMD_MEM_LIMIT` | `3500m` | `clamd` — see the sizing rule below. |
| `MAILROOM_SERVER_MEM_LIMIT` | `1g` | `mailroom` — a small corpus needs a fraction of this. |

(`role-bootstrap-owner`, `role-bootstrap-app` and `migrate` also carry
`mem_limit`/`pids_limit`, but as fixed values, not variables — they are
short-lived one-shot jobs that don't scale with anything.)

**`mailroom` and `postgres` scale with the mailbox(es) synced, not with
anything in this repo.** A real corpus is larger than the small test corpus the
defaults above were sized against — measure the real one with
`mailroom-census` and size the deployment host from it, then set
these two rather than trusting the default.

**The clamd sizing rule: reload peak, plus its tmpfs.** A signature reload
roughly **doubles clamd's anonymous memory**, because `ConcurrentDatabaseReload`
(on by default) builds the new engine while the old one keeps serving scans.
Measured on a standalone `clamav/clamav:1.5.4` container with this directory's
`clamd.conf`:

| limit | outcome of `clamdscan --reload` | anon | page cache | swap |
|---|---|---|---|---|
| none | reloaded | 991 MiB → **1,989 MiB** | ~113 MiB | 0 |
| 1400m, swap available | reloaded, **by swapping** | capped ~1,455 MiB | ~113 MiB | **830 MiB** peak |
| 1400m, no swap | **OOM-killed** | capped ~1,438 MiB | ~113 MiB | 0 |

So the doubling is anonymous memory, not reclaimable cache. On top of it,
`/tmp` in the `clamd` container is a 512 MiB tmpfs, and tmpfs pages are charged
to the **same** memory cgroup. clamd writes INSTREAM data and decompressed
archive members there (`StreamMaxLength 100M`, `MaxScanSize 400M` in
`clamd.conf`). The ceiling therefore has to clear **~2 GiB of reload plus a
worst-case 512 MiB tmpfs**. The 3500m default does, with headroom.

⚠ **A clamd limit that is too tight is not self-healing, and depends on
whether the host has swap.** With swap, the reload survives but pages hundreds
of MB out, and scans slow down while it does. Without swap, the reload is
OOM-killed. **The container then stays `running` with a clamd that no longer
answers** (`PING timeout exceeded; No response from clamd`, `RestartCount` 0),
so `restart: unless-stopped` never fires. Because scanning is fail-closed (see
*"It is fail-closed, so a dead scanner stops mail"* above), **mail sync stays
paused until someone restarts `clamd` by hand.** Nothing is lost, but nothing
recovers either. And signature updates arrive on their own schedule, so the
failure appears hours after the limit was set, not at `up`. Do not set
`MAILROOM_CLAMD_MEM_LIMIT` below ~2.5 GiB.

## 6d. Statement timing (pg_stat_statements)

The postgres service loads `pg_stat_statements`, and `role-bootstrap-owner`
creates it in the `mailroom` database. Postgres then keeps, for each
normalised statement, how many times it ran and how long it took in total.
It stores the statement text with every literal replaced by `$1`, `$2`, …,
never the values, so no mail content is recorded.

⚠ Keep `pg_stat_statements.track_utility=off`. That normalisation covers only
ordinary queries. Utility statements such as `ALTER ROLE … PASSWORD '…'`,
which the bootstrap steps run on every `up`, would otherwise be recorded
verbatim, passwords included.

To see where the time goes, read it as the superuser. This is read-only:

```sh
docker exec -i <postgres-container> psql -U postgres -d mailroom -X <<'SQL'
SELECT calls, round(total_exec_time)::bigint AS total_ms,
       round(mean_exec_time::numeric, 1) AS mean_ms,
       round(max_exec_time::numeric, 1) AS max_ms,
       left(regexp_replace(query, '\s+', ' ', 'g'), 120) AS statement
  FROM pg_stat_statements
 WHERE dbid = (SELECT oid FROM pg_database WHERE datname = 'mailroom')
 ORDER BY total_exec_time DESC
 LIMIT 20;
SQL
```

The counters cover everything since the last reset. To measure one action,
reset them, do the action, then read again:
`SELECT pg_stat_statements_reset();`. The reset discards history, so treat it
as a measurement step, not routine housekeeping.

On an existing installation, two things are needed. Postgres must restart
once to load the library; `docker compose up -d` does this when the compose
file has changed. Then `role-bootstrap-owner` must run again to create the
extension, which `docker compose up -d` also does. Until both have happened,
the query above fails with `relation "pg_stat_statements" does not exist`
or `pg_stat_statements must be loaded via shared_preload_libraries`.

## 7. Tear down

```sh
docker compose -f deploy/compose.yml down       # keep data (pgdata + blobs volumes)
docker compose -f deploy/compose.yml down -v    # also wipe pgdata, blobs, and search
```

**Upgrading an existing instance from an older release:** the `role-bootstrap-*` steps
used to compose a password-bearing DSN and write it to a named volume
(`runtime-secrets`) shared with `migrate`/`crypto`/`mailroom`. That volume no
longer exists — those processes now read a password from a Docker secret and
assemble the connection string in memory instead (see the note below) — but
an instance deployed before this change still has the old volume sitting on
disk with a stale plaintext copy of the owner and app DSNs on it. Remove it
after upgrading:

```sh
docker volume rm <project>_runtime-secrets   # e.g. mailroom-deploy_runtime-secrets
```

(`docker volume ls` shows the actual name if the project name differs from
the default.) It is safe to remove any time after the upgraded stack has come
up healthy — nothing reads it any more.

## Notes, trade-offs, and follow-ups

- **Schema vs. migrations:** `db/schema.sql` and `db/migrations/*.sql` are
  proven equal in CI (`scripts/migrate-check.sh`) but are two independent
  entry points to the same end state, not a chain. This deployment uses
  migrations exclusively (see `deploy/compose.yml`'s header comment) so
  there is exactly one operational procedure, and so that every future
  schema change after this initial deploy uses the same path a fresh deploy
  does.
- **No DSN is ever composed onto disk:** the connection URLs
  (`MAILROOM_MIGRATE_DATABASE_URL`, `MAILROOM_DATABASE_URL`) carry no
  password at all. Each process that needs one — `migrate`, `crypto`,
  `mailroom` — reads its role's password separately from the SAME Docker
  secret file the corresponding `role-bootstrap-*` step used to set that
  role's password (`MAILROOM_MIGRATE_DATABASE_PASSWORD_FILE` /
  `MAILROOM_DATABASE_PASSWORD_FILE`), and assembles the two into a
  connection string in memory, right before connecting
  (`internal/pgdsn.WithPasswordFile`). Nothing is ever written back to disk,
  so there is no second plaintext copy of a DB credential beyond the secret
  file itself.
- **`role-bootstrap-*` containers run as `postgres`, not root:** in older releases
  they ran as the image's default root user because they needed write
  access to a freshly created volume (owned root:root until something
  chowned it) to compose the DSN above onto. With that volume gone, both
  steps only talk to Postgres and read read-only secret files, so they run
  as the same non-root user the `postgres` image itself uses — the least
  privilege they could plausibly need. They are also short-lived one-shot
  jobs, not the long-running server, and `cap_drop: [ALL]` +
  `no-new-privileges` on both remove the capabilities that would make root
  meaningfully dangerous even before this change. The `mailroom` server
  itself runs as a fixed non-root user (uid 100 in the image),
  `read_only: true`, `cap_drop: [ALL]`.
- **The role's password never appears in SQL text:** the bootstrap steps
  pass it to `psql` as a `-v pw=...` variable and reference it as `:'pw'`
  inside the SQL (a properly quoted string literal), rather than
  interpolating it into a `-c "..."` command string. That is also what makes
  a password containing `'`, `\`, or other SQL metacharacters safe, which
  string interpolation was not.
- **No TLS between containers:** Postgres traffic is unencrypted on the
  compose-internal network (`sslmode=disable` in the generated DSNs). That
  network isn't published to the host beyond `mailroom`'s port 8080, but a
  production deployment should terminate TLS (e.g. the Caddy proxy called
  out in spec §16) in front of `mailroom` and consider `sslmode=require` (or
  mTLS) to Postgres if it's ever reachable off-box.
- **`mailroom_admin`** is left `NOLOGIN` — nothing in the current server
  binary uses the admin-plane role yet, so it isn't bootstrapped here.
  Revisit once an admin plane exists.
- **Compose profiles** (`minimal` / `enhanced(+Ollama)`, spec §16) aren't
  modeled yet — this stack is deliberately just Postgres + migrate +
  server. Add profiles once the search index / Ollama-backed intelligence
  tier / web client are real services to gate.
