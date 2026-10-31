// SPDX-License-Identifier: AGPL-3.0-only

// "Come back to this later" (spec §6, #51). Presets for the common answers, a
// picker for everything else, and — when the selection is already snoozed — the
// way back out.
//
// Every choice shows the instant it resolves to. A snooze is a promise about the
// future, and a menu that says only "next week" is asking to be misread; the
// resolution is computed in core/snooze.ts so this component stays presentation.

import { useMemo, useState } from "react";
import type { JSX } from "react";

import { effectiveEmail } from "../../core/store";
import {
  customSnoozeTime,
  earliestSnoozeInput,
  formatWake,
  snoozePresets,
} from "../../core/snooze";
import type { SnoozePresetKey } from "../../core/snooze";
import type { Id } from "../../core/jmap/types";
import { useApp, useMailState } from "../state";
import { t } from "../i18n";
import { snoozedText } from "./undoText";
import type { MessageKey } from "../i18n";

const PRESET_LABEL: Record<SnoozePresetKey, MessageKey> = {
  laterToday: "snooze.laterToday",
  thisEvening: "snooze.thisEvening",
  tomorrow: "snooze.tomorrow",
  thisWeekend: "snooze.thisWeekend",
  nextWeek: "snooze.nextWeek",
};

interface Props {
  ids: Id[];
  onClose: () => void;
  /** Called when a snooze is actually applied (not on a plain close), so the
   * shell can advance a reading pane whose message just left the view. Absent
   * when this view will still list the message afterwards. */
  onSnoozed?: (() => void) | undefined;
}

export function SnoozeMenu({ ids, onClose, onSnoozed }: Props): JSX.Element {
  const state = useMailState();
  const { actions, offerUndo } = useApp();
  const [custom, setCustom] = useState("");
  const [error, setError] = useState<string | null>(null);

  // Resolved once per open: a list that re-resolved on every render would drift
  // under the user's cursor.
  const presets = useMemo(() => snoozePresets(), []);
  const earliest = useMemo(() => earliestSnoozeInput(), []);

  // The wake time already in force, when every selected message shares one —
  // which is what makes "un-snooze" an honest offer rather than a guess.
  const currentWake = useMemo(() => {
    const wakes = ids.map((id) => effectiveEmail(state, id)?.snoozedUntil ?? null);
    const first = wakes[0] ?? null;
    return first && wakes.every((w) => w === first) ? first : null;
  }, [ids, state]);

  const pick = (iso: string): void => {
    void actions.setSnoozed(ids, iso);
    // The undo offer lives HERE rather than in useListVerbs (#375 increment 6),
    // because this is where the snooze actually happens: `b` opens this menu,
    // it does not put anything away. Every surface that can snooze opens this
    // one component, so registering it once covers all of them.
    offerUndo(snoozedText(ids.length), async () => {
      await actions.setSnoozed(ids, null);
      return true;
    });
    onSnoozed?.();
    onClose();
  };

  return (
    <div className="sheet sheet-small" role="dialog" aria-modal="true" aria-label={t("snooze.title")}>
      <header className="sheet-header">
        <h2>{t("snooze.title")}</h2>
        <button type="button" className="icon-button" aria-label={t("action.close")} onClick={onClose}>
          ✕
        </button>
      </header>

      {ids.length === 0 ? <p className="empty">{t("snooze.noSelection")}</p> : null}

      {currentWake ? (
        <p className="snooze-current" role="status">
          {t("snooze.currently", { when: formatWake(currentWake) })}
        </p>
      ) : null}

      <ul className="snooze-list">
        {presets.map((p) => (
          <li key={p.key}>
            <button
              type="button"
              className="snooze-choice"
              disabled={ids.length === 0}
              onClick={() => pick(p.at.toISOString())}
            >
              <span className="snooze-choice-label">{t(PRESET_LABEL[p.key])}</span>
              <span className="snooze-choice-when">{formatWake(p.at.toISOString())}</span>
            </button>
          </li>
        ))}
      </ul>

      <form
        className="inline-form"
        onSubmit={(ev) => {
          ev.preventDefault();
          const iso = customSnoozeTime(custom);
          if (!iso) {
            setError(t("snooze.mustBeFuture"));
            return;
          }
          pick(iso);
        }}
      >
        <input
          className="text-input"
          type="datetime-local"
          min={earliest}
          aria-label={t("snooze.pickTime")}
          value={custom}
          onChange={(ev) => {
            setCustom(ev.target.value);
            setError(null);
          }}
        />
        <button type="submit" className="button" disabled={ids.length === 0}>
          {t("snooze.set")}
        </button>
      </form>
      {error ? (
        <p className="banner banner-error" role="alert">
          {error}
        </p>
      ) : null}

      {currentWake ? (
        <button
          type="button"
          className="button snooze-unsnooze"
          onClick={() => {
            void actions.setSnoozed(ids, null);
            onSnoozed?.();
            onClose();
          }}
        >
          {t("snooze.unsnooze")}
        </button>
      ) : null}
    </div>
  );
}
