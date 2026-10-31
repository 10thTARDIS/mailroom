# Mailroom

Mailroom is a self-hosted email **client** for the IMAP/SMTP accounts you
already have. It is **never a mail server**: your mail stays with your
provider. Mailroom keeps a synchronised copy, indexes it for fast local
search, sorts it, and writes your changes back. It speaks
[JMAP](https://jmap.io) to its clients and IMAP/SMTP upstream; the bundled
React web app is one JMAP client, and a conformant JMAP client can connect too.

It is a Go server on PostgreSQL 18, with forced row-level security so no user
can see another's rows, message bodies in an encrypted blob store, and PGP
keys behind a separate crypto process. It runs as a Docker Compose stack.

**Status: 1.x, shipping.** Every release is tagged, recorded in
[CHANGELOG.md](CHANGELOG.md), and published as a GitHub release; the newest one
is at the top of the changelog. [docs/mailroom-spec.md](docs/mailroom-spec.md) is the authoritative
specification.

## What it does

- **Accounts and sync.** Several IMAP/SMTP accounts per user, in one unified
  view or one at a time. Incremental CONDSTORE sync with IDLE push for the
  Inbox, plus reconciliation for what a plain poll misses (mail moved or
  deleted in another client, UIDVALIDITY changes, silently truncated fetches).
  An ingest-only mode observes a mailbox without ever writing to it. Bulk
  import of mbox, maildir and Google Takeout archives. Outlook and Microsoft
  365 accounts connect by signing in (OAuth), with no passwords.
- **The desk.** The inbox arranged into **areas** (built from category tabs
  and labels), split by **the line**, a learned priority score you can move,
  into what needs you now and per-area drawers that can auto-archive. Starred
  mail collects in the in-tray. A day-one wizard turns a folder-sorted mailbox
  into a desk without silently moving anything.
- **Sorting.** **Move to area** for any selection, or everything from one
  sender, with one Undo. **Learned placement** files a sender's new mail where
  you have filed it before, only for authenticated senders, behind a
  correction gate. Deterministic rules with a preview before you save, and
  "apply to existing mail". Rule suggestions induced from your own filing.
  Snooze, pin and mute.
- **Search.** Per-user Tantivy indexes with typed operators (`from:`, `to:`,
  `subject:`, `label:`, `area:`, `list:`, `filename:`, `larger:`, `is:`,
  `has:pdf`, relative dates like `newer_than:7d`), `OR` and brackets, search
  by domain (`from:shop.example`), phrases and typo tolerance. Results update
  as you type, ordered newest first or by best match; an options form writes
  the query for you; contacts and areas are suggested; and saved searches
  work as smart folders.
- **Reading safely.** HTML sanitised on the server and rendered in a
  sandboxed frame; remote images blocked by default and, when allowed, fetched
  through your own server; tracking pixels suppressed; sender-authentication
  notices and spoofing warnings; attachments previewed in a viewer and scanned
  with ClamAV, with quarantine and release.
- **Writing.** Rich text, plain text or Markdown; alias-aware From with
  server-side validation; Reply, Reply all and Forward; autosaved drafts;
  Undo Send, Send later and Send & archive; signatures and templates; an
  Outbox; bounce (DSN) tracking; contact autocomplete.
- **Encryption.** PGP encryption and decryption through a separate crypto
  process, signing and signature checks, WKD key publishing and automatic
  discovery of correspondents' keys.
- **Contacts, calendar and away.** Contacts built from your mail, with notes,
  merging and VIPs; meeting invitations answered from the message; an
  out-of-office reply managed through the provider's ManageSieve.
- **AI tools, on your terms.** An MCP server with scoped API keys, rate and
  budget limits, and approval before anything is sent or deleted.
- **The web app.** Responsive from phone to desktop, installable as a PWA,
  keyboard-first with a command palette, light and dark themes, Web Push
  notifications, and accessibility checked in CI with axe-core and the
  browser's accessibility tree.
- **Running it.** OIDC sign-in with sessions that renew quietly and a
  signed-in devices page, with every unauthenticated route closed and tested; per-user isolation enforced by row-level security and a required
  negative-test suite; backups with a scripted restore drill; Prometheus
  metrics; per-user export and hard delete; an operator CLI whose writing
  commands report before they act.

## What is planned

- **Next in 1.x:** live Gmail sync through OAuth (connecting works today; sync
  is switched on after live testing); OAuth sign-in for AI tools; a redesigned
  Settings and Contacts.
- **2.x:** optional, self-hosted LLM features; native iOS and Android apps;
  CardDAV contacts; bulk unsubscribe; semantic search; S/MIME.
- **3.x:** a desktop client, shared mailboxes and delegation, CalDAV, and
  hardware-key PGP.

## Running it

The deployment stack (Postgres, migrations, the crypto process, malware
scanning and the server, with secrets as mounted files) is in
[`deploy/`](deploy/). The walkthrough, including secrets, mail accounts and
OIDC, is **[deploy/README.md](deploy/README.md)**. In short:

```sh
cd deploy
cp .env.example .env      # set OIDC, the operator, and MAILROOM_IMAGE to a released tag
#   ...create deploy/secrets/* (deploy/README.md §2)
docker compose -f compose.yml up -d
```

Pin `MAILROOM_IMAGE` to an exact release (`ghcr.io/10thtardis/mailroom:X.Y.Z`);
`:latest` is never a pin. The server runs non-root on a read-only root
filesystem and refuses to start without a usable authentication path.

## Development

A Go module (`mailroom`) plus a TypeScript web app under `web/`. CGO is used
only for the Tantivy search engine, behind a build tag. The canonical build
and test path is Docker, so no local Go or Node toolchain is needed:

```sh
sh scripts/integration-test.sh    # store + RLS isolation tests (real Postgres)
sh scripts/migrate-check.sh       # migrations apply and match db/schema.sql
sh scripts/mailsync-test.sh       # sync engine against a live Dovecot
sh scripts/tantivy-test.sh        # the CGO search backend
sh scripts/gate-e2e.sh            # the login gate against a real OIDC provider
sh scripts/hostile-content-e2e.sh # hostile mail neutralised in headless Chromium
sh scripts/restore-drill.sh       # back up, restore, prove the restore is alive
```

CI runs these and more on every push, with `golangci-lint`, `govulncheck` and
the web build. One invariant holds the design together: **every mutation of a
JMAP-visible object appends a `change_log` row in the same transaction**,
enforced by the store layer and its tests. See
Development and testing and
Architecture.

To let users connect Gmail and Outlook accounts, see the operator guide
[docs/oauth-providers.md](docs/oauth-providers.md).

Reference documents live in [docs/](docs/) (the spec, the feature matrix,
the data model, the hostile-content threat model, the runbooks) and
[docs/adr/](docs/adr/); [db/schema.sql](db/schema.sql) is the reference DDL.

## License

Copyright (C) 2026 Ryan Brooks.

Licensed under the **GNU Affero General Public License, version 3** — see
[`LICENSE`](LICENSE). Source files carry a one-line
`SPDX-License-Identifier: AGPL-3.0-only` rather than full header banners.

The AGPL was chosen deliberately (see
[ADR-011](docs/adr/ADR-011-license.md)). Mailroom exists because every capable
AI-triage mail product is cloud SaaS that egresses your mail; the AGPL is what
keeps a hosted fork from quietly becoming that. If you run a modified Mailroom
as a network service, your users are entitled to your source. Self-hosting it
for yourself, your family or your team carries no such obligation — that is the
case this project is built for, and the licence asks nothing of you there.
