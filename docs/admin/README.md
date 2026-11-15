<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Running a Mailroom server

Everything you need to install, configure, secure, upgrade and look after a Mailroom instance.
It assumes you are comfortable with Docker, DNS and an OIDC provider.

New here? Start with the [install guide](../../deploy/README.md), then work down the tasks below.

## Day-to-day tasks

| I want to... | Go to |
|---|---|
| Install Mailroom | [Install guide](../../deploy/README.md) |
| Look up a setting, its default, or a secret file | [Configuration](configuration.md) |
| Add a mail account, rotate a password, or re-enable a paused account | [Accounts](accounts.md#change-a-live-account) |
| Let users connect Gmail or Outlook | [Gmail and Outlook](../oauth-providers.md) |
| Cut over to a real mailbox safely (ingest-only) | [Ingest-only mode](accounts.md#ingest-only-mode), [first-mailbox runbook](../production-cutover-runbook.md) |
| Set up sign-in, admins, or sign-out | [Authentication](authentication.md#set-up-oidc) |
| Disable or re-enable a user | [Authentication](authentication.md#disable-and-re-enable-a-user) |
| Fix a wrong operator subject | [Authentication](authentication.md#recover-from-a-wrong-operator-subject) |
| Check disk encryption, TLS and the scanner | [Security](security.md) |
| Publish or back up a PGP key | [PGP keys](pgp-keys.md) |
| Upgrade, or roll back | [Upgrading](upgrading.md) |
| Run a command from the admin CLI | [Admin CLI](admin-cli.md) |
| Watch health, metrics and logs | [Monitoring](monitoring.md) |
| Back up and restore | [Backup runbook](../backup-restore-runbook.md), [key escrow](../key-escrow.md) |
| Connect an AI tool | [MCP tools](../mcp.md) |
| Work out why something is broken | [Troubleshooting](troubleshooting.md) |

## Pages

- [Configuration](configuration.md): every environment variable and secret file.
- [Accounts](accounts.md): the accounts file, aliases, OAuth, ingest-only mode and the writeback ledger.
- [Authentication](authentication.md): OIDC, the login gate, users and dev-auth.
- [Security](security.md): volume encryption, TLS, ClamAV, the crypto process and root keys.
- [PGP keys](pgp-keys.md): generating, publishing and backing up keys.
- [Upgrading](upgrading.md): new versions, post-upgrade commands and rollback.
- [Admin CLI](admin-cli.md): `mailroom-admin` and the report-first convention.
- [Monitoring](monitoring.md): probes, metrics, logs and slow queries.
- [Troubleshooting](troubleshooting.md): symptom, cause, fix.

## Related

- [Install guide](../../deploy/README.md)
- [Documentation index](../README.md)
- [Schema migrations](../migrations.md)
