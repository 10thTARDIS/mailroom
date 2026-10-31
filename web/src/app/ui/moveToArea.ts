// SPDX-License-Identifier: AGPL-3.0-only

// MOVE A SELECTION (OR A SENDER'S WHOLE HISTORY) INTO AN AREA (#708).
//
// This is NOT a second re-file path. The swap itself is `refileChanges` /
// `invertChanges` (core/refile.ts) and the write is `actions.relabel`, exactly
// as the desk, the tray and the sender card's "Change area" use them. What this
// adds is the part those never needed, because they only ever act on rows
// already on screen:
//
//   * THE MESSAGES MAY NOT BE LOADED. "Select all matching" and "every message
//     from this address" hand back IDS, and `refileChanges` skips an id it
//     cannot read (`effectiveEmail` is undefined) -- silently. A move over 1,000
//     ids of which 50 are loaded would "succeed" for 50 and say nothing. So the
//     missing ones are fetched first, and anything that still cannot be read is
//     COUNTED and said, never dropped.
//   * THE SET MAY BE TRUNCATED. `allMatchingIds` is capped, so the caller says
//     how many matched in total and this says how many were actually looked at.
//   * ONE TOAST, ONE UNDO. Counts are per case (moved / already there / not
//     read), because a single number cannot tell "nothing to do" from "the
//     thing I measured is always empty".
//
// Batching is the actions layer's: `relabel` queues one Email/set patch per
// message and the queue drains them in SET_CHUNK-sized Email/set calls.

import { invertChanges, refileChanges } from "../../core/refile";
import type { MailActions } from "../../core/actions";
import type { Id } from "../../core/jmap/types";
import type { MailStore } from "../../core/store";
import { effectiveEmail } from "../../core/store";
import type { SyncEngine } from "../../core/sync";
import { t } from "../i18n";
import type { Toast } from "../state";

/** Email/get is asked for this many ids at a time, well under the server's
 * `maxObjectsInGet`, so a 1,000-message move is five reads rather than one
 * request the server may refuse. */
const GET_CHUNK = 200;

export interface MoveDeps {
  store: Pick<MailStore, "getState">;
  sync: Pick<SyncEngine, "loadEmails">;
  actions: Pick<MailActions, "relabel">;
  offerUndo: (label: string, inverse: () => Promise<boolean>) => void;
  pushToast: (toast: Omit<Toast, "id">) => number;
}

export interface MoveOutcome {
  /** Ids the move was asked to look at. */
  looked: number;
  /** Messages whose labels actually changed. */
  moved: number;
  /** Messages already exactly in the area -- no-ops, not errors. */
  alreadyThere: number;
  /** Messages that could not be read (gone, or the read failed). */
  unreadable: number;
  /** How many matched in all, when the id set may have been capped. */
  total: number;
  /** True when `total` exceeds what was looked at. */
  truncated: boolean;
}

const fmt = (n: number): string => n.toLocaleString("en-US");

/** The sentence the toast shows. Exported so the tests assert the words a
 * person reads rather than a re-derivation of them. */
export function moveText(o: MoveOutcome, area: string): string {
  const parts: string[] = [];
  if (o.truncated) {
    parts.push(t("areaMove.partial", { moved: fmt(o.moved), total: fmt(o.total), area }));
  } else {
    parts.push(
      o.moved === 1 ? t("areaMove.moved.one", { area }) : t("areaMove.moved.other", { n: fmt(o.moved), area }),
    );
  }
  if (o.alreadyThere > 0) parts.push(t("areaMove.alreadyThere", { n: fmt(o.alreadyThere), area }));
  if (o.unreadable > 0) parts.push(t("areaMove.unreadable", { n: fmt(o.unreadable) }));
  if (o.truncated) parts.push(t("areaMove.notTouched", { n: fmt(o.total - o.looked) }));
  return parts.join(" ");
}

/**
 * Files `ids` into `areaId`. Resolves to what happened, or null when nothing
 * was attempted (the area or its label is not loaded, or the read failed) -- in
 * which case an error toast has already said why.
 *
 * `total` is the number of messages that matched in all; pass it only when
 * `ids` may be a capped slice of them.
 */
export async function moveToArea(
  deps: MoveDeps,
  ids: Id[],
  areaId: Id,
  total: number = ids.length,
): Promise<MoveOutcome | null> {
  const { store, sync, actions, offerUndo, pushToast } = deps;
  const area = store.getState().areas[areaId];
  if (!area) return null;
  if (!store.getState().labels[area.labelId]) {
    pushToast({ kind: "error", message: t("desk.refile.labelMissing", { area: area.name }) });
    return null;
  }

  // Read what is not loaded yet; refileChanges cannot swap labels it cannot see.
  const missing = ids.filter((id) => !store.getState().emails[id]);
  for (let i = 0; i < missing.length; i += GET_CHUNK) {
    const { failed } = await sync.loadEmails(missing.slice(i, i + GET_CHUNK));
    if (failed) {
      pushToast({ kind: "error", message: t("areaMove.readFailed") });
      return null;
    }
  }

  const state = store.getState();
  const readable = ids.filter((id) => effectiveEmail(state, id) !== null);
  // The same swap the desk makes; messages already exactly there are omitted.
  const changes = refileChanges(state, readable, areaId);
  const outcome: MoveOutcome = {
    looked: ids.length,
    moved: changes.length,
    alreadyThere: readable.length - changes.length,
    unreadable: ids.length - readable.length,
    total: Math.max(total, ids.length),
    truncated: total > ids.length,
  };

  if (changes.length === 0) {
    pushToast({ kind: "info", message: nothingText(outcome, area.name) });
    return outcome;
  }
  const message = moveText(outcome, area.name);
  // Awaited: it resolves once the intents are QUEUED, which is what a caller that
  // goes on to ask the server about the labelling (Rule/induce) needs.
  await actions.relabel(changes, message);
  // The exact inverse of what changed per message (core/refile.ts).
  const inverse = invertChanges(changes);
  offerUndo(message, async () => {
    await actions.relabel(inverse, t("areaMove.undone"));
    return true;
  });
  return outcome;
}

function nothingText(o: MoveOutcome, area: string): string {
  const parts = [t("areaMove.nothing", { area })];
  if (o.alreadyThere > 0) parts.push(t("areaMove.alreadyThere", { n: fmt(o.alreadyThere), area }));
  if (o.unreadable > 0) parts.push(t("areaMove.unreadable", { n: fmt(o.unreadable) }));
  if (o.truncated) parts.push(t("areaMove.notTouched", { n: fmt(o.total - o.looked) }));
  return parts.join(" ");
}
