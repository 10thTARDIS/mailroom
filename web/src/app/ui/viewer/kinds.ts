// SPDX-License-Identifier: AGPL-3.0-only

// WHAT THE VIEWER SHOWS FOR ONE ATTACHMENT (#449, design pass-2 §9), and what a
// HELD file shows instead of itself.
//
// The decisions live here rather than in Viewer.tsx for the reason
// attachmentPreviewPolicy.ts and attachmentScanPolicy.ts both give: this repo's
// web tests are pure-function tests (no jsdom, no React Testing Library), so
// anything a mistake in would be a security or safety bug has to be reachable
// without a renderer. Three such decisions are in this file:
//
//   viewerMode      which of the six things the file area renders — and the
//                   ORDER of its checks is the safety property, because a held
//                   file must never fall through to a renderer;
//   heldPanelSpec   which button is the PRIMARY one, and whether releasing asks
//                   again, for each of the two held cases (ADR-027 §8: the
//                   language never accuses when nothing has actually looked at
//                   the file, and a flagged file is never one click from open);
//   kindGlyph       the three-or-four letters the thumbnail strip prints, which
//                   are derived from a SENDER-CONTROLLED filename and therefore
//                   have to be narrowed to something that cannot carry a
//                   sentence, a direction change or a lie.

import type { AttachmentScan, AttachmentScanPart, EmailBodyPart } from "../../../core/jmap/types";
import type { MessageKey } from "../../i18n";
import { t } from "../../i18n";
import { previewKind } from "../attachmentPreviewPolicy";
import { isWithheld, scanStateFor } from "../attachmentScanPolicy";

/**
 * What the file area of the viewer renders for the current attachment.
 *
 * Six values, not "previewable or not": every one of them is a different thing
 * on screen with different controls beside it (Fit/100% for an image, Fit width
 * for a PDF, a Download-only panel for an .xlsx, a release decision for a held
 * file), and the two HELD cases are separate because they are separate findings
 * — "the scanner believes this is harmful" and "nothing has checked this" are
 * not the same sentence and must not share one.
 */
export type ViewerMode =
  | "image"
  | "pdf"
  | "text"
  | "nopreview"
  | "held-unscanned"
  | "held-flagged";

/**
 * Which of those six this part is.
 *
 * ⚠ THE WITHHELD CHECK COMES FIRST AND IS EXCLUSIVE OF THE REST. A held part
 * must never reach a renderer: the download and preview routes both answer 403
 * for it (internal/api/blob.go), so a "held image" that fell through to the image
 * branch would fetch nothing, render nothing and say nothing — a blank pane where
 * an explanation belongs. The order of these two checks IS the feature; it is what
 * probe (1) in the issue deliberately breaks.
 *
 * `isWithheld` reads the server's own `quarantined` list rather than
 * re-deriving "blocked" from `state`/`released` — see its doc comment for why
 * that must never be reimplemented here.
 *
 * An unrecognised `state` that the server nevertheless quarantined lands on
 * `held-unscanned`, i.e. on the panel that does NOT accuse. If Mailroom ever
 * grows a third held state, the failure mode of this line is telling a user
 * nothing has checked their file — never telling them it is malware.
 */
export function viewerMode(
  part: EmailBodyPart,
  scan: AttachmentScan | null | undefined,
): ViewerMode {
  if (isWithheld(scan, part.partId)) {
    return scanStateFor(scan, part.partId)?.state === "flagged"
      ? "held-flagged"
      : "held-unscanned";
  }
  // No blobId means no bytes to ask the server for (a multipart container); it is
  // not renderable and it is not held, so it is the same dead end as an .xlsx.
  if (!part.blobId) return "nopreview";
  const kind = previewKind(part.type);
  if (kind === "image") return "image";
  if (kind === "pdf") return "pdf";
  if (kind === "text") return "text";
  return "nopreview";
}

/** Whether this mode is one of the two held panels — one check, so a caller
 *  cannot enumerate the held cases and miss one. */
export function isHeldMode(mode: ViewerMode): boolean {
  return mode === "held-flagged" || mode === "held-unscanned";
}

/** Which of the two buttons the held panel leads with. */
export type HeldAction = "release" | "keep";

/**
 * The shape of the held panel for one scan state: its tone, its two sentences,
 * which button is PRIMARY, and whether the release asks once more.
 *
 * ⚠ THE FLAGGED CASE'S PRIMARY IS `keep`, AND THAT IS THE WHOLE POINT OF THIS
 * FUNCTION EXISTING. Releasing a flagged file is a real risk to the device it
 * gets opened on, so the emphasised, focused, muscle-memory answer must be the
 * safe one, and the release is a second, plainer control that then asks again.
 * The could-not-be-scanned case is the opposite: nothing has been found, the
 * usual cause is a password-protected archive, and making the user hunt for the
 * way to open their own file would be the app treating "unchecked" as "guilty"
 * (ADR-027 §8, which is also why neither sentence below calls the file
 * dangerous unless the scanner actually said so).
 *
 * Returned as DATA rather than decided in the JSX because it is exactly what a
 * test can pin without a renderer — probe (3) in the issue makes the flagged
 * primary a release and this is what goes red.
 */
export interface HeldPanelSpec {
  mode: "held-flagged" | "held-unscanned";
  /** `warn` is the scanner's verdict; `held` is "waiting on your decision". */
  tone: "warn" | "held";
  titleKey: MessageKey;
  bodyKey: MessageKey;
  primary: HeldAction;
  secondary: HeldAction;
  /** The label on the control that releases, which differs BY WEIGHT: one
   *  says what you get, the other says what you are accepting. */
  releaseKey: MessageKey;
  /** Whether the release goes through a second confirmation. */
  asksAgain: boolean;
}

export function heldPanelSpec(state: string | undefined): HeldPanelSpec {
  if (state === "flagged") {
    return {
      mode: "held-flagged",
      tone: "warn",
      titleKey: "viewer.held.flagged.title",
      bodyKey: "viewer.held.flagged.body",
      primary: "keep",
      secondary: "release",
      releaseKey: "viewer.held.flagged.release",
      asksAgain: true,
    };
  }
  return {
    mode: "held-unscanned",
    tone: "held",
    titleKey: "viewer.held.unscanned.title",
    bodyKey: "viewer.held.unscanned.body",
    primary: "release",
    secondary: "keep",
    releaseKey: "viewer.held.unscanned.release",
    asksAgain: false,
  };
}

/**
 * The mark a released part keeps wearing — `released by you · 12 Aug 2026` — or
 * null for a part nobody released.
 *
 * IT OUTLIVES THE HOLD, deliberately. Once released, the part stops being in
 * `quarantined` and renders like any other file; without this the only trace of
 * a decision about malware would be the absence of a chip. The server keeps
 * reporting `released` even when a re-scan comes back `clean` for the same
 * reason (internal/api/attachment_scan.go).
 *
 * The date is dropped rather than invented when `releasedAt` is missing (a
 * server older than #484) or unparseable: "released by you" is true either way,
 * and "released by you · Invalid Date" is the kind of line that gets screenshotted.
 * Day-then-month with a locale month name, matching desk/format.ts rowTime — the
 * ORDER is fixed on purpose there and this is the same fact in a wider column.
 */
export function releasedNote(part: AttachmentScanPart | null | undefined): string | null {
  if (!part?.released) return null;
  const iso = part.releasedAt;
  if (!iso) return t("viewer.released");
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return t("viewer.released");
  const month = d.toLocaleDateString(undefined, { month: "short" });
  return t("viewer.releasedOn", { date: `${d.getDate()} ${month} ${d.getFullYear()}` });
}

/** Extensions whose file's own name is not the word people know it by. */
const GLYPH_ALIASES: Readonly<Record<string, string>> = {
  jpeg: "jpg",
  tiff: "tif",
  htm: "html",
  text: "txt",
  markdown: "md",
};

/** When the name says nothing, the SERVER'S type does. Keyed on the full type
 *  rather than the subtype so that `application/vnd.…sheet` gets `xlsx` instead
 *  of forty characters of vendor tree. */
const GLYPH_TYPES: Readonly<Record<string, string>> = {
  "application/pdf": "pdf",
  "application/zip": "zip",
  "application/gzip": "gz",
  "application/x-tar": "tar",
  "application/rtf": "rtf",
  "application/json": "json",
  "application/msword": "doc",
  "application/vnd.openxmlformats-officedocument.wordprocessingml.document": "docx",
  "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": "xlsx",
  "application/vnd.openxmlformats-officedocument.presentationml.presentation": "pptx",
  "application/vnd.ms-excel": "xls",
  "application/vnd.ms-powerpoint": "ppt",
  "application/vnd.oasis.opendocument.text": "odt",
  "application/vnd.oasis.opendocument.spreadsheet": "ods",
  "application/vnd.oasis.opendocument.presentation": "odp",
  "message/rfc822": "eml",
  "text/plain": "txt",
  "text/calendar": "ics",
};

/** The hard cap on a glyph, in characters. Four fits `docx`/`xlsx`/`pptx`, and
 *  anything longer is not a glyph — it is a filename in a 40px box. */
const GLYPH_MAX = 4;

/**
 * The little kind word the strip prints for a part: `jpg`, `pdf`, `zip`, `docx`.
 *
 * ⚠ THE NAME IS SENDER-CONTROLLED TEXT AND THIS IS OUR OWN CHROME. An extension
 * taken raw off `invoice.pdf<U+202E>exe` would put a direction override in a
 * 40-pixel box beside every other one; `report.this-is-not-really-an-extension`
 * would put a sentence there. So the extension is accepted only if it is already
 * exactly what a glyph can be — ASCII letters and digits, at most four of them —
 * and otherwise the SERVER'S type decides, which is a value the sender does not
 * choose (blob.go picks it off its own allowlist; see attachmentPreviewPolicy.ts).
 * That is the same "the type is the server's, not the part's" rule the preview
 * itself runs on, applied to a label rather than to a renderer.
 *
 * The declared type is used ONLY here, for a word; nothing is rendered from it.
 */
export function kindGlyph(type: string | null | undefined, name: string | null | undefined): string {
  const ext = extensionOf(name);
  if (ext) return GLYPH_ALIASES[ext] ?? ext;

  const media = (type ?? "").split(";")[0]?.trim().toLowerCase() ?? "";
  const known = GLYPH_TYPES[media];
  if (known) return known;

  // Fall back to the SUBTYPE when it is glyph-shaped on its own (`image/png` →
  // `png`, `image/webp` → `webp`), which covers every raster type the preview
  // allowlist names without listing them twice.
  const subtype = media.split("/")[1] ?? "";
  const clean = glyphShaped(subtype.replace(/^x-/, ""));
  if (clean) return GLYPH_ALIASES[clean] ?? clean;

  // `application/octet-stream` lands here, and so does a part with no type at
  // all: the bucket blob.go downgrades every scripting context into. It is
  // honestly "a file" and nothing more.
  return t("viewer.kind.file");
}

/** The last dot-segment of a filename, lowercased — only when it is already
 *  glyph-shaped. `archive.tar.gz` gives `gz`, which is the word people read. */
function extensionOf(name: string | null | undefined): string | null {
  const dot = (name ?? "").lastIndexOf(".");
  if (dot < 1) return null;
  return glyphShaped((name ?? "").slice(dot + 1));
}

/** `s` lowercased if it is ASCII alphanumeric and at most GLYPH_MAX long, else
 *  null. Deliberately a WHOLE-STRING test rather than a strip-and-truncate: a
 *  truncation would turn hostile text into a plausible-looking glyph, whereas a
 *  refusal falls back to a value the sender does not control. */
function glyphShaped(s: string): string | null {
  const lower = s.toLowerCase();
  if (lower.length === 0 || lower.length > GLYPH_MAX) return null;
  return /^[a-z0-9]+$/.test(lower) ? lower : null;
}
