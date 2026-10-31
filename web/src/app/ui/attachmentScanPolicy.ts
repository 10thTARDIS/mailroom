// SPDX-License-Identifier: AGPL-3.0-only

// What the WITHHELD-attachment chip needs to decide (#396/#412), split out of
// the JSX for the same reason attachmentPreviewPolicy.ts is: this repo's web
// tests are all pure-function tests (no React Testing Library / jsdom is
// installed — see attachmentPreviewPolicy.test.ts's own note), so the
// load-bearing decisions have to live somewhere a test can reach them without a
// renderer. AttachmentList in ThreadPane.tsx just prints whatever this returns.

import type { AttachmentScan, AttachmentScanPart } from "../../core/jmap/types";
import type { MessageKey } from "../i18n";

/** The scan record for one part, or null when this message carries no
 * projected-parts scan at all (`attachmentScan` is null) or this partId is not
 * one of its projected parts. */
export function scanStateFor(
  scan: AttachmentScan | null | undefined,
  partId: string | null,
): AttachmentScanPart | null {
  if (!scan || !partId) return null;
  return scan.parts[partId] ?? null;
}

/**
 * Whether the download route will refuse this part right now.
 *
 * ⚠ MEMBERSHIP IN `quarantined`, NOT A CLIENT-SIDE RE-DERIVATION FROM
 * `state`/`released`. The server computes that list from the exact same
 * `attachscan.Blocked` rule the download gate itself calls
 * (internal/api/attachment_scan.go) — reimplementing "blocked" here from the
 * two fields would agree today and silently diverge the moment that rule
 * changes, which is exactly the kind of two-readings-of-one-decision bug
 * `quarantined` exists to prevent.
 */
export function isWithheld(scan: AttachmentScan | null | undefined, partId: string | null): boolean {
  if (!scan || !partId) return false;
  return scan.quarantined.includes(partId);
}

/**
 * The i18n key for WHY a withheld part was withheld.
 *
 * Only `"flagged"` and `"unknown"` ever reach `quarantined` in practice —
 * `attachscan.Blocked` admits no other status — but an unrecognised value
 * still gets a true, generic sentence rather than nothing, so a future scan
 * state does not ship a withheld chip with no explanation on it at all.
 *
 * The two real cases are deliberately DIFFERENT sentences, not one generic
 * "withheld" message: "flagged" is a verdict and "unknown" is the scanner
 * failing to reach one, and conflating them would tell a user the scanner
 * caught something it never actually looked at.
 */
export function withheldReasonKey(state: string | undefined): MessageKey {
  switch (state) {
    case "flagged":
      return "attachment.withheldReason.flagged";
    case "unknown":
      return "attachment.withheldReason.unknown";
    default:
      return "attachment.withheldReason.generic";
  }
}
