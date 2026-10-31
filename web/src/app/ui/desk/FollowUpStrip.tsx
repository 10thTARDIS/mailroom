// SPDX-License-Identifier: AGPL-3.0-only

// FOLLOW UP (#58): the nudges, on the desk, under the in-tray strip.
//
//   Follow up  2
//   No one answered your mail to ann@example.org from Tue, Oct 7   [Done] [Snooze…]
//     Plans for the weekend
//   You haven't replied to Bob in 4 days                          [Done] [Snooze…]
//     Re: Dinner
//
// WHY A DESK SECTION AND NOT A PAGE. The measured volume is low: well under one a week
// for "no one answered" and a little more for the opt-in "you haven't
// replied" (#58's report), and a nudge expires by itself after
// 30 days. A page of its own — a route, a nav entry, a list with grouping and
// a cursor like the tray's — would be built for a list that is almost always
// empty and never long. The desk is where the user decides what to do next,
// which is exactly the question a nudge raises, so the nudges sit there, the
// way the tray's strip does. NOTHING IS DRAWN WHEN NOTHING IS DUE: an empty
// "Follow up" heading on every visit would be furniture, and furniture is
// what people learn to stop seeing.
//
// EACH ROW IS THREE CONTROLS. The row itself opens the message. Its two
// actions sit in a group NAMED BY THE ROW (aria-labelledby the row's button),
// because "Done" on its own does not say which conversation it closes — axe
// does not require a group to have a name, so the a11y lane checks that one
// itself (scripts/a11y-check.cjs followUpSurfaces).

import type { JSX } from "react";

import type { FollowUpItem } from "../../../core/jmap/types";
import { t } from "../../i18n";
import { followUpText } from "./followUp";

export function FollowUpStrip({
  items,
  now,
  pending,
  onOpen,
  onDone,
  onSnooze,
}: {
  items: FollowUpItem[];
  now: number;
  /** Ids whose Done is in flight: their buttons are disabled meanwhile. */
  pending: ReadonlySet<string>;
  onOpen: (item: FollowUpItem) => void;
  onDone: (item: FollowUpItem) => void;
  onSnooze: (item: FollowUpItem) => void;
}): JSX.Element | null {
  if (items.length === 0) return null;
  return (
    <section className="desk-followup" aria-labelledby="desk-followup-title">
      <h2 className="desk-followup-title" id="desk-followup-title">
        {t("followUp.title")} <span className="desk-followup-count">{items.length}</span>
      </h2>
      <ul className="desk-followup-list">
        {items.map((item) => {
          const rowId = `followup-${item.emailId}`;
          const busy = pending.has(item.emailId);
          return (
            <li className={`desk-followup-item is-${item.kind}`} key={item.emailId}>
              <button
                type="button"
                className="desk-followup-open"
                id={rowId}
                onClick={() => onOpen(item)}
              >
                <span className="desk-followup-why">{followUpText(item, now)}</span>{" "}
                <span className="desk-followup-subject">{item.subject.trim() || t("followUp.noSubject")}</span>
              </button>
              <div className="desk-followup-actions" role="group" aria-labelledby={rowId}>
                {/* link-button, as the tray strip's "Open tray": the one button
                    style already measured at AAA on this sunken band. */}
                <button type="button" className="link-button" disabled={busy} onClick={() => onDone(item)}>
                  {t("followUp.done")}
                </button>
                <button
                  type="button"
                  className="link-button"
                  aria-haspopup="dialog"
                  disabled={busy}
                  onClick={() => onSnooze(item)}
                >
                  {t("followUp.snooze")}
                </button>
              </div>
            </li>
          );
        })}
      </ul>
    </section>
  );
}
