<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Mailroom documentation

Find the page you need by what you want to do.

## Using Mailroom

The [user guide](guide/README.md) covers everything in the app, with no technical
knowledge assumed.

- [Getting started](guide/getting-started.md): signing in and finding your way around.
- [The desk and areas](guide/desk-and-areas.md) · [Sorting](guide/sorting.md) · [Search](guide/search.md)
- [Reading safely](guide/reading.md) · [Writing](guide/writing.md) · [Encryption](guide/encryption.md)
- [AI assistants](guide/ai-assistants.md) · [Keyboard shortcuts](guide/keyboard-shortcuts.md) · [FAQ](guide/faq.md)

## Running a Mailroom server

1. [Install Mailroom](../deploy/README.md) with Docker Compose.
2. Use the [operator guide](admin/README.md) for everything after that:
   [configuration](admin/configuration.md), [accounts](admin/accounts.md),
   [sign-in](admin/authentication.md), [upgrading](admin/upgrading.md),
   [admin commands](admin/admin-cli.md), [PGP keys](admin/pgp-keys.md), [monitoring](admin/monitoring.md) and
   [troubleshooting](admin/troubleshooting.md).
3. Connect [Gmail and Outlook](oauth-providers.md) and [AI tools over MCP](mcp.md).
4. Keep it safe: [backups and restore](backup-restore-runbook.md),
   [key escrow](key-escrow.md) and [security](admin/security.md).
5. Moving real mail in for the first time: [connecting your first real mailbox](production-cutover-runbook.md).

## Working on the code

- [Developing Mailroom](development.md): layout, tests and house rules.
- [Contributing](../CONTRIBUTING.md) and the [security policy](../SECURITY.md).
- [The web app](../web/README.md) and its [accessibility decisions](../web/ACCESSIBILITY.md).
- [Migrations](migrations.md): how schema changes ship.
- [Licence](license.md): AGPL-3.0-only, and why.

## Reference

| Document | What it is |
|---|---|
| [Specification](mailroom-spec.md) | What Mailroom is meant to be. Authoritative; changes need an ADR. |
| [Feature matrix](mailroom-feature-matrix.md) | Every feature, its status and where it is tracked. |
| [Decision records (ADRs)](adr/README.md) | Why each design decision was made. |
| [Data model](mailroom-data-model.md) | The database design, with [`db/schema.sql`](../db/schema.sql). |
| [Hostile content](hostile-content.md) | The threat model for reading mail. |
| [MIME policy](mime-policy.md) | How messages are parsed and what is refused. |
| [OpenAPI description](openapi.yaml) | The HTTP endpoints outside JMAP. |
| [Design review](mailroom-design-review.md), [client design exploration](client-design-exploration.md), [search spike](search-spike.md) | Background: how the design was reached. |
