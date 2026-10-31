# Backup & restore runbook (DR)

**Status:** operational control (spec v0.4 §11, §16; ADR-005; ADR-008).
Companion to [`key-escrow.md`](key-escrow.md), which owns the master-key escrow
procedure. This document owns **the sequence you follow when the box is gone.**

> **A backup that hasn't restored is a hypothesis.** (spec §11)
> That is why `scripts/restore-drill.sh` exists and why it runs on a schedule.
> If you are reading this during an outage, skip to [At 3am](#4-at-3am).

---

## 1. What is backed up: the restore unit

`scripts/backup.sh` produces **one encrypted artifact** containing:

| In the artifact | Why |
| --- | --- |
| `postgres.dump` | The system of record. `pg_dump --format=custom`. |
| `blobs/` | Every message's raw MIME, already AEAD-encrypted at rest. Copied verbatim; the backup script never decrypts anything. |
| `manifest.json` | Format, timestamp, **the goose schema version**, row counts, and an explicit record of what is *not* here. |
| `SHA256SUMS` | Per-file digests, checked on restore. |

**The wrapped key store is inside `postgres.dump`.** In 0.5 the sealed key
material is a set of columns — `app.accounts.credentials_ct`,
`app.user_keys.private_key_ct`, `app.api_keys.api_key_ct` — not a separate store,
so the dump carries it. (See §6 for an important caveat about how 0.5 actually
derives its keys.)

### What is deliberately NOT in the artifact

**The search index.** It is a disposable cache, rebuilt from Postgres
(spec §7/§11). A restored-but-stale index is *worse* than no index — it would
answer queries with mail that no longer exists and hide mail that does. So it is
never backed up, and the drill **rebuilds** it, which is the only honest way to
know that it can be rebuilt.

**The root secrets — both of them.** This is the line the entire design rests
on:

> An artifact that carried both the ciphertext and the key that opens it would
> not be a backup. It would be a copy.

They are escrowed **separately and offline** ([`key-escrow.md`](key-escrow.md),
ADR-005, ADR-024). `backup.sh` has no code path that reads either, and it
refuses to write an artifact if it finds anything key-shaped in the payload.

**A restore therefore needs three secrets from three different places:**

| Secret | What it opens | Where it lives |
| --- | --- | --- |
| **age identity** (`AGE-SECRET-KEY-1…`) | the artifact | with whoever restores — *not* on the mail server |
| **master key** (64 hex chars) | the mail inside it | offline escrow, off-site |
| **PGP root** (64 hex chars) | the PGP private keys, and so every message ever encrypted to them | offline escrow, off-site — a *different* envelope |

The mail server holds **none** of them, and the crypto process holds only the
third. It has only the age *public* recipient, so a compromised Mailroom host
cannot decrypt its own backups, and an attacker who steals the backup bucket
gets encrypted noise.

> ⚠ **Missing the PGP root is the only one of the three that fails silently.**
> Without the age identity you cannot open the artifact. Without the master key
> nothing decrypts and the blobs cannot even be located. Without the PGP root
> the restore looks *perfect* — Postgres comes up, the blobs decrypt, search
> rebuilds, the mailbox opens and reads — and the mail that arrived encrypted is
> simply gone. That is why step 5 of the drill exists, and why
> `restore-drill.sh` refuses to run without either `--pgp-root` or an explicit
> `--no-pgp-root`.

---

## 2. The trap this design closes (read this once, now)

Every Mailroom table is `FORCE ROW LEVEL SECURITY`, and every policy keys off
`app.current_user_id()`, which is `NULL` unless the transaction sets it.
**`pg_dump` never sets it.**

So a dump taken as any role without `BYPASSRLS` — **including `mailroom_owner`,
the role that owns the schema and that migrations run as, the single most natural
choice** — cannot see the mail.

⚠ **This section used to say that such a dump silently emits empty `COPY` blocks
and exits 0. That is wrong, and it teaches you to distrust the wrong thing.**
Measured on PostgreSQL 18 (`scripts/backup.sh` measured the same on 16 and
has said so all along):

```
$ pg_dump --role=mailroom_owner --data-only -t app.users -d mailroom
pg_dump: error: query failed: ERROR:  query would be affected by row-level
         security policy for table "users"
HINT:  To disable the policy for the table's owner, use
       ALTER TABLE NO FORCE ROW LEVEL SECURITY.
exit code: 1        <-- LOUD. Postgres is on our side here.
```

**Postgres refuses rather than lying to you.** A backup job that runs as the
wrong role goes red; it does not hand you a plausible empty artifact.

There is exactly one way to get the silent version, and it is a flag:

```
$ pg_dump --role=mailroom_owner --enable-row-security --data-only -t app.users …
exit code: 0        <-- and ZERO rows, with no error at all
```

`--enable-row-security` tells `pg_dump` to dump only what the role can *see*,
which under these policies is nothing. **`backup.sh` never passes it, and never
should.** That is the real rule to carry away: not "pg_dump lies", but "one
specific flag turns a refusal into an empty file".

`backup.sh` therefore **refuses to dump** unless the connecting role bypasses
RLS — a preflight that turns an obscure mid-dump error into a refusal naming the
actual problem — and then refuses to write an artifact containing zero users, as
a backstop for the day someone adds that flag or a future Postgres relaxes the
error.

> **Back up as a superuser (or a role with `BYPASSRLS`). Never as `mailroom_owner`.**

---
## 3. Taking a backup

**One-time setup — create the artifact keypair.** Generate it **off the mail
server**; the whole point is that the server cannot decrypt its own backups.

```sh
age-keygen -o backup-identity.txt                            # PRIVATE — store with the escrow
age-keygen -y backup-identity.txt > backup-recipients.txt    # public — safe to put on the box
```

⚠ **`backup.sh` needs raw container names, not service names, and they depend
on the Compose project.** Docker Compose names every container
`<project>-<service>-1`. `deploy/compose.yml` sets `name: mailroom-deploy`, so
the plain `docker compose -f deploy/compose.yml up -d` command
`deploy/README.md` gives produces `mailroom-deploy-postgres-1`,
`mailroom-deploy-mailroom-1`, and so on. Passing `-p <name>` overrides the
project, and therefore every container name that follows from it — this used
to send the two documents' examples to different container names entirely,
which read as "no such container" at exactly this step.

**Find the real names rather than assuming them:**

```sh
docker compose -f deploy/compose.yml ps
```

**Then, on the host, with a superuser DSN** (the default-project names shown
below; substitute whatever `ps` above actually printed if you deployed with
`-p`):

```sh
MAILROOM_BACKUP_DSN='postgres://postgres:...@127.0.0.1:5432/mailroom?sslmode=disable' \
MAILROOM_BACKUP_PG_CONTAINER=mailroom-deploy-postgres-1 \
MAILROOM_BACKUP_BLOB_CONTAINER=mailroom-deploy-mailroom-1 \
MAILROOM_BACKUP_RECIPIENTS=/etc/mailroom/backup-recipients.txt \
MAILROOM_BACKUP_OUT="/var/backups/mailroom-$(date -u +%Y%m%dT%H%M%SZ).age" \
  sh scripts/backup.sh
```

`--pgp-root` is required. If this instance genuinely has no PGP root, pass
`--no-pgp-root` instead — the drill then states, in its verdict, that it proved
nothing about encrypted mail.

If it prints **THE RESTORE IS ALIVE**, the backup is good and everything from here
is mechanics. If it fails, it names which of the six stages failed and what that
means. **Do this before you rebuild anything.**

## 3a. Running it unattended: retention, and the signal that goes red

⚠ **As of v1.70.0 nothing in this repository schedules a backup, so an instance
set up from this repository alone produces no backups at all.** Check rather than
assume: look for a crontab entry, a file in `/etc/cron.d` or a systemd timer that
runs `scripts/backup.sh`, and for recent `.age` artifacts. The
scheduled caller belongs in the host's configuration — it needs Docker access
and a BYPASSRLS DSN that should not live in the compose environment — so what
this repository provides is the other half: bounded retention, and a check that
goes red when runs stop.

### Two invocation styles, and they want different things

| | manual, before a rebuild | scheduled, unattended |
|---|---|---|
| `MAILROOM_BACKUP_OUT` | a **dated** name, as in §3 above | a **fixed** path, e.g. `/var/backups/mailroom.age` |
| `MAILROOM_BACKUP_KEEP` | unset (0) | the number of nights to keep |
| grows without bound? | yes — you are expected to clean up | no — pruned to `KEEP` |

The §3 example hand-stamps the filename, which is right for a one-off you take
before touching something: it cannot overwrite anything. Left on a timer it
would fill the disk. A fixed path with `KEEP` is the inverse and is what a
schedule wants.

### Retention

`MAILROOM_BACKUP_KEEP=N` keeps `N` dated copies **beside** `$OUT`, named
`mailroom.age.20260912T211500Z`. `$OUT` itself stays the canonical latest path,
because `scripts/restore-drill.sh` and this runbook both name a single file.

**Each night's artifact is a genuinely new, independently-encrypted file** —
`scripts/backup.sh` writes to `$OUT.partial.<pid>` and `mv -f`s it onto `$OUT`,
which is a new inode every run. `scripts/backup-retain.sh` then `ln -f`s
**that** inode to tonight's dated name, so the hard link only avoids storing
*tonight's* artifact twice (once as `$OUT`, once under its dated name) — it
does not make yesterday's and tonight's bytes the same file. `KEEP=N` therefore
stores **N full artifacts**, each its own complete `age` ciphertext: the true
cost of `MAILROOM_BACKUP_KEEP=N` is `N ×` one artifact's size, not one artifact
plus deltas. (On a filesystem without hard-link support, `backup-retain.sh`
falls back to a `cp -p`, which is the same cost made explicit.)

**Why keep history at all**, when a nightly run rewrites `$OUT` anyway: one
artifact is one chance. A backup taken while the database was already damaged
overwrites the last good one, and damage is normally noticed *after* that.
Retention is not what gives you "a backup" — it is what gives you "a backup from
before the thing that went wrong."

**Sizing:** measure your own mailbox rather than trusting a number quoted here —
the census tool (`/mailroom-census`) reports per-folder counts and bytes.
An artifact is roughly the blob store's size plus a small Postgres dump (the
search index is deliberately excluded — it is rebuildable). Choose `KEEP`
against the volume you actually have, and against the true cost above: `KEEP`
full artifacts, not one plus deltas.

Retention never fails a backup. If the dated copy cannot be made — no hard-link
support, a read-only directory — `scripts/backup-retain.sh` exits **3**,
`backup.sh` prints a warning, and `$OUT` is still complete. Failing a good
backup over its own bookkeeping would be the worse outcome.

### The artifact is written atomically

`docker cp` onto the final path is not atomic: it truncates the destination and
then streams, so a failure part-way used to leave a truncated artifact at `$OUT`
**and destroy the only good one in the same stroke** — across a multi-GB write,
on a schedule nobody was watching. The copy now goes to `$OUT.partial.<pid>` and
is renamed into place, so at every instant `$OUT` is either the previous
complete artifact or the new one.

### The freshness check

```sh
MAILROOM_BACKUP_OUT=/var/backups/mailroom.age sh scripts/backup-freshness.sh
sh scripts/backup-freshness.sh --path /var/backups/mailroom.age --max-age-hours 36
sh scripts/backup-freshness.sh --path /var/backups/mailroom.age --prom-out /var/lib/node_exporter/mailroom_backup.prom
```

Exit **0** = an artifact exists, carries the age header, is non-empty and is
within the bound. Exit **1** = the alerting condition, with the reason on
stderr. Exit **2** = a broken invocation, which is deliberately neither, so a
mistyped check cannot read as a healthy backup.

⚠ **It checks more than an mtime, and that is the point.** A partial copy, a
full disk, or a lost rename all leave a file at the expected path with a current
timestamp; an mtime-only check calls that healthy. So "fresh" here means *a
plausible artifact exists* — non-empty, and beginning with
`age-encryption.org/v1`. It does **not** mean the artifact restores. Only
§5's drill proves that, and claiming otherwise here would be the same overclaim
in a new place.

**Set `--max-age-hours` from the schedule, at roughly 1.5× the interval** (the
default 36 suits a nightly run). A single transient failure then does not page
anyone; two consecutive ones do.

Both halves are proven in CI rather than asserted — `backup-freshness.sh
--self-test` and `backup-retain.sh --self-test` run on every push, each with a
positive control, because the acceptance criteria ask for an alert that "has
been proven to fire when backups stop".

### What still has to live on the host

Not in this repository, and left to the deployment's own automation:

- the timer or unit that calls `backup.sh`, with its environment;
- the superuser DSN, kept out of the compose environment;
- handing the artifact to wherever this host's paths are already archived
  offsite, so that Mailroom's own restore unit is what gets shipped rather than
  a raw-volume snapshot that loses the schema-version manifest;
- wiring `backup-freshness.sh` to whatever actually pages someone.

⚠ One trap for whoever writes that, now closed rather than merely noted: `backup.sh` seals the artifact by running `age` **inside a throwaway
container**, named by `MAILROOM_AGE_IMAGE`.

- **Leave `MAILROOM_AGE_IMAGE` unset and the script derives it from the blob
  container** — `docker inspect -f '{{.Config.Image}}' "$MAILROOM_BACKUP_BLOB_CONTAINER"`
  — which is the Mailroom image already running on the box. That image now
  carries `age` at build time (the Dockerfile), at the exact digest the
  deployment already pinned, so sealing needs **no container egress at all**
  and no new configuration: the scheduled caller in the previous section does
  not have to know this happened. Only works when
  `MAILROOM_BACKUP_BLOB_CONTAINER` (not `_BLOB_VOLUME`) names a live
  container — if not, or if `docker inspect` fails, the script falls back to
  plain `alpine:3.20`, exactly the old behaviour, and needs egress at backup
  time (`apk add`).
- **Set `MAILROOM_AGE_IMAGE` explicitly to pin something else** — a
  different digest, a vendored image — and that wins over both of the above.

Every run logs which of the three happened and why (`backup: sealing image
… (from MAILROOM_AGE_IMAGE|from the blob container …|fallback)`) — read that
line before assuming a nightly backup needed no network. And regardless of
which image is in play, the sealing step now installs `age` **only when it is
missing**: `age: preinstalled` on stderr means the image already had it and no
`apk add` ran at all; `age: installing` means it fell back to the networked
path. `sh scripts/backup.sh --self-test` (CI: the `docker-image` job) proves
the pinned path needs no network at all, with a negative control (plain
`alpine:3.20`, which has no `age`) that proves the same check can fail.

⚠ **`backup.sh` stages a PLAINTEXT copy of everything it backs up, on the
HOST, for the duration of the run.** `WORK="$DIR/.backup-tmp.$$"`,
where `$DIR` is the checkout `backup.sh` itself runs from (the parent of
`scripts/`) — **not** `MAILROOM_BACKUP_OUT`'s directory, which may be
somewhere else entirely. Under it, `payload/` holds the unencrypted
`postgres.dump` (streamed straight from `pg_dump`, since the change below) and
an unencrypted copy of every blob file, right up until step 7 seals them into
the `.age` artifact; `cleanup()` removes `$WORK` on exit, success or failure.
**Wherever this repository is checked out on the host must therefore also be
on encrypted storage** — the same requirement `deploy/README.md` §0a states
for `pgdata`, `search` and `blobs`, extended to the one place outside a
container this script ever writes plaintext.

### Step 1 — Open the artifact

```sh
age -d -i backup-identity.txt -o payload.tar.gz mailroom-….age
mkdir payload && tar -xzf payload.tar.gz -C payload
cd payload && sha256sum -c SHA256SUMS
```

`age` authenticates every chunk *and* the end of the stream, so a truncated or
tampered artifact fails here rather than halfway through a restore.

### Step 2 — Check the schema version

```sh
grep schema_version payload/manifest.json
ls db/migrations | tail -1          # the head of the code you are deploying
```

- **Equal** → carry on.
- **Artifact older than the code** → restore as-is, then `mailroom-migrate up` to
  carry it forward (migrations are forward-only, ADR-008), then re-drill. Spec §16
  requires drills to exercise exactly this.
- **Artifact newer than the code** → you are restoring with *old code*. Check out
  the revision whose migration head matches, first. **Never downgrade a database.**

### Step 3 — Create the roles, then restore Postgres

The roles must exist **before** `pg_restore`: the dump preserves ownership and the
RLS grants, and `pg_restore` cannot invent the roles they name. This is the step
people forget.

```sh
psql -U postgres -c "CREATE ROLE mailroom_owner NOLOGIN CREATEROLE;" \
                 -c "CREATE ROLE mailroom_app   NOLOGIN;" \
                 -c "CREATE ROLE mailroom_admin NOLOGIN;" \
                 -c "CREATE ROLE mailroom_wkd   NOLOGIN;" \
                 -c "GRANT mailroom_wkd TO mailroom_owner;" \
                 -c "CREATE DATABASE mailroom OWNER mailroom_owner;"

pg_restore -U postgres -d mailroom --exit-on-error payload/postgres.dump
```

Two details that are easy to drop and expensive to debug:

- **`CREATEROLE` on `mailroom_owner`.** It is what the deployed database gives that
  role, and migrations run as it — so a migration that creates a role (schema 35
  does) fails on a restored box that was set up without it, long after the restore
  looked successful. The drill caught exactly this on itself.
- **`mailroom_wkd`, and the grant that follows it.** From schema 35 it owns
  `app.wkd_public_key`, so a dump taken at 35 or later names it as an owner, and
  `pg_restore` cannot invent it any more than it can invent the other three. The
  `GRANT … WITH ADMIN OPTION` matters for the same reason the migration is careful
  about it: `ALTER FUNCTION … OWNER TO` requires the current owner to be a *member*
  of the target role, and a role you pre-create here was not created by
  `mailroom_owner`, so it does not get that membership automatically. Without the
  grant, migrating a restored database forward stops with `permission denied to
  grant role`. The drill found this by doing it.

`--exit-on-error` is **not optional**: without it `pg_restore` prints its errors,
carries on regardless, and exits 0.

Then grant `LOGIN` to `mailroom_app` out of band (the schema deliberately creates
it `NOLOGIN`), with the password the app is configured with.

### Step 4 — Restore the blob store

Copy `payload/blobs/` to the blob root (`MAILROOM_BLOB_ROOT`, default
`/var/lib/mailroom/blobs`). It must be readable and writable by the container's
non-root `mailroom` user, and it must sit on an at-rest-encrypted filesystem
(LUKS/dm-crypt — spec §11 makes this a deployment requirement).

Blob **paths are derived by HMAC from the master key**, so do not rename or
re-shard anything. Put the tree back exactly as it came out.

### Step 5 — Install BOTH roots from escrow

There are **two**, they fail in completely different ways, and only one of them
tells you when it is wrong.

**The master key** — install at `MAILROOM_MASTER_KEY_FILE` (default
`/run/secrets/mailroom_master_key`).

Getting this wrong does **not** produce a clean "bad key" error. It shows up as
**`blob: not found`** — because the blob *paths* are derived from the master key
too, so the wrong key goes looking for files that were never there. If you see
that, you have the wrong key; you have not lost the blob store.

**The PGP root** — install at `MAILROOM_PGP_ROOT_KEY_FILE` (default
`/run/secrets/mailroom_pgp_root_key`).

⚠ **This step was added late, and its absence is unrecoverable.** The
crypto process unseals stored PGP keys **lazily, on first use**, so for a long
time a wrong or freshly generated root produced no error at boot, no error at
login, and a perfectly healthy-looking instance. It surfaced the first time
somebody signed or decrypted, possibly months later, when every stored private
key was already unopenable and `pgp-export-wrapped` could not help either.

**Since migration 00055 the crypto service refuses to start on the wrong
root — provided the database you restored had recorded one.** The first time the
crypto process boots against a database it stores a one-way check value of its
root in `app.pgp_root_check`, and every boot after compares against it. That
table is in the backup. So with the wrong root installed, the `crypto` container
keeps restarting, never turns healthy, and logs:

    PGP ROOT MISMATCH: refusing to start

That is the check doing its job: **find the right escrow envelope, not a way
round it.** Mail keeps flowing meanwhile; only signing and decryption wait.

⚠ **Two things it cannot catch.** A backup taken before the crypto process ever
ran against 00055 carries no record, so the first boot after restoring it
*adopts* whatever root you give it. And the check proves "the root this database
recorded", not "a root that opens the stored keys". For a restore those
coincide, but only the drill proves the second.

⚠ **The deployment guide will offer to generate a new one.** `deploy/README.md`
§2 hands you an `openssl rand` line for both roots, because it is written for a
*first* install. **On a restore, do not run it.** Against a database with a
recorded root, a generated one now stops the crypto service; against one without,
it is still indistinguishable from the right one until the moment it matters.

**Prove it before you trust it.** Run the drill against the artifact you just
restored (§5 below) — it unseals every stored PGP key and fingerprint-verifies
them, and its `--inject wrong-pgp-root` case exists precisely to prove that check
can fail.

**If the PGP root is genuinely lost** — no escrow copy anywhere — every stored
private key is unrecoverable whatever you do, and the crypto service will keep
refusing the new root you are forced to generate. Clearing the record is then a
deliberate act for a database superuser, and it is the moment you accept that
loss:

    psql -U postgres -d mailroom -c 'DELETE FROM app.pgp_root_check;'

The next boot records the new root. Every key sealed under the old one stays
unopenable: users with keys need new ones, and their correspondents need the new
public keys. The application cannot do this for you — it can read and insert
that row, and nothing else.

### Step 6 — Boot, and let the index rebuild

Start Mailroom. Unseal is automatic (ADR-005). The search index is not in the
backup; the indexer rebuilds it from Postgres by itself. On a large archive that
takes a while, and **search is incomplete until it finishes**. That is expected
and is not a restore failure.

### Step 7 — Prove it is alive, then re-enable sync

Do not declare victory because the box booted. Run the drill (Step 0) against the
restored system, or at minimum confirm by hand: one message decrypts, one index
rebuilds, one search returns a hit.

**Only then** re-enable account sync. A Mailroom that is up but *wrong* will
cheerfully write its wrongness back to your real IMAP accounts
(`internal/writeback`).

---

## 5. The drill

```sh
sh scripts/restore-drill.sh              # fixture: build a world, back it up, drill it
sh scripts/restore-drill.sh --self-test  # prove the drill can FAIL, then that it passes
sh scripts/restore-drill.sh --artifact … --identity … --master-key … --pgp-root …
```

It runs weekly in CI (`.github/workflows/restore-drill.yml`) in **fixture** mode:
it builds a real Mailroom (real migrations via `cmd/mailroom-migrate`, real
envelope-encrypted mail via `internal/ingest`), takes a real backup with
`scripts/backup.sh`, **destroys the source**, and restores the artifact into a
scratch container. That proves the backup/restore code path works against the
current schema, every week, without putting a single production secret in CI.

It proves the restore is **alive**, not merely present:

1. the artifact opens (age) and every file checksums clean;
2. its schema version matches the code's migration head;
3. `pg_restore` succeeds and every row the manifest counted came back;
4. **one message per user decrypts** with the escrowed master key, and its content
   hash re-verifies;
5. that user's **sealed account credentials open** — the wrapped key store;
6. the **search index rebuilds from Postgres**, with the real Tantivy engine
   (ADR-006), not the in-memory fallback;
7. **one search returns a hit** — and the word it searches for lives *only* inside
   the encrypted message body, never in a Postgres column, so a hit is impossible
   unless the blob was genuinely restored, decrypted with the escrowed key, parsed
   and indexed;
8. the **second escrowed root is present, well-formed, is not the master key
   again, matches this database's own recorded check value if it has one**
   (`app.pgp_root_check`, migration 00055), **and actually unseals every
   PGP private key this user has stored** (ADR-024). Steps 1–7 all turn on one
   secret, and every one of them passes without this one — so a drill that
   stopped at 7 would print a clean bill of health over precisely the hole it
   cannot see.

   PGP keys persist in `app.user_keys` (sealed in `private_key_ct`), so
   step 8 opens every one it finds and checks the
   opened key's fingerprint against the stored column — opening to
   plausible-looking bytes is not proof, a fingerprint match is. A mismatched
   check value is always a **hard failure**, checked *before* any key is
   opened: it means the escrowed root you were handed and the one this
   database was actually built with have drifted apart, which is the disaster
   ADR-024 exists to catch, and unsealing with a root already known to be
   wrong would only repeat the same verdict less precisely. **Any stored key
   that then fails to unseal is also a hard failure** — the escrowed root and
   *that key* have drifted apart even though the database-wide check value
   agreed, so mail encrypted to it is unrecoverable, and steps 1–7 passing
   tells you nothing about it.

   Short of a hard failure, step 8 reports what it actually proved rather than
   collapsing every case into one OK: **N keys unsealed and
   fingerprint-verified** is the strongest result and always attempted when
   there is anything to unseal, whether or not the check value matched too.
   With **0 keys stored**, there is nothing to unseal, but that is
   no longer automatically *unproven*: if this database recorded a check value
   and it matches, the root is still **proven** to be the one the database was
   built with — a narrower claim than "opens a key", worded as one, but a real
   pass. Only when the check value is **absent** (the backup predates
   migration 00055, or its crypto process — real or, in a fixture, `seed`
   standing in for one — has never booted) *and* there are 0 keys to unseal is
   the leg reported *unproven*, exactly as before the check value existed.

### A drill that cannot fail is not a drill

`--self-test` injects each failure and requires the drill to catch it:

| Injection | What it simulates | Where it fails |
| --- | --- | --- |
| `corrupt` | truncated artifact (half-copied upload, full disk) | 1/6 — age refuses to authenticate the payload |
| `no-key` | nobody could find the escrow envelope | 6/6 — "THE ESCROWED MASTER KEY IS NOT HERE" |
| `wrong-key` | the escrow envelope from the *other* box | 6/6 — `blob: not found` (wrong derived paths) |
| `no-pgp-root` | the *second* escrow envelope was never retrieved | 6/6 step 5 — "PGP ROOT FAILED" |
| `same-root` | both secrets pointed at the same file | 6/6 step 5 — "BYTE-IDENTICAL to the master key" |
| `wrong-pgp-root` | the PGP root escrow envelope from the *other* box: present, well-formed, genuinely distinct from the master key — just not the one the stored keys were sealed under | 6/6 step 5 — "this root does NOT match app.pgp_root_check" |
| `schema` | old artifact, newer code | 2/6 — schema version mismatch |
| `no-hit` | mail restored, but the index does not answer | 6/6 — search returned NO HITS |
| `missing-blobs` | half the restored blob files deleted: Postgres comes back whole, the blob store does not | 6/6 step 3 — "REINDEX FAILED" (the reindex reads every body from the blob store, so it hits the gap before the decrypt step's single sample would) |

`same-root` is the one worth dwelling on. It is not a corruption or an absence:
it is a stack that boots, serves mail, and drills perfectly clean while having
exactly **one** root — which silently deletes the only reason the crypto process
exists, because whoever holds the master key can then derive every PGP private
key. Nothing at runtime reports it. The drill is the only thing that looks.

`wrong-pgp-root` is the one whose fixture actually has stored keys to test
against, but it no longer needs them to fail: it is caught by the
check-value comparison (`app.pgp_root_check`) before any key is opened at
all, the same way `no-pgp-root` and `same-root` are caught by presence and
equality checks alone. All three now share that shape — a secret that is
wrong is refused before it ever touches a stored key — and it is still the
specific disaster ADR-024 exists to catch: the escrowed root and the one this
database was actually built with have drifted apart, so mail encrypted to
that user's PGP keys is gone. Unsealing every stored key remains the
*stronger* proof step 5 performs once the check value has matched (see §5's
step 8 above) — `wrong-pgp-root` just no longer needs to reach it to fail.

Against a **production** artifact, point `--master-key` and `--pgp-root` at the
escrow copies and destroy both decrypted copies afterwards. The drill redacts message subjects and
derived queries by default (`--show-content=false`), printing them only in fixture
mode where the mail is synthetic.

---

## 5a. Drilling a REAL artifact (not the fixture)

Everything above runs the drill in **fixture** mode: it builds its own world,
backs it up, destroys it, and restores that. That proves the code path against
the current schema. It says nothing about *your* artifact, and until v1.70.0 the
drill had **never once been pointed at a real backup**.

```sh
sh scripts/restore-drill.sh \
  --artifact   /var/backups/mailroom.age \
  --identity   /media/escrow/recovery-key.txt \
  --master-key /media/escrow/master.key \
  --pgp-root   /media/escrow/pgp-root.key \
  --user-id    <the mailbox you are proving>
```

**It cannot touch the running instance.** The build-and-destroy sequence is
gated on fixture mode; with `--artifact` the drill only ever reads the file you
give it and restores into throwaway containers whose names and cleanup label
carry a per-run unique suffix. Verified, not assumed.

**`--user-id` is usually necessary on a real database, and it is not a
weakening.** The drill's rule is that a user with no messages is a failed
restore — correct for the mailbox you are recovering, and what stops a restore
that silently produced nothing from reporting success. But this instance
auto-provisions any identity that completes the OIDC flow, so a real database
normally contains accounts with no mail, and a one-user fixture never can.
Scope the proof to the mailbox that matters; do not relax the rule.

⚠ **The identity you pass is the one the artifact was sealed to.** If you have
lost it, stop — see the loss branch in `docs/key-escrow.md`. No amount of
drilling recovers an artifact whose recipient key is gone.

### Measured — the first real-artifact drill

The drill was run against a small test instance. Its phases, in order:

| phase | scales with |
|---|---|
| *(taking the backup — `scripts/backup.sh`)* | bytes |
| 1/6 open the artifact (age decrypt + checksum every payload file) | bytes |
| 2/6 schema version | constant |
| 3/6 `pg_restore` into a scratch PostgreSQL 18 | bytes |
| 4/6 row counts vs the manifest | rows |
| 5/6 select the users to prove | constant |
| 6/6 decrypt + reindex + search + both roots | messages |

The whole drill took on the order of a minute or two. Phase 6 breaks down into building
`cmd/mailroom-drill` with CGO/Tantivy in a container, then **rebuilding the search
index** from Postgres, reading every body out of the blob store. The build shrinks on
a warm Go/module cache; the reindex does not.

**Extrapolating to a large mailbox is where to be careful.** The reindex is the part
that scales with message count; `pg_restore` and the age decrypt scale with bytes,
not rows. Any whole-run estimate for a large mailbox is an *extrapolation from a
small one*, which is exactly the kind of figure this project has been wrong about
before. Re-measure on your own mailbox after the census rather than quoting this.

### Capture the timings correctly, or do not bother

```sh
( sh scripts/restore-drill.sh … ; echo $? > drill.rc ) 2>&1 \
  | while IFS= read -r l; do printf '%s %s\n' "$(date -u +%H:%M:%S)" "$l"; done \
  > drill.timed.log
rc=$(cat drill.rc)
```

⚠ The exit status is written **inside the subshell**, never taken from the
pipeline. A pipeline reports its *last* command's status, so `drill … | ts` would
report the timestamper's success and a failed drill would read as a pass. This
is the same failure that once produced a "CLEAN" report while `pg_restore` had
actually died, and the same reason `--exit-on-error` is mandatory in step 3.

## 6. Known divergence: the spec's key hierarchy vs. what 0.5 actually does

Spec §9, ADR-005 and `key-escrow.md` describe:

```
master key ──wraps──▶ per-user KEK ──wraps──▶ per-purpose DEK ──seals──▶ data
```

and say the wrapped KEKs/DEKs "live in your database/blob store and are included
in normal backups". **In 0.5 they are not stored at all.** The mail-blob key, the
credentials key and the blob *path* key are all **derived** from the master key
with HKDF (`crypto.DeriveKey`; `syncrunner.MailBlobInfoV1`,
`onboarding.CredentialsInfoV1`, `blobPathInfo` in `cmd/mailroom`).
`internal/syncrunner/resolver.go` says so explicitly and calls the wrapped
random-KEK hierarchy "1.0 hardening".

What that means for backup and restore — which is all that matters here:

- The "wrapped key store" leg of the restore unit is, today, just the sealed
  ciphertext **columns** in Postgres. There is no separate key store to copy.
  `backup.sh`'s manifest records that explicitly rather than pretending otherwise.
- The master key is **more** load-bearing than the spec implies. It is not merely
  the top of a wrapping chain: it is the seed for every derived key *and for the
  blob paths*. Lose it and the blob store is not just unreadable, it is
  unaddressable.
- Nothing about the escrow procedure changes. If anything, it matters more.

When the wrapped-KEK hierarchy lands, `backup.sh` **must be revisited**: if key
material ever moves out of Postgres, the artifact needs a fourth member and §1 of
this runbook needs a new row. The manifest's `wrapped_key_store.location` field
exists so that day is a deliberate change and not a silent omission.

**ADR-024 does not change any of the above, and that is deliberate.** The crypto
process holds PGP keys *only*; the at-rest paths described here are untouched,
and ADR-013's deferral of the wrapped hierarchy stays open on its own terms. What
ADR-024 changes is the number of escrowed roots, which is a backup question
rather than a hierarchy one — hence §1's third row and the drill's step 5. When
increment 3 persists sealed PGP keys into Postgres, they travel in the same
`pg_dump` as the sealed credential columns; the artifact does not gain a member,
but it does gain rows nobody can open without the second escrow envelope.

---

## 7. Do / don't

- **Do** run the drill on a schedule and **read the result**. A scheduled job
  nobody reads is a scheduled job that is failing.
- **Do** keep the age identity, the master-key escrow, the PGP-root escrow and
  the backups in different places. Four secrets in one drawer is one secret.
- **Do** label the two root-key envelopes with *which root* each holds. An
  unlabelled envelope five years from now is a coin flip.
- **Do** re-drill after every master-key rotation (ADR-005) and after every
  migration.
- **Don't** back up as `mailroom_owner`. See §2.
- **Don't** back up the search index. Rebuild it.
- **Don't** commit key material. Throwaway drill keys are gitignored; real ones
  never come near the repo.

## References

- Spec v0.4 §7 (index as cache), §9 (key hierarchy), §11 (restore unit + drill),
  §16 (backups record the schema version).
- ADR-004 (content-addressed encrypted blobs), ADR-005 (master key + offline
  escrow), ADR-006 (Tantivy), ADR-008 (forward-only migrations), ADR-010 (RLS).
- [`key-escrow.md`](key-escrow.md) — the escrow procedure itself.
- `scripts/backup.sh`, `scripts/restore-drill.sh`, `cmd/mailroom-drill`.
