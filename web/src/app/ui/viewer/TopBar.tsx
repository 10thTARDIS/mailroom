// SPDX-License-Identifier: AGPL-3.0-only

// THE VIEWER'S TOP BAR (#449, design pass-2 §9): what this file IS, and the four
// things you can do with it.
//
// WORDS, NOT GLYPHS, and no shortcut printed on any of them. The desk settled that
// argument for this app (ui/desk/SelectionToolbar.tsx): a row of symbols is a
// puzzle, and a key name on a button is documentation in the wrong place — the
// shortcuts sheet is where keys are written down.
//
// WHAT IS DELIBERATELY ABSENT:
//
//   * A PAGE COUNT for a PDF. The design asks for one "if the browser exposes it";
//     it does not. The framed document has an opaque origin (docs/hostile-content.md
//     §2.4a), so nothing on this side of the boundary can count its pages, and the
//     honest answer is to omit the field rather than to print a guess.
//   * FIND IN FILE, and a text view for PDFs. Both need the server's attachment
//     text extraction, which exists for the search index and is not exposed to
//     this client at all. A search box that searched nothing would be worse than
//     no search box; the follow-up is to expose the extracted text through the
//     API, and then this bar gets a field.

import type { JSX } from "react";

import type { AttachmentScanPart } from "../../../core/jmap/types";
import { t } from "../../i18n";
import { GuardedBlobLink } from "../blobGuard";
import type { BlobGuard } from "../blobGuard";
import { releasedNote } from "./kinds";

export function ViewerTopBar({
  name,
  sizeText,
  dimensions,
  sender,
  scanPart,
  held,
  downloadHref,
  downloadName,
  tabHref,
  guard,
  onForward,
  onClose,
}: {
  /** Already bidi-stripped by the caller. */
  name: string;
  sizeText: string;
  /** `1600 × 1200`, once an image has decoded. Null for everything else. */
  dimensions: string | null;
  /** `from <Sender>` — the sender's own text, already bidi-stripped. */
  sender: string;
  /** This part's scan record, for the `released by you · <date>` mark. */
  scanPart: AttachmentScanPart | null;
  /** While held, the file cannot be saved, opened or forwarded — so those three
   *  controls are not rendered rather than rendered and failing at the server. */
  held: boolean;
  downloadHref: string | null;
  downloadName: string;
  /** The URL a new tab can actually SHOW this at, or null when there is none. */
  tabHref: string | null;
  /** Checks a download or new-tab URL before navigating to it (#47). */
  guard: BlobGuard;
  onForward: () => void;
  onClose: () => void;
}): JSX.Element {
  const released = releasedNote(scanPart);
  return (
    <header className="viewer-topbar">
      <div className="viewer-facts">
        {/* The filename is sender-authored text in our own chrome, stripped by the
            caller (core/hostileText.ts) and isolated by the stylesheet. */}
        <span className="viewer-name">{name}</span>
        <span className="viewer-fact">{sizeText}</span>
        {dimensions ? <span className="viewer-fact">{dimensions}</span> : null}
        <span className="viewer-fact">{t("viewer.from", { sender })}</span>
        {/* The mark a release leaves, in the viewer as well as on the tile: it is a
            fact about the user's own decision and it outlives the hold. */}
        {released ? <span className="viewer-released">{released}</span> : null}
      </div>
      <div className="viewer-actions">
        {/* HELD MEANS ALL THREE ARE GONE, not disabled: the download route answers
            403, the preview route answers 403, and the draft create refuses a
            quarantined blobId outright (internal/api/draft.go). A control that
            exists and fails is a worse answer than the panel below, which says
            what is going on and offers the release. */}
        {!held && downloadHref ? (
          <GuardedBlobLink
            guard={guard}
            className="button"
            href={downloadHref}
            download={downloadName}
            ariaLabel={t("attachment.downloadNamed", { name })}
          >
            {t("attachment.download")}
          </GuardedBlobLink>
        ) : null}
        {!held ? (
          <button type="button" className="button" onClick={onForward}>
            {t("viewer.forward")}
          </button>
        ) : null}
        {!held && tabHref ? (
          // `rel="noopener"` so the opened document gets no `window.opener` handle
          // back into this app — belt and braces beside the response's own
          // `CSP: sandbox`, which already makes it an opaque origin.
          <GuardedBlobLink guard={guard} className="button" href={tabHref} newTab>
            {t("viewer.openInTab")}
          </GuardedBlobLink>
        ) : null}
        <button type="button" className="button" onClick={onClose}>
          {t("action.close")}
        </button>
      </div>
    </header>
  );
}
