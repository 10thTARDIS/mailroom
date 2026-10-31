// SPDX-License-Identifier: AGPL-3.0-only

// "HISTORY" (#815): the collapsed section under each list of keys. A real
// button that says whether it is open (aria-expanded) and which region it
// opens (aria-controls); the region is not rendered while closed, so its
// controls are not in the tab order then. Collapsed by default: the keys in use
// are what the page is mostly visited for.
//
// Not a <details>: "Replaced by" has to be able to open it to reach a key
// inside, and a controlled button says its state the same way in every
// browser's accessibility tree.

import { useId } from "react";
import type { JSX, ReactNode } from "react";

import { t } from "../../../i18n";

export function HistoryDisclosure({
  open,
  onToggle,
  count,
  label,
  children,
}: {
  open: boolean;
  onToggle: (open: boolean) => void;
  /** How many entries it holds, said on the button (null: not known). */
  count: number | null;
  /** Whose history it is ("your keys"), after the visible words in the name. */
  label: string;
  children: ReactNode;
}): JSX.Element {
  const regionId = useId();
  const buttonId = useId();
  return (
    <div className="key-history">
      <h5 className="key-history-heading">
        <button
          type="button"
          id={buttonId}
          className="key-history-toggle"
          aria-expanded={open}
          aria-controls={regionId}
          // The visible words first ("History (2)"), then whose: WCAG 2.5.3.
          aria-label={
            count === null
              ? `${t("keys.history.heading")}, ${label}`
              : `${t("keys.history.heading")} (${count}), ${label}`
          }
          onClick={() => onToggle(!open)}
        >
          <span aria-hidden="true" className="key-history-caret">
            {open ? "▾" : "▸"}
          </span>{" "}
          {t("keys.history.heading")}
          {count !== null ? <span className="key-history-count"> ({count})</span> : null}
        </button>
      </h5>
      {open ? (
        <div id={regionId} role="region" aria-labelledby={buttonId} className="key-history-body">
          {children}
        </div>
      ) : (
        <div id={regionId} hidden />
      )}
    </div>
  );
}
