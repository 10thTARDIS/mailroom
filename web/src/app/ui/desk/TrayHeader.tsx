// SPDX-License-Identifier: AGPL-3.0-only

// THE TRAY PAGE'S HEADER SENTENCE (#375 increment 4).
//
//   In-tray · all accounts · 12 · oldest waiting 9 d · synced 40 s ago
//
// The desk's header (DeskHeader) states the three numbers the desk is made of;
// this states the one the tray is made of, plus the nag that is the tray's
// whole reason for existing — how long the oldest thing has been waiting.
//
// It is a SECOND component rather than a mode of the first because the two
// sentences share only their shape: same live region, same trailing connection
// clause, entirely different middle. A `variant` prop would have made one
// function whose body is two sentences with an `if` between every clause.
//
// IT IS THE PAGE'S ONLY LIVE REGION, exactly as DeskHeader is the desk's, and
// for the same reason: everything else on screen is a row, and a reader who was
// announced each group heading as the list settled would be read a running
// commentary instead of an answer.
//
// WHEN THE WINDOW IS A BOUND, IT SAYS SO. The tray loads a generous page
// (sync.ts TRAY_PAGE), and past that the list on screen is a SAMPLE. Saying
// "showing the newest 200 of 312" is the difference between a bound and a lie
// (CLAUDE.md) — and it is what makes the "oldest waiting" clause honest, since
// that figure is computed over the rows that loaded, which on a bounded window
// are the NEWEST ones and therefore the least likely to hold the true oldest.

import type { JSX } from "react";

import type { HeaderState } from "../../../core/headerState";
import { t } from "../../i18n";
import { HeaderSentence } from "./HeaderSentence";
import { ScopeLine } from "./ScopeLine";
import { syncedAgoText, waitText } from "./format";

export function TrayHeader({
  scopeName,
  loading,
  count,
  total,
  oldestWaitMs,
  syncedSeconds,
  state,
  now,
  onRetry,
  onDetails,
  onScope,
  phone = false,
}: {
  /** The account the tray is scoped to, or null for every account. */
  scopeName: string | null;
  /** The first load has not come back yet: no numbers exist to state. */
  loading: boolean;
  /** How many rows are actually loaded. */
  count: number;
  /** What the server says the tray holds. */
  total: number;
  /** core/desk.ts trayStats — null for an empty tray, which is not zero. */
  oldestWaitMs: number | null;
  /** core/desk.ts syncedAgoSeconds, or null before the first round trip. */
  syncedSeconds: number | null;
  /** The connection's one truth, decided in core/headerState.ts — shared with
   * the desk so both pages say the same thing about the same failure. */
  state: HeaderState;
  now: number;
  onRetry: () => void;
  onDetails: () => void;
  /** Open the account switcher: on the phone the scope is its trigger. */
  onScope: () => void;
  /** The phone form (#375 increment 7) — the scope and the freshness, with the
   * tray's own numbers moving onto the list's group headings below, which is
   * where they are next to the rows they count. */
  phone?: boolean;
}): JSX.Element {
  const scope = scopeName ?? t("desk.header.allAccounts");
  const parts: string[] = [t("tray.title"), scope];

  if (loading) {
    parts.push(t("desk.header.loading"));
  } else {
    parts.push(String(total));
    if (count < total) parts.push(t("tray.bounded", { n: count, total }));
    if (oldestWaitMs !== null) parts.push(t("desk.tray.oldest", { wait: waitText(oldestWaitMs) }));
    // Dropped when something is wrong, exactly as on the desk: the state's own
    // clause says the same thing in the words that matter there.
    if (state.kind === "ok") parts.push(syncedAgoText(syncedSeconds));
  }

  if (phone) {
    return (
      <ScopeLine
        scope={scope}
        // The tray's own numbers ride along: unlike the desk's, they have no
        // strip and no tab counts to move onto, and "how many are waiting" is
        // the one thing this page exists to say.
        note={loading ? t("desk.header.loading") : parts.slice(2).join(" · ")}
        state={state}
        now={now}
        onScope={onScope}
        onRetry={onRetry}
        onDetails={onDetails}
      />
    );
  }

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
