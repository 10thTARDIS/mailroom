// SPDX-License-Identifier: AGPL-3.0-only

// DRAGGING MAIL ONTO A LABEL OR A FOLDER (#62, matrix §4).
//
// The DOM-free half: what a drag carries, and which targets may accept it. The components
// own the events; this owns the rules, so they can be tested without a browser.
//
// WHAT A DRAG CARRIES IS A SET, NOT A ROW. Dragging a row that is part of the current
// selection drags the WHOLE selection — that is what every mail client does, and the
// alternative (silently acting on one row while five are highlighted) is the kind of
// surprise that loses mail. Dragging a row that is NOT selected drags just that row and
// leaves the selection alone.
//
// DRAG IS NEVER THE ONLY WAY. Every drop this file permits already has a keyboard and menu
// equivalent (the label menu, `e` to archive, `#` to trash), and it has to stay that way:
// a mouse-only gesture is inaccessible to anyone who cannot make it, and a pointer drag is
// also the least reliable input on a touch screen. This is an accelerator.
//
// WHICH TARGETS ARE OFFERED IS DELIBERATELY NARROW. Labels, and the three folder roles the
// client can already move mail into optimistically (inbox, archive, trash). Not Sent or
// Drafts — "move a received message into Sent" is not a thing a person means, and Mailroom
// would have to invent a meaning for it. Not Junk either: a drop INTO Junk is "mark as
// junk", which the server refuses as a keyword (the verdict is the provider's) and which
// as a move is a separate decision (#528). The optimistic-move overlay (store.LocalMove)
// has no junk state for the same reason.
//
// A drop OUT of Junk onto the Inbox is offered, and means "Not junk" — see inboxDropPlan.

/** The MIME type the drag payload travels under.
 *
 * A custom type, not `text/plain`: a mail id list dropped into a text field elsewhere on the
 * page — or into another application — would be meaningless leakage, and a custom type is
 * simply not offered to those. */
export const DRAG_MIME = "application/x-mailroom-emails";

/** What a drop target is. `label` files; `role` moves between folders. */
export type DropTarget =
  | {
      kind: "label";
      labelId: string;
      /** The IMAP mirror atom, or null for a LOCAL-ONLY label. Since #704 both are
       * droppable — a local label is applied by id (MailActions.applyLabel), exactly as
       * the label menu applies it — so this is informational; the drop reads the label
       * itself. */
      keyword: string | null;
    }
  | { kind: "role"; role: "inbox" | "archive" | "trash" }
  /** An AREA in the rail (#728). A drop is a MOVE, not an add: it swaps the other
   * areas' own labels out and the target's in (core/refile.ts), as Re-file does. */
  | { kind: "area"; areaId: string; labelId: string };

/** The ids a drag is carrying, serialised for the DataTransfer. */
export interface DragPayload {
  ids: string[];
}

/**
 * The set a drag should carry: the whole selection when the dragged row is part of it,
 * otherwise just that row.
 */
export function dragSet(draggedId: string, selected: Iterable<string>): string[] {
  const sel = new Set(selected);
  if (sel.has(draggedId)) return [...sel];
  return [draggedId];
}

/** Serialises a payload for `DataTransfer.setData`. */
export function encodeDrag(ids: string[]): string {
  return JSON.stringify({ ids });
}

/**
 * Reads a payload back, returning [] for anything it does not recognise.
 *
 * TOTAL BY DESIGN: this parses a string that reached us through the platform's clipboard
 * machinery, so it can be absent, truncated, or something another application put there.
 * A drop it cannot understand must do NOTHING — never throw (which would leave a
 * half-handled drop), and never guess.
 */
export function decodeDrag(raw: string | null | undefined): string[] {
  if (!raw) return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== "object" || parsed === null) return [];
    const ids = (parsed as { ids?: unknown }).ids;
    if (!Array.isArray(ids)) return [];
    // Validated element by element rather than trusted wholesale: one non-string in the
    // array would otherwise reach an Email/set as an id.
    return ids.filter((v): v is string => typeof v === "string" && v !== "");
  } catch {
    return [];
  }
}

/**
 * Whether a target may accept a drop, given the view the drag started in.
 *
 * The one rule with teeth: A TARGET THAT WOULD DO NOTHING IS REFUSED. Dropping mail onto
 * the folder it is already in, or onto a label it already carries, produces no change —
 * and a drop that animates, lands, and changes nothing reads as a bug. Refusing it makes
 * the cursor say so before the user commits.
 */
export function canDrop(target: DropTarget, ctx: DropContext): boolean {
  if (ctx.ids.length === 0) return false;
  if (target.kind === "role") {
    // Already in that folder: nothing to do.
    return ctx.currentRole !== target.role;
  }
  if (target.kind === "area") {
    // A move is a no-op only where the target's label is on and no other area's is.
    const others = (ctx.areaLabelIds ?? []).filter((l) => l !== target.labelId);
    return ctx.ids.some((id) => {
      const on = ctx.labelsOn[id] ?? [];
      return !on.includes(target.labelId) || others.some((l) => on.includes(l));
    });
  }
  // A label every dragged message already has: nothing to do. If even one lacks it, the
  // drop is meaningful for that one, so it is allowed — partial progress is still progress
  // and matches what the label menu does.
  return ctx.ids.some((id) => !(ctx.labelsOn[id] ?? []).includes(target.labelId));
}

/** Which write each message dropped on the Inbox gets. Every id lands in exactly one. */
export interface InboxDropPlan {
  /** Out of Trash: the existing restore (core/actions.ts untrash). */
  untrash: string[];
  /** Out of Junk: "Not junk" (core/actions.ts notJunk). */
  notJunk: string[];
  /** From anywhere else: a plain move back to the Inbox (core/actions.ts restoreToInbox). */
  restoreToInbox: string[];
}

/**
 * What dropping mail on the Inbox actually does, per message (#528).
 *
 * ⚠ THIS USED TO BE `untrash` FOR EVERY DROP, AND FOR ANY MESSAGE NOT IN TRASH THAT FILED
 * IT INTO ARCHIVE. `untrash` sends `mailboxIds/<trash>: false`. On a message that is not
 * trashed the server reads that as a Restore with no destination named, and a Restore with
 * no destination goes to Archive (internal/store ApplyEmailUpdate, "the message came out of
 * Trash and has to land somewhere"). So dragging a false positive out of Junk onto the Inbox
 * — the most natural rescue gesture there is — silently archived it, and so did dragging
 * anything from a folder, a label or a search.
 *
 * The rule now follows WHERE THE DRAG STARTED, and within a view that is not a role, WHAT
 * EACH MESSAGE IS:
 *
 *   - from Trash: untrash, unchanged. Trash's restore is its own deliberate behaviour (it
 *     restores to Archive and the toast says so) and is not what this issue is about.
 *   - otherwise, a message in Junk: notJunk — clear the verdict AND move it, so a provider
 *     that tags junk sees the ham signal too. That includes a junk message dragged out of a
 *     search, which is why this is decided per message and not per view.
 *   - otherwise: restoreToInbox, the same move `archive`'s undo uses.
 *
 * `isJunk` is core/junk.ts `isInJunk`, bound to the current state by the caller; it is a
 * parameter so this stays free of the store.
 */
export function inboxDropPlan(
  ids: string[],
  currentRole: string | null,
  isJunk: (id: string) => boolean,
): InboxDropPlan {
  const plan: InboxDropPlan = { untrash: [], notJunk: [], restoreToInbox: [] };
  for (const id of ids) {
    if (currentRole === "trash") plan.untrash.push(id);
    else if (currentRole === "junk" || isJunk(id)) plan.notJunk.push(id);
    else plan.restoreToInbox.push(id);
  }
  return plan;
}

export interface DropContext {
  ids: string[];
  /** The role of the folder the drag started in, when the current view is one. */
  currentRole: string | null;
  /** Label ids already on each dragged message. */
  labelsOn: Record<string, string[]>;
  /** Every area's own label id (#728), so an area drop can tell "already exactly
   * there" from "there, but another area's label still outranks it". */
  areaLabelIds?: string[];
}
