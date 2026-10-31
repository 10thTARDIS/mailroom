// SPDX-License-Identifier: AGPL-3.0-only

// A DRAWER (#375 increment 3).
//
//   Promotions · 824 · auto-archives after 30 d        [Open] [Archive all]
//
// One closed row per area on Everything else. Closed is the point: this is the
// mail the line decided not to show you, and opening every drawer to render the
// tab would be a query per area to fill a screen nobody asked to read. The
// first paint of the tab waits for none of them.
//
// THE SWEEP SENTENCE HAS TO CONSULT TWO FACTS, and the data layer deliberately
// refuses to fold them into one. `drawerPeriodDays` answers "what is this
// area's period" — it does NOT answer "does the sweep run", because that is a
// login-level master switch (`DeskPreferences.drawerSweepEnabled`, #443) which
// DEFAULTS TO FALSE. On an instance nobody has switched on, every area reports
// a period and not one of them sweeps; printing "auto-archives after 30 d"
// there would be a promise about the user's mail that nothing keeps. So the
// row reads the switch itself and appends "· sweep off".

import type { JSX } from "react";

import { t } from "../../i18n";
import { drawerButtonLabel } from "./format";

export function DrawerRow({
  name,
  count,
  periodDays,
  sweepEnabled,
  open,
  busy,
  onToggle,
  onArchiveAll,
}: {
  name: string;
  count: number;
  /** core/desk.ts drawerPeriodDays: the period, or null for "never" — and also
   * null while the preferences have not loaded, which is why the caller passes
   * `unknown` rather than this being allowed to guess. */
  periodDays: number | null;
  /** The login-level master switch. */
  sweepEnabled: boolean;
  open: boolean;
  busy: boolean;
  onToggle: () => void;
  onArchiveAll: () => void;
}): JSX.Element {
  const sweep = periodDays === null ? t("desk.drawer.noSweep") : t("desk.drawer.sweep", { n: periodDays });
  // "· sweep off" is appended rather than replacing the period: the period is
  // still this area's setting and still what would happen if the switch were
  // turned on, and hiding it would make the drawer look unconfigured.
  const sweepLine = periodDays !== null && !sweepEnabled ? `${sweep} · ${t("desk.drawer.sweepOff")}` : sweep;

  return (
    <div className={open ? "desk-drawer is-open" : "desk-drawer"}>
      <span className="desk-drawer-name">{name}</span>
      <span className="desk-drawer-count">{count}</span>
      <span className="desk-drawer-sweep">{sweepLine}</span>
      <button
        type="button"
        className="button"
        aria-expanded={open}
        // WHOSE DRAWER, HOW MUCH, AND WHAT HAPPENS TO IT ON ITS OWN (#447). The
        // visible word stays "Open"/"Close" — the state is `aria-expanded`'s job,
        // not the name's (ui/desk/format.ts drawerButtonLabel).
        //
        // The name LEADS WITH that visible word (WCAG 2.5.3, axe 4.14
        // label-content-name-mismatch): "Open Promotions drawer, 24 messages, …".
        aria-label={`${open ? t("desk.drawer.close") : t("desk.drawer.open")} ${drawerButtonLabel(name, count, periodDays, sweepEnabled)}`}
        onClick={onToggle}
      >
        {open ? t("desk.drawer.close") : t("desk.drawer.open")}
      </button>
      <button
        type="button"
        className="button"
        // Named for the drawer too: "Archive all" on eleven rows is eleven buttons
        // a reader cannot tell apart, and this one empties an area.
        aria-label={t("desk.drawer.archiveAllNamed", { area: name })}
        onClick={onArchiveAll}
        // Nothing to archive is not an error worth a dialog; the button simply
        // has nothing to act on.
        disabled={busy || count === 0}
      >
        {t("desk.drawer.archiveAll")}
      </button>
    </div>
  );
}
