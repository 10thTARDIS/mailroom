// SPDX-License-Identifier: AGPL-3.0-only

// WHAT THE UNDO TOAST SAYS HAPPENED (#375 increment 6).
//
// One function per verb, here rather than inline in `useListVerbs`, for two
// reasons that are both about not lying:
//
//   * ENGLISH NEEDS TWO FORMS and this app has no ICU library on purpose
//     (app/i18n.ts). "Archived 1 messages" is the kind of thing that ships;
//     a named function with the ternary inside it is the kind of thing a
//     reviewer can check once.
//   * THE FLAG HAS TWO NAMES. `$flagged` is "the tray" on the desk and "starred"
//     in the message list — the same keyword, two vocabularies the user learned
//     from two surfaces — so the verb that toggles it has to be told which
//     surface is speaking rather than picking one and renaming things as the
//     user moves around the app.
//
// Pure: strings in, string out, no store and no React. The `t()` catalog is the
// only dependency, exactly as ui/desk/format.ts is.

import { t } from "../i18n";

/** Which word this surface uses for `$flagged`. The desk and the tray say
 * "tray"; the message list says "star". */
export type FlagWord = "tray" | "star";

export function archivedText(n: number): string {
  return n === 1 ? t("undo.archived.one") : t("undo.archived.other", { n });
}

export function trashedText(n: number): string {
  return n === 1 ? t("undo.trashed.one") : t("undo.trashed.other", { n });
}

/** What an undone TRASH actually did. The server restores to Archive, not to
 * the inbox (core/actions.ts untrash), so this is the sentence that stops the
 * undo claiming something that did not happen. */
export function restoredToArchiveText(n: number): string {
  return n === 1
    ? t("undo.restoredToArchive.one")
    : t("undo.restoredToArchive.other", { n });
}

/** What "Not junk" did (#528): the message is in the Inbox now. Said once, as a
 * plain notice — there is no undo to offer, see core/actions.ts notJunk. */
export function notJunkText(n: number): string {
  return n === 1 ? t("undo.notJunk.one") : t("undo.notJunk.other", { n });
}

export function filedText(n: number, area: string): string {
  return n === 1 ? t("undo.filed.one", { area }) : t("undo.filed.other", { n, area });
}

export function flaggedText(n: number, flagged: boolean, word: FlagWord): string {
  if (word === "tray") {
    if (flagged) return n === 1 ? t("undo.trayed.one") : t("undo.trayed.other", { n });
    return n === 1 ? t("undo.untrayed.one") : t("undo.untrayed.other", { n });
  }
  if (flagged) return n === 1 ? t("undo.starred.one") : t("undo.starred.other", { n });
  return n === 1 ? t("undo.unstarred.one") : t("undo.unstarred.other", { n });
}

/**
 * Crossing the line by hand (#375 increment 7, #464) — "Moved 3 messages above
 * the line".
 *
 * SAYS "MOVED", AND MEANS IT. Since #464 a `Triage/correct` writes the
 * placement (`messages.line_side`) in the same transaction as the feedback
 * event and appends an `Email` change row, so the message genuinely is on the
 * other side of the line afterwards and the desk's next refresh draws it there.
 * Before that server half exists the correction is recorded and the row does
 * not move — which is why this wording ships WITH it and not ahead of it.
 *
 * The desk's own two words: above the line is the SURFACE, below it is a
 * DRAWER (ui/routes/DeskRoute.tsx, core/desk.ts). "Marked important" would be
 * the model's vocabulary for a gesture the user makes about their own desk.
 */
export function crossedLineText(n: number, toSurface: boolean): string {
  if (toSurface) return n === 1 ? t("undo.toSurface.one") : t("undo.toSurface.other", { n });
  return n === 1 ? t("undo.toDrawer.one") : t("undo.toDrawer.other", { n });
}

export function snoozedText(n: number): string {
  return n === 1 ? t("undo.snoozed.one") : t("undo.snoozed.other", { n });
}

export function seenText(n: number, seen: boolean): string {
  if (seen) return n === 1 ? t("undo.read.one") : t("undo.read.other", { n });
  return n === 1 ? t("undo.unread.one") : t("undo.unread.other", { n });
}

/** What a BULK importance correction did (#921). The count is how many
 * actually CHANGED, not how many were selected: rows that already carried the
 * value are not re-sent, so "Marked 3" after a selection of five says what
 * happened rather than what the selection was. */
export function markedImportantText(n: number): string {
  return n === 1 ? t("undo.markedImportant.one") : t("undo.markedImportant.other", { n });
}

export function markedNotImportantText(n: number): string {
  return n === 1 ? t("undo.markedNotImportant.one") : t("undo.markedNotImportant.other", { n });
}
