// SPDX-License-Identifier: AGPL-3.0-only

// SETTINGS › NOTIFICATIONS, THE THREE CHOICES #59 ADDED: what THIS DEVICE is
// told about, when the user is not to be disturbed at all, and which mail
// accounts notify. Each is a different scope — device, login, account — and
// the row's Scope tags say so.
//
// THE ACCESSIBLE NAME OF EVERY CONTROL IS ITS VISIBLE TEXT (WCAG 2.5.3): each
// input sits inside its <label>, and no aria-label replaces the words, so a
// speech user saying what they see reaches the control (the a11y lane
// asserts exactly that, NOTIFY in scripts/a11y-check.cjs). Groups are
// fieldsets with legends, which is how a screen reader announces "Quiet hours,
// group" before the first field.
//
// NOTHING IS OPTIMISTIC. Each control writes and then shows the server's
// answer (DeskPreferences/get after the set, Account/get after Account/set,
// the device's registration re-read) — a quiet-hours form that showed a
// period the server had refused would be a promise of silence that does not
// exist.

import { useEffect, useState } from "react";
import type { JSX } from "react";

import type { PushMode, QuietHours } from "../../../../core/jmap/types";
import { t } from "../../../i18n";
import { readDeviceMode, setDeviceMode } from "../../../push";
import type { PushStatus } from "../../../push";
import { useApp, useMailState } from "../../../state";
import { useAccounts } from "../../useAccounts";
import type { AccountEntry } from "../../useAccounts";
import {
  PUSH_MODES,
  WEEK,
  browserTimeZone,
  initialQuietHours,
  minutesToTime,
  quietHoursProblem,
  timeToMinutes,
  timeZoneChoices,
} from "../notifyPrefs";

// ---------------------------------------------------------------------------
// 1. This device's mode
// ---------------------------------------------------------------------------

/** Shown only while push is ON here: a mode for a device that is not
 * registered would be a choice nothing reads. */
export function DeviceModeControl({ status }: { status: PushStatus | null }): JSX.Element | null {
  const { client, pushToast } = useApp();
  const [device, setDevice] = useState<{ id: string; mode: PushMode } | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (status !== "on") return;
    let cancelled = false;
    void readDeviceMode(client)
      .then((d) => {
        if (!cancelled) setDevice(d);
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [client, status]);

  if (status !== "on" || device === null) return null;

  const choose = (mode: PushMode): void => {
    setBusy(true);
    void (async () => {
      try {
        const ok = await setDeviceMode(client, device.id, mode);
        if (!ok) throw new Error(t("settings.notify.mode.refused"));
        const fresh = await readDeviceMode(client);
        setDevice(fresh);
      } catch (err) {
        pushToast({
          kind: "error",
          message: t("settings.notifications.error", { error: err instanceof Error ? err.message : String(err) }),
        });
      } finally {
        setBusy(false);
      }
    })();
  };

  return (
    <fieldset className="menu-group notify-mode">
      <legend>{t("settings.notify.mode.legend")}</legend>
      {PUSH_MODES.map((mode) => (
        <label key={mode} className="radio">
          <input
            type="radio"
            name="notify-mode"
            value={mode}
            checked={device.mode === mode}
            disabled={busy}
            onChange={() => choose(mode)}
          />
          {t(`settings.notify.mode.${mode}`)}
        </label>
      ))}
      <p className="field-hint">{t("settings.notify.mode.hint")}</p>
    </fieldset>
  );
}

// ---------------------------------------------------------------------------
// 2. Quiet hours
// ---------------------------------------------------------------------------

export function QuietHoursControl(): JSX.Element | null {
  const { actions } = useApp();
  const stored = useMailState().deskPreferences?.quietHours;
  // Not drawn against a server before #59: a form for a setting that does not
  // exist there would save nothing and say it had.
  if (!stored) return null;
  return <QuietHoursForm stored={stored} save={(q) => actions.updateDeskPreferences({ quietHours: q }, t("settings.quiet.failed"))} />;
}

/** The form itself, separate so a test can drive it without the app. */
export function QuietHoursForm({
  stored,
  save,
}: {
  stored: QuietHours;
  save: (q: QuietHours) => Promise<boolean>;
}): JSX.Element {
  const browser = browserTimeZone();
  const [draft, setDraft] = useState<QuietHours>(() => initialQuietHours(stored, browser));
  const [busy, setBusy] = useState(false);
  // A fresh server answer replaces the draft (another tab, or this save).
  const storedKey = JSON.stringify(stored);
  useEffect(() => {
    setDraft(initialQuietHours(stored, browser));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [storedKey]);

  const problem = quietHoursProblem(draft);
  const dirty = JSON.stringify(draft) !== storedKey;
  const zones = timeZoneChoices(draft.timeZone, browser);
  const setDay = (day: number, on: boolean): void =>
    setDraft({ ...draft, days: on ? [...new Set([...draft.days, day])].sort() : draft.days.filter((d) => d !== day) });

  return (
    <form
      className="menu-group quiet-hours"
      aria-label={t("settings.quiet.legend")}
      onSubmit={(ev) => {
        ev.preventDefault();
        if (problem) return;
        setBusy(true);
        void save(draft).finally(() => setBusy(false));
      }}
    >
      <fieldset>
        <legend>{t("settings.quiet.legend")}</legend>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={draft.enabled}
            onChange={(ev) => setDraft({ ...draft, enabled: ev.target.checked })}
          />
          {t("settings.quiet.enabled")}
        </label>
        <div className="quiet-hours-times">
          <label className="field">
            <span className="field-label">{t("settings.quiet.from")}</span>
            <input
              type="time"
              className="text-input"
              value={minutesToTime(draft.start)}
              onChange={(ev) => {
                const m = timeToMinutes(ev.target.value);
                if (m !== null) setDraft({ ...draft, start: m });
              }}
            />
          </label>
          <label className="field">
            <span className="field-label">{t("settings.quiet.until")}</span>
            <input
              type="time"
              className="text-input"
              value={minutesToTime(draft.end)}
              onChange={(ev) => {
                const m = timeToMinutes(ev.target.value);
                if (m !== null) setDraft({ ...draft, end: m });
              }}
            />
          </label>
        </div>
        <fieldset className="quiet-hours-days">
          <legend>{t("settings.quiet.days")}</legend>
          {WEEK.map(({ day, key }) => (
            <label key={day} className="checkbox">
              <input
                type="checkbox"
                checked={draft.days.includes(day)}
                onChange={(ev) => setDay(day, ev.target.checked)}
              />
              {t(`settings.quiet.day.${key}`)}
            </label>
          ))}
        </fieldset>
        <label className="field">
          <span className="field-label">{t("settings.quiet.zone")}</span>
          <select
            className="text-input"
            value={draft.timeZone}
            onChange={(ev) => setDraft({ ...draft, timeZone: ev.target.value })}
          >
            {zones.map((z) => (
              <option key={z} value={z}>
                {z}
              </option>
            ))}
          </select>
        </label>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={draft.allowVip}
            onChange={(ev) => setDraft({ ...draft, allowVip: ev.target.checked })}
          />
          {t("settings.quiet.allowVip")}
        </label>
        <p className="field-hint">{t("settings.quiet.hint")}</p>
        {problem ? (
          <p className="field-hint field-error" role="alert">
            {problem}
          </p>
        ) : null}
        <button type="submit" className="button button-quiet menu-action" disabled={busy || !dirty || problem !== null}>
          {busy ? t("settings.notifications.working") : t("settings.quiet.save")}
        </button>
      </fieldset>
    </form>
  );
}

// ---------------------------------------------------------------------------
// 3. Per account
// ---------------------------------------------------------------------------

/** One account's switch. Also used inside the account's own settings screen,
 * so the choice has one control in two places that cannot disagree — both
 * write Account.notifyPush and both re-read the account after. */
export function AccountPushToggle({ entry }: { entry: AccountEntry }): JSX.Element | null {
  const { actions } = useApp();
  const { reload } = useAccounts();
  const [busy, setBusy] = useState(false);
  const on = entry.record?.notifyPush;
  if (typeof on !== "boolean") return null;
  return (
    <label className="checkbox">
      <input
        type="checkbox"
        checked={on}
        disabled={busy}
        onChange={() => {
          setBusy(true);
          void (async () => {
            const ok = await actions.updateAccount(entry.id, { notifyPush: !on }, t("settings.notify.account.failed"));
            if (ok) await reload();
            setBusy(false);
          })();
        }}
      />
      {t("settings.notify.account.toggle", { name: entry.name })}
    </label>
  );
}

export function AccountPushControl(): JSX.Element | null {
  const { accounts } = useAccounts();
  const known = accounts.filter((a) => typeof a.record?.notifyPush === "boolean");
  if (known.length === 0) return null;
  return (
    <fieldset className="menu-group">
      <legend>{t("settings.notify.account.legend")}</legend>
      {known.map((a) => (
        <AccountPushToggle key={a.id} entry={a} />
      ))}
      <p className="field-hint">{t("settings.notify.account.hint")}</p>
    </fieldset>
  );
}
