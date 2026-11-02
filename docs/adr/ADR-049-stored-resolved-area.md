# ADR-049: Each message stores its resolved area, maintained by triggers

**Status:** Accepted (increment 1 in v1.99.0-beta.N: expand, backfill, report; readers switch in increment 2). Deviates from nothing in the spec; changes how
the area resolver of #442 (`internal/store/areas_resolve.go`) is used, not what it
answers.

## Context

A message's area is derived, never stored: labels first (the earliest-positioned
area fed by one of its labels), else the area fed by its resolved tab (a mapped
label tab, else the classifier verdict), else Needs sorting. One SQL resolver
(`areaJoinSQL`) answers it inside every query that needs it, and a Go resolver
(`internal/areas.Resolve`) mirrors it, held together by
`TestAreaResolverAgreesWithTheQueryExpression`.

Deriving it on every read has a cost proportional to the mail that has to be
resolved, not to the answer:

- **The desk** resolves only the desk's messages since #63, which made it cheap.
- **An area's own list** (`Email/query` with `inArea`, thread-collapsed, arrival
  order) cannot be scoped that way. To show the first page it must know every
  message of the area, and for a tab-fed area that is most of the mailbox. On a
  large real mailbox `plan-check --explain` shows the resolution dominating the
  statement, ahead of the ranking and sort. A bounded keyset page was built and
  agreed with the current query row for row, but every batch and the exact
  count the client needs repeated the full resolution, so it was not shipped
  (see #27).
- **The count beside it**, the drawer sweep and day-one setup pay the same
  resolution again.

The resolution's inputs change rarely compared with how often it is read: a
message's labels, its classifier verdict, a label's tab mapping, and the user's
area configuration (areas, their order, their feeds).

## Decision

### 1. A stored column, defined as the resolver's answer

`app.messages.area_id app.ulid NULL REFERENCES app.areas(id) ON DELETE SET NULL`
holds `COALESCE(area_la.area_id, area_tf.area_id)` exactly as `areaJoinSQL`
computes it. NULL means Needs sorting. It is a **cache of a derived value**: it
adds no new meaning, and every rule about what an area is stays in the resolver.

A per-user readiness flag (`app.area_index_state (user_id PRIMARY KEY, ready_at
timestamptz)`, RLS like its neighbours) says whether a user's column is
complete. Until it is set, readers keep using the resolver (§5).

### 2. Maintained by triggers, not by application code

The project's costliest defects have been write paths that forgot something
(#505, #557). A derived column maintained from Go would need every path that
touches a label, a verdict or the configuration to remember it. So it is
maintained by triggers, which no write path can skip:

- **`app.message_labels`, AFTER INSERT OR DELETE, per row:** recompute that
  message's area.
- **`app.messages`, BEFORE UPDATE OF `classified_tab`, per row:** recompute that
  message's area when the verdict changes. Also BEFORE INSERT, so a new
  message is stored with its area. Its labels arrive afterwards and are covered
  by the first trigger.
- **`app.area_feeds`, `app.areas` (insert, delete, position change) and
  `app.labels` (tab mapping change), AFTER, per statement:** recompute every
  message of the affected users. These are rare, user-initiated configuration
  changes. A full recompute costs about what one unscoped area list costs today.

Each trigger calls one function, `app.resolve_message_area(user_id,
message_id)`, whose body is the resolver's single-message form. The bulk form
reuses it set-based. The functions are `SECURITY INVOKER`, so they run under
the caller's identity and RLS, and see only the caller's rows. A trigger fired under
an identity that cannot see the user's rows (a `SECURITY DEFINER` sweep, a user
deletion's cascade) updates nothing, because the `UPDATE` cannot see the message
either. That is a no-op, never a wrong value, and the standing report (§6) would
show any gap it left. Every query is
keyed on `user_id`, so a message can only ever be resolved through its own
user's labels, feeds and areas, as `areaJoinSQL` is today.

### 3. No new change_log rows

Changing a label already appends an Email change in the same transaction, and
that is the only way a message's own inputs change. Configuration changes do not
append per-message Email rows today; clients refetch the desk on Area changes.
The stored column changes nothing about what is JMAP-visible, so it appends
nothing. The load-bearing invariant is untouched.

### 4. Indexes

`messages_user_area_arrival ON app.messages (user_id, area_id,
COALESCE(snooze_woke_at, received_at) DESC, id DESC)` serves an area's list in
its own order and its count, and leads with `user_id`, as every hot index here
must.

### 5. Rollout (expand, backfill, switch)

1. **Expand:** add the column, the readiness table, the functions, the triggers
   and the index, all in one migration. From that moment every change keeps
   touched messages correct.
2. **Backfill:** `mailroom-admin area-index`, report-first. With no flags it
   counts per user how many messages the backfill would set, and to which areas.
   `--apply` fills each user's column in batches, using the same resolver, and
   then sets `ready_at`. It is online and idempotent. A message changed during
   the backfill is already correct through its trigger, and recomputing it gives
   the same answer.
3. **Switch:** readers (`Email/query`'s `inArea`/`unsorted`, the area counts,
   the drawer sweep, day-one setup) use `m.area_id` when the user is ready, else
   the resolver.

### 6. Proof it never drifts

- **Standing report:** `mailroom-admin area-index` (no flags, on a ready user)
  also compares the stored column with the resolver for every message, and
  prints the count of disagreements per area and per cause.
- **Integration agreement test:** it drives random sequences of every input
  change (label add and remove, verdict change, tab mapping, feed add and
  remove, area create, delete and reorder, label delete) and asserts after each
  step that the stored column equals the resolver for every message. A probe,
  dropping one trigger, must turn it red.
- **The resolver stays** as the oracle, as the reader for users who aren't
  ready, and as what the triggers call. `TestAreaResolverAgreesWithTheQueryExpression`
  keeps the Go and SQL resolvers together, as now.

### 7. Cost on writes

A label change now also re-resolves its message: one indexed single-message
query inside a write that already touches the same rows. Bulk relabels (rules,
move-all, learned placement backfill) pay it per row. That is acceptable at the
scale a single-user instance relabels, and is measured in the PR with the
integration fixture's bulk paths. A configuration change pays one full recompute
for that user.

## Amendments found while building increment 1

- **A foreign-key cascade runs as the table owner, which row-level security
  shows no rows**, so a trigger fired by a cascade resolves from nothing and
  leaves the stored area stale (a wrong value, not the harmless no-op §2
  describes). The label delete path that matters is closed by a
  `BEFORE DELETE` trigger on `app.labels` that removes the label from its
  messages in the caller's own context, where the message-label triggers see
  the caller's rows. Cascades from a message, an area and a user are unaffected
  (the rows they remove are gone, or the area's own statement trigger recomputes
  afterwards).
- **The message-label triggers are statement-level** (transition tables): one
  statement per SQL statement resolves every touched message (two indexed
  lateral lookups each) and writes only rows whose area differs. A set-based
  DISTINCT ON form was tried and measured slower, because the planner cannot
  size a DISTINCT ON and picked a nested loop. The single-message function
  remains for the `BEFORE` triggers on `app.messages`.
- **Noted, not done in increment 1:** a configuration edit issues several
  statements (one per feed), each firing a full per-user recompute, so a
  per-transaction dedupe is worth doing in increment 2; and deleting an area
  makes the foreign key's SET NULL scan `app.messages`, because no index leads
  with `area_id`.
- **`app.resolve_message_area` takes the verdict as a third argument** (a
  two-argument form reads it from the row), because the `BEFORE` triggers run
  before the row holds the new verdict.
- **The tab precedence is a function, `app.area_tab_precedence()`**, and a test
  holds it to `tabs.PrecedenceNames()`.
- **A backfill batch locks its rows (`FOR UPDATE`) before it resolves them**,
  so a concurrent change cannot be overwritten with an answer read before it.

## Consequences

- An area's list and its count become an index range scan in the list's own
  order, so the bounded keyset page (branch `area-list-page`) can be revisited on
  top of it.
- The drawer sweep, the area counts and day-one setup can drop their resolver
  passes once ready.
- There is a new derived column to keep correct. The triggers, the standing
  report and the agreement test exist for that; if they disagree, the resolver is
  right and the column is rebuilt with `area-index --apply`.
- The Go resolver and the SQL resolver must still agree. The trigger function
  is a third consumer of the SQL resolver, not a third resolver.

## Trade-off accepted for increment 1

A label change now also rewrites the message row's `area_id`. Because `area_id`
is indexed, that is a non-HOT update across every index on `app.messages`, and no
trigger design avoids it. Statement-level triggers over the transition table did
not make it cheaper. A single-statement bulk relabel of many messages (day-one
setup, "Move all from", a rule applied to existing mail, a placement backfill) is
therefore several times slower than before, and a single label change is a small
multiple of a write that was already cheap. That is accepted, because those bulk
paths are rare and user-initiated while an area's list is read constantly.

If the bulk cost proves to matter, the alternative is a narrow side table
(`message_areas (user_id, message_id, area_id, arrival)`) maintained by the same
triggers. Changing it would not rewrite the message row or touch its indexes, at
the cost of keeping `arrival` in step on snooze wake.

## References

- #27 (the measurements and the area-list attempt), #63 (desk-scoped resolver).
- ADR-021, ADR-022 (category tabs and the stored classifier verdict), ADR-031
  (area labels are local), ADR-008 (expand-and-contract migrations).
- `internal/store/areas_resolve.go`, `internal/areas`.
