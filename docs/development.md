# Developing Mailroom

Mailroom is a Go module (`mailroom`) plus a TypeScript/React web app under
[`web/`](../web/). CGO is used only for the Tantivy search engine, behind a
build tag ([ADR-006](adr/ADR-006-search-tantivy-go-pending-spike.md)). The
canonical build and test path is Docker, so you need neither a local Go nor a
local Node toolchain — only Docker and a POSIX shell.

## Layout

| Path | What lives there |
|---|---|
| `cmd/` | The server (`mailroom`), the operator CLI (`mailroom-admin`), the crypto process, import, export, migrations and the restore drill |
| `internal/store` | The **only** package that touches PostgreSQL (row-level security, the change log) |
| `internal/api` | JMAP, the web app's endpoints and the image proxy; `internal/mcp` is the MCP server |
| `internal/mailsync`, `internal/sync` | The IMAP sync engine and writeback |
| `internal/search` | Per-user Tantivy indexes and the query language |
| `web/` | The bundled web app ([web/README.md](../web/README.md)) |
| `db/` | Migrations and `schema.sql`, the reference DDL |
| `deploy/` | The Docker Compose stack and its walkthrough |
| `docs/` | The spec, the data model, threat models, runbooks, ADRs |

## Running the tests

Unit tests need nothing but Go (or Docker running the `golang` image):

```sh
go test ./...
```

Everything that needs real infrastructure has its own script, each self-contained
in Docker:

```sh
sh scripts/integration-test.sh    # store + RLS isolation tests (real Postgres)
sh scripts/migrate-check.sh       # migrations apply and match db/schema.sql
sh scripts/mailsync-test.sh       # sync engine against a live Dovecot
sh scripts/tantivy-test.sh        # the CGO search backend
sh scripts/gate-e2e.sh            # the login gate against a real OIDC provider
sh scripts/hostile-content-e2e.sh # hostile mail neutralised in headless Chromium
sh scripts/a11y-e2e.sh            # axe-core and the accessibility tree, every surface
sh scripts/restore-drill.sh       # back up, restore, prove the restore is alive
```

The web app builds and tests with `npm ci && npm run build && npm test` in
`web/`.

CI ([`.github/workflows/ci.yml`](../.github/workflows/ci.yml)) runs all of
these and more on every pull request, along with `gofmt`, `golangci-lint`,
`govulncheck`, OpenAPI lint and the portability check. Every job is required
before merge.

## The rules that hold the design together

- **Every mutation of a JMAP-visible object appends a `change_log` row in the
  same transaction.** That one ordered stream drives JMAP `/changes`, the
  search indexer and client sync cursors. It is enforced by the store layer and
  its tests; there is no other write path.
- **No identity, no rows.** Every user table has forced row-level security.
  Code gets a database handle only through `Store.UserTx` (a user identity) or
  `Store.SystemTx` (no user rows visible). The negative-test isolation suite is
  a required CI job.
- **Report before you act.** Operator commands that change data print what they
  would do and need `--apply` to do it.
- **Probe your tests.** When a new test passes first time, break the thing it
  covers and watch it go red.

[`CLAUDE.md`](../CLAUDE.md) holds the full set of project conventions, and
[`docs/mailroom-spec.md`](mailroom-spec.md) is the authoritative specification.
A change that deviates from the spec needs an ADR in [`docs/adr/`](adr/).

## Releases

Versions are semantic. Every change lands in [`CHANGELOG.md`](../CHANGELOG.md)
under `[Unreleased]`, written for the person using Mailroom. Tagging `vX.Y.Z`
on a commit whose CI on `main` is green publishes the image
`ghcr.io/10thtardis/mailroom:X.Y.Z` and a GitHub release with that version's
changelog section
([`.github/workflows/tag-provenance.yml`](../.github/workflows/tag-provenance.yml)).

## Related guides

- [Deploying Mailroom](../deploy/README.md)
- [Connecting Gmail and Outlook with OAuth](oauth-providers.md)
- [AI tools over MCP](mcp.md)
- [Migrations](migrations.md)
- [Backups and restore](backup-restore-runbook.md)
