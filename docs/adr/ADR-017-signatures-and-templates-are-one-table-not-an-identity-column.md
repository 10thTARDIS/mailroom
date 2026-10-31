# ADR-017: Several signatures and templates live in one table beside `identities.signature_html`

**Status:** Accepted (resolves a conflict inside the spec; raised by
per-identity signatures + templates)

## Context

`app.identities` already has a `signature_html` column, and it is **not**
Mailroom's invention: RFC 8621 §3 puts `htmlSignature` and `textSignature` on the
`Identity` object, so any conformant client can read and write one. It is served
by `internal/api/identity.go` and must keep working.

`docs/mailroom-spec.md` §17's data-model sketch describes exactly that and no
more:

> `identities` (alias, **signature**, default key, allowed-sender)

But §5 asks for "per-identity signatures **+ default keys**,
templates/snippets", and `docs/mailroom-feature-matrix.md` §12 carries a separate
row:

> | Multiple signatures | | v1.x |

One column holds one signature. It cannot satisfy "multiple signatures", and it
has nowhere to put a template at all — a template belongs to the user, not to an
address. So the spec asks for two things that its own data-model line cannot both
express (§5 vs §17); the conflict is internal, and something has to give.

Three shapes were available:

1. **Widen `identities`** — `signature_2`, or a JSONB array of signatures.
   Rejected: JSONB is for payloads, never for queryable state (`CLAUDE.md`), and
   numbered columns are a table wearing a disguise.
2. **A `signatures` table and a separate `templates` table.** Honest about the two
   features being different, at the cost of two nearly identical tables, two JMAP
   object types, two `change_log` object types and two management screens for
   objects that differ in one field.
3. **One table of named reusable compose text**, with a `kind` discriminator and
   an optional identity binding, sitting *beside* the RFC 8621 column.

## Decision

**Option 3.** `app.compose_snippets` holds both, distinguished by
`kind IN ('signature', 'template')`, exposed as one JMAP object type
(`ComposeSnippet`, capability `urn:mailroom:composesnippets`).

**`identities.signature_html` stays exactly as it is** — RFC 8621 requires the
field, and removing or shadowing it would break conformance and any other client.
The two are ordered, not merged: a default snippet wins where one exists, and the
identity's own signature is the fallback where none does
(`web/src/core/snippets.ts` → `automaticSignatureBody`). That ordering is the
whole reconciliation, and it is tested. Silently ignoring a signature the user had
already configured would read as this feature having broken it.

The shape follows from what actually differs between the two:

- A **signature** may be bound to an identity (`identity_id`), which is the point
  of it under spec §5's alias model — the sign-off for mail from `shop@` is not
  the one for `work@`. `NULL` means "any address".
- A **template** must not be (`compose_snippets_template_has_no_identity`
  enforces `kind <> 'template' OR identity_id IS NULL`). A template is a message
  you start from, not a way you sign; binding one to an address would be a scope
  with no meaning.
- Otherwise they are the same object: a name, a body in both HTML and text, an
  `is_default` flag, and a per-user unique name within their kind.

`is_default` is scoped by `(user_id, kind, identity_id)` via two partial unique
indexes — one for the bound slots, one for the `NULL` identity slot, because
PostgreSQL's `UNIQUE` treats `NULL`s as distinct and would otherwise allow any
number of unbound defaults. Setting a default clears the previous one in the same
transaction (`clearSnippetDefaultExcept`, which matches the slot with
`IS NOT DISTINCT FROM` so the `NULL` case is handled by the same statement).

## Consequences

- **The sketch in §17 is now incomplete rather than wrong**: the column it names
  still exists and still works; there is simply a second, richer home for the
  several-signatures case it cannot express. This ADR is the record; the spec is
  not rewritten (deviations live in `docs/adr/`, per `CLAUDE.md`).
- **Two homes for one concept is a real cost**, and it is accepted only because RFC
  8621 forces the first one to exist. The mitigation is that exactly one function
  decides which applies, and nothing else may ask the question.
- Writes are **full-object**, never partial. `kind` and `identity_id` together
  decide which picker offers a snippet, so a partial update could silently move a
  signature to a different address — the same class of bug as an earlier one where
  a narrow write dropped a field something else depended on.
- Which signature applies is one decision in one place
  (`web/src/core/snippets.ts`: `automaticSignature` and `offeredSignatures`),
  DOM-free and tested. The rule that matters is the negative one: a signature
  bound to identity A is **never** offered or auto-inserted for identity B.
  Putting the wrong name at the bottom of a message is the failure the whole
  per-identity mechanism exists to prevent.
- Adding a third kind later (a canned reply, a footer) is a `CHECK` change and a
  filter, not a new table, a new object type or a new sync cursor.
- A wildcard/catchall identity (`*@example.com`) is **not** bindable: it denotes
  "any local part at this domain", a scope `identity_id` cannot express and the
  picker could not honour. The client filters those out of the binding control.

## References

- `docs/mailroom-spec.md` §5 (compose surface, alias model), §17 (data-model
  sketch — the line this ADR departs from)
- `docs/mailroom-feature-matrix.md` §3 (per-identity signatures, templates),
  §12 (multiple signatures)
- `db/migrations/00022_compose_snippets.sql`, `internal/store/compose_snippets.go`,
  `internal/api/compose_snippet.go`, `web/src/core/snippets.ts`
