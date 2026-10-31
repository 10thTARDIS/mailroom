# ADR-010: Isolation = forced RLS + identity-scoped tx wrapper + negative tests + per-user DEK backstop

**Status:** Accepted (spec v0.4 §10; design review P0-5/D10)

## Context

"We use RLS" is a slogan, not a design. With pooled connections, one missed
`SET` = full-table exposure; an app role with table ownership or `BYPASSRLS`
silently disables policies.

## Decision

Layered design, each layer independently real:

1. **Postgres:** `FORCE ROW LEVEL SECURITY` on every user table;
   deny-by-default policies keyed on `app.current_user_id()`; the app
   connects as `mailroom_app` (non-owner, no BYPASSRLS); `mailroom_admin`
   has **no grants at all** on mail-content tables.
2. **Tx wrapper:** the store layer's `UserTx` is the only way code touches
   user data — it requires a user identity and sets `mailroom.user_id`
   transaction-locally (`SET LOCAL` semantics; pool-safe). `SystemTx` leaves
   it unset: policies see NULL ⇒ user rows invisible; cross-user work only
   via narrow `SECURITY DEFINER` functions. **Fail closed: no identity, no
   rows.**
3. **Blobs/ML/keys:** HMAC-derived per-user prefixes + per-user DEKs
   (ADR-004); per-user model files; crypto-process API takes authenticated
   user context.
4. **API:** user id from the token, never a request parameter; ULID ids.

**Isolation is tested, not assumed:** the five negative-test suites of
mailroom-data-model.md §5 run in CI as a required job; any failure is a red
build.

## Consequences

- `internal/store` is the single DB door; adding any other connection path
  is a policy violation caught in review.
- Every hot index leads with `user_id` (RLS adds the predicate to every plan).
- The search index remains the honestly-weakest layer; compensated by
  volume encryption + the DEK backstop (ADR-006).

## References

spec §10 · mailroom-data-model.md §5 · design review P0-5, deep-dive D · D10
