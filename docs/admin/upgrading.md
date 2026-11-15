<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Upgrading

How to move to a new Mailroom version, what to run afterwards, and how far back you can roll.

**On this page:** [Upgrade to a new version](#upgrade-to-a-new-version) ·
[Run the post-upgrade commands](#run-the-post-upgrade-commands) · [Roll back](#roll-back) ·
[Older instances](#older-instances)

## Upgrade to a new version

A release ships **images only**. A change to the compose file or a variable needs its own update
on each host, so take the deploy files from the same release.

1. Read the release's **Upgrading** notes (in `CHANGELOG.md` and the release page). They name
   migrations, new settings and post-upgrade commands.
2. Take a restorable snapshot, because migrations cannot be undone (see [Roll back](#roll-back)):

   ```sh
   docker exec <postgres> pg_dump -U postgres -d mailroom -Fc > pre-upgrade.dump
   docker exec -i <postgres> pg_restore --list < pre-upgrade.dump | grep -c 'TABLE DATA'
   ```

   Stream it as shown; do not write to `/tmp` and copy out. The second line proves the dump is not
   empty. Find the container name with `docker compose -f deploy/compose.yml ps`. For a full backup
   see the [backup runbook](../backup-restore-runbook.md).
3. Update the deploy files in your checkout to the release tag: `git fetch --tags && git checkout vX.Y.Z`.
   Compare `deploy/.env.example` for new settings.
4. Pull the new image and pin it by digest. Never use `:latest` as a production pin.

   ```sh
   docker pull ghcr.io/10thtardis/mailroom:X.Y.Z      # prints "Digest: sha256:..."
   # in deploy/.env:
   MAILROOM_IMAGE=ghcr.io/10thtardis/mailroom:X.Y.Z@sha256:<digest>
   ```

5. Apply it. Do not build from source on this host.

   ```sh
   docker compose -f deploy/compose.yml up -d
   ```

   Compose recreates changed services and runs, in order, `role-bootstrap-owner`, `migrate` (applies
   every new migration as `mailroom_owner`) and `role-bootstrap-app`, then starts `mailroom`.
   Migrations run on every start, so there is no separate step. Large-table migrations are
   quick or online; the notes call out the exceptions (for example an index that briefly holds up
   writes to the audit log, so upgrade at a quiet moment).
6. Check it came up healthy and is the version you meant:

   ```sh
   docker compose -f deploy/compose.yml ps
   docker compose -f deploy/compose.yml exec mailroom /mailroom --version
   curl -s -o /dev/null -w '%{http_code}\n' "http://localhost:${MAILROOM_HTTP_PORT:-8080}/readyz"   # 200
   ```

   Then run the post-upgrade commands, if the notes list any.

## Run the post-upgrade commands

When a release adds data that older mail lacks, its notes ask for a one-off command. They all
follow one pattern, and are safe to run twice:

1. Run the command with no `--apply`. It only reports what it would change.
2. Read the report. A zero is a finding, not necessarily "nothing to do": per-signal counts show
   which.
3. Run it again with `--apply`. It works in batches while Mailroom keeps running.

Until you do, the feature works but older mail does not benefit.

| Release | Command | What it does |
|---|---|---|
| v1.102 | `mradmin backfill-recipients [--user <id>]` | Fills the searchable To, Cc, Bcc and list-id columns for older mail. Until then older mail does not match recipient and list searches. |
| v1.103 | `mradmin placement-backfill-summaries --user-id <id>` | Writes the Activity entries for placement backfills run before they existed. Run once per user who ran one. |

Add `--apply` to write. `mradmin` is defined in [admin CLI](admin-cli.md#run-a-command).

## Roll back

- **No migration crossed.** Check out the old tag and pin the old image:

  ```sh
  git checkout vX.Y.Z      # the OLD tag
  MAILROOM_IMAGE=ghcr.io/10thtardis/mailroom:X.Y.Z@sha256:<digest> \
    docker compose -f deploy/compose.yml up -d
  ```

  It takes seconds. Pin `MAILROOM_IMAGE`: a floating tag is whatever was pulled last, so an
  unpinned rollback runs the version you are leaving. A job kind the old build does not know logs
  `no handler registered for job kind` and retries; it dead-letters after 5 attempts, so after a
  long rollback look for `state='dead'` rows in `app.jobs` and re-enqueue them. Mail that arrived
  meanwhile is re-ingested by the next sync.
- **A migration crossed.** Migrations are forward-only, with no `down`. Restore the pre-upgrade
  dump with the [backup runbook](../backup-restore-runbook.md), creating the four roles before the
  restore (`pg_dump` does not carry them), then start the old tag. The whole sequence has not been
  timed end to end, so rehearse it on a throwaway copy if the upgrade crosses a migration.
- **Not reversible by any rollback:** an expunge already made on the provider. See
  [first-mailbox runbook](../production-cutover-runbook.md#43-rolling-back-the-decision).

A release's notes may add a rollback caveat of its own (for example, state a newer version added
that an older one cannot read). Read them before rolling back.

## Older instances

Skip this on an instance first installed recently.

- **Secrets moved to files.** If `MAILROOM_SESSION_KEY` and `MAILROOM_OIDC_CLIENT_SECRET` are still
  in `.env`, move them to `secrets/mailroom_session_key.txt` and `secrets/mailroom_oidc_client_secret.txt`.
  The variables are no longer read.
- **Remove the old DSN volume.** Instances deployed before DSNs stopped being written to disk keep
  a stale plaintext copy of the database login in a volume. After the upgraded stack is healthy:
  `docker volume rm <project>_runtime-secrets` (`docker volume ls` shows the real name).
- **`pg_stat_statements`.** Postgres must restart once to load it and `role-bootstrap-owner` must
  run again to create it. `docker compose up -d` does both when the compose file changed.
- **Port binding.** Older versions published the port on all interfaces. It is now `127.0.0.1`
  unless `MAILROOM_HTTP_BIND` is set.

## Related

- [Admin CLI](admin-cli.md)
- [Backup runbook](../backup-restore-runbook.md)
- [Monitoring](monitoring.md)
- [Troubleshooting](troubleshooting.md)
