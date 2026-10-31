// SPDX-License-Identifier: AGPL-3.0-only

// SEND LATER (#56, spec §5/§16).
//
// THE ENGINE WAS ALREADY BUILT. A submission IS a durable job with a fire-at time,
// and the ordinary send has always been "now + the undo window" — so scheduling one
// adds no machinery at all, it only stops defaulting the time. That is why this
// file is a menu and not a subsystem.
//
// It reuses the SNOOZE presets (core/snooze.ts) deliberately. "This evening" and
// "tomorrow morning" mean the same instants whether you are deferring a message you
// received or one you are about to send, and two lists that drift apart would be two
// different definitions of tomorrow in one product. The presets already drop
// anything in the past and collapse pairs that resolve to the same answer.
//
// EVERY ROW SHOWS THE INSTANT IT RESOLVES TO, which is the snooze menu's rule and
// matters more here: deferring your own mail to the wrong hour is a message that
// goes out at 3am. "What you meant" on the left, "which instant that is" on the
// right.

import { useMemo } from "react";

import { formatWake, snoozePresets } from "../../core/snooze";
import { t } from "../i18n";
import type { MessageKey } from "../i18n";

/** The preset labels, shared with the snooze menu's vocabulary. */
const PRESET_LABEL: Record<string, MessageKey> = {
  laterToday: "snooze.laterToday",
  thisEvening: "snooze.thisEvening",
  tomorrow: "snooze.tomorrow",
  thisWeekend: "snooze.thisWeekend",
  nextWeek: "snooze.nextWeek",
};

export function SendLaterMenu({
  maxDelayedSendSeconds,
  onPick,
  onClose,
}: {
  /** The server's advertised horizon (session `maxDelayedSend`). A preset beyond it
   * is not offered, because the server now ENFORCES it — offering a choice that
   * will be refused is worse than not offering it. */
  maxDelayedSendSeconds: number | null;
  onPick: (iso: string) => void;
  onClose: () => void;
}) {
  const presets = useMemo(() => {
    const now = new Date();
    const all = snoozePresets(now);
    if (maxDelayedSendSeconds === null) return all;
    const horizon = now.getTime() + maxDelayedSendSeconds * 1000;
    return all.filter((p) => p.at.getTime() <= horizon);
  }, [maxDelayedSendSeconds]);

  return (
    <div className="sheet sheet-small" role="dialog" aria-modal="true" aria-label={t("sendLater.title")}>
      <header className="sheet-header">
        <h2>{t("sendLater.title")}</h2>
        <button type="button" className="icon-button" aria-label={t("action.close")} onClick={onClose}>
          ✕
        </button>
      </header>
      <ul className="snooze-list">
        {presets.map((p) => (
          <li key={p.key}>
            <button
              type="button"
              className="snooze-choice"
              onClick={() => onPick(p.at.toISOString())}
            >
              <span className="snooze-choice-label">{t(PRESET_LABEL[p.key] ?? "sendLater.title")}</span>
              <span className="snooze-choice-when">{formatWake(p.at.toISOString())}</span>
            </button>
          </li>
        ))}
        {presets.length === 0 ? (
          <li>
            <p className="field-hint">{t("sendLater.noneAvailable")}</p>
          </li>
        ) : null}
      </ul>
    </div>
  );
}
