// SPDX-License-Identifier: AGPL-3.0-only

// WHICH DRAFT BECAME WHICH (#610, #612).
//
// A JMAP Email is immutable apart from its keywords and mailboxIds (RFC 8621
// §4.6), so an autosave does not edit a draft: it creates the next revision and
// destroys the previous one in the same Email/set (MailActions.saveDraft). The
// server is right to model it that way, and it means a draft being typed into
// changes id every fifteen seconds. Everything in the client that holds an id
// — the list's keyboard cursor, the composer's URL — was left pointing at a
// message that no longer exists, and each fell back to something wrong: the
// cursor to the row BELOW the draft (so a keyboard archive hit the neighbour),
// the URL to a draft a reload can never load.
//
// The continuity is not expressible on the server, and must not be faked there:
// sharing a Message-ID across revisions makes the header expunge destroy the
// revision just saved (internal/compose/compose.go, KeepMessageID). But the one
// client that performed the save KNOWS it — it named the old id in the destroy
// and got the new id back in the same response. This module is that knowledge.
//
// ⚠ IT IS RECORDED ONLY FOR A SAVE THIS CLIENT MADE, AND ONLY WHEN THE SERVER
// SAID IT RETIRED THE OLD REVISION. A draft edited in another client arrives as
// an unrelated create and destroy, and is deliberately NOT welded to anything
// here: it is a different message, and store.RetireDraft's conflict rule treats
// it as one (duplication over data loss). Guessing a lineage from a change
// batch would hide exactly the edit that rule exists to preserve.

import type { Id } from "./jmap/types";

/** old revision id → the NEWEST revision it has become. */
export type Successors = Readonly<Record<Id, Id>>;

/**
 * Records that `from` was replaced by `to`, returning a new map.
 *
 * Chains are collapsed as they are written — every older revision that pointed
 * at `from` is re-pointed at `to` — so a lookup is one step however long the
 * composer has been open, and a map entry never names a revision that is itself
 * already gone.
 */
export function recordSuccessor(map: Successors, from: Id, to: Id): Record<Id, Id> {
  if (from === to) return { ...map };
  const next: Record<Id, Id> = {};
  for (const [k, v] of Object.entries(map)) next[k] = v === from ? to : v;
  next[from] = to;
  // A revision that has itself been superseded can never be anyone's newest.
  delete next[to];
  return next;
}

/** The newest revision `id` is known to have become — `id` itself when this
 * client never replaced it. */
export function latestRevision(map: Successors, id: Id): Id {
  return map[id] ?? id;
}

/**
 * Where the list's keyboard cursor belongs after the list changed (#610 M2).
 *
 * The rule it replaces was "still there, or else the first row". That is right
 * for a message that genuinely left — archived here, moved elsewhere — and wrong
 * for a draft that was REPLACED: the cursor landed on the first row, or, while
 * the replacement had not arrived yet, on the row below the draft, and stayed
 * there. With nothing checked the list's actions target the cursor, so the next
 * keyboard archive or trash went to a message the user never chose while they
 * were typing and not looking.
 *
 * So a cursor that vanished follows its own successor when that successor is in
 * the list. It is keyed on the cursor's OWN id — a cursor the user moved
 * elsewhere mid-save no longer names the old revision and is never remapped —
 * and it falls back to the old rule for everything else.
 */
export function repairCursor(
  ids: readonly Id[],
  cursor: Id | null,
  successors: Successors,
): Id | null {
  if (cursor && ids.includes(cursor)) return cursor;
  if (cursor) {
    const next = latestRevision(successors, cursor);
    if (next !== cursor && ids.includes(next)) return next;
  }
  return ids[0] ?? null;
}

/**
 * The checked set, with every replaced draft swapped for its newest revision.
 *
 * The cursor is not the only thing holding a draft's id: a CHECKED draft stays
 * checked across a save only if its id is carried over, and a checked set
 * naming a destroyed revision makes the next bulk action quietly skip the
 * draft the user ticked. Returns the SAME set when nothing was replaced, so a
 * caller can use identity to skip a re-render.
 */
export function followSelection(selected: ReadonlySet<Id>, successors: Successors): ReadonlySet<Id> {
  let changed = false;
  const next = new Set<Id>();
  for (const id of selected) {
    const to = latestRevision(successors, id);
    if (to !== id) changed = true;
    next.add(to);
  }
  return changed ? next : selected;
}
