# ADR-011: License = AGPL-3.0

**Status:** Accepted (spec v0.4 §18.3; design review P2-14, D11)

## Context

The project stays private at least until stable through v1, so there was time
to defer this. It could not be deferred past publication: relicensing after
outside contributions land requires CLAs or unanimous contributor consent, so
the decision had to be made while Mailroom is still single-contributor.

Dependency licenses impose no constraint. The tree is uniformly permissive —
the emersion stack MIT, Tantivy/tantivy-go MIT, go-crypto BSD-style, pgx MIT,
the `golang.org/x` and Prometheus packages BSD/Apache-2.0 — all of which may be
incorporated into an AGPL work.

Candidates on file were AGPL-3.0 versus Apache-2.0/MIT.

## Decision

**AGPL-3.0-only.** The full text is `LICENSE` at the repo root; source files
carry a one-line `SPDX-License-Identifier: AGPL-3.0-only` rather than full
header banners.

Four reasons, in the order that drove the decision:

1. **It is the reversible choice.** As sole copyright holder pre-publication,
   relicensing AGPL → permissive stays available indefinitely. Permissive →
   AGPL after outside contributions is what needs CLAs or unanimous consent.
   Starting permissive forecloses tightening; starting AGPL forecloses nothing.
   This asymmetry, not ideology, is the load-bearing argument.
2. **It matches the project's reason for existing.** Spec §0 is built on the
   finding that every capable AI-triage email product is cloud SaaS,
   provider-tied and content-egressing. AGPL is the license that structurally
   prevents Mailroom from becoming the thing it was built to replace.
3. **The adoption argument is weak for this software class.** Permissive
   licensing maximizes adoption when code is *embedded* in other products —
   libraries, SDKs, frameworks. Mailroom is a self-hosted end-user application:
   operators deploy it, they do not link against it. AGPL's network-use
   obligations bite only someone offering it as a service, which is the case
   worth binding, and impose nothing on the target user self-hosting for
   themselves.
4. **It is the category norm.** Comparable self-hosted network services —
   Nextcloud, Mastodon, Bitwarden server, Grafana — are AGPL; Roundcube and
   Forgejo are GPL-family.

Copyright is asserted personally, as "Ryan Brooks". The portability rule in
`CLAUDE.md` bars operator-identifying *infrastructure* — domains, hostnames,
service URLs, personal email addresses — not authorship. A copyright notice
needs an identifiable holder to be worth anything, and single-holder
attribution is also what keeps the relicensing option in the decision above
genuinely open.

## Consequences

- **Some organizations ban AGPL outright.** This knowingly forgoes a slice of
  corporate adoption and contribution. Accepted as the cost of (1) and (2).
- Anyone running a modified Mailroom as a network service must offer their
  users the corresponding source. That is the intended effect, not a
  side effect.
- Vendoring copyleft code remains a decision that constrains the *future* — it
  would not conflict with AGPL, but it would block a later move to permissive.
  That still requires revisiting this ADR, as the open version of it said.
- New source files are expected to carry the SPDX identifier. It is a one-line
  convention precisely so that it survives file copying without imposing a
  banner on every file.
- The choice is recorded as `AGPL-3.0-only`, not `AGPL-3.0-or-later`: adopting
  a future FSF version is a decision to take deliberately, not automatically.

## References

spec §18.3, §0 · design review P2-14, D11 · `LICENSE`
