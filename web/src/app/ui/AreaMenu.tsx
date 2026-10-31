// SPDX-License-Identifier: AGPL-3.0-only

// A "<verb> ▾" button that opens a menu of the user's areas (#708).
//
// The same markup, classes and wording as the desk's Re-file menu
// (desk/SelectionToolbar.tsx) so the two cannot drift, plus the keyboard a menu
// owes: it opens with focus on the first area, Up/Down walk it, Escape closes
// it and puts focus back on the button. The button's accessible name IS its
// visible text (no aria-label that could drop it, WCAG 2.5.3); the menu carries
// its own label saying what choosing an area will do.

import { useEffect, useRef, useState } from "react";
import type { JSX } from "react";

import { t } from "../i18n";
import type { RefileTarget } from "./desk/SelectionToolbar";

export function AreaMenu({
  buttonText,
  menuLabel,
  areas,
  onPick,
  disabled = false,
}: {
  /** The button's visible text, without the ▾ (which is decoration). */
  buttonText: string;
  /** What the menu is for, announced when it opens. */
  menuLabel: string;
  areas: RefileTarget[];
  onPick: (areaId: string) => void;
  disabled?: boolean;
}): JSX.Element {
  const [open, setOpen] = useState(false);
  const wrapRef = useRef<HTMLSpanElement | null>(null);
  const buttonRef = useRef<HTMLButtonElement | null>(null);

  const items = (): HTMLButtonElement[] =>
    Array.from(wrapRef.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]:not(:disabled)') ?? []);

  // Focus moves INTO the menu on open: a menu opened from the keyboard that
  // leaves focus on the button makes every area a Tab stop away.
  useEffect(() => {
    if (open) items()[0]?.focus();
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onDown = (ev: PointerEvent): void => {
      if (!wrapRef.current?.contains(ev.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", onDown);
    return () => document.removeEventListener("pointerdown", onDown);
  }, [open]);

  return (
    <span
      className="desk-refile"
      ref={wrapRef}
      onKeyDown={(ev) => {
        if (!open) return;
        if (ev.key === "Escape") {
          // The shell's own Escape chain peels a route; this layer goes first.
          ev.stopPropagation();
          ev.preventDefault();
          setOpen(false);
          buttonRef.current?.focus();
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
      <button
        ref={buttonRef}
        type="button"
        className="button"
        aria-expanded={open}
        aria-haspopup="menu"
        disabled={disabled}
        onClick={() => setOpen((v) => !v)}
      >
        {buttonText} ▾
      </button>
      {open ? (
        <div className="desk-refile-menu" role="menu" aria-label={menuLabel}>
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
                setOpen(false);
                onPick(area.id);
              }}
            >
              {area.name}
            </button>
          ))}
        </div>
      ) : null}
    </span>
  );
}
