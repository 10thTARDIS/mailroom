// SPDX-License-Identifier: AGPL-3.0-only

// WHAT TO DO WITH THE ROWS YOU PICKED (#375 increment 3).
//
//   2 selected · Archive · In-tray · Mark read · Mark unread · Mark as important · Label ▾ · Re-file ▾ · Clear
//
// It appears only with a selection and sticks to the top of the list, so the
// verbs stay reachable after scrolling down forty rows to build the selection.
//
// NO KEYS ARE PRINTED HERE, OR ANYWHERE ELSE ON THE DESK. Every button carries
// words. The shortcuts exist and are worth learning, but a surface that
// annotates each control with its keystroke teaches the shortcuts to people
// already using the mouse and clutters the row for everyone else; the Keys
// sheet is the one place they are listed.
//
// RE-FILE IS THE DESK'S OWN VERB, and it is not "apply a label" wearing a
// different hat by accident — an area OWNS a label (jmap/types.ts Area), so
// placing mail in an area IS applying that label, which is what makes the
// arrangement survive in any other mail client. A LOCAL-ONLY label has no
// keyword atom to put on the message, so that area is offered DISABLED with the
// reason, rather than silently doing nothing (the same choice LabelMenu makes).

import type { JSX } from "react";

import { t } from "../../i18n";
import type { Id } from "../../../core/jmap/types";

export interface RefileTarget {
  id: Id;
  name: string;
  /** Null when the area can be filed into; otherwise why it cannot. */
  blocked: string | null;
}

export function SelectionToolbar({
  count,
  areas,
  refileOpen,
  trayAction = "add",
  onArchive,
  onTray,
  onMarkRead,
  onMarkUnread,
  onImportant,
  onLabel,
  onToggleRefile,
  onRefile,
  onClear,
  allImportant,
}: {
  count: number;
  areas: RefileTarget[];
  refileOpen: boolean;
  /**
   * What the tray button means HERE, and where it sits.
   *
   * On the desk it PUTS mail in the tray and follows Archive. On the tray page
   * it takes mail OUT, and it leads — that is the page's whole verb, and a
   * toolbar whose first button archives on the one screen where "I have dealt
   * with this" usually means unflagging would put the more destructive action
   * under the hand first. Same callback either way (`onTray`): the route
   * decides the direction, because on the tray "toggle" would re-flag a row
   * that somehow was not flagged.
   */
  trayAction?: "add" | "remove";
  onArchive: () => void;
  onTray: () => void;
  onMarkRead: () => void;
  onMarkUnread: () => void;
  /** Mark the selection important (#921). The LABEL below names the direction
   * the verb will take, so it can never offer to un-mark a mixed selection. */
  onImportant: () => void;
  /** EVERY selected message is already important (#921): the button then
   * offers "Not important" instead of "Mark as important". */
  allImportant: boolean;
  onLabel: () => void;
  onToggleRefile: () => void;
  onRefile: (areaId: Id) => void;
  onClear: () => void;
}): JSX.Element {
  const tray = (
    <button type="button" className="button" onClick={onTray}>
      {trayAction === "remove" ? t("tray.remove") : t("desk.selection.tray")}
    </button>
  );
  return (
    <div className="desk-toolbar" role="toolbar" aria-label={t("desk.selection.label")}>
      {/* THE COUNT IS A STATUS (#447). The selection is built with the keyboard —
          `x`, Space, Shift+Space, Shift+click — and without this the only feedback
          for "did that one go in?" is a number nobody is looking at. `role="status"`
          is polite, so it never interrupts, and it re-announces on every change: "2
          selected", "3 selected".

          ⚠ ITS FIRST APPEARANCE MAY NOT BE HEARD. A live region has to be in the
          document BEFORE its text changes, and this toolbar does not exist until
          something is selected — so the announcement that is certain is the second
          selection onwards. Stated rather than papered over with a permanently
          mounted empty region, which would be a second live region on a surface
          whose one live region is the header sentence (ui/desk/DeskHeader.tsx). */}
      <span className="desk-toolbar-count" role="status">
        {t("desk.selection.count", { n: count })}
      </span>
      {trayAction === "remove" ? tray : null}
      <button type="button" className="button" onClick={onArchive}>
        {t("action.archive")}
      </button>
      {trayAction === "remove" ? null : tray}
      <button type="button" className="button" onClick={onMarkRead}>
        {t("desk.selection.markRead")}
      </button>
      <button type="button" className="button" onClick={onMarkUnread}>
        {t("desk.selection.markUnread")}
      </button>
      {/* THE LABEL NAMES THE VERB (#921): "Mark as important" while the
          selection is not all important, "Not important" only when every
          selected message already is. */}
      <button type="button" className="button" onClick={onImportant}>
        {allImportant ? t("priority.markNotImportant") : t("priority.markImportant")}
      </button>
      <button type="button" className="button" onClick={onLabel}>
        {t("action.label")} ▾
      </button>
      <span className="desk-refile">
        <button
          type="button"
          className="button"
          aria-expanded={refileOpen}
          aria-haspopup="menu"
          onClick={onToggleRefile}
        >
          {t("desk.selection.refile")} ▾
        </button>
        {refileOpen ? (
          <div className="desk-refile-menu" role="menu" aria-label={t("desk.refile.label")}>
            {areas.length === 0 ? (
              <p className="desk-refile-none">{t("desk.refile.noAreas")}</p>
            ) : null}
            {areas.map((area) => (
              <button
                key={area.id}
                type="button"
                role="menuitem"
                className="desk-refile-item"
                disabled={area.blocked !== null}
                title={area.blocked ?? undefined}
                onClick={() => onRefile(area.id)}
              >
                {area.name}
              </button>
            ))}
          </div>
        ) : null}
      </span>
      <button type="button" className="button button-quiet" onClick={onClear}>
        {t("desk.selection.clear")}
      </button>
    </div>
  );
}
