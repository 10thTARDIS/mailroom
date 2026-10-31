// SPDX-License-Identifier: AGPL-3.0-only

// AN AREA'S HEADING on Review first (#375 increment 3).
//
//   Bills · 4
//   Needs sorting · 12 · place once → label + rule
//
// THE COUNT COMES FROM THE SUMMARY, not from the rows underneath it. The two
// are different numbers on purpose: the summary is what the server says the
// area holds, the rows are the window that has been fetched. Printing the row
// count instead would make "Bills · 4" true of the screen and false of the
// mailbox, and the desk's whole claim is that its numbers are the server's.
//
// NEEDS SORTING CARRIES A HINT, because it is the only section that asks for
// work rather than just showing mail: placing a message there once is what
// creates the label and the rule that place the next one, and a heading that
// said only "Needs sorting · 12" would look like a twelfth area rather than the
// pile the other eleven were made out of.
//
// IT IS A BUTTON NOW (#375 increment 7), and it is one on every screen size.
// The phone needs the fold — a dozen areas do not fit four inches — but a
// heading that is a control on one width and a label on another is two
// behaviours to learn and two accessibility trees to get right, so the fold
// exists everywhere and only the MEMORY of it is per device (app/prefs.ts).
// `aria-expanded` is on the button rather than on a wrapper because the button
// is what the user activates and what a screen reader reads the state from; the
// count stays printed while it is folded, which is what makes folding safe
// rather than a way to lose an area.

import type { JSX } from "react";

import { t } from "../../i18n";

export function SectionHeading({
  name,
  count,
  needsSorting,
  collapsed,
  onToggle,
}: {
  /** The area's name, or the Needs-sorting label. */
  name: string;
  count: number;
  needsSorting: boolean;
  collapsed: boolean;
  onToggle: () => void;
}): JSX.Element {
  const className = [
    "desk-heading",
    needsSorting ? "is-unsorted" : "",
    collapsed ? "is-collapsed" : "",
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <h2 className={className}>
      <button
        type="button"
        className="desk-heading-button"
        aria-expanded={!collapsed}
        onClick={onToggle}
      >
        {/* The caret is decorative: `aria-expanded` above is the state, and a
            reader that heard both would hear it twice. */}
        <span className="desk-heading-caret" aria-hidden="true">
          {collapsed ? "▸" : "▾"}
        </span>
        <span className="desk-heading-name">{name}</span>
        <span className="desk-heading-count">{count}</span>
        {needsSorting ? (
          <span className="desk-heading-hint">{t("desk.section.needsSortingHint")}</span>
        ) : null}
      </button>
    </h2>
  );
}
