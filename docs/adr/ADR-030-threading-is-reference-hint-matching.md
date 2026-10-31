# ADR-030: Threading is Message-ID/References hint matching; JWZ stays unwired until a mailbox needs it

**Status:** Accepted (Forgejo #434, v1.81.1; a deliberate deviation from spec §4)

## Context

Spec §4 says:

> Threading via JWZ (References/Message-ID) unified across accounts, with
> restricted subject-fallback: subject-only merging is allowed only within the
> same account, same correspondent set, and a bounded time window. Thread
> membership is stored but recomputable, and merges are logged for
> diagnosability.

What has shipped since 1.0 is simpler. `store.FindThreadByHints`
(`internal/store/ingest.go`), called from ingest and from draft saving, matches
the new message's Message-ID, References and In-Reply-To against existing
messages in four directions and takes the **first** match (oldest
`received_at`). Measured against §4:

| Spec §4 | Shipped |
| --- | --- |
| JWZ threading | Hint matching, first match wins |
| Merge when References span several threads | Not done |
| Restricted subject fallback | Absent |
| Membership stored and recomputable | Stored; nothing recomputes it |
| Merges logged | No merges happen |

`internal/thread` is a complete, tested implementation of §4: the five JWZ
steps, loop detection, root-set pruning and the restricted fallback (#12). It
has no importers. Until #429/#434 nobody had noticed the deviation, because a
correct JWZ package in the tree makes it look conformant, and #210's first fix
was made in that package and changed nothing.

The package was kept, documented as unused
(#434 option C), and no ADR was written because that was a deferral. The
deviation is now recorded as a decision.

**What the decision rests on.** On a small test mailbox (measured
for #434): few messages carry References, only a handful of those resolve to an existing
thread, and none resolve to more than one. JWZ would have merged nothing. That
zero has a tiny denominator: it shows the mailbox cannot exercise the
difference, not that the difference does not matter. A large long-used mailbox
has not been measured.

## Decision

1. **Threading is Message-ID/References/In-Reply-To hint matching, first match
   wins**, exactly as `FindThreadByHints` does today. There is no subject
   fallback and no cross-thread merge. This supersedes spec §4's threading
   sentence for the 1.x line.
2. **`internal/thread` stays in the tree, unwired**, with its ⚠ banner pointing
   here. Deleting it would make the revisit below expensive for no gain; the
   banner removes the hazard that made it misleading.
3. **Revisit when a real mailbox shows the gap**, measured rather than felt.
   The trigger is either of:
   - messages whose References resolve to more than one existing thread (the
     merges JWZ would perform) are more than a handful on a real
     mailbox; or
   - a user reports a conversation that split, or two that welded
     together, that the hint matcher caused.

   The measurement is a report first, in the project's usual order: count
   messages with References, those resolving to one thread and those
   resolving to more than one, per account. Only then decide whether to wire
   JWZ, which needs an incremental form, because ingest sees one message at a
   time and JWZ is a batch recomputation.

## Consequences

- No subject-only merge exists, so the false-merge risk
  `docs/mailroom-design-review.md` item 7 warns about cannot happen. The cost
  is that a reply from a client that drops References (some mobile clients,
  some web forms) starts a new conversation.
- A message whose References point at two existing threads joins the older
  one, and the two threads stay separate.
- Thread membership is written once at ingest. Changing the algorithm later
  needs a recompute job over existing mail, which does not exist yet.
- `docs/mailroom-feature-matrix.md` already says threading ships as
  first-match hint matching; it now cites this ADR.

## References

- Spec §4 (`docs/mailroom-spec.md`, the threading sentence)
- #434, #429, #210, #12
- `internal/store/ingest.go` (`FindThreadByHints`), `internal/thread/`
- `docs/mailroom-design-review.md` item 7 (threading false merges)
