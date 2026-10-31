// SPDX-License-Identifier: AGPL-3.0-only

// THE EXPLICIT OPEN SIGNAL (#842).
//
// The priority model learns what matters to the user from what they do with
// their mail. Reading was the one thing it could not see: the client marks a
// message read when it opens it, the server cannot tell that from a bulk
// "mark all read", and for a long time it recorded every read as a NEGATIVE —
// so the model learned that read mail did not matter, and stopped marking
// anything important.
//
// This hook is the honest signal: a message counts as opened once it has been
// shown in the reading pane for OPEN_DWELL_MS, continuously, with the page
// visible. A flick past it while moving down the list — j, j, j — is not an
// open and reports nothing. Each message is reported at most once per session;
// the server also records it at most once per message (Triage/open).

import { useEffect, useRef } from "react";
import type { Id } from "../../core/jmap/types";

/**
 * How long a message must stay in the reading pane before it counts as read.
 * Four seconds: longer than moving the cursor through a list (a keypress every
 * few hundred milliseconds), shorter than reading the first lines of anything
 * a person actually reads.
 */
export const OPEN_DWELL_MS = 4000;

/** Ids already reported this session. Module-level so a remount (switching
 * between the desk and a list) does not report the same message again. */
const reported = new Set<Id>();

/**
 * Reports `openId` through `report` once it has been open for `dwellMs`.
 * Changing or clearing `openId` before then cancels it. A page that is hidden
 * when the dwell elapses does not count — nobody read it — and the message is
 * not marked reported, so opening it again later can still count.
 */
export function useOpenDwell(openId: Id | null, report: (id: Id) => void, dwellMs: number = OPEN_DWELL_MS): void {
  const reportRef = useRef(report);
  reportRef.current = report;
  useEffect(() => {
    if (!openId || reported.has(openId)) return;
    const timer = setTimeout(() => {
      if (reported.has(openId)) return;
      if (typeof document !== "undefined" && document.visibilityState === "hidden") return;
      reported.add(openId);
      reportRef.current(openId);
    }, dwellMs);
    return () => clearTimeout(timer);
  }, [openId, dwellMs]);
}

/** Test seam: forget what this session has reported. */
export function resetOpenDwellForTests(): void {
  reported.clear();
}
