<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Security

What the host must provide, and how the scanner, the crypto process and the root keys fit together.

**On this page:** [Encrypt the volumes](#encrypt-the-volumes) · [Terminate TLS in front](#terminate-tls-in-front) ·
[Trust sender authentication results](#trust-sender-authentication-results) ·
[Attachment scanning with ClamAV](#attachment-scanning-with-clamav) ·
[The crypto process](#the-crypto-process) · [Root keys and escrow](#root-keys-and-escrow)

The stack runs every service non-root with `cap_drop: [ALL]` and `no-new-privileges`. `mailroom`,
`crypto` and `clamd` are read-only containers. The `backend` network is internal; only `mailroom`
(and the signature updater) can reach the internet.

## Encrypt the volumes

Mailroom encrypts the blob store itself. It cannot cover the rest, and nothing at boot checks this.
Both items below are the host's job and look identical to the server whether done or not.

| Volume | What is in it in the clear | Needed |
|---|---|---|
| `search` | The plaintext of every message: subject and the first 256 KiB of each body, including mail that arrived PGP-encrypted. The index has no encryption of its own. | **Required** |
| `pgdata` | `subject`, `snippet`, sender name and address for every message, plus every correspondent. | **Required** |
| `blobs` | Ciphertext, plus wrapped key material and file layout. | Defence in depth |

Use LUKS/dm-crypt or equivalent under Docker's data root.

**Check on the host.** The stack is encrypted if at least one row has `TYPE=crypt`:

```sh
src=$(df --output=source /var/lib/docker/volumes/mailroom-deploy_search/_data | tail -1)
lsblk -s -o NAME,TYPE,FSTYPE "$src"
```

(Use your Compose project name if it is not `mailroom-deploy`.)

**Check from inside the container**, where there is no `lsblk`:

```sh
docker compose -f deploy/compose.yml exec mailroom grep ' /var/lib/mailroom/search ' /proc/self/mountinfo
docker compose -f deploy/compose.yml exec mailroom cat /sys/block/dm-0/dm/uuid
#   CRYPT-LUKS2-<uuid>-<name>  -> encrypted        LVM-<ids>  -> a plain logical volume
docker compose -f deploy/compose.yml exec mailroom ls /sys/block/dm-0/slaves/   # one layer down; repeat per dm-*
```

Two traps:

- `/dev/mapper/` proves nothing: LVM volumes and dm-crypt mappings live there together. The
  discriminator is the `dm/uuid` prefix, `CRYPT-` versus `LVM-`.
- LVM on top of LUKS reports `LVM-` at the top yet is encrypted. Walk `slaves/` recursively and
  call the stack encrypted if any ancestor is `CRYPT-`.

**Container writable layers.** With the classic overlay2 driver they sit under `/var/lib/docker`
with the volumes. With the containerd image store (the default on fresh Docker 29+ installs) they
are under `/var/lib/containerd`, which the checks above never look at. `docker info | grep -i
driver-type` shows `io.containerd.snapshotter.v1` on that store. Check it the same way:

```sh
lsblk -s -o NAME,TYPE,FSTYPE "$(df --output=source /var/lib/containerd | tail -1)"
```

`mailroom`, `crypto` and `clamd` are read-only (`mailroom` and `clamd` use a `tmpfs` `/tmp`), so only
`postgres` can write a layer, and it has a `tmpfs` `/tmp` too. Any other tool that writes scratch
files inside `postgres` can still land in a layer.

**Snapshots by hand must be streamed.** `docker cp` cannot read a tmpfs, so write nothing to
`/tmp`. Dump as the Postgres superuser (tables force row-level security) and check it:

```sh
docker exec <postgres> pg_dump -U postgres -d mailroom -Fc > pre-upgrade.dump
docker exec -i <postgres> pg_restore --list < pre-upgrade.dump | grep -c 'TABLE DATA'
```

The second line tells a real dump from an empty file. `scripts/backup.sh` also stages a plaintext
copy on the host while it runs, so its checkout must be on encrypted storage too
([backup runbook](../backup-restore-runbook.md)).

**Swap.** Decrypted text lives in process memory and can page to swap. Run without swap, or put it
on an encrypted device:

```sh
swapon --show          # no output: no swap, nothing to check
lsblk -s -o NAME,TYPE,FSTYPE "$(swapon --show=NAME --noheadings | head -1)"   # want TYPE=crypt
```

## Terminate TLS in front

The stack terminates no TLS and publishes one port, on `127.0.0.1` by default. Put a reverse proxy
in front. Four things depend on it, and each fails quietly:

1. **`Secure` cookies and HSTS** come only from the scheme of `MAILROOM_OIDC_REDIRECT_URL`. An
   `http://` URL gives non-`Secure` cookies even behind a working proxy.
2. **`MAILROOM_HTTP_BIND=0.0.0.0` bypasses the proxy** unless a host firewall stops it.
3. **Device addresses** show the proxy's address unless `MAILROOM_TRUSTED_PROXIES` lists it. Mailroom
   then reads `X-Forwarded-For` right to left from those proxies only. Display only.
4. **`/metrics` must not be public.** See [monitoring](monitoring.md#metrics).

**Verify with a negative control, not a successful page load.** From a second machine:

```sh
curl -sS --max-time 5 http://<host>:8080/healthz    # MUST fail: refused or timed out
curl -sS --max-time 5 http://<host>:8080/metrics    # MUST fail, the same way
curl -sSI https://<public-name>/auth/login | grep -i strict-transport-security
```

In browser devtools the session cookie must show `Secure`. Traffic between containers is not
encrypted (`sslmode=disable` on the internal network). Consider `sslmode=require` if Postgres is
ever reachable off-host.

## Trust sender authentication results

`MAILROOM_AUTHSERV_IDS` lists the `Authentication-Results` ids you trust (comma-separated). It gates
whether a passing DKIM/DMARC result may suppress the "someone is claiming your own domain" warning,
and whether the reading pane's sender-authentication notice shows anything. **Unset, neither reads
anything.**

- The id belongs to the server that did the authentication, normally your mail provider. Read it
  off a delivered message: the first thing in the header value, before the first `;`
  (`Authentication-Results: mx.example.com; dmarc=pass …` means `mx.example.com`). Do not guess.
- Unset or wrong is safe, in opposite directions: the spoofing warning gets noisier and the
  notice goes silent. Nothing can make either show something false. The header is attacker-writable
  unless its id is checked, which is why there is no default.
- A provider that changed its id: `MAILROOM_AUTHSERV_IDS=mx.provider.example,old.provider.example<2026-07-01`
  trusts the old id only for mail whose INTERNALDATE is before that date (00:00 UTC). A malformed
  bound stops the server and `mailroom-admin`. Choose the date with `auth-report --authserv`.
- With an id set, inbound mail without a trusted header reads "unauthenticated". Locally authored
  mail and Sent/Drafts show nothing. Bounded ids never apply to archive imports.

`MAILROOM_LOCAL_DKIM` (default on) verifies DKIM itself, before storing, for mail with no trusted
result. It uses DNS (3 seconds per lookup, at most 4 lookups and 8 seconds per message; a DNS
failure stores nothing and is not retried). Only a pass aligned with the From domain is shown
("Verified by Mailroom (DKIM, example.com)"). A failure is never shown. It needs outbound DNS from
`mailroom`. Outcomes are in `mailroom_local_dkim_total{result}`.

## Attachment scanning with ClamAV

Each attachment is scanned at delivery by `clamd`, a sealed container with **no network**, reached
over a Unix socket. A second container, `clamav-signatures`, holds no mail and fetches signature
updates. Each attachment gets a stored verdict:

| Verdict | Meaning | Download and attach |
|---|---|---|
| `clean` | Scanned, nothing found. | Allowed |
| `flagged` | A signature matched. | Refused until the user releases the content |
| `unknown` | Could not be judged (encrypted or too-deep archive, over-size, a momentary failure). | Refused, same door |
| `unscanned` | No scanner ran, as on an instance without one. | Allowed |

The refusal applies at the part-download and preview routes and to attaching to outgoing mail.
Release is keyed on the content, so it covers every copy of those bytes. The raw message download
is deliberately not blocked (one bad attachment would make the message unreadable and
unexportable); it carries an `X-Mailroom-Attachment-Scan` header. `deploy/clamd.conf` turns on
`AlertEncrypted` and `AlertExceedsMax`, without which those files return plain `OK`.

- **A dead scanner stops mail.** Scanning is fail-closed: if `clamd` does not answer, every
  account's sync pauses, cursors are withheld, the expunge reaper is skipped, and account health
  says the scanner is unreachable. Nothing is lost, and it recovers by itself. Unset
  `MAILROOM_CLAMD_SOCKET` to turn scanning off.
- **A cold start pauses sync for about 70 seconds** while `clamd` loads the shipped database.
  freshclam updates afterwards and does not gate it.
- **A stale database looks healthy.** `clamav-signatures` has a healthcheck (freshclam running and
  the daily database written within 3 days; scraped, never a `depends_on`, so a CDN outage cannot
  stop mail). Alert on
  `time() - mailroom_scanner_signature_published_timestamp_seconds > 259200`. It reads zero, and
  so alerts, when the publication date is unknown.
- **After a deploy that recreated both clam containers,** compare loaded and on-disk signatures.
  If freshclam publishes during clamd's 70-second load, clamd stays one update behind:

  ```sh
  docker compose -f deploy/compose.yml exec clamd clamdscan --version
  docker compose -f deploy/compose.yml exec clamav-signatures sh -c 'sigtool --info /var/lib/clamav/daily.c?d | grep Version'
  docker compose -f deploy/compose.yml exec clamd clamdscan --reload     # if they differ
  ```

- **A large archive has a long first sweep.** Attachments are re-checked 50 messages at a time with a
  30-second pause (about 1,000 chunks, eight hours or more, for 50,000 messages). Each chunk takes
  at most two minutes and an interrupted sweep resumes. The job dispatcher is sequential, so a
  send can wait up to about two minutes behind a chunk. When importing an archive: import, let the
  sweep finish, then rely on the instance to send. `attachment-report` shows the queue; `due now`
  reaching 0 is the signal.
- **At cutover there is a trade-off.** Importing with `MAILROOM_CLAMD_SOCKET` unset leaves mail
  visible and `unscanned`, swept later. Importing with the scanner on can leave a file hit by a
  momentary failure withheld as `unknown` until the database next moves.

Check the scanner and its isolation (the `wget` and `ip route` checks must fail):

```sh
docker compose -f deploy/compose.yml exec clamd clamdscan --ping 1                 # PONG
docker compose -f deploy/compose.yml exec clamd wget -qO- -T3 https://example.com  # Network unreachable
docker compose -f deploy/compose.yml exec clamd ip route                           # no default route
docker compose -f deploy/compose.yml exec clamd sh -c 'clamconf | grep -iE "^AlertEncrypted =|^AlertExceedsMax|^StreamMaxLength"'   # yes, yes, 104857600
docker compose -f deploy/compose.yml logs mailroom | grep "attachment scanning"    # "...is ON; a scanner outage will PAUSE sync..."
```

## The crypto process

PGP private keys are held by a separate `crypto` container with its own root secret and a Unix
socket, so an attacker who has the master key still cannot derive them. It has no port and no
internet (backend network only).

```sh
docker compose -f deploy/compose.yml exec crypto ls /run/secrets/   # mailroom_pgp_root_key, mailroom_app_password
docker compose -f deploy/compose.yml exec mailroom ls /run/secrets/ # no mailroom_pgp_root_key
```

- `crypto` must not have the master key; `mailroom` must not have the PGP root. It does have the
  `mailroom_app` database password, since keys live in `app.user_keys` under the same row-level
  security. The server can read the sealed bytes but cannot open them.
- There is no in-memory fallback: with no database the service refuses to start. A fallback would
  report healthy while creating keys that vanish on restart.
- With the socket set, a user with a bound signing key **cannot send while `crypto` is down**.
  Signing (and opening encrypted mail) fails rather than sending unsigned or guessing. Identities
  without a bound key, and ordinary mail, are unaffected.
- On start it compares its root with a check value the database recorded: `outcome` is `recorded`
  (first boot), `verified`, or `mismatch` (the container restarts and never turns healthy).
  See [troubleshooting](troubleshooting.md#crypto-never-turns-healthy-pgp-root-mismatch).

## Root keys and escrow

Two independent secrets matter: `mailroom_master_key.txt` and `mailroom_pgp_root_key.txt`.

- Losing the master key loses the whole mailbox, loudly: blob paths derive from it, so files cannot
  even be located.
- Losing the PGP root loses every message encrypted to your keys, **quietly**. The mailbox restores
  and reads normally.
- They are not copies of each other. Never point both at one file: it silently collapses the
  separation.
- Rotating the session key invalidates sign-ins in flight only.

Escrow both, separately and off this host, before relying on the instance:
[key escrow](../key-escrow.md). Never generate new roots on a restore:
[backup and restore](../backup-restore-runbook.md).

## Related

- [Install guide](../../deploy/README.md)
- [Authentication](authentication.md)
- [Troubleshooting](troubleshooting.md)
- [Key escrow](../key-escrow.md)
