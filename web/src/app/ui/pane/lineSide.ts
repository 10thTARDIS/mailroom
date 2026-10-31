// SPDX-License-Identifier: AGPL-3.0-only

// THE SENTENCE FOR Email.lineSide (urn:mailroom:desk, #464).
//
// `lineSide` is the OVERRIDE, not the effective side (see the server's own
// warning in internal/api/line_side_email.go and core/jmap/types.ts): `null`
// means the score still decides, not "surface". So the only thing this file
// says is "you placed this yourself" — never a verdict about where the
// message currently renders, which the desk's `onSurface` filter already
// answers and this must not re-derive.
//
// Pure and tested on purpose, like fold.ts beside it: a one-line sentence is
// exactly the kind of thing that is easy to swap "surface" and "drawer" in
// without a test noticing.

import type { MessageKey } from "../../i18n";

/**
 * The message key for the sentence to show, or null to say nothing at all.
 *
 * `undefined` (an older server, or a fetch that never asked for the
 * property) and `null` (asked, and the user has not placed this one) both
 * read as "nothing to say" — the two are indistinguishable to a reader and
 * must render identically.
 */
export function lineSideSentence(
  lineSide: "surface" | "drawer" | null | undefined,
): MessageKey | null {
  if (lineSide === "surface") return "pane.lineSide.surface";
  if (lineSide === "drawer") return "pane.lineSide.drawer";
  return null;
}
