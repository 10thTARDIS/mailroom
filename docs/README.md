# Mailroom documentation

## Running Mailroom

- [Deploying Mailroom](../deploy/README.md) — the Docker Compose stack, secrets,
  mail accounts and sign-in, step by step.
- [Connecting Gmail and Outlook](oauth-providers.md) — registering the OAuth
  apps so users can sign in to their mail providers.
- [AI tools over MCP](mcp.md) — connecting an assistant with a scoped API key.
- [Backups and restore](backup-restore-runbook.md) and
  [moving to a new instance](production-cutover-runbook.md).
- [Key escrow](key-escrow.md) — the two root secrets and what losing each costs.
- [Migrations](migrations.md) — how schema changes ship.

## Working on Mailroom

- [Developing Mailroom](development.md) — layout, test scripts, house rules.
- [Contributing](../CONTRIBUTING.md) — how to help, and the privacy rule for
  issues and fixtures.
- [Security policy](../SECURITY.md) — reporting a vulnerability privately.
- [Licence](license.md) — AGPL-3.0-only, and why.

## Reference

- [The specification](mailroom-spec.md) — authoritative; deviations need an ADR.
- [Feature matrix](mailroom-feature-matrix.md) — every feature, its status and
  where it is tracked.
- [Data model](mailroom-data-model.md) and [`db/schema.sql`](../db/schema.sql).
- [Hostile content](hostile-content.md) — the threat model for reading mail.
- [MIME policy](mime-policy.md).
- [Design review](mailroom-design-review.md) and
  [client design exploration](client-design-exploration.md) — why things are
  the way they are.
- [Architecture decision records](adr/README.md).
- [OpenAPI description](openapi.yaml) of the non-JMAP endpoints.
- [Accessibility](../web/ACCESSIBILITY.md) — the decisions the web app's
  accessibility depends on.
