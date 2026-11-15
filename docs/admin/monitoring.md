<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Monitoring

How to tell that Mailroom is healthy: probes, metrics, logs, slow queries and backups.

**On this page:** [Health and readiness](#health-and-readiness) · [Metrics](#metrics) · [Logs](#logs) ·
[Find slow queries](#find-slow-queries) · [Backups](#backups)

## Health and readiness

| Probe | Meaning |
|---|---|
| `GET /healthz` | Liveness: the process is up. |
| `GET /readyz` | Readiness: a real database round trip. 200 means roles, migrations and network all work. |
| `docker compose -f deploy/compose.yml exec crypto /mailroom-crypto -healthcheck` | `ok` when the crypto socket answers (its root also passed the boot check). |
| `docker compose -f deploy/compose.yml exec clamd clamdscan --ping 1` | `PONG` when the scanner is up (about 70 seconds after a cold start). |

The HTTP probes need no session. The compose healthchecks use these, so
`docker compose -f deploy/compose.yml ps` shows every service `healthy` when all is well. The port is
`MAILROOM_HTTP_PORT` (default 8080; `deploy/.env.example` sets 8090).

## Metrics

`GET /metrics` serves Prometheus text, unauthenticated by design. **Do not expose it publicly.** It
carries no mail and no addresses, but `mailroom_sync_disabled_accounts{user}` and
`mailroom_sync_lag_seconds{account}` use ULIDs as labels, and a ULID's leading bits are its creation
time. An anonymous scrape therefore reveals how many users and accounts exist, when each was created,
how far behind sync is, how much writeback is owed and how many messages each account sent today.
The proxy or firewall is the only thing that scopes it.

| Metric | Watch for |
|---|---|
| `mailroom_sync_lag_seconds{account}` | Sync falling behind. |
| `mailroom_sync_disabled_accounts{user}` | Above 0: mail is not arriving for an account (failed validation or paused). |
| `mailroom_action_queue_depth`, `mailroom_writeback_failures_total` | Changes owed to the provider; repeated failures. |
| `mailroom_index_cursor_lag_seconds` | Search index behind. |
| `mailroom_job_queue_depth`, `mailroom_job_dead_letters_total{kind}` | Backlog; jobs that exhausted retries. |
| `mailroom_outbound_ledger_level{account}` | Sends drawn from today's budget (UTC). |
| `mailroom_scanner_signature_published_timestamp_seconds` | Alert on `time() - x > 259200` (3 days). Zero means unknown, treated as stale. Read within seconds of boot, then every 10 minutes. |
| `mailroom_scanner_signature_version` | Loaded daily signature database. |
| `mailroom_jmap_method_duration_seconds{method}`, `mailroom_jmap_request_duration_seconds` | API latency. `method` is a known method name or `unknown`. |
| `mailroom_search_duration_seconds{kind="search"\|"snippet"}` | Search-index latency. |
| `mailroom_proxy_refusals_total{reason}`, `mailroom_proxy_fetches_total{result}` | Image-proxy refusals and successes. |
| `mailroom_proxy_prefetch_total{result}` | Prefetched images (`fetched`, `already_cached`, `refused`, `empty`) and `cap_reached`. |
| `mailroom_proxy_cache_bytes{user,kind}`, `mailroom_proxy_cache_entries` | Kept-image size, `referenced` or `unreferenced`. Only `unreferenced` is cap-bounded. |
| `mailroom_local_dkim_total{result}` | Outcomes of Mailroom's own DKIM check. |

Other series cover the learned-placement decisions, the flag sweep, mailbox-count cache and
attachment re-scans; list them with `curl .../metrics | grep '^# HELP'`.

Examples:

```
# search p95 over a day; read this before changing the web client's 250 ms search pause
histogram_quantile(0.95, sum by (le) (rate(mailroom_search_duration_seconds_bucket{kind="search"}[1d])))
# image-proxy refusal rate
sum(rate(mailroom_proxy_refusals_total[1h])) / (sum(rate(mailroom_proxy_refusals_total[1h])) + sum(rate(mailroom_proxy_fetches_total[1h])))
```

Counters reset on restart, so `mailroom-admin proxy-report` (which reads the database) does not show
them. Each proxy refusal also writes one INFO line, `image proxy refused`, with a fixed `reason`
(for example `egress_refused`, `upstream_status`, `content_type`, `too_large`, `rate_limited`). When
the sender failed, it also names the sender's registrable domain only, `ip-literal` for an address,
never the full host, path or query (which can carry a recipient token).

## Logs

Logs are JSON by default (`MAILROOM_LOG_FORMAT`) and rotate at 10 MB times 5 files per container. A
grep written for text format matches nothing against JSON.

```sh
docker compose -f deploy/compose.yml logs -f mailroom
docker compose -f deploy/compose.yml logs mailroom | grep 'sync pass complete'
```

**What normal looks like.** Two lines matter, and they are different:

```
"msg":"account synced","pass":0,"name":"…","mailboxes":6,"mailbox_errors":0
"msg":"sync pass complete","pass":0,"accounts":1,"disabled":0,"synced":1,"failed":0
```

- `synced` equal to the number of accounts, with `failed` and `disabled` at 0 and `mailbox_errors` at
  0, is the check. A pass with nothing due logs `"accounts":0,"synced":0`, which looks calm and
  means nothing ran.
- Once caught up, a pass reports `messages:0, flag_updates:0, bodies_fetched:0`. The first sync of a
  multi-gigabyte mailbox can take hours, and the attachment sweep takes more.
- `mailboxes_deferred` is not an error count: mailboxes the pass did not ask about because the
  server rate-limited `UID SEARCH`. They are due next pass. If it stays above 0 on an account with
  many folders, lower `MAILROOM_IMAP_SEARCH_RATE` (default 30 a minute): the provider answers fewer searches than that.
- `mailboxes_observed` is the SEARCH rate (one `UID SEARCH` per folder observed). Compare its
  hourly sum with `MAILROOM_IMAP_SEARCH_RATE`. `mailboxes_unchanged` and `observations_skipped` are
  work skipped on purpose and read 0 on the first pass after an upgrade.
- Boot lines: `operator ready`, `declared account onboarded` / `account already configured`,
  `user fan-out: starting per-user loops`, `attachment scanning is ON`, and `login provisioned` at
  each sign-in.

**WARN lines that are expected:**

- `mailboxes deferred to the next pass`: the account is being paced, not failing.
- The dev-auth warning on every boot of a dev-auth instance.
- The prefetch ceiling WARN, at most hourly. Nothing kept is deleted.
- `no handler registered for job kind` after rolling back to an older version. It retries.
- `writeback is DISABLED instance-wide` when you set ingest-only on purpose.

A WARN naming a disabled account, a scanner that is unreachable, or `PGP ROOT MISMATCH` (ERROR) is
not benign. See [troubleshooting](troubleshooting.md).

## Find slow queries

Postgres loads `pg_stat_statements`. It stores statement text with every literal replaced by `$1`,
`$2`, never the values, so no mail content is recorded. Keep `pg_stat_statements.track_utility=off`:
otherwise utility statements such as `ALTER ROLE … PASSWORD '…'`, which the bootstrap steps run on
every `up`, would be recorded verbatim.

Read it as the superuser. This is read-only:

```sh
docker compose -f deploy/compose.yml exec -T postgres psql -U postgres -d mailroom -X <<'SQL'
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

The counters cover everything since the last reset. To measure one action, run
`SELECT pg_stat_statements_reset();`, do the action, then read again. The reset discards history, so
treat it as a measurement step. On an existing install, the query fails with `relation
"pg_stat_statements" does not exist` until Postgres has restarted once and `role-bootstrap-owner`
has run again; `docker compose up -d` does both when the compose file changed.

## Backups

`scripts/backup.sh` writes one encrypted artifact: the Postgres dump (including the sealed keys), the
encrypted blobs, a manifest with the schema version, and checksums. The search index is never backed
up; it is rebuilt. `scripts/restore-drill.sh` proves a backup restores, and
`scripts/backup-freshness.sh` is the signal that goes red when backups stop. The unit that calls
them, and the offsite copy, belong to your host automation.

> **Important:** a backup that has not been restored is a hypothesis. Run the drill. Back up the
> root keys separately: [key escrow](../key-escrow.md).

Full procedure: [backup and restore runbook](../backup-restore-runbook.md).

## Related

- [Troubleshooting](troubleshooting.md)
- [Security](security.md)
- [Backup and restore runbook](../backup-restore-runbook.md)
