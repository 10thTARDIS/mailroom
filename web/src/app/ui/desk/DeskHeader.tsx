// SPDX-License-Identifier: AGPL-3.0-only

// THE HEADER SENTENCE (#375 increment 3).
//
//   Inbox · all accounts · 12 in tray · 6 to review · 37 in drawers · synced 40 s ago
//
// One line that states, in order: where you are, what it covers, and every
// number the screen below is about. It exists because a desk that shows a
// partition of your mail has to be able to say what the partition currently
// holds — otherwise "Everything else" is a promise the user has to verify by
// scrolling.
//
// IT IS THE ONLY LIVE REGION ON THE DESK. `role="status"` + `aria-live="polite"`
// here and nowhere else: a screen reader that announced every section heading,
// every count and every toast as they moved would be unusable, and this
// sentence already contains every number the others would have announced.
//
// AND IT IS NEVER ABSENT. Rendering an empty list — or the word "Nothing" —
// with no sentence above it leaves the reader unable to tell "there is no mail"
// from "we could not ask": the offline and cannot-reach cases append their own
// clause to THIS line rather than replacing the screen, so the numbers stay on
// screen next to the reason they may be stale.

import type { JSX } from "react";

import type { HeaderState } from "../../../core/headerState";
import { t } from "../../i18n";
import { HeaderSentence } from "./HeaderSentence";
import { ScopeLine } from "./ScopeLine";
import { syncedAgoText } from "./format";

export function DeskHeader({
  scopeName,
  loading,
  trayCount,
  reviewCount,
  drawerCount,
  syncedSeconds,
  state,
  now,
  onRetry,
  onDetails,
  onScope,
  phone = false,
}: {
  /** The account the desk is scoped to, or null for every account. */
  scopeName: string | null;
  /** The first load has not come back yet: no numbers exist to state. */
  loading: boolean;
  trayCount: number;
  reviewCount: number;
  drawerCount: number;
  /** core/desk.ts syncedAgoSeconds, or null before the first round trip. */
  syncedSeconds: number | null;
  /** Offline, stale, an error, or fine — core/headerState.ts, which is where
   * the precedence between those is decided (#375 increment 6). It replaced two
   * booleans that each described one symptom and could both be true. */
  state: HeaderState;
  now: number;
  onRetry: () => void;
  onDetails: () => void;
  /** Open the account switcher — on the phone the scope IS the switcher's
   * trigger, because the rail that normally holds it is a pane away. */
  onScope: () => void;
  /**
   * The phone form (#375 increment 7): the scope with a `▾` on one line and
   * how long ago it synced under it, instead of one sentence of six clauses.
   *
   * NO NUMBER IS LOST, it moves. "12 in tray" becomes the in-tray line
   * directly below (TrayStrip's phone form), and "6 to review · 37 in
   * drawers" becomes the two counts on the segmented control below that — both
   * of them next to the thing they are a count OF, which is the trade a
   * four-inch screen makes for the one-line summary a monitor has room for.
   */
  phone?: boolean;
}): JSX.Element {
  const scope = scopeName ?? t("desk.header.allAccounts");
  const parts: string[] = [t("desk.header.inbox"), scope];

  if (loading) {
    parts.push(t("desk.header.loading"));
  } else {
    parts.push(t("desk.header.inTray", { n: trayCount }));
    parts.push(t("desk.header.toReview", { n: reviewCount }));
    parts.push(t("desk.header.inDrawers", { n: drawerCount }));
    // ONLY WHEN NOTHING IS WRONG. Offline states this better — "last synced
    // 14 min ago", the same fact with the word that makes it matter — and
    // printing both would put "synced 14 min ago · Offline… · last synced
    // 14 min ago" in one sentence.
    if (state.kind === "ok") parts.push(syncedAgoText(syncedSeconds));
  }

  if (phone) {
    return (
      <ScopeLine
        scope={scope}
        // Only when nothing is wrong, exactly as above: the warning sentence
        // states the same fact in the words that make it matter, and printing
        // both would say "synced 14 min ago" over "last synced 14 min ago".
        note={loading ? t("desk.header.loading") : syncedAgoText(syncedSeconds)}
        state={state}
        now={now}
        onScope={onScope}
        onRetry={onRetry}
        onDetails={onDetails}
      />
    );
  }

  // The connection clause comes LAST, after the numbers, because it is about
  // the numbers: "…37 in drawers · Offline. Showing what was loaded" reads as
  // the caveat it is. Which clause that is, and which one wins when more than
  // one is true, is HeaderSentence's job now.
  return (
    <HeaderSentence
      parts={parts}
      state={state}
      now={now}
      onRetry={onRetry}
      onDetails={onDetails}
    />
  );
}
