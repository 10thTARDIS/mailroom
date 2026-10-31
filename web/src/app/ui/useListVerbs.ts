// SPDX-License-Identifier: AGPL-3.0-only

// THE VERBS A LIST SURFACE HAS (#375 increment 3), lifted out of ListRoute so
// the desk can have the same ones rather than a second implementation of them.
//
// This is a MOVE, not a rewrite: every callback below is the one ListRoute had,
// with the same dependency list and the same comments, and ListRoute now calls
// this hook instead of declaring them. What made the move worth doing is that
// these are not helpers — they are behaviour a user learns once: that archiving
// the open message advances to the next one rather than dumping you back to the
// list, that walking with `j` while the pane is open REPLACES the history entry,
// that a multi-select star takes its direction from the first row so it acts as
// one coherent gesture. Two surfaces with two copies of that is two surfaces
// that drift.
//
// AND EVERY DESTRUCTIVE ONE NOW REGISTERS ITS INVERSE (#375 increment 6). The
// undo log is reached through `useApp()` rather than passed in as an option,
// because that is what the shared-behaviour argument above already implies: if
// archiving offered an undo on the desk and not in the message list, that would
// be one more thing a user has to learn per surface. Registering it here means
// there is exactly one place the offer can be missing from, and it is the same
// place the action is.
//
// WHAT IS NOT OFFERED, and why not: pin, mute, un-snooze and "Not
// junk". Not junk's inverse is marking the message as junk, which the server
// refuses (#528). The others' inverses are not derivable from the call —
// un-snoozing has to restore a per-message wake time nothing here remembers,
// so "the opposite" is not a thing this layer can send. An undo button that
// put back something other than what was there would be worse than no button.
// Importance was the last of these, and left in #921: a SINGLE correction's
// answer is a RE-SCORE rather than the value we sent (core/store.ts
// `important`), so the opposite of one verdict is debatable — but a BULK
// correction's honest inverse IS derivable, re-correcting the same rows in
// the other direction, and the verb registers it (doImportant below).
//
// WHAT STAYED IN ListRoute, and why: the needs-a-category verbs (`doDismiss`,
// the batch answer, the rule suggestion). They are about ONE view's workflow —
// answering the classifier's open questions — and the desk's Needs-sorting
// section is a different gesture with a different write behind it (#375
// increment 5). Moving them here would have made this hook the union of two
// surfaces rather than their intersection.

import { useCallback, useEffect } from "react";

import { effectiveEmail } from "../../core/store";
import type { EffectiveEmail } from "../../core/store";
import type { Id } from "../../core/jmap/types";
import type { Route } from "../../core/routes";
import { withCompose, withM } from "../../core/routes";
import { leaveTo, navigate } from "../routing";
import { useApp, useMailState } from "../state";
import { importantChange, importantTarget } from "./importantVerb";
import {
  archivedText,
  flaggedText,
  markedImportantText,
  markedNotImportantText,
  notJunkText,
  restoredToArchiveText,
  seenText,
  trashedText,
} from "./undoText";
import type { FlagWord } from "./undoText";
import { paneLeaveVerdict } from "./compose/sendArchive";
import type { PendingPaneLeave } from "./compose/sendArchive";

/**
 * The composer's note that a Send & archive has closed out the conversation in
 * the reading pane (#648; ./compose/sendArchive.ts `paneToLeaveAfterSend`).
 *
 * Module state, like ComposeRoute's `adoptedDrafts`, because it has to cross
 * from the composer to whichever list surface is mounted beneath it, and it
 * must be written BEFORE the navigation that closes the composer: the surface
 * reads it in the effect that navigation triggers.
 */
let pendingPaneLeave: PendingPaneLeave | null = null;

/** Ask the mounted list surface to move the reading pane off `openId` the way
 * Archive does, once the composer has closed. */
export function requestPaneLeave(openId: Id, at: number = Date.now()): void {
  pendingPaneLeave = { openId, at };
}

export interface ListVerbs {
  openMessage: (id: Id, opts?: { replace?: boolean }) => void;
  closeMessage: () => void;
  advancePastOpen: (acted: Id[]) => void;
  afterRemoval: (acted: Id[]) => void;
  doArchive: (id?: Id) => Promise<void>;
  doTrash: (id?: Id) => Promise<void>;
  doUntrash: (id?: Id) => Promise<void>;
  doNotJunk: (id?: Id) => Promise<void>;
  doStar: (id?: Id) => Promise<void>;
  doPin: (id?: Id) => Promise<void>;
  doSnooze: (id?: Id) => void;
  doImportant: (id?: Id) => Promise<void>;
  /** Whether EVERY message the button would act on is already important (#921)
   * — the selection bars label their button from it. */
  allImportant: (id?: Id) => boolean;
  doMute: (id?: Id) => Promise<void>;
  doRead: (seen: boolean, id?: Id) => Promise<void>;
  doToggleRead: (id?: Id) => Promise<void>;
  replyTo: (email: EffectiveEmail) => void;
  replyAllTo: (email: EffectiveEmail) => void;
  forwardOn: (email: EffectiveEmail) => void;
}

export interface ListVerbsOptions {
  /** The list as it is on screen, in display order — what "the next one" means
   * to `advancePastOpen`, and what a shift-range walks. */
  ids: Id[];
  /** The open message, which is the URL's (#373). */
  openId: Id | null;
  /** The route these verbs navigate within: the LIST route, so a message opened
   * while settings is on top of the list lands back on the list. */
  route: Route;
  /** The messages a verb acts on: the selection, or the row it was invoked on. */
  targets: (id?: Id | null) => Id[];
  setCursor: (id: Id) => void;
  clearSelection: () => void;
  /** Snooze needs a WHEN, so it opens a menu the surface owns. */
  openSnoozeMenu: (ids: Id[]) => void;
  /** True in a view that lists snoozed mail (the Snoozed view), where the
   * obvious meaning of the shortcut is "bring it back" rather than "open a menu
   * of future times". */
  unsnoozeInstead: boolean;
  /**
   * What THIS surface calls `$flagged` — needed only by the undo toast, which
   * is the one place these verbs have to speak.
   *
   * The desk and the tray call it the TRAY; the message list calls it the STAR.
   * The keyword is the same and the gesture is the same, but the word is one
   * the user learned from the surface they are on, and a toast that answered a
   * tray keystroke with "Starred 3 messages" would be the app renaming its own
   * furniture mid-sentence.
   */
  flagWord: FlagWord;
}

export function useListVerbs(opts: ListVerbsOptions): ListVerbs {
  const { actions, offerUndo, pushToast } = useApp();
  const state = useMailState();
  const {
    ids,
    openId,
    route,
    targets,
    setCursor,
    clearSelection,
    openSnoozeMenu,
    unsnoozeInstead,
    flagWord,
  } = opts;

  /**
   * Open a message: put it in the URL and move the cursor to it.
   *
   * `replace` for the openings that are not navigations the user made — walking
   * the list with j/k while the pane is open, and the auto-advance after an
   * action removes the open message. Pushing those would bury the list under a
   * history entry per row.
   *
   * ⚠ AND FOR EVERY OPENING WHILE A MESSAGE IS ALREADY OPEN. The reading pane is
   * ONE history entry over the list, however many messages it has shown. Each
   * click used to push, so opening A, B and C stacked three entries; Escape and
   * the pane's close button go BACK one entry (routing.ts leaveTo), and so
   * "closed" C by showing B, then A — the operator's "it skips me between emails
   * I've previously opened, in reverse order". Replacing keeps the entry below
   * the pane the list: close, Escape and the browser's Back button all return
   * to it in one step, and Forward reopens the last message shown.
   */
  const openMessage = useCallback(
    (id: Id, options: { replace?: boolean } = {}) => {
      setCursor(id);
      navigate(withM(route, id), { replace: options.replace ?? openId !== null });
    },
    [route, openId, setCursor],
  );

  /** Close the reading pane, agreeing with the back button (routing.ts). */
  const closeMessage = useCallback(() => {
    leaveTo(withM(route, null));
  }, [route]);

  /**
   * Auto-advance: when an action removes the OPEN message from this view, show
   * the next one down — or the previous, when it was the last — rather than
   * leaving a message on screen that the list no longer contains, or dumping the
   * reader back to a bare list mid-flow.
   *
   * It reads the PRE-action list to find the neighbour. Callers may invoke it
   * either side of the action itself: this callback closes over the list as it
   * was when the handler was created, which is what "the next one" has to mean —
   * a moment later the optimistic overlay has already dropped that row.
   *
   * Whether the message actually leaves is the caller's call: in a view that
   * still lists it — snoozing from Starred, say — keeping it open is right.
   */
  const advancePastOpen = useCallback(
    (acted: Id[]) => {
      if (openId === null || !acted.includes(openId)) return;
      const i = ids.indexOf(openId);
      const next = i === -1 ? null : ids[i + 1] ?? ids[i - 1] ?? null;
      if (next) openMessage(next, { replace: true });
      else navigate(withM(route, null), { replace: true });
    },
    [ids, openId, openMessage, route],
  );

  /**
   * SEND & ARCHIVE LEAVES THE PANE AS ARCHIVE DOES (#648). The composer left a
   * note (`requestPaneLeave`) when the send was accepted; this acts on it once
   * the composer is out of the URL, through `advancePastOpen` — the same call
   * `doArchive` makes — so `e` and Send & archive cannot end differently. The
   * decision is ./compose/sendArchive.ts `paneLeaveVerdict`, tested there.
   */
  useEffect(() => {
    const verdict = paneLeaveVerdict(pendingPaneLeave, {
      openId,
      composing: route.compose !== undefined,
      at: Date.now(),
    });
    if (verdict === "none" || verdict === "wait") return;
    pendingPaneLeave = null;
    if (verdict === "leave" && openId !== null) advancePastOpen([openId]);
  }, [route, openId, advancePastOpen]);

  /** After an action that removes messages from THIS view: drop the selection
   * and move the reading pane on. Note it advances rather than closing, and only
   * when the OPEN message was one of the ones acted on — archiving a selection
   * that does not include it leaves the pane alone, because nothing happened to
   * what is on screen. */
  const afterRemoval = useCallback(
    (acted: Id[]) => {
      clearSelection();
      advancePastOpen(acted);
    },
    [clearSelection, advancePastOpen],
  );

  /**
   * Archive, with the offer to take it back.
   *
   * The inverse is a move to the INBOX, not a move to wherever each message
   * came from: nothing here remembers that, and the only honest opposite of
   * "this is done" is "this is not done" (core/actions.ts restoreToInbox says
   * the rest). Registered only when the archive actually started — `archive`
   * answers false when the account has no Archive folder, and an undo offer for
   * something that did not happen is the worst kind of button.
   */
  const doArchive = useCallback(
    async (id?: Id) => {
      const list = targets(id);
      if (list.length === 0) return;
      if (!(await actions.archive(list))) return;
      afterRemoval(list);
      offerUndo(archivedText(list.length), () => actions.restoreToInbox(list));
    },
    [actions, targets, afterRemoval, offerUndo],
  );

  /**
   * Trash, with the offer to take it back — and a SECOND toast when it lands,
   * because the undo does not do what the word implies.
   *
   * The server restores a trashed message to Archive, not to where it was
   * (core/actions.ts untrash, mirroring internal/store's Restore ⇒ MoveToRole
   * "archive"). Saying nothing would leave the user looking for a message in an
   * inbox it is not in; saying "Restored" would be the client claiming a place.
   * So it says which place, once, on success only.
   */
  const doTrash = useCallback(
    async (id?: Id) => {
      const list = targets(id);
      if (list.length === 0) return;
      await actions.trash(list);
      afterRemoval(list);
      offerUndo(trashedText(list.length), async () => {
        const ok = await actions.untrash(list);
        if (ok) pushToast({ kind: "info", message: restoredToArchiveText(list.length) });
        return ok;
      });
    },
    [actions, targets, afterRemoval, offerUndo, pushToast],
  );

  const doUntrash = useCallback(
    async (id?: Id) => {
      const list = targets(id);
      if (list.length === 0) return;
      await actions.untrash(list);
      afterRemoval(list);
    },
    [actions, targets, afterRemoval],
  );

  /**
   * "Not junk" (#528, spec §6): rescue a false positive into the Inbox.
   *
   * It removes the message from the Junk list, so the pane moves on exactly as
   * it does for archive. It SAYS where the message went, because a row leaving
   * the Junk list otherwise reads as "it was deleted" — and it offers no undo,
   * because the inverse is marking it as junk (core/actions.ts notJunk).
   */
  const doNotJunk = useCallback(
    async (id?: Id) => {
      const list = targets(id);
      if (list.length === 0) return;
      if (!(await actions.notJunk(list))) return;
      afterRemoval(list);
      pushToast({ kind: "info", message: notJunkText(list.length) });
    },
    [actions, targets, afterRemoval, pushToast],
  );

  const doStar = useCallback(
    async (id?: Id) => {
      const list = targets(id);
      if (list.length === 0) return;
      const first = effectiveEmail(state, list[0] as Id);
      const flagged = !(first?.isFlagged ?? false);
      await actions.setFlagged(list, flagged);
      // The inverse is the same write with the direction reversed, over the
      // same ids — NOT "toggle again", which would take its direction from the
      // first row a second time and could flip a selection the user had since
      // changed.
      offerUndo(flaggedText(list.length, flagged, flagWord), async () => {
        await actions.setFlagged(list, !flagged);
        return true;
      });
    },
    [actions, targets, state, offerUndo, flagWord],
  );

  /** Toggle pin (#51). Like star, the FIRST target decides the direction, so a
   * multi-select acts as one coherent "pin these" rather than flipping each
   * message to its own opposite. */
  const doPin = useCallback(
    async (id?: Id) => {
      const list = targets(id);
      if (list.length === 0) return;
      const first = effectiveEmail(state, list[0] as Id);
      await actions.setPinned(list, !(first?.isPinned ?? false));
    },
    [actions, targets, state],
  );

  /** Open the snooze menu on the current targets (#51). Snooze needs a WHEN, so
   * unlike pin and star it opens a menu rather than toggling — except from the
   * Snoozed view, where the obvious meaning of the shortcut is "bring it back",
   * and making the user pick "now" from a menu of future times would be silly.
   * Bringing one back removes it from that view, so the pane advances. */
  const doSnooze = useCallback(
    (id?: Id) => {
      const list = targets(id);
      if (list.length === 0) return;
      if (unsnoozeInstead) {
        void actions.setSnoozed(list, null);
        afterRemoval(list);
        return;
      }
      openSnoozeMenu(list);
    },
    [actions, targets, unsnoozeInstead, afterRemoval, openSnoozeMenu],
  );

  /** Correct the model's importance verdict (#154) — and mark a whole
   * SELECTION important at once (#921).
   *
   * ⚠ THE DIRECTION IS NOT THE FIRST ROW ANYMORE. The old first-row toggle
   * un-marked a mixed selection whose first row was already important; the
   * rule ui/importantVerb.ts `importantTarget` states instead — mark important
   * unless every selected message already is — can never silently un-mark
   * anything. Only the rows that actually CHANGE are sent (and that is what
   * the undo offers back), and a selection that already is where the verb
   * points changes nothing and says nothing. */
  const doImportant = useCallback(
    async (id?: Id) => {
      const list = targets(id);
      if (list.length === 0) return;
      const flags = list.map((lid) => effectiveEmail(state, lid)?.isImportant ?? false);
      const { important, changed } = importantChange(list, flags);
      if (changed.length === 0) return;
      if (!(await actions.setImportant(changed, important))) return;
      offerUndo(
        important ? markedImportantText(changed.length) : markedNotImportantText(changed.length),
        () => actions.setImportant(changed, !important),
      );
    },
    [actions, targets, state, offerUndo],
  );

  /** Whether EVERY message the button would act on is already important (#921):
   * the selection bars label their button from it. Render-safe in the routes
   * whose `targets` is plain (desk, tray); ListRoute computes the same value
   * itself because its hook targets record the bulk run and may not run while
   * rendering (ui/routes/ListRoute.tsx). */
  const allImportant = useCallback(
    (id?: Id) =>
      !importantTarget(
        targets(id).map((lid) => effectiveEmail(state, lid)?.isImportant ?? false),
      ),
    [targets, state],
  );

  /** Toggle mute on the CONVERSATION the target belongs to (#51) — mute is
   * thread-scoped, so a message is only ever the handle for its thread. */
  const doMute = useCallback(
    async (id?: Id) => {
      const list = targets(id);
      const first = list.length > 0 ? effectiveEmail(state, list[0] as Id) : null;
      const threadId = first?.threadId;
      if (!threadId) return;
      const muted = state.threads[threadId]?.isMuted ?? false;
      await actions.setThreadMuted(threadId, !muted);
    },
    [actions, targets, state],
  );

  const doRead = useCallback(
    async (seen: boolean, id?: Id) => {
      const list = targets(id);
      if (list.length === 0) return;
      await actions.setSeen(list, seen);
      offerUndo(seenText(list.length, seen), async () => {
        await actions.setSeen(list, !seen);
        return true;
      });
    },
    [actions, targets, offerUndo],
  );

  const doToggleRead = useCallback(
    async (id?: Id) => {
      const list = targets(id);
      if (list.length === 0) return;
      const first = effectiveEmail(state, list[0] as Id);
      const seen = !(first?.isSeen ?? false);
      await actions.setSeen(list, seen);
      offerUndo(seenText(list.length, seen), async () => {
        await actions.setSeen(list, !seen);
        return true;
      });
    },
    [actions, targets, state, offerUndo],
  );

  /**
   * Forward a message (#582). The same route shape as a reply, so a half-written
   * forward survives a reload too.
   *
   * ⚠ IT TAKES ONE MESSAGE, NOT THE SELECTION, and that is not an oversight: the
   * composer's forward intent carries exactly one parent (ui/Compose.tsx quotes
   * its body and re-attaches its parts), so "forward these twelve" is not a
   * thing it can express. Every other verb here acts on `targets(id)`; this one
   * deliberately does not.
   *
   * The composer for this has existed since #448 and, until now, the ONLY thing
   * in the client that opened it was the attachment viewer's Forward button —
   * so a message without an attachment could not be forwarded at all.
   */
  const forwardOn = useCallback(
    (email: EffectiveEmail) => {
      navigate(withCompose(route, { forward: email.id }));
    },
    [route],
  );

  /** Reply to a message — a route now (`?compose=reply:<id>`), so a half-written
   * reply survives a reload and the composer can be linked to. */
  const replyTo = useCallback(
    (email: EffectiveEmail) => {
      navigate(withCompose(route, { reply: email.id }));
    },
    [route],
  );

  /** Reply all (#646) — the same route shape as `replyTo`, one compose kind over,
   * so it gets the same reload survival and shareable link for free. */
  const replyAllTo = useCallback(
    (email: EffectiveEmail) => {
      navigate(withCompose(route, { replyAll: email.id }));
    },
    [route],
  );

  return {
    forwardOn,
    openMessage,
    closeMessage,
    advancePastOpen,
    afterRemoval,
    doArchive,
    doTrash,
    doUntrash,
    doNotJunk,
    doStar,
    doPin,
    doSnooze,
    doImportant,
    allImportant,
    doMute,
    doRead,
    doToggleRead,
    replyTo,
    replyAllTo,
  };
}
