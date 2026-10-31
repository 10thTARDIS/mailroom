// SPDX-License-Identifier: AGPL-3.0-only

// WHICH PARTS THE READING PANE LISTS UNDER A MESSAGE (#675).
//
// RFC 8621's body algorithm (internal/api/body.go) puts a cid-referenced image
// in `htmlBody`, not in `attachments`, because the body is where it shows. That
// is right only while the body actually DOES show it, and two cases broke the
// assumption silently:
//
//   - A HELD inline image. The quarantine refuses its bytes (ADR-027 §8), the
//     cid: reference is stripped from the frame, and since it was never in
//     `attachments` it got no withheld chip either. The reader saw a gap, or
//     nothing, and had no way to learn something was held or to release it.
//     Encrypted mail makes this common: every part is unscannable (#405).
//   - An image part with a Content-ID that the HTML NEVER REFERENCES. It is in
//     `htmlBody` because it has a Content-ID, it is not displayed because nothing
//     points at it, and it is in no list — so it appeared nowhere at all.
//
// So the pane lists `attachments` plus every inline media part the body will
// not display. A held part is listed whether or not the body references it;
// that does not depend on the body text, so it holds before the body has
// loaded. "Unreferenced" does depend on it, and is only decided once the body
// values are in — before that, every inline image would read as unreferenced
// and flash into the list for one render.
//
// The viewer pages through the SAME list (Viewer.tsx), so a chip's index and
// the viewer's index name the same part.
//
// Nothing here weakens the quarantine. Listing a held part renders the withheld
// chip, which offers no preview and no download (ThreadPane's AttachmentList
// returns before either); the viewer opens onto the held panel (kinds.ts checks
// the gate first); and HtmlBody no longer even asks for its bytes
// (fetchableInlineRefs). The server's gate (internal/api/blob.go) is unchanged
// and is still what actually refuses them.

import type { AttachmentScan, EmailBodyPart, EmailBodyValue } from "../../core/jmap/types";
import { htmlToRender } from "./bodyChoice";
import { isWithheld } from "./attachmentScanPolicy";
import { normalizeCid, referencedCids } from "./inlineImages";

/** What listedParts reads off an Email. */
export interface PartsSource {
  attachments?: EmailBodyPart[];
  htmlBody?: EmailBodyPart[];
  textBody?: EmailBodyPart[];
  bodyValues?: Record<string, EmailBodyValue>;
  attachmentScan?: AttachmentScan | null;
}

/** Inline MEDIA: the only part types body.go puts in a body list other than
 * text (isBodyPart), and the only ones a reader can lose track of. */
function isInlineMedia(p: EmailBodyPart): boolean {
  return /^(image|audio|video)\//i.test(p.type);
}

/** The HTML the reading pane renders — the same join ThreadPane's bodyText
 * makes of `htmlBody`, so "referenced" means referenced by what is on screen. An
 * empty string means the pane shows the plain-text body, which references no
 * cid at all. */
function renderedHtml(src: PartsSource): string {
  // Same rule as the pane (#871): a plain-only message renders no HTML.
  return htmlToRender(src);
}

/**
 * Every part the reading pane lists for this message: its attachments, then
 * each inline media part the body will not display — held by the quarantine,
 * or (once the body is loaded) referenced by no `cid:` in the rendered HTML.
 * Each part appears once, keyed by partId.
 */
export function listedParts(src: PartsSource): EmailBodyPart[] {
  const attachments = src.attachments ?? [];
  const seen = new Set<string>();
  for (const a of attachments) seen.add(a.partId ?? `blob:${a.blobId ?? ""}`);

  const bodyLoaded = src.bodyValues !== undefined;
  const shown = new Set(referencedCids(renderedHtml(src)));
  const scan = src.attachmentScan;

  const extra: EmailBodyPart[] = [];
  for (const p of [...(src.htmlBody ?? []), ...(src.textBody ?? [])]) {
    if (!p.blobId || !isInlineMedia(p)) continue;
    const key = p.partId ?? `blob:${p.blobId}`;
    if (seen.has(key)) continue;
    const held = isWithheld(scan, p.partId);
    const referenced = p.cid !== null && shown.has(normalizeCid(p.cid));
    if (held || (bodyLoaded && !referenced)) {
      seen.add(key);
      extra.push(p);
    }
  }
  return extra.length > 0 ? attachments.concat(extra) : attachments;
}
