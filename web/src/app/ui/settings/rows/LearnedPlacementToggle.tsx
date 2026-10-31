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

import { useState } from "react";
import type { JSX } from "react";

import { t } from "../../../i18n";
import { useApp, useMailState } from "../../../state";

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
    </fieldset>
  );
}
