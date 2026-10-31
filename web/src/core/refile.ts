// SPDX-License-Identifier: AGPL-3.0-only

// Re-filing mail between areas (#704), as a pure computation over the store.
//
// An area OWNS a label, so filing a message into an area is applying that
// label. That alone used to be the whole of re-filing, and it was wrong in a
// way nothing showed until the area labels became applicable at all: the
// server resolves a message's area from its labels FIRST, earliest area
// POSITION wins (internal/store/areas_resolve.go, area_la's ORDER BY
// a.position). So adding Updates' label (position 3) to a message that still
// carries Promotions' (position 2) leaves it in Promotions — the re-file
// "succeeds" and nothing moves.
//
// So a re-file is a SWAP: remove every OTHER area's own label the message
// carries, and add the target's. Computed per message, because a selection can
// span areas, and its undo is the exact inverse of what changed for each
// message — re-adding only the labels that were actually removed and removing
// the target only where it was actually added — so the prior label set is
// restored, not approximated.
//
// ⚠ WHAT THIS DOES NOT MOVE. An area can also be fed by OTHER labels and by a
// category tab (area_feeds). A message in Promotions because it carries a
// user label that feeds Promotions keeps that label — it is the user's topic,
// not the area's bookkeeping — and if that area sorts before the target it
// stays put. Removing someone's own topic labels to make a re-file stick
// would be a worse surprise than a re-file that is visibly incomplete.

import type { LabelChange } from "./actions";
import type { Area, Id, Label } from "./jmap/types";
import type { MailState } from "./store";
import { effectiveEmail } from "./store";

/** The per-message label swap that files `ids` into `targetAreaId`. Messages
 * already exactly there (target on, no other area label) produce no change and
 * are omitted. Empty when the target area or its label is not loaded. */
export function refileChanges(state: MailState, ids: Id[], targetAreaId: Id): LabelChange[] {
  const target = state.areas[targetAreaId];
  const targetLabel = target ? state.labels[target.labelId] : undefined;
  if (!target || !targetLabel) return [];
  const others: Label[] = [];
  for (const a of Object.values(state.areas)) {
    if (a.id === target.id || a.labelId === target.labelId) continue;
    const l = state.labels[a.labelId];
    if (l) others.push(l);
  }
  const out: LabelChange[] = [];
  for (const id of ids) {
    const e = effectiveEmail(state, id);
    if (!e) continue;
    const remove = others.filter((l) => e.labelIds[l.id] === true);
    const add = e.labelIds[targetLabel.id] === true ? [] : [targetLabel];
    if (add.length + remove.length > 0) out.push({ id, add, remove });
  }
  return out;
}

/** The exact inverse of a set of changes: what was added comes off, what was
 * removed goes back. Applied to the post-refile state it restores each
 * message's prior label set. */
export function invertChanges(changes: LabelChange[]): LabelChange[] {
  return changes.map((c) => ({ id: c.id, add: c.remove, remove: c.add }));
}

/** The user's areas in the order the desk draws them: by position, then id —
 * the same total order `deskSections` uses, so a menu of areas cannot disagree
 * with the desk about which is third. */
export function areasInDeskOrder(areas: MailState["areas"]): Area[] {
  return Object.values(areas).sort((a, b) =>
    a.position !== b.position ? a.position - b.position : a.id.localeCompare(b.id),
  );
}
