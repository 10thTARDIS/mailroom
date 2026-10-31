# ADR-022: Filtering by tab stores the CLASSIFIER'S VERDICT, never the tab — amends ADR-021

**Status:** Accepted (amends ADR-021; raised by the tab client surface)

## Context

ADR-021 says a message's category tab is **derived at read time, never stored**, and that is
load-bearing: it is what makes re-pointing one label move all of its mail on the next read,
with no backfill, no job, and no window where the mailbox disagrees with itself.

Then the tab client surface needs `Email/query` to filter by tab, because a tab strip that cannot narrow the
list is decorative. And filtering happens in **SQL** (`store.EmailQuery` is a struct of
predicates assembled into a `WHERE` clause) while the classifier is **Go**
(`internal/tabs.Classify`, a first-match decision list over `kind.Facts`).

Those two facts are in direct tension, and resolving it badly has a known cost. An earlier decision settled
the analogous case for `Rule/preview`: it must use the *same* matcher as ingest, because "a
preview that can disagree with the engine is worse than none". Here a disagreement would be
between what the tab strip **counts** and what the list **shows** — visible, and corrosive in
exactly the way the classification epic's invariants warn about.

## Options considered

1. **Re-express the classifier's rules in SQL.** Two implementations of one decision list,
   guaranteed to drift the first time either is touched. Rejected on the `Rule/preview` precedent.
2. **Materialise the tab.** Contradicts ADR-021 outright and reinstates the
   backfill-on-every-mapping-edit problem the derived design exists to avoid.
3. **Compute in Go over a candidate set.** Viable only when the tab filter is always paired
   with something narrowing. A tab strip is the opposite: it *is* the top-level filter, so this
   is O(mailbox) per query with no pagination or total count. Rejected.
4. **Filter on the mapping only, and let everything else fall to Primary.** Ships with no
   migration at all — and puts every unlabelled message in Primary. On a real corpus
   that is a noticeable share of messages, i.e. precisely the population tabs exist to sort. Rejected as
   defeating the feature.

## Decision

**Store the classifier's VERDICT. Do not store the tab.** They are different things, and the
difference is the whole ADR.

`app.messages` gains `classified_tab` (the tab `tabs.Classify` returns for this message) and
`classifier_version`. The **tab** remains derived, by an expression that is now writable in
both languages:

```
resolved_tab := COALESCE(mapping_tab, classified_tab)
```

- **`mapping_tab`** — the narrowest-precedence tab among the message's mapped labels, read
  LIVE through the existing join. Unchanged, unstored, instant. **ADR-021's actual guarantee
  is preserved in full**: re-pointing a label still moves its mail on the next read.
- **`classified_tab`** — the Go classifier's answer, computed once at ingest and recomputed
  only when the classifier itself changes.

The cadences are the point. A mapping edit is a **user action** and must be instant. A rule
change is a **release**, and a bounded backfill at release time is an ordinary migration
concern rather than a latency the user feels.

### The precedence order is passed in, not duplicated

The tie-break for a message whose labels span several tabs is ordered with
`array_position($1::text[], l.tab::text)`, where the array is bound from `tabs.Precedence()` at
query time. **There is no second definition of the precedence order in the database** — not a
lookup table, not an `IMMUTABLE` function, nothing to drift. The one place it lives is
`internal/tabs`.

### The enabled-fold is expanded by the caller, not encoded in SQL

`tabs.Fold` sends mail from a disabled tab to Primary. Rather than express that as a `CASE`
against `app.category_tabs`, the API reads the user's enabled set (five rows) and expands the
predicate:

- querying an enabled tab `T` → `resolved_tab = T`
- querying `primary` → `resolved_tab = 'primary' OR resolved_tab IN (<disabled tabs>)`
- querying a disabled tab → refused as `invalidArguments`, since the client should not be
  offering it

So the fold has exactly one implementation too, in Go, and SQL only ever sees a literal set.

### What is actually duplicated, and how it is pinned

Only the **shape** of `COALESCE(mapping, classified)` plus the precedence `ORDER BY`. That is
pinned by a **differential test**: over a fixture corpus, every message's tab computed by the
Go resolver must equal the tab the SQL predicate selects it under, for all five tabs. A
disagreement fails the build. This is the `Rule/preview` remedy adapted to a boundary that cannot share
code — we cannot make one implementation, so we make disagreement a red build.

## Consequences

- **ADR-021's "never stored" is narrowed to "the mapping half is never stored".** Its stated
  benefit — an instant, backfill-free mapping edit — is unaffected, because the mapping half is
  still a live join and still wins over the stored verdict.
- **A classifier change now has a migration cost** it did not have before: `classifier_version`
  bumps, and `classified_tab` is stale until a backfill runs. Accepted deliberately, and it is
  why the version column exists rather than a bare timestamp — staleness must be *detectable*,
  not inferred.
- **A message with no stored verdict and no mapped label is invisible to a tab filter.** That
  is the honest failure mode of a nullable column, and it is why `mailroom-admin tabs` must
  report the count of messages with `classified_tab IS NULL` rather than let a partial backfill
  read as a complete answer. A bounded result reports its bound.
- **Ingest gains a classifier call.** Cheap — it is a first-match list over facts ingest has
  already parsed — and it introduces no train/serve gap, because there is exactly one
  classifier and it reads the same `kind.Facts` on both paths.
- `Email.categoryTab` and the query filter now agree by construction rather than by care, since
  both read `COALESCE(mapping, classified)`.

## Verification

The differential test above is the load-bearing one, and it must be probed the way this project
probes everything: change the precedence array bound into the query and confirm the Go and SQL
answers diverge and the test goes red.

`mailroom-admin tabs` gains the `classified_tab IS NULL` count, so a half-run backfill is
visible rather than silently narrowing every tab.
