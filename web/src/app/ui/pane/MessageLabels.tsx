// SPDX-License-Identifier: AGPL-3.0-only

// THE LABELS A MESSAGE WEARS, ON THE MESSAGE (#375 increment 5).
//
//   [ Personal ]  ( Receipts )  ( Newsletters )   + label
//
// One row, under the meta line, and it answers a question the pane could not
// answer before at all: WHERE IS THIS FILED. The reading pane knew about
// provenance ("labelled by the rule X") and about suggestions, but never simply
// showed the labels the message carries — so the only way to see them was to
// open the label menu, which is a dialog for CHANGING them.
//
// THE AREA IS DRAWN DIFFERENTLY FROM THE REST BECAUSE IT IS DIFFERENT. An area
// is where the message lives on the desk; the other labels are what it is
// about. Same underlying keyword, two different questions, so the area is a
// raised chip and the others are outlined.
//
// COLOUR IS NEVER THE ONLY SIGNAL. Every chip prints its name; the label's
// colour is spent on the OUTLINE and a dot, never on the text — tokens.css
// records that three of the four label tones sit just under 4.5:1 as plain text
// on the light background, which is fine for a rule and a dot beside a word and
// is not fine for the word itself.
//
// CLICKING A CHIP NAVIGATES to all-mail narrowed by that area or label, which
// is the list the rail already links to — carrying the account scope, because
// "Receipts" from one account's desk means that account's receipts.

import { useMemo } from "react";
import type { JSX } from "react";

import type { EffectiveEmail } from "../../../core/store";
import type { Id } from "../../../core/jmap/types";
import { navigate, useListRoute } from "../../routing";
import { useMailState } from "../../state";
import { t } from "../../i18n";
import { areaProvenanceKey, labelColor, messageLabels } from "./labels";

/** The sentence fragment for who filed this message, from the area label's own
 * provenance (labels.ts areaProvenanceKey, the one table). */
function provenanceText(by: string | null): string {
  return t(areaProvenanceKey(by));
}

export function MessageLabels({
  email,
  onAddLabel,
}: {
  email: EffectiveEmail;
  /** Opens the existing LabelMenu on this message — the one dialog that
   * changes labels, rather than a second way to do it. */
  onAddLabel: () => void;
}): JSX.Element | null {
  const state = useMailState();
  const listRoute = useListRoute();
  const { area, others } = useMemo(
    () => messageLabels(email, state.labels, state.areas),
    [email, state.labels, state.areas],
  );

  // Only the account scope rides along: it is spelled as its own object rather
  // than a Partial<Route> because spreading a partial route into a literal
  // widens the page discriminant and TypeScript then cannot tell which member
  // of the union is being built.
  const scope: { acct?: Id } = listRoute.acct ? { acct: listRoute.acct } : {};

  // An area whose Area row has not loaded has no name to print, so it is
  // omitted rather than drawn as an id. It comes back on the next render once
  // the reference data lands.
  const areaName = area?.area?.name ?? null;

  if (!areaName && others.length === 0) {
    // Still offer "+ label": a message with no labels is exactly the one that
    // most needs the affordance.
    return (
      <p className="pane-labels" aria-label={t("pane.labels")}>
        <button type="button" className="pane-label-add" onClick={onAddLabel}>
          {t("pane.addLabel")}
        </button>
      </p>
    );
  }

  return (
    <p className="pane-labels" aria-label={t("pane.labels")}>
      {area && areaName ? (
        <button
          type="button"
          className="pane-label-area"
          title={t("pane.area.title", { area: areaName, how: provenanceText(area.provenance) })}
          aria-label={t("pane.area.title", { area: areaName, how: provenanceText(area.provenance) })}
          onClick={() => navigate({ page: "all", area: area.areaId, ...scope })}
        >
          {areaName}
        </button>
      ) : null}
      {others.map((label, i) => (
        <button
          key={label.id}
          type="button"
          className="pane-label-chip"
          style={{ borderColor: labelColor(label, i) }}
          title={t("pane.label.title", { label: label.name })}
          onClick={() => navigate({ page: "all", label: label.id, ...scope })}
        >
          <span
            className="pane-label-dot"
            style={{ backgroundColor: labelColor(label, i) }}
            aria-hidden="true"
          />
          {label.name}
        </button>
      ))}
      <button type="button" className="pane-label-add" onClick={onAddLabel}>
        {t("pane.addLabel")}
      </button>
    </p>
  );
}
