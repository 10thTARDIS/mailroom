// SPDX-License-Identifier: AGPL-3.0-only

// THE READING PANE'S RE-FILE PICKER (#921): what the pane's More menu opens, so
// a message being read moves to another desk area without going back to the list.
//
// SAME PICTURE AS THE DESK'S RE-FILE MENU (desk/SelectionToolbar.tsx): one item
// per area, and an area whose label has not loaded is offered DISABLED with the
// reason — an area OWNS a label, so there is then nothing to apply. The shell
// is the small sheet's (ui/LabelMenu.tsx); the backdrop and the Escape are the
// dialog's (desk/ConfirmDialog.tsx), and the Escape stops HERE — the shell's
// own chain peels a route, this layer goes first (ui/AreaMenu.tsx). Up/Down
// walk the enabled items, the way a menu owes.

import { useEffect, useRef } from "react";
import type { JSX } from "react";

import { t } from "../i18n";
import type { RefileTarget } from "./desk/SelectionToolbar";

export function RefilePicker({
  areas,
  onPick,
  onClose,
}: {
  areas: RefileTarget[];
  onPick: (areaId: string) => void;
  onClose: () => void;
}): JSX.Element {
  const title = t("refile.picker.title");
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const closeRef = useRef<HTMLButtonElement | null>(null);

  const items = (): HTMLButtonElement[] =>
    Array.from(wrapRef.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]:not(:disabled)') ?? []);

  // Focus moves IN on open, the way a menu owes: a dialog that opens with focus
  // behind it leaves every area a Tab stop away. With nothing enabled the ✕ is
  // what focus falls on, so Escape and Enter still have a home.
  useEffect(() => {
    const first = wrapRef.current?.querySelector<HTMLButtonElement>('[role="menuitem"]:not(:disabled)');
    if (first) first.focus();
    else closeRef.current?.focus();
  }, []);

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(ev) => {
        if (ev.target === ev.currentTarget) onClose();
      }}
      onKeyDown={(ev) => {
        if (ev.key === "Escape") {
          // The shell's own Escape chain peels a route; this layer goes first.
          ev.stopPropagation();
          ev.preventDefault();
          onClose();
          return;
        }
        if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
          const list = items();
          if (list.length === 0) return;
          ev.preventDefault();
          const at = list.indexOf(document.activeElement as HTMLButtonElement);
          const step = ev.key === "ArrowDown" ? 1 : -1;
          list[(at + step + list.length) % list.length]?.focus();
        }
      }}
    >
      <div ref={wrapRef} className="sheet sheet-small" role="dialog" aria-modal="true" aria-label={title}>
        <header className="sheet-header">
          <h2>{title}</h2>
          <button ref={closeRef} type="button" className="icon-button" aria-label={t("action.close")} onClick={onClose}>
            ✕
          </button>
        </header>
        <div className="sheet-body" role="menu" aria-label={title}>
          {areas.length === 0 ? <p className="desk-refile-none">{t("desk.refile.noAreas")}</p> : null}
          {areas.map((area) => (
            <button
              key={area.id}
              type="button"
              role="menuitem"
              className="desk-refile-item"
              disabled={area.blocked !== null}
              title={area.blocked ?? undefined}
              onClick={() => {
                // Pick, then close: the route receives the id before the state
                // that unmounts this goes away.
                onPick(area.id);
                onClose();
              }}
            >
              {area.name}
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
