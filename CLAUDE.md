# Mailroom — project instructions

## Open-source portability (from the spec header — binding for all contributions)

> **⚠ Open-source portability (add this to `claude.md` at project start):** This project may be open-sourced. All docs, examples, config samples, and code must stay free of anything identifying the operator or their self-hosted infrastructure — no personal domains, internal hostnames, VPS/homelab details, private repo/auth/service URLs, or personal email addresses. Use neutral placeholders (`example.com`, `mail.example.com`, `git.example.com`, `${OIDC_ISSUER}`, `you@example.com`). Naming external/third-party products generically (Gmail, Migadu, Ollama, Authentik, Caddy, etc.) is fine; naming anything the operator personally hosts is not. The project is private until at least a stable v1; the license decision is deliberately deferred until pre-publication (see §18).

**The licence is decided: AGPL-3.0-only** ([ADR-011](docs/adr/ADR-011-license.md)). `LICENSE` is in the tree and source
files carry `SPDX-License-Identifier: AGPL-3.0-only`. The quoted spec header's
"deferred" predates that decision.

**Enforced, not remembered.** `scripts/portability-check.sh` runs as a required
CI job. It has two layers: an **operator denylist** supplied from outside the
repository (the `MAILROOM_PORTABILITY_PATTERNS` repository secret — writing
those strings into the tree would itself be the leak), and an in-tree **URL host
allowlist** (`scripts/portability-hosts.allow`), so every external host this
repo names is a deliberate line rather than something that slipped past a list
of forbidden words. Reserved forms (`*.example`, `*.test`, `example.com` and
friends) and single-label compose service names pass structurally.

The rule has been broken before by someone who knew it: a private repo
reference landed in a CI workflow shortly after the tree had been audited for
exactly that. It was a bare `owner/repo#nnn` reference — no scheme, no host —
which is why the denylist layer is mandatory and the job is red without it. History is a separate problem with a separate answer (a fresh
public repo with no history at publication); no CI job can fix a hashed object.

**Checked before the push, not only after.** On a public forge a push is
published the moment it lands, so CI can report a leak but never prevent one.
`scripts/hooks/pre-push` is the gate in front of the forge: it checks the tree
of every pushed branch, every line any pushed commit adds, and every pushed
commit's message and author/committer identity against the same operator
denylist, read from a file outside the repository
(`$MAILROOM_PORTABILITY_PATTERNS_FILE`, default
`~/.config/mailroom/portability-patterns`). It refuses the push when that file
is missing. Enable it once per clone with
`git config core.hooksPath scripts/hooks`, and never push with `--no-verify`.

**Public places hold product facts only.** Issues, pull requests, review
comments, commit messages and release notes describe how Mailroom behaves —
never a particular deployment: no hostnames, addresses, mailbox contents or
measurements, provider accounts, or incident details from a real instance.
Those belong in the operator's own private notes. Test data is always
synthetic; a finding from a real mailbox reaches the repository only as a
general statement of what the code must handle.

## Authority

`docs/mailroom-spec.md` (v0.4) is authoritative; deviations require an ADR in
`docs/adr/`. Companion documents: `docs/mailroom-feature-matrix.md`,
`docs/mailroom-design-review.md` (context for why decisions were made), and
`docs/mailroom-data-model.md` + `db/schema.sql` (the DB layer's reference
design). If documents conflict, the spec wins — flag the conflict rather than
silently resolving it.

## Load-bearing invariant

**Every mutation of a JMAP-visible object (messages, mailboxes, labels,
threads, identities, contacts, submissions) appends a `change_log` row in the
same transaction as the mutation.** This single ordered stream drives JMAP
`/changes`, the search indexer, and client sync cursors (spec §13/§14,
data-model §3). It is enforced by the store layer plus tests — there is no
other legitimate write path.

## Database access

- PostgreSQL 18. All user tables carry `FORCE ROW LEVEL SECURITY` with
  deny-by-default policies; the app connects as `mailroom_app` (non-owner, no
  `BYPASSRLS`).
- `internal/store` is the **only** package that touches the database. Code
  obtains a handle exclusively through `Store.UserTx` (requires a user
  identity; sets `mailroom.user_id` transaction-locally) or `Store.SystemTx`
  (identity unset — user rows invisible; cross-user work only via narrow
  `SECURITY DEFINER` functions). **Fail closed: no identity, no rows.**
- RLS isolation is tested, not assumed: the negative-test suite
  (data-model §5) is a required CI job; a red isolation test blocks merge.

## Conventions

- **Go** (module `mailroom`), latest stable toolchain, CGO reserved for the
  search engine only (ADR-006). `gofmt` + `golangci-lint` clean;
  `govulncheck` clean. Wrap third-party protocol libraries behind our own
  interfaces (anti-corruption layers, spec §3).
- **IDs are ULIDs** (ADR-007), generated app-side. Timestamps are
  `timestamptz` UTC. JSONB is for payloads, never for queryable state.
- Every hot index leads with `user_id` (RLS appends that predicate to every
  plan).
- Migrations are forward-only, expand-and-contract, with online backfills
  (ADR-008); `db/schema.sql` is the reference DDL that CI applies to a clean
  PostgreSQL 18.
- Tests that need a database are build-tagged `integration` and run against
  the docker-compose Postgres (`scripts/integration-test.sh`); unit tests
  must pass with no database present.
- Work is tracked in GitHub issues and milestones; commits reference issues. CI runs on GitHub Actions (`.github/workflows/`).

## Measurement & verification discipline

This project's most expensive mistakes have not been broken code — they have been
**code that ran without erroring while doing nothing**, and **conclusions drawn from
measurements nobody audited**. These rules are earned, each from a named incident.

- **Build the report before the fix.** Whenever "it ran and changed nothing" is a
  plausible outcome, ship the read-only report first and make `--apply` opt-in.
  A backfill once looked obviously correct and would have changed only a handful of messages
  silently; a diagnostic run before the planned work showed the plan had a
  ceiling of under one message per rule and redirected the whole issue; a
  byte-level diff of real duplicates turned a header denylist into an allowlist.
  Report per-signal or per-case counts, never one total — a single number
  cannot distinguish "nothing to do" from "the thing I measured is always empty".
- **A measurement is a claim about the code that produced it.** Audit the pipeline
  before trusting its verdict. Every number the classification epic was judged on for
  a month came from an evaluation with four defects, one of which made sender
  memorisation a *fixed point of the training procedure* — so the conclusion
  "structural features don't generalise" had never actually been tested.
  Numbers from an unaudited pipeline are not findings.
- **Choose the metric that matches the harm.** Respectable ranking (AUC 0.62–0.72)
  coexisted with unusable precision (0.20–0.60) at the threshold that decides what a
  user actually sees. Ask what the feature risks, measure *that*, and gate on the
  bottom of its confidence interval rather than a point estimate.
- **Probe your own tests.** If a new suite passes on the first run, delete the
  mechanism it supposedly covers and confirm a test goes red. This caught a test whose
  subject was never exercised, and identified which test actually guarded a
  mail-destroying edge case when an older one only caught it by accident of its
  fixture. A test that cannot fail is documentation.
- **Negative-control every safety or security assertion.** Three assertions about a
  sandbox once passed against a page that could not have loaded at all. Prove
  the test can observe the failure it claims to exclude.
- **A bounded or empty result must report its bound.** A lower bound reads as a total;
  zero rows can be a design finding rather than an empty result.
- **The absence of an error proves nothing on its own.** A capability left undeclared
  failed one write path while the whole read path rendered perfectly; a cast to
  a property that did not exist compiled, read fine, and was `undefined` forever.
  Ask explicitly: *what would I observe if this were broken?*

## Releases & changelog

- **Versioning is semantic (spec §16).** While pre-1.0, each delivered feature
  **wave** bumps the MINOR version (`v0.6.0`, `v0.7.0`, …); patch bumps are for
  fixes within a wave; `v1.0.0` will mark the MVP milestone. Tags are annotated
  (`git tag -a vX.Y.Z -m "…"`) and pushed.
- **Every change lands in `CHANGELOG.md`** (Keep-a-Changelog style, semver).
  Accumulate entries under `[Unreleased]` as work merges; when you cut a tag,
  rename that section to the version with its date and start a fresh
  `[Unreleased]`. Do not put repository/host URLs in the changelog (portability).
- **Release notes are written for the person using Mailroom.** A release's
  changelog section, which is also its release body, must explain comprehensively and in plain English what the release adds
  and changes: a one-paragraph lead, then `###` sections with short bullets
  (what's new, grouped by feature; safety or behaviour changes; fixes; under the
  hood; upgrading steps when any apply). Say what the user can now do and what
  they will notice, not how the code does it; leave issue numbers out of the
  changelog (they belong in code, commits and ADRs). A final release after release candidates gathers
  everything its rcs added rather than pointing back at them. A paragraph-only
  summary is not enough. Write the notes before cutting the release PR, so the
  tag carries them.
- **When a feature — or a coherent, verified wave — lands:**
  1. Update `CHANGELOG.md`.
  2. Cut and push the annotated tag once the increment is green
     (build + both test lanes + the e2e/live suites it touches).
  3. Publish a **release** for the tag, marked **pre-release while pre-1.0**
     (`prerelease: true`), with the changelog entry as the body. The forge may
     auto-create a plain release when a tag is pushed — set/patch it to
     pre-release. `1.0.0` and later are full releases.
  4. Deploy that tag to a test instance and confirm it came up healthy, so the
     feature can actually be dogfooded.
- Deployment specifics (hosts, credentials, secrets) MUST NOT be committed to
  this repo — the portability rule above applies. Describe deployment here only
  in neutral terms.
- A reader should be able to tell, from the tag plus the changelog, exactly what
  a given deployed version contains.
