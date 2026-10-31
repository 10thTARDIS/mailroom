// SPDX-License-Identifier: AGPL-3.0-only

// A CONFIRMATION THAT SAYS WHAT IT WILL DO (#375 increment 3).
//
// Generic, because the desk has one of these today (archive a whole drawer) and
// will have more, and because `window.confirm` cannot do the two things that
// matter here: name the consequence in the body, and label the buttons with the
// action rather than "OK".
//
// FOCUS IS TRAPPED, and that is not decoration. This dialog is rendered as a
// sibling of a list whose keyboard verbs are live — `e` archives, `#` trashes —
// so a Tab that walked out of the dialog would put focus on a surface where the
// next keystroke acts on mail while a modal question is still on screen. Escape
// cancels, and cancelling is what the backdrop does too.

import { useEffect, useRef } from "react";
import type { JSX } from "react";

import { t } from "../../i18n";
import { trapTab } from "../focusTrap";

export function ConfirmDialog({
  title,
  body,
  confirmLabel,
  busy,
  onConfirm,
  onCancel,
}: {
  title: string;
  body: string;
  confirmLabel: string;
  busy: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}): JSX.Element {
  const dialogRef = useRef<HTMLDivElement | null>(null);
  const cancelRef = useRef<HTMLButtonElement | null>(null);

  // CANCEL TAKES FOCUS, not the confirm button: this dialog's confirm is a bulk
  // action over hundreds of messages, and a dialog that opens with the
  // destructive answer under the return key is one people confirm by reflex.
  useEffect(() => {
    cancelRef.current?.focus();
  }, []);

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(ev) => {
        if (ev.target === ev.currentTarget) onCancel();
      }}
      onKeyDown={(ev) => {
        if (ev.key === "Escape") {
          ev.stopPropagation();
          onCancel();
          return;
        }
        // The trap: the two edges wrap onto each other, so focus can never reach
        // the live list behind this (ui/focusTrap.ts, shared with the viewer and
        // the composer since #447).
        trapTab(dialogRef.current, ev);
      }}
    >
      <div className="sheet desk-confirm" role="dialog" aria-modal="true" aria-label={title} ref={dialogRef}>
        <header className="sheet-header">
          <h2>{title}</h2>
        </header>
        <div className="sheet-body">
          <p>{body}</p>
          <div className="sheet-actions">
            <button type="button" className="button" onClick={onCancel} ref={cancelRef}>
              {t("action.cancel")}
            </button>
            <button type="button" className="button button-primary" disabled={busy} onClick={onConfirm}>
              {confirmLabel}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
