// SPDX-License-Identifier: AGPL-3.0-only

// WHAT ONE ROW OF THE DESK IS CALLED, IN WORDS (#447).
//
// The desk's list is a grid whose current row is announced through
// `aria-activedescendant` (ui/desk/VirtualList.tsx). The thing a screen reader
// then reads is the ROW's accessible name — one string — so that string has to
// carry everything the row carries. Before this it carried two: the row's inner
// buttons were named "Select mail from Ada" and "Ada: Your invoice", and
// everything else on the row was either a glyph with its own label (heard only if
// you went looking for it) or a font weight (heard by nobody).
//
// SO THE MARKS ARE WORDS, IN A FIXED ORDER, AND UNREAD IS FIRST.
//
//   "Unread, Ada Lovelace, Your invoice is ready, in Bills, arrived at
//    invoices@, 09:14, In the tray, Attachment held, 3 messages in this
//    conversation"
//
// * UNREAD LEADS because it is the fact that decides whether the reader cares
//   about the rest, and because it is the one the SCREEN states by weight alone
//   (`.desk-row.is-unread` is bold plus a dot — shape, not colour, but neither is
//   a word). "Read" is not stated: the absence of "Unread" is the honest form,
//   and a client that said "Read" on nine rows out of ten would be padding every
//   announcement with the least interesting thing about it.
// * THE ORDER IS FIXED because it is what makes the list scannable by ear. A name
//   assembled in whatever order the fields happened to be truthy would put the
//   subject in a different place on every row.
// * EVERY CLAUSE IS OMITTED WHEN ITS FACT IS ABSENT, and several of these facts
//   are absent for a reason worth knowing rather than a bug: `senderWarnings` and
//   `attachmentScan` are BODY properties (jmap/types.ts EMAIL_BODY_PROPERTIES),
//   so on a row that has only ever been fetched as part of a list they are simply
//   not there, and the mark is not claimed. The alternative — a client-side guess
//   — would be a warning about a sender nobody checked.
//
// Pure, and separate from the component for the reason every decision in this
// directory is: this repo's web tests have no renderer (ui/attachmentPreviewPolicy
// .ts's note), so a name built inside JSX is a name no test can read.

import { senderIdentity, senderText, stripBidiControls } from "../../../core/hostileText";
import { senderWarningsList } from "../../../core/senderWarnings";
import type { SenderWarning } from "../../../core/senderWarnings";
import type { AttachmentScan, EmailAddress, SenderWarningRaw } from "../../../core/jmap/types";
import { t } from "../../i18n";
import { aliasOf, rowTime } from "./format";

/**
 * The sentence a warning mark carries, in the reader's language.
 *
 * Reuses the READING PANE's wording (#397) rather than inventing a shorter one:
 * the row has room for a glyph, and the explanation belongs in the label, where it
 * can be as long as the truth needs. It lives here rather than in DeskRow.tsx
 * because the row's NAME needs the same sentence the row's glyph does, and two
 * derivations of one warning is one of them getting a re-wording wrong.
 */
export function warningText(w: SenderWarning): string {
  if (w.kind === "selfSpoof") return t("thread.senderWarning.selfSpoof", { domain: w.domain });
  if (w.kind === "lookalikeDomain") {
    return t("thread.senderWarning.lookalikeDomain", {
      domain: w.domain,
      resembles: w.resembles ?? "",
    });
  }
  return w.unicode
    ? t("thread.senderWarning.punycodeDomain", { domain: w.domain, unicode: w.unicode })
    : t("thread.senderWarning.punycodeDomainOpaque", { domain: w.domain });
}

/** Every fact a row's name is built from, already reduced to what it IS rather
 * than to the field it came from — so the test cases are about the sentence and
 * not about the store. */
export interface RowNameInput {
  unread: boolean;
  sender: string;
  subject: string;
  /** The area this row is in, or null — including for a surface that never asked
   * (the tray page groups by how long something has waited, not by area). */
  areaName?: string | null;
  /**
   * Which account this arrived on, when the row shows the account DOT — and null
   * whenever it does not (one account, or a desk scoped to one).
   *
   * ⚠ THE DOT IS A COLOUR AND NOTHING ELSE. It had an `aria-label` on a bare
   * `<span>`, which ARIA does not require anyone to expose, so the account was the
   * one fact on the row carried by hue alone (tokens.css: never colour alone). It
   * is a word here, on exactly the rows that draw the dot.
   */
  accountName?: string | null;
  /** `format.ts aliasOf` — "invoices@", or "" for a message with no
   * `deliveredTo`. */
  alias?: string;
  /** `format.ts rowTime` — the SAME string the row prints, so the name and the
   * screen cannot disagree about what time it is. */
  time?: string;
  /** In the tray (`$flagged`). */
  flagged?: boolean;
  pinned?: boolean;
  /** A snooze brought this back (`returnedAt`), which is why an old message is
   * near the top. */
  returned?: boolean;
  hasAttachment?: boolean;
  /** The download route is refusing at least one of this message's parts
   * (ui/attachmentScanPolicy.ts). */
  heldAttachment?: boolean;
  /** The sender warning's whole sentence, or null. */
  warning?: string | null;
  /** How many messages are in the conversation, when it is known and > 1. */
  threadCount?: number | null;
  /** A local change for this row has not reached the server yet. */
  queued?: boolean;
  /** Some intent for this message is still un-reconciled. */
  pending?: boolean;
}

/**
 * The row's accessible name.
 *
 * Joined with ", " — commas rather than the row's own visual "·" separators,
 * because a screen reader reads a comma as a pause and reads "·" out loud (or
 * not at all, depending on punctuation settings). What the eye needs to separate
 * columns and what the ear needs to separate clauses are not the same character.
 */
export function rowAccessibleName(input: RowNameInput): string {
  const parts: string[] = [];
  if (input.unread) parts.push(t("desk.row.unread"));
  parts.push(input.sender);
  parts.push(input.subject);
  if (input.areaName) parts.push(t("desk.row.inArea", { area: input.areaName }));
  if (input.accountName) parts.push(t("desk.row.account", { account: input.accountName }));
  if (input.alias) parts.push(t("desk.row.alias", { alias: input.alias }));
  if (input.time) parts.push(input.time);
  if (input.flagged) parts.push(t("desk.row.flagged"));
  if (input.pinned) parts.push(t("desk.row.pinned"));
  if (input.returned) parts.push(t("desk.row.returned"));
  if (input.hasAttachment) parts.push(t("list.attachment"));
  if (input.heldAttachment) parts.push(t("desk.row.heldAttachment"));
  if (input.warning) parts.push(input.warning);
  if (input.threadCount !== null && input.threadCount !== undefined) {
    parts.push(t("desk.row.thread", { n: input.threadCount }));
  }
  if (input.queued) parts.push(t("row.waitingToSend"));
  if (input.pending) parts.push(t("list.pending"));
  return parts.join(", ");
}

/**
 * What the row's name needs of a message.
 *
 * STRUCTURAL, NOT `EffectiveEmail`: every field here is one that type already
 * has, so a real row satisfies it — and a TEST does not have to build a
 * thirty-field store object to assert a sentence.
 */
export interface RowNameEmail {
  isSeen: boolean;
  isFlagged: boolean;
  isPinned: boolean;
  pending: boolean;
  returnedAt: string | null;
  receivedAt: string;
  from?: EmailAddress[] | null;
  subject?: string;
  deliveredTo?: string | null;
  hasAttachment?: boolean;
  attachmentScan?: AttachmentScan | null;
  senderWarnings?: SenderWarningRaw[] | null;
}

/**
 * The row's name for a real message — the hostile-text layer, the warning list
 * and the time format applied exactly as ui/desk/DeskRow.tsx applies them to what
 * it DRAWS, so the name and the row are two renderings of one set of facts rather
 * than two readings of the store.
 */
export function rowNameFor(
  email: RowNameEmail,
  ctx: {
    /** The area whose section this row was drawn in, or null. */
    areaName: string | null;
    /** The account whose dot this row draws, or null when it draws none. */
    accountName: string | null;
    threadCount: number | null;
    queued: boolean;
    now: number;
  },
): string {
  const identity = senderIdentity(email.from?.[0]);
  const [warning] = senderWarningsList(email.senderWarnings);
  return rowAccessibleName({
    unread: !email.isSeen,
    sender: senderText(identity, t("list.noSender")),
    subject: stripBidiControls(email.subject) || t("list.noSubject"),
    areaName: ctx.areaName,
    accountName: ctx.accountName,
    alias: aliasOf(email.deliveredTo),
    time: rowTime(email.receivedAt, ctx.now),
    flagged: email.isFlagged,
    pinned: email.isPinned,
    returned: email.returnedAt !== null,
    hasAttachment: email.hasAttachment === true,
    // A message is "held" when the download route is refusing at least one of its
    // parts. `quarantined` is the server's own list, never re-derived here — the
    // rule ui/attachmentScanPolicy.ts states at length.
    heldAttachment: (email.attachmentScan?.quarantined.length ?? 0) > 0,
    warning: warning ? warningText(warning) : null,
    threadCount: ctx.threadCount,
    queued: ctx.queued,
    pending: email.pending,
  });
}
