// SPDX-License-Identifier: AGPL-3.0-only

// THE THUMBNAIL STRIP'S CONTENT (#449, design pass-2 §9).
//
// EVERY attachment on the message, not the previewable ones. That is the change
// from the lightbox this replaces (#200), whose ring was the previewable subset
// so that paging could never land somewhere with nothing to show. The viewer has
// somewhere to show for all of them now — a no-preview panel with a Download in
// it, and, for a held file, the explanation of why it is held — so hiding the
// rest would mean the strip disagreed with the paperclips in the reading pane
// about how many files the message has.
//
// A HELD FILE IS MARKED IN THE STRIP, and that is the load-bearing line in this
// module: the mark is what makes "held" knowable BEFORE it is landed on, so a
// reader paging through six attachments is not surprised by a panel where a file
// should be. It is carried twice on purpose — as a flag the stylesheet draws a
// dashed outline from, and as the word `held` inside the accessible name — because
// colour and border are not available to a screen reader, and the WCAG 1.4.1 rule
// this repo follows is that no state is ever carried by appearance alone.
//
// Pure, and tested, for the reason kinds.ts gives: no jsdom in this repo, so the
// facts worth pinning have to be reachable without a renderer.

import type { AttachmentScan, EmailBodyPart } from "../../../core/jmap/types";
import { stripBidiControls } from "../../../core/hostileText";
import { t } from "../../i18n";
import { isWithheld } from "../attachmentScanPolicy";
import { kindGlyph } from "./kinds";

export interface StripItem {
  /** Position in the message's attachment list — what selecting this passes
   *  back, so the strip and the file area cannot disagree about which part is
   *  open. */
  index: number;
  /** React key: the blobId when there is one, else the position. */
  key: string;
  /** `jpg`, `pdf`, `zip`… — the kind word, never a filename (kinds.ts). */
  glyph: string;
  /** The file name as our own chrome may print it: bidi-stripped, and falling
   *  back to the type for a part that carried no name at all. */
  name: string;
  /** The scan gate would refuse these bytes right now. */
  held: boolean;
  /** This is the attachment the file area is showing. */
  current: boolean;
  /** The whole accessible name for the item's control — name, kind, and the
   *  word `held` when it is. */
  label: string;
}

export interface StripModel {
  items: StripItem[];
  /** `2 of 5` for the current attachment, or "" when the index names nothing
   *  (an empty message, or an index past the end — the viewer closes on both,
   *  and this must not invent a position for either). */
  position: string;
  /** Whether ←/→ have anywhere to go. */
  navigable: boolean;
}

/**
 * The strip, for one message's attachments and one current index.
 *
 * `scan` is the message's `attachmentScan` — null for a message with no
 * projected parts, which is NOT an all-clear (see the type's doc comment) and
 * simply means nothing is marked held.
 */
export function stripItems(
  parts: readonly EmailBodyPart[],
  scan: AttachmentScan | null | undefined,
  index: number,
): StripModel {
  const items = parts.map((part, i) => {
    const held = isWithheld(scan, part.partId);
    const name = stripBidiControls(part.name) || stripBidiControls(part.type);
    const glyph = kindGlyph(part.type, part.name);
    return {
      index: i,
      key: part.blobId ?? `part:${i}`,
      glyph,
      name,
      held,
      current: i === index,
      label: held
        ? t("viewer.strip.itemHeld", { name, kind: glyph })
        : t("viewer.strip.item", { name, kind: glyph }),
    };
  });
  const inRange = index >= 0 && index < parts.length;
  return {
    items,
    position: inRange ? t("attachment.counter", { n: index + 1, total: parts.length }) : "",
    navigable: parts.length > 1,
  };
}

/**
 * The index `←`/`→` land on. Wraps, as the lightbox's did: with a handful of
 * attachments, a "next" that stops dead at the end is a control that silently
 * stops working.
 */
export function stepIndex(index: number, delta: number, total: number): number {
  if (total < 1) return 0;
  return (((index + delta) % total) + total) % total;
}
