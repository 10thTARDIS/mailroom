<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Mail accounts

How to declare, change and pause the mail accounts that Mailroom syncs.

**On this page:** [The accounts file](#the-accounts-file) ·
[What editing the file does](#what-editing-the-file-does) ·
[Change a live account](#change-a-live-account) ·
[Send as aliases](#send-as-aliases-is-a-permission-at-the-provider) ·
[Gmail and Outlook](#gmail-and-outlook-oauth) · [Ingest-only mode](#ingest-only-mode) ·
[Replay or discard the ledger](#replay-or-discard-the-ledger)

Users can also add accounts themselves in the web client. The file is for accounts you want present
from first boot. Commands run as shown in [admin CLI](admin-cli.md#run-a-command).

## The accounts file

`deploy/secrets/accounts.json` is read at boot and mounted at `/run/secrets/mailroom_accounts`.
Use `{"accounts": []}` if you declare none. It is owned by the operator user from
`MAILROOM_OPERATOR_SUBJECT` and `MAILROOM_OPERATOR_EMAIL`; leave those unset for a pure
multi-user instance.

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
      "password": "<app password>",
      "identities": ["you@example.com"],
      "wildcard_domains": [],
      "writeback_enabled": true
    }
  ]
}
```

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | Stable key. Renaming it creates a second account. |
| `imap_host`, `smtp_host`, `username`, `password` | yes | Connection details. |
| `imap_port`, `smtp_port` | no | Default `993` and `465` (implicit TLS). |
| `provider_profile` | no | Provider quirks, default `generic`. |
| `smtp_username`, `smtp_password` | no | Separate submission login. Default: the IMAP login. |
| `identities` | no | Exact addresses this account may send as. Default: just the username. |
| `wildcard_domains` | no | Domains where every address may be sent from. Only list what the provider has granted. |
| `writeback_enabled` | no | `false` starts the account [ingest-only](#ingest-only-mode). Honoured only at creation. |

Parsing is strict: an unknown key is a boot error. Duplicate names are refused.

Onboarding dials IMAP to validate each new account. A failure does not stop the boot:

- **The server rejected it** (bad password, refused configuration): the account is stored with
  sync **disabled**. Retrying a wrong password can lock the account at the provider. Fix the
  credentials, then run `enable-account` (below).
- **Anything else** (connection, DNS, TLS, timeout): the account stays enabled and the sync loop
  retries with backoff.

While any account is disabled, every sync pass names it at WARN and
`mailroom_sync_disabled_accounts` is above zero.

## What editing the file does

The file is additive, not declarative. Boot onboards new names and adds identities. It never
changes or removes an existing account, so a redeploy cannot re-dial a working account or rotate
a password under a running sync.

| You change in the file | Boot does |
|---|---|
| Add a new account entry | Onboards it (IMAP-validated). |
| Add `identities` or `wildcard_domains` to an existing account | Adds them. Never removes. |
| Change `password` or `username` | Nothing. |
| Change hosts, ports or `provider_profile` | Nothing. |
| Change `writeback_enabled` | Nothing. |
| Remove an entry | Nothing. The account keeps running. |

Automation may assume that a new entry onboards and an added identity is granted. It must not
assume anything else changes live.

To see where the file and the database disagree (it never prints a password):

```sh
mradmin accounts-report --user-id <id> --fail-on-drift
```

It reports per field `matches` or `DIFFERS`, accounts stored with sync disabled, and identities
not yet added. `--fail-on-drift` exits non-zero when anything needs action, so it can gate a
deploy.

After a deploy that changed the file, do not trust the absence of an ERROR line: a rejected
account logs at WARN. Check the sync pass:

```sh
docker compose -f deploy/compose.yml logs mailroom | grep 'sync pass complete'
# "accounts":N,"disabled":0,"synced":N,"failed":0
```

`disabled` or `failed` above zero is a real problem. Run `accounts-report` to find which.

## Change a live account

| Task | Command or path |
|---|---|
| Rotate credentials | Web client account settings, or `set-account-credentials` (below). |
| Change hosts, ports or profile | Web client account settings (re-supplies the whole connection). |
| Re-enable a disabled account | `mradmin enable-account --user-id <id> --account-id <id>`. Re-validates first, `--force` skips that. A restart does not do this. |
| Pause an account | `mradmin disable-account --user-id <id> --account-id <id>` |
| Turn writeback on or off | JMAP `Account/set` with `writebackEnabled`. The CLI only sets it at creation, with `add-account --ingest-only`. |
| Remove an account | Settings, "Remove account". There is no CLI command, and deleting the file entry does nothing. |
| Find ids | `mradmin list-accounts --user-id <id>` |

Rotate a password from the CLI. Both forms validate the new credential first (IMAP always; SMTP
too whenever one is written), and re-seal it only on success. A rejected credential leaves the
stored one untouched and exits non-zero. It prints, before writing, which of IMAP and SMTP will
change. Never pass a password on the command line.

```sh
mradmin set-account-credentials --user-id <id> --account-id <id> \
  --from-accounts-file --account <name>      # reads the rotated entry from the accounts file
mradmin set-account-credentials --user-id <id> --account-id <id> \
  --password-file /run/secrets/new_password [--smtp-too]
```

`--password-file` carries one password. When the account's stored SMTP login differs from its
IMAP login, it keeps the SMTP pair and says `SMTP login KEPT`. Add `--smtp-too` to write the new
password to the SMTP login as well (the SMTP username is kept). The web client has no such
protection: it seals the IMAP login as the SMTP login too. `--from-accounts-file` writes the file's
own SMTP login and refuses `--smtp-too`.

## Send as aliases is a permission at the provider

If the mailbox receives at a catch-all or derived addresses and you reply **from** them, the
provider must allow this mailbox to send as those addresses. Receiving and sending as an address
are separate permissions; a catch-all grants only the first. Without the second, each send is
refused at the envelope sender:

```
mail from: 550 "5.7.1 sender address rejected: not allowed"
```

Mailroom cannot detect this in advance. Compose accepts the identity, and the refusal arrives
after the undo window, reported in the Outbox. Grant it per mailbox (a second mailbox on the
same domain does not inherit it) and prove it with one send from a derived address to an external
recipient that actually arrives.

## Gmail and Outlook (OAuth)

Users can connect Gmail or Outlook.com / Microsoft 365 by signing in at the provider. You register
your own OAuth client and set its id and secret. Outlook.com no longer accepts IMAP passwords, so
this is its only route. The full guide is [Gmail and Outlook](../oauth-providers.md).

In short: set `MAILROOM_OAUTH_GOOGLE_CLIENT_ID` / `_SECRET_FILE` and/or the Microsoft pair (see
[configuration](configuration.md#sign-in)), register
`https://mail.example.com/oauth/callback/google` and `/oauth/callback/microsoft` at the provider
(the boot log prints the exact values as `redirect_uri`), and restart. A provider with only one of
its id and secret stops the server.

A connected Gmail account is currently **held with sync off** (health code `gmail_sync_pending`).
Outlook and Microsoft 365 accounts sync straight away. Preview a Gmail mailbox read-only with
`gmail-census`.

## Ingest-only mode

Ingest-only mode stops Mailroom from changing anything on the provider: no moves, flags or
expunges, and **no sending**. The account still syncs. Use it for a shadow period before cutting
over to a real mailbox.

- Per account: `writeback_enabled: false` in the file (honoured at creation) or
  `add-account --ingest-only`.
- Whole instance: `MAILROOM_WRITEBACK_DISABLED=true`. The boot log then carries the WARN
  `writeback is DISABLED instance-wide`. No such line and no boot failure means it is not set.
- Sending is refused by `Email/set` and, independently, by the dispatcher.

Actions taken during the shadow period are recorded as `suppressed` ledger entries. Lifting
ingest-only does **not** replay them. See the [first-mailbox runbook](../production-cutover-runbook.md#2-the-shadow-period-ingest-only-adr-028).

## Replay or discard the ledger

`writeback-ledger` reports the suppressed entries per account and per operation (flag, move,
append, expunge...), never as one total.

```sh
mradmin writeback-ledger --user-id <id> --account-id <id>                   # report
mradmin writeback-ledger --user-id <id> --account-id <id> --apply           # replay to pending
mradmin writeback-ledger --user-id <id> --account-id <id> --discard         # report what would be discarded
mradmin writeback-ledger --user-id <id> --account-id <id> --discard --apply # discard for good
```

- **`--apply` refuses while the account is still shadowed**, checking both the account setting
  and `MAILROOM_WRITEBACK_DISABLED`. Lift ingest-only first, then replay. Otherwise the replayed
  entries sit invisible until both switches lift, then fire unreviewed.
- **Stale and superseded entries are skipped and stay in the ledger.** Stale: the message gained
  a live copy after the entry was made. Superseded: a later append of the same draft exists. The
  report prints both counts and how many `--apply` would replay.
- **`--discard --apply` is final:** it moves the ledger to the terminal `discarded` state.

## Related

- [Authentication](authentication.md)
- [Gmail and Outlook](../oauth-providers.md)
- [Admin CLI](admin-cli.md)
- [Connecting your first real mailbox](../production-cutover-runbook.md)
