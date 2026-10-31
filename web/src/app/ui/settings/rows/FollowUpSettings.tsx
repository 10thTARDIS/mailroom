// SPDX-License-Identifier: AGPL-3.0-only

// FOLLOW-UP REMINDERS (#58), on the in-tray settings: the tray is the user's
// "come back to this", and follow-ups are the desk saying "come back to this"
// on their behalf.
//
// Two checkboxes and one delay, written as ONE followUp object (the server
// wants the whole object on every write, like quiet hours):
//   * "no one answered my mail" — default ON;
//   * "mail I haven't replied to" — default OFF, opt-in, and the sentence under
//     it says why (#58's measured precision: about one in six);
//   * the delay for the second, 1-14 days, default 3 — only while it is on.
//
// Labels wrap their inputs, so every control is named by its visible words
// (WCAG 2.5.3), with no aria-label to drift from them. NOT SHOWN against a
// server that does not send `followUp` (before #58).

import { useState } from "react";
import type { JSX } from "react";

import type { FollowUpSettings as Settings } from "../../../../core/jmap/types";
import { t, tCount } from "../../../i18n";
import { useApp, useMailState } from "../../../state";

/** The delays offered. The server accepts 1-14. */
export const FOLLOW_UP_DAY_CHOICES = [1, 2, 3, 5, 7, 14] as const;

export function FollowUpSettings(): JSX.Element | null {
  const { actions } = useApp();
  const current = useMailState().deskPreferences?.followUp;
  const [busy, setBusy] = useState(false);
  if (!current) return null;

  const write = (next: Settings): void => {
    setBusy(true);
    // From the server's answer, not optimistically: updateDeskPreferences
    // re-reads the record and the controls follow it.
    void actions
      .updateDeskPreferences({ followUp: next }, t("settings.followUp.failed"))
      .finally(() => setBusy(false));
  };
  const days = FOLLOW_UP_DAY_CHOICES.includes(current.inboundDays as (typeof FOLLOW_UP_DAY_CHOICES)[number])
    ? FOLLOW_UP_DAY_CHOICES
    : [...FOLLOW_UP_DAY_CHOICES, current.inboundDays].sort((a, b) => a - b);

  return (
    <fieldset className="menu-group follow-up-settings">
      <legend>{t("settings.followUp.title")}</legend>
      <label className="checkbox">
        <input
          type="checkbox"
          checked={current.outbound}
          disabled={busy}
          onChange={() => write({ ...current, outbound: !current.outbound })}
        />
        {t("settings.followUp.outbound")}
      </label>
      <label className="checkbox">
        <input
          type="checkbox"
          checked={current.inbound}
          disabled={busy}
          onChange={() => write({ ...current, inbound: !current.inbound })}
        />
        {t("settings.followUp.inbound")}
      </label>
      <p className="field-hint">{t("settings.followUp.inboundExplain")}</p>
      {current.inbound ? (
        <label className="field">
          <span className="field-label">{t("settings.followUp.inboundDays")}</span>
          <select
            className="text-input"
            value={current.inboundDays}
            disabled={busy}
            onChange={(e) => write({ ...current, inboundDays: Number(e.target.value) })}
          >
            {days.map((n) => (
              <option key={n} value={n}>
                {tCount("settings.followUp.days", n)}
              </option>
            ))}
          </select>
        </label>
      ) : null}
      <p className="field-hint">{t("settings.followUp.explain")}</p>
    </fieldset>
  );
}
