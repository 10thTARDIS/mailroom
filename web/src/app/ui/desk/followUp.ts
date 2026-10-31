// SPDX-License-Identifier: AGPL-3.0-only

// THE WORDS A FOLLOW-UP NUDGE IS MADE OF (#58). Pure, like desk/format.ts:
// every sentence takes its `now`, so it can be tested at a fixed instant.
//
//   No one answered your mail to ann@example.org from Tue, Oct 7
//   You haven't replied to Ann in 4 days
//
// The first names a DAY, because what the user remembers is when they wrote;
// the second names a DURATION, because what matters is how long it has been
// waiting on them. Both are rounded DOWN (a nag that overstates itself is
// one people learn to discount — desk/format.ts waitText).

import type { FollowUpItem } from "../../../core/jmap/types";
import { t, tCount } from "../../i18n";

const DAY_MS = 24 * 60 * 60 * 1000;

/** "Tue, Oct 7" in the reader's own locale and time zone. */
export function followUpDay(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  return at.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric" });
}

/** Who the nudge is about, as a person reads it: the name when there is one. */
export function followUpWho(item: FollowUpItem): string {
  return item.who.name.trim() || item.who.email.trim();
}

/** The sentence that says why this message is here. */
export function followUpText(item: FollowUpItem, now: number): string {
  const who = followUpWho(item);
  if (item.kind === "outbound") {
    const day = followUpDay(item.since);
    return who ? t("followUp.outbound", { who, day }) : t("followUp.outboundNoWho", { day });
  }
  const since = new Date(item.since).getTime();
  const days = Number.isNaN(since) ? 0 : Math.max(0, Math.floor((now - since) / DAY_MS));
  return who ? tCount("followUp.inbound", days, { who }) : tCount("followUp.inboundNoWho", days);
}
