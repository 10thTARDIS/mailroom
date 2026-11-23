<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Admin CLI (`mailroom-admin`)

How to run `mailroom-admin`, and the commands an operator actually needs, grouped by task.

**On this page:** [Run a command](#run-a-command) · [Report first, then `--apply`](#report-first-then---apply) ·
[Users](#users) · [Accounts](#accounts) · [Reports and health](#reports-and-health) ·
[Backfills and repairs](#backfills-and-repairs) · [PGP](#pgp) · [Rules and sorting](#rules-and-sorting) ·
[Other commands](#other-commands)

## Run a command

`mailroom-admin` ships in the `mailroom` image at `/mailroom-admin` and needs that container's
environment and secrets. Run it inside the stack:

```sh
mradmin() { docker compose -f deploy/compose.yml exec mailroom /mailroom-admin "$@"; }
mradmin <command> --user-id <id> [flags]
```

Add `-T` to `exec` in scripts with no terminal. Other binaries in the image (`/mailroom-migrate`,
`/mailroom-census`, `/mailroom-crypto`) need `docker run --entrypoint`, because the image entrypoint
is `/mailroom` and `docker run` appends arguments to it.

Most commands take a user's id (a ULID) as `--user-id` (some use `--user`). Find ids in the
`login provisioned` log line, or:

```sh
docker compose -f deploy/compose.yml exec postgres psql -U postgres -d mailroom \
  -c 'select id, email, role, disabled_at from app.users'
```

## Report first, then `--apply`

Anything that changes data prints a report and writes nothing until you pass `--apply`.
Read the report, which gives per-case counts rather than one total, then rerun with `--apply`.
Backfills work in batches while Mailroom keeps running, and a second run changes nothing.

Exceptions that act immediately: `add-account`, `enable-account`, `disable-account`,
`set-account-credentials`, `sync-once`, `user`, every `pgp-*` command that writes (`pgp-revoke` is
irreversible), `quarantine --release/--revoke`, `placement-report --reset-gate` and
`drawer-report --enable/--disable`. Some older commands have `--dry-run` (default true) as well as
`--apply`.

## Users

| Command | What it does |
|---|---|
| `user disable\|enable <id-or-email>` | Reversible block of sign-in, sessions, keys and background work. Refuses to disable the last enabled user. |
| `delete-user --user-id U [--apply --confirm-user-id U --actor A --accept-key-loss]` | Irreversible hard delete of a user and all their data. Report first; `--apply` needs the id typed again. |
| `activity [--since 7d\|DATE] [--source S,..] [--user ID [--limit N]]` | Activity records per kind, or one user's merged entries. Read-only, never content. |
| `export-report --user-id U` | Per-table export disposition. Read-only. |

## Accounts

| Command | What it does |
|---|---|
| `add-account --user-id U --name N --imap-host H --imap-user X (--imap-pass-file F) --smtp-host H [--smtp-port --smtp-user --smtp-pass-file --provider-profile --ingest-only]` | Create an account. `--imap-pass-file` keeps the password off the command line. |
| `list-accounts --user-id U` | Accounts and ids. |
| `accounts-report --user-id U [--accounts-file F] [--fail-on-drift]` | File versus database differences, read-only. |
| `enable-account --user-id U --account-id A [--force]` | Re-validate and switch sync back on. |
| `disable-account --user-id U --account-id A` | Pause an account. |
| `set-account-credentials --user-id U --account-id A (--password-file F \| --from-accounts-file --account NAME) [--smtp-too --username]` | Rotate credentials, validating first. |
| `sync-once --user-id U` | Run a sync pass now. |
| `writeback-ledger --user-id U --account-id A [--apply] [--discard]` | Report, replay or discard ingest-only entries. |
| `provision-identity --user-id U (--domain D \| --address A) [--account-id --display-name --apply]` | Add a sending identity. |
| `consolidate-folders --user-id U --account-id A --from PATTERN --into FOLDER [--delete-empty PATTERN] [--max-folders N] [--apply] [--despite-ingest-only]` | Move mail between folders and delete emptied ones **on the provider**. |
| `gmail-census --user-id U --account-id A [--limit N --top N --budget-mb N --skip-dedup]` | Read-only preview of a Gmail import. |
| `mailbox-roles [--user U] [--verbose]` | Which special folders the provider advertises. Exits non-zero if drafts or sent is missing. |
| `sieve-report --user-id U [--account NAME] [--show-active]` | What the provider's ManageSieve holds. Read-only. |

See [accounts](accounts.md).

## Reports and health

| Command | What it does |
|---|---|
| `attachment-report [--user U] [--verbose] [--apply]` | Attachments by scan verdict and the re-scan queue (`due now`). `--apply` repairs only wrong attachment flags. |
| `quarantine --user U [--release MSG:PART] [--revoke MSG:PART]` | What the download gate refuses; release or revoke one part. |
| `jobs-report` | Background jobs per kind and state, and what the daily sweep would delete. |
| `blob-report [--user U] [--verbose]` | Blob files versus database rows. |
| `proxy-report --user-id U [--top N --fetch --prefetch-window D]` | Remote-image hosts, cache size and what prefetch would fetch. Counters live in `/metrics`. |
| `auth-report --user-id U [--authserv] [--no-ar] [--dkim]` | Sender-authentication corpus. `--authserv` per id and month, for choosing a date bound. |
| `sender-report --user U [--verbose]` | Sender-warning corpus. |
| `compact-changelog --user-id U [--retention D] [--apply]` | Change history older than retention (default 90 days, minimum 7). |

## Backfills and repairs

| Command | What it does |
|---|---|
| `backfill-recipients [--user U] [--apply] [--batch N]` | Fill To, Cc, Bcc and list-id columns for older mail. |
| `placement-backfill-summaries --user-id U [--apply]` | Write Activity rows for older placement backfills. |
| `area-index [--user-id U] [--apply] [--batch N]` | Fill the stored area of existing mail. |
| `backfill-attachments --user U [--apply]` | Create missing attachment rows. |
| `backfill-model-blobs [--user U] [--apply]` | Register model files that lack a blob row (else they leak). |
| `backfill-dedup-hash --user-id U [--apply] [--recompute]`, `collapse-duplicates --user-id U [--apply --limit N]` | Hash and merge duplicate messages. |
| `collapse-provenance --user-id U [--apply]` | Restore label attribution lost by past merges. |
| `contact-dates-backfill`, `contact-names-clean --user-id U [--apply]` | Correct contact first/last seen dates; strip control characters from names. |
| `repair-contacts`, `repair-sent-drafts`, `backfill-alias --user-id U [--apply]` | Contact, Sent/Draft and alias clean-ups. |
| `thread-report --user U [--apply]` | Find (and with `--apply` merge) conversations split by a shared Message-ID. |
| `resnippet --user-id U [--apply]` | Recompute message snippets. |

All take `--batch N` where batching applies. See [upgrading](upgrading.md#run-the-post-upgrade-commands).

## PGP

| Command | What it does |
|---|---|
| `pgp-generate --user-id U --email E [--name --expires-days N --print-public]` | Make a key. Expires after 730 days by default, `0` never. |
| `pgp-import --user-id U --file F [--passphrase-file F]` | Import a key. |
| `pgp-export-wrapped --user-id U --key-id K --passphrase-file F --out F` | Passphrase-protected backup of a private key. |
| `pgp-list --user-id U` | Keys and ids. Read-only. |
| `pgp-bind --user-id U --key-id K --identity-id I` | Make a key an identity's signing key. |
| `pgp-publish`, `pgp-unpublish --user-id U --key-id K` | Serve or withdraw a key over WKD. |
| `pgp-retire --user-id U --key-id K [--unbind]` | Decrypt-only and unpublished. |
| `pgp-extend --user-id U --key-id K --days N` | Re-sign with a new expiry; same fingerprint. |
| `pgp-revoke --user-id U --key-id K --reason compromised\|superseded\|retired --confirm LAST8` | Real OpenPGP revocation. Irreversible. |
| `key-discover --user U [--apply] [--limit N]` | Look up correspondents' published keys. |
| `pgp-report`, `pgp-verify --user-id U` | Signing prevalence and key expiry; signature checks. Read-only. |

Keys, backup and DNS: [PGP keys](pgp-keys.md).

## Rules and sorting

| Command | What it does |
|---|---|
| `area-plan --user-id U --plan F [--apply]` | Load an area plan; creates and updates, never deletes. |
| `area-report --user-id U [--plan F]` | How mail would resolve into areas. Read-only. |
| `materialize-area-labels --user-id U [--apply]` | Write an area's label onto mail that only landed there by verdict. |
| `day-one --user-id U [--account-id A --window N --plan F --apply --undo RUN --runs]` | Turn a folder-sorted mailbox into a desk: census and preview first, `--undo` reverses a run. |
| `day-one-server-archive --user-id U [--apply --limit N]` | Move day-one archives still in INBOX on the server to Archive. |
| `placement-report --user-id U [--reset-gate AREA]` | Learned placement outcomes and correction gates. Read-only. |
| `placement-measure --user-id U [--split-days N --window-days N --recent-days N --auth-scan-max N --no-headers]` | Measure learned sorting before changing it. Read-only counts: why arrivals were not placed (30 days), filed senders refused only for authentication, recency for mixed senders, the "opened and left alone" and "archived unread" signals scored on a time split, and the List-Id ceiling. |
| `placement-backfill --user-id U [--exclude A] [--apply] [--undo BATCH]` | Apply learned placement to mail already on the desk. |
| `placement-explain --user-id U (--message-id M \| --sender A)` | Why placement did or did not move a message. |
| `drawer-report --user-id U [--apply] [--enable\|--disable]` | Preview the drawer auto-archive sweep; flip its switch. |
| `suggest-rules`, `accept-rules`, `apply-rules --user-id U [--apply]` | Persist, accept and apply rules derived from your filing. `apply-rules --undo` reverses. |
| `delete-label --user-id U --name N [--apply]` | Delete a label. |
| `untangle-labels --user-id U --plan F [--apply]` | Batch label clean-up from a plan. |
| `classify-tabs`, `map-label-tabs --user-id U [--apply]` | Fill stored tab verdicts; point labels at tabs. |

### `placement-measure`

Read-only, and it changes nothing about where mail goes. It prints counts and your area names only (no addresses, senders or subjects). Area names are yours: review them before pasting the output anywhere public. Its six sections:

1. **Why arrivals were not placed.** The last 30 days, per reason, from a per-day tally kept at
   arrival. It records going forward, so the section says how many of the 30 days it covers.
2. **Authentication-only refusals.** Messages from senders you have filed (confident or thin
   evidence, not already placed by a label) that are refused for authentication and nothing
   else, how many senders that is, and how many an enabled rule already covers. It reads each
   message's raw headers like `placement-backfill`; `--no-headers` uses the stored failure flag
   instead, which is a lower bound. `--auth-scan-max` caps the headers read and the report says
   when it bit.
3. **Recency.** For senders you file into more than one area, how many end in a run of the last
   2, 3 or 5 filings into one area, and how much mail they received lately.
4. **"Left alone."** Desk placements you opened and did not move within 7 days, as a candidate
   signal: events before the split day teach, later mail scores. Shown beside the same signal
   with "opened" dropped and the rate at which you move mail anyway.
5. **"Archived unread."** Senders whose mail you archive unopened, and what you later did with
   their mail in Primary, per area of origin.
6. **List-Id.** How many addresses send under more than one List-Id, and the ceiling on mixed
   senders a List-Id key could separate.

`--split-days` (default 45) sets the split day D, printed in the report, and `--window-days`
(default 365) how far back sections 4 and 5 read.

## Other commands

Developer, diagnostic or tuning commands. They report first; those that write take `--apply`:

`classify` kind counts · `kind-sweep` kind thresholds · `induce` rules a pattern would propose ·
`suggest-report` rule-derived suggestions · `tabs` tab distribution · `tab-eval` tab classifier
replay · `rescore` · `train-labels` · `backfill-function-signals` (sender-function signals; `--apply`) · `priority-report` · `priority-reply-backfill` ·
`priority-rescore` · `priority-train` (priority model; the last three take `--apply`) ·
`diff-duplicates` duplicate byte diffs · `css-report` · `pixel-report` · `contact-dates-report` ·
`nudge-report` follow-up accuracy · `from-field-report` odd From fields · `plaintext-report`
plain-text wrapping · `plan-check` query-plan timing.

Every subcommand is registered in `cmd/mailroom-admin/main.go`. Run it with a bad name to list them.

## Related

- [Upgrading](upgrading.md)
- [Accounts](accounts.md)
- [Monitoring](monitoring.md)
- [Admin guide](README.md)
