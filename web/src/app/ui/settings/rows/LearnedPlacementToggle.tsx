// SPDX-License-Identifier: AGPL-3.0-only

// LEARNED PLACEMENT'S OFF SWITCH (#714), on the Areas row because it is one of
// the things that decides which area a message lands in.
//
// One checkbox, default ON (the chosen default: it acts immediately). The
// switch is a DeskPreferences field the SERVER reads at ingest, so turning it
// off here stops it for every client and every device, not just this tab.
//
// THE ACCESSIBLE NAME IS THE VISIBLE TEXT (WCAG 2.5.3, label in name): the
// <label> wraps the input, so the checkbox is named by exactly the words
// printed beside it, and a speech user saying "click sort new mail…" hits it.
// No aria-label, which is how the two would drift apart.
//
// NOT SHOWN AT ALL against a server that does not send the field (before
// #714): a box drawn checked there would claim a feature that does not exist,
// and one drawn unchecked would claim it is off.
//
// BELOW THE SWITCH, while it is on, a short STATUS block (LearnedPlacement/
// getStatus): what learned sorting filed in the last 30 days and how many of
// those the user moved, then the senders the user keeps filing by hand that it
// is not learning, each with the reason and a "Make a rule" button that runs the
// rule offer's own flow. FAILURE IS SILENCE: it is a status line, so a failed or
// unreadable call draws nothing, never an error.

import { useEffect, useState } from "react";
import type { JSX } from "react";

import type { LearningStatus, LearningStatusSender } from "../../../../core/jmap/types";
import { t } from "../../../i18n";
import type { MessageKey } from "../../../i18n";
import { useApp, useMailState } from "../../../state";
import { makeRuleForSender } from "../../ruleSuggestion";

/** Reasons we have a plain-language phrase for; anything else shows no phrase. */
const REASON_KEYS: Record<string, MessageKey> = {
  mixed: "settings.learned.reason.mixed",
  thin: "settings.learned.reason.thin",
  none: "settings.learned.reason.thin",
  unauthenticated: "settings.learned.reason.unauthenticated",
  contradicted: "settings.learned.reason.contradicted",
  gated: "settings.learned.reason.gated",
  elsewhere: "settings.learned.reason.elsewhere",
  blocked: "settings.learned.reason.blocked",
  warned: "settings.learned.reason.warned",
  ambiguous: "settings.learned.reason.ambiguous",
};

export function LearnedPlacementToggle(): JSX.Element | null {
  const { actions } = useApp();
  const prefs = useMailState().deskPreferences;
  const [busy, setBusy] = useState(false);
  if (prefs === null || typeof prefs.learnedPlacementEnabled !== "boolean") return null;
  const on = prefs.learnedPlacementEnabled;

  return (
    <fieldset className="menu-group">
      <legend>{t("settings.learned.title")}</legend>
      <label className="checkbox">
        <input
          type="checkbox"
          checked={on}
          disabled={busy}
          onChange={() => {
            setBusy(true);
            // Written from the server's answer, not optimistically:
            // updateDeskPreferences re-reads the record, and the box follows it.
            void actions
              .updateDeskPreferences({ learnedPlacementEnabled: !on }, t("settings.learned.failed"))
              .finally(() => setBusy(false));
          }}
        />
        {t("settings.learned.toggle")}
      </label>
      <p className="field-hint">{t("settings.learned.explain")}</p>
      {on ? <LearningStatusBlock /> : null}
    </fieldset>
  );
}

function filedText(s: LearningStatus): string {
  if (s.placed <= 0) return t("settings.learned.status.none");
  const filed =
    s.placed === 1
      ? t("settings.learned.status.filed.one")
      : t("settings.learned.status.filed.other", { count: s.placed });
  if (s.corrected <= 0) return filed;
  if (s.placed === 1) return `${filed} ${t("settings.learned.status.movedIt")}`;
  const moved =
    s.corrected === 1
      ? t("settings.learned.status.moved.one")
      : t("settings.learned.status.moved.other", { count: s.corrected });
  return `${filed} ${moved}`;
}

function LearningStatusBlock(): JSX.Element | null {
  const app = useApp();
  const { actions } = app;
  const areas = useMailState().areas;
  const [status, setStatus] = useState<LearningStatus | null>(null);
  const [made, setMade] = useState<ReadonlySet<string>>(new Set());
  const [working, setWorking] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    void actions.learningStatus().then(
      (s) => {
        if (live) setStatus(s);
      },
      () => {
        if (live) setStatus(null);
      },
    );
    return () => {
      live = false;
    };
  }, [actions]);

  if (!status || !status.enabled) return null;
  const key = (s: LearningStatusSender): string => `${s.areaId}\n${s.address}`;
  const lines = (status.senders ?? []).filter((s) => areas[s.areaId] !== undefined && !made.has(key(s)));

  const make = (s: LearningStatusSender): void => {
    setWorking(key(s));
    void makeRuleForSender(
      { store: app.store, sync: app.sync, actions, offerUndo: app.offerUndo, pushToast: app.pushToast },
      s.areaId,
      s.address,
    )
      .then((ok) => {
        if (ok) setMade((m) => new Set(m).add(key(s)));
      })
      .catch(() => undefined)
      .finally(() => setWorking(null));
  };

  return (
    <div className="learned-status">
      <p className="field-hint">{filedText(status)}</p>
      {lines.length > 0 ? (
        <>
          <h4>{t("settings.learned.senders.heading")}</h4>
          <ul>
            {lines.map((s) => {
              const reasonKey = REASON_KEYS[s.reason];
              return (
                <li key={key(s)}>
                  <strong>{s.address}</strong> → {areas[s.areaId]?.name}
                  {reasonKey ? ` · ${t(reasonKey)}` : ""}{" "}
                  <button
                    type="button"
                    disabled={working !== null}
                    aria-label={t("settings.learned.senders.makeFor", { address: s.address })}
                    onClick={() => make(s)}
                  >
                    {t("settings.learned.senders.make")}
                  </button>
                </li>
              );
            })}
          </ul>
          {status.moreSenders > 0 ? (
            <p className="field-hint">
              {status.sendersTruncated
                ? t("settings.learned.senders.atLeastMore", { count: status.moreSenders })
                : t("settings.learned.senders.more", { count: status.moreSenders })}
            </p>
          ) : null}
        </>
      ) : null}
    </div>
  );
}
