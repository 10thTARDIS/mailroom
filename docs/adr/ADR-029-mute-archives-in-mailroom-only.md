# ADR-029: Mute archives a new reply in Mailroom only, with no writeback and no push

**Status:** Accepted (Forgejo #629, v1.76.0; a deliberate deviation from spec §6)

## Context

Spec §6 lists "Snooze / mute-thread / pin / 'archive = done', implemented
locally with writeback where representable".

Until v1.76.0, mute stored `threads.muted_at` and showed a banner saying new
replies "won't be surfaced". Nothing else read `muted_at`: replies still landed
in the inbox and still sent a push. The wiki review (#622) found the banner
promising something the code did not do.

What mute does to a new reply *is* representable on the server, as a move to
Archive. By the spec's wording it should therefore be written back.

## Decision

Decision (2026-09-19): mute acts **in Mailroom only**:

1. **Archive on arrival, locally.** A NEW message that arrives in the INBOX of a
   muted conversation is placed in its account's Archive at live ingest. It uses
   the same function as a skip-inbox rule (`store.skipInboxForNewMessage`,
   reached through `DivertMutedArrival`). No writeback intent is created, so the
   server copy stays in INBOX and another mail client still shows it there.
2. **Read state is untouched.** An unread reply stays unread, so it is still
   recognisably new when the user opens Archive or the conversation.
3. **No push.** Such arrivals are counted (`mailsync.Summary.MessagesMuted`) and
   left out of the new-mail push (`syncrunner.pushCount`; ADR-016 amended).
4. **Forward-only.** Muting moves nothing already there. Unmuting lets the next
   reply surface and leaves earlier ones in Archive.
5. **Not diverted:**
   - mail that did not arrive in the inbox (a Sent copy, a label folder);
   - mail in an account with no Archive folder (it stays in the inbox rather
     than being hidden);
   - bulk archive imports, which load history rather than deliver replies.

## Why not write back

Mute is a statement about what *Mailroom* should bring to the user's attention.
Writing the move back would reach into every other client the user runs,
including a phone's native mail app, which has its own idea of what matters. It
would also turn a reversible preference into a server-side filing decision that
unmuting could not undo. A skip-inbox rule already makes the same local-only
placement without an ADR. This record exists because mute is named in the spec
and the spec says otherwise.

## Consequences

- The banner has to say this, and does: "…That happens in Mailroom only: nothing
  moves on the server, so other mail apps still show them in the inbox."
- A muted reply that a skip-inbox rule archived first is still reported as muted,
  so whether it notifies does not depend on which of the two ran first.
- The opt-in thread merge (`mailroom-admin thread-report --apply`,
  `store.MergeSplitThreadGroups`) discards losing threads' `muted_at`, as its
  comment says. That was harmless while mute did nothing; now a merge can
  un-mute half a conversation. It is an explicit operator action, not an
  automatic path.
