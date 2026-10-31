// SPDX-License-Identifier: AGPL-3.0-only

// EVERY MESSAGE FROM ONE SENDER, MOVED IN ONE ACT (#59 block, #53 clean-up).
//
// The #708 machinery, generalised past areas. The sender card's "Move all
// from … to…" (ui/pane/SenderPopover.tsx) asks the server for every message
// from an EXACT address (`sync.allMatchingIds` over the `from:` search),
// states the count, waits for a confirm, moves through the ordinary path with
// one Undo, and offers "Move the rest" past the 1,000-message cap. Blocking a
// sender (move their existing mail to Junk or Trash) and the clean-up view
// (archive all, move all to an area, block) need exactly that, to more places.
//
// NOTHING HERE IS A NEW WRITE PATH. An area is `moveToArea` (the desk's label
// swap); Archive, Junk and Trash are `actions.archive`, `actions.moveToJunk`
// and `actions.trash` — the moves every list already makes, queued, written
// back to IMAP by the server. What this adds is the part those never needed
// because they act on rows already on screen: read what is not loaded, count
// per case (moved / already there / unreadable / not touched), one Undo.
//
// "THE REST" DEPENDS ON WHERE THE MAIL WENT, and getting it wrong skips mail:
//
//   * An AREA or ARCHIVE leaves the moved mail IN the `from:` search (a search
//     covers every folder but Trash and Junk), so the next page starts where
//     this one stopped — `position + looked`, as #708 does.
//   * JUNK and TRASH take it OUT of the search (Email/query excludes both
//     unless asked: internal/api/email.go), so the next page starts at 0 —
//     once the queue has drained, or the server would still return the mail
//     this page moved. `restStart` is that rule; `actions.settle` the wait.

import { moveToArea } from "./moveToArea";
import type { MoveDeps, MoveOutcome } from "./moveToArea";
import type { MailActions } from "../../core/actions";
import type { Id, Mailbox } from "../../core/jmap/types";
import type { MailStore } from "../../core/store";
import { effectiveEmail } from "../../core/store";
import type { SyncEngine } from "../../core/sync";
import { t } from "../i18n";
import type { Toast } from "../state";

/** Where one sender's mail is being moved. */
export type BulkTarget =
  | { kind: "area"; areaId: Id }
  | { kind: "archive" }
  | { kind: "junk" }
  | { kind: "trash" };

/** The three folder targets, by role. */
type BinRole = "archive" | "junk" | "trash";

const GET_CHUNK = 200;

export interface BulkDeps extends MoveDeps {
  store: Pick<MailStore, "getState">;
  sync: Pick<SyncEngine, "loadEmails">;
  actions: Pick<MailActions, "relabel" | "archive" | "moveToJunk" | "trash" | "restoreToInbox" | "untrash">;
  offerUndo: (label: string, inverse: () => Promise<boolean>) => void;
  pushToast: (toast: Omit<Toast, "id">) => number;
}

/** The name a target is shown under: the area's, or the folder's. */
export function targetName(target: BulkTarget, areas: Readonly<Record<Id, { name: string } | undefined>>): string {
  switch (target.kind) {
    case "area":
      return areas[target.areaId]?.name ?? "";
    case "archive":
      return t("senderBulk.archive");
    case "junk":
      return t("senderBulk.junk");
    case "trash":
      return t("senderBulk.trash");
  }
}

/** Where the next page of a truncated move starts (see the header). */
export function restStart(target: BulkTarget, position: number, looked: number): number {
  return target.kind === "junk" || target.kind === "trash" ? 0 : position + looked;
}

/** Whether every mailbox a message is in has this role — "already there". */
function onlyIn(mailboxIds: Record<Id, boolean>, mailboxes: Record<Id, Mailbox>, role: BinRole): boolean {
  const roles = Object.keys(mailboxIds)
    .filter((id) => mailboxIds[id])
    .map((id) => mailboxes[id]?.role ?? null);
  return roles.length > 0 && roles.every((r) => r === role);
}

/**
 * Moves `ids` to `target`. Resolves to what happened, or null when nothing was
 * attempted (an error toast has already said why). `total` is how many
 * matched in all, when `ids` may be a capped slice.
 */
export async function moveSenderMail(
  deps: BulkDeps,
  ids: Id[],
  target: BulkTarget,
  total: number = ids.length,
): Promise<MoveOutcome | null> {
  if (target.kind === "area") return moveToArea(deps, ids, target.areaId, total);
  const { store, sync, actions, offerUndo, pushToast } = deps;
  const role: BinRole = target.kind;
  const name = targetName(target, {});

  // Read what is not loaded: "already there" is a fact about each message, and
  // one that cannot be read is COUNTED, never silently dropped.
  const missing = ids.filter((id) => !store.getState().emails[id]);
  for (let i = 0; i < missing.length; i += GET_CHUNK) {
    const { failed } = await sync.loadEmails(missing.slice(i, i + GET_CHUNK));
    if (failed) {
      pushToast({ kind: "error", message: t("areaMove.readFailed") });
      return null;
    }
  }
  const state = store.getState();
  const readable: Id[] = [];
  const toMove: Id[] = [];
  for (const id of ids) {
    const e = effectiveEmail(state, id);
    if (!e) continue;
    readable.push(id);
    if (!onlyIn(e.mailboxIds, state.mailboxes, role)) toMove.push(id);
  }
  const outcome: MoveOutcome = {
    looked: ids.length,
    moved: toMove.length,
    alreadyThere: readable.length - toMove.length,
    unreadable: ids.length - readable.length,
    total: Math.max(total, ids.length),
    truncated: total > ids.length,
  };
  const message = bulkText(outcome, name);
  if (toMove.length === 0) {
    pushToast({ kind: "info", message });
    return outcome;
  }
  let ok: boolean;
  switch (role) {
    case "archive":
      ok = await actions.archive(toMove);
      break;
    case "junk":
      ok = await actions.moveToJunk(toMove);
      break;
    case "trash":
      await actions.trash(toMove);
      ok = true;
      break;
  }
  if (!ok) return null; // the action has already said why (no such folder)
  // ONE Undo for the whole move: Archive and Junk come back to the Inbox (the
  // inverse every archive in this client already has), Trash to Archive, which
  // is where the server restores trashed mail (actions.untrash).
  offerUndo(message, async () => {
    if (role === "trash") return actions.untrash(toMove);
    return actions.restoreToInbox(toMove);
  });
  return outcome;
}

/** The toast's sentence: moveToArea's own wording, with the folder's name. */
export function bulkText(o: MoveOutcome, name: string): string {
  const fmt = (n: number): string => n.toLocaleString("en-US");
  const parts: string[] = [];
  if (o.moved === 0) {
    parts.push(t("areaMove.nothing", { area: name }));
  } else if (o.truncated) {
    parts.push(t("areaMove.partial", { moved: fmt(o.moved), total: fmt(o.total), area: name }));
  } else {
    parts.push(
      o.moved === 1 ? t("areaMove.moved.one", { area: name }) : t("areaMove.moved.other", { n: fmt(o.moved), area: name }),
    );
  }
  if (o.alreadyThere > 0) parts.push(t("areaMove.alreadyThere", { n: fmt(o.alreadyThere), area: name }));
  if (o.unreadable > 0) parts.push(t("areaMove.unreadable", { n: fmt(o.unreadable) }));
  if (o.truncated) parts.push(t("areaMove.notTouched", { n: fmt(o.total - o.looked) }));
  return parts.join(" ");
}
