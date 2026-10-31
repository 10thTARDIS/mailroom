// SPDX-License-Identifier: AGPL-3.0-only

// IN-TRAY (#374 increment 3) — one number, and one sentence that is the honest
// half of the feature.
//
// The tray is the flag: `#/tray` IS the starred view (core/routes.ts), and a
// message is in the tray because the user put it there. `trayNagDays` is a
// per-login number this row reads and writes.
//
// ⚠ NOTHING READS IT BACK YET. The strip's "over a week" nudge
// (ui/desk/TrayStrip.tsx) comes from a fixed `WEEK_MS` (core/desk.ts), not from
// this setting — so today the field records an intention the tray strip does
// not act on.
//
// ⚠ THE SENTENCE IS TRUE TODAY AND IS NOT A PLACEHOLDER. Returned snoozes and
// bounces are not routed into the tray — a snooze that wakes comes back to the
// list it left, and a bounce is a delivery failure on the account's health card.
// A user who expected the tray to collect them would otherwise conclude it was
// LOSING things, which is the worst thing a holding area can be suspected of. The
// state line says it too (SettingsRoute.tsx trayFacts), and it is repeated here
// because this is where someone comes looking for the setting that would change it.
//
// NOTHING ELSE OF THE TRAY'S IS HERE, deliberately. The tray's other behaviours —
// what goes in it, how the strip groups by wait — are the desk's, and a second
// copy of them on this page would be a second answer to the same question. The
// follow-up reminders (#58, rows/FollowUpSettings.tsx) sit at the foot of this
// row because they are the desk's own "come back to this", not the tray's.

import { useEffect, useState } from "react";
import type { JSX } from "react";

import { t, tCount } from "../../../i18n";
import { useApp, useMailState } from "../../../state";
import { FollowUpSettings } from "./FollowUpSettings";

/** The nag a FRESH login starts on, from `db/schema.sql` (`tray_nag_days int NOT
 * NULL DEFAULT 7`). Hard-coded for `LINE_DEFAULT_THRESHOLD`'s reason: no method
 * asks a server what a new user's value would have been, and this is only ever
 * the reset target and the thing `changed` is measured against. */
export const TRAY_DEFAULT_NAG_DAYS = 7;

export function TrayBody(): JSX.Element {
  const { actions } = useApp();
  const state = useMailState();
  const prefs = state.deskPreferences;
  const [draft, setDraft] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // The field follows the SERVER's value whenever it moves — another device, or
  // this one's own write landing. Without this the input would keep showing what
  // was typed here while the rest of the app used the value that arrived.
  const stored = prefs?.trayNagDays ?? null;
  useEffect(() => {
    setDraft(null);
  }, [stored]);

  if (stored === null) {
    // Not "7": a number this client has not read is not the default, it is unread,
    // and a field pre-filled with a guess is a field that writes the guess back.
    return <p className="field-hint">{t("settings.tray.unread")}</p>;
  }

  const shown = draft ?? String(stored);
  const changed = stored !== TRAY_DEFAULT_NAG_DAYS;

  const write = (n: number): void => {
    if (!Number.isFinite(n) || n <= 0) return;
    const days = Math.floor(n);
    if (days === stored) {
      setDraft(null);
      return;
    }
    setBusy(true);
    void (async () => {
      const ok = await actions.updateDeskPreferences(
        { trayNagDays: days },
        t("settings.tray.failed"),
      );
      setBusy(false);
      // The draft is dropped only on success, so a refused write leaves the number
      // the user typed in front of them rather than reverting it silently.
      if (ok) setDraft(null);
    })();
  };

  return (
    <>
      <fieldset className="menu-group">
        <legend>
          {t("settings.tray.nag")}
          {changed ? (
            <span className="setting-changed">
              <span className="setting-changed-word">{t("settings.changed")}</span>
              <button
                type="button"
                className="link-button"
                disabled={busy}
                onClick={() => write(TRAY_DEFAULT_NAG_DAYS)}
              >
                {t("settings.tray.reset", { n: TRAY_DEFAULT_NAG_DAYS })}
              </button>
            </span>
          ) : null}
        </legend>
        <p className="menu-note">{tCount("settings.state.trayNag", stored)}</p>
        <form
          className="field-inline"
          onSubmit={(e) => {
            e.preventDefault();
            write(Number(shown));
          }}
        >
          <input
            type="number"
            className="text-input setting-days-input"
            min={1}
            value={shown}
            disabled={busy}
            aria-label={t("settings.tray.nag")}
            onChange={(e) => setDraft(e.target.value)}
          />
          <button
            type="submit"
            className="button button-quiet"
            // A whole positive number that DIFFERS. Without the first two clauses an
            // empty or fractional field leaves the button live and the click does
            // nothing at all (the write guards on the same condition), which is the
            // "ran and changed nothing" shape one control wide.
            disabled={
              busy ||
              draft === null ||
              !Number.isInteger(Number(draft)) ||
              Number(draft) <= 0 ||
              Number(draft) === stored
            }
          >
            {t("settings.areas.apply")}
          </button>
        </form>
        <p className="field-hint">{t("settings.tray.explain")}</p>
      </fieldset>

      <p className="field-hint">{t("settings.state.trayNotRouted.long")}</p>

      {/* Follow-up reminders (#58): the desk's own "come back to this". */}
      <FollowUpSettings />
    </>
  );
}
