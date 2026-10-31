// SPDX-License-Identifier: AGPL-3.0-only

// The one modal shell every Settings → Keys dialog uses (#796), so generate,
// import, extend, back up, retire and revoke cannot differ in how they hold
// focus or let go of it.
//
// FOCUS IS TRAPPED, AND THE TRAP STOPS HERE. These dialogs open INSIDE the
// settings page, which is itself a trapped modal (routes/SettingsRoute.tsx).
// Tab is handled here and does not propagate, so the page's own trap never
// sees a Tab that this dialog already placed; Escape closes this dialog only
// (stopPropagation keeps it from also peeling the settings route).
//
// RENDERED INTO <body> THROUGH A PORTAL (KeyPortal). The settings page is a
// `.sheet`, whose transform makes it the containing block of every fixed
// descendant, so a backdrop rendered inside it is fixed to the page's scrolled
// content rather than to the screen: measured, the dialog's header sat above
// the top of the window once the page had scrolled. React still bubbles a
// portal's events through its React parents, which is why the Tab and Escape
// stops above still matter.
//
// Focus starts on `initialFocus` (or the first control) and goes back to the
// control that opened the dialog when it closes, if that is still on the page.
// Closing never happens while `busy`: a revoke half-sent and then dismissed
// would leave the user not knowing whether it happened.

import { useEffect, useId, useRef } from "react";
import { createPortal } from "react-dom";
import type { JSX, ReactNode, RefObject } from "react";

import { t } from "../../../i18n";
import { closeFocusTarget, focusablesIn, trapTab } from "../../focusTrap";

export function KeyDialog({
  title,
  className,
  busy = false,
  onClose,
  initialFocus,
  dismissable = true,
  children,
}: {
  title: string;
  className?: string;
  busy?: boolean;
  onClose: () => void;
  initialFocus?: RefObject<HTMLElement | null>;
  /** false: no ✕, and neither Escape nor a click outside closes it — only a
   * control inside the dialog does (the API key token dialog, #49: closing it
   * is the moment the token is gone for good, so it must be deliberate).
   * Escape is still stopped here, so it cannot peel the page behind. */
  dismissable?: boolean;
  children: ReactNode;
}): JSX.Element {
  const dialogRef = useRef<HTMLDivElement | null>(null);
  const headingId = useId();
  const close = (): void => {
    if (!busy && dismissable) onClose();
  };

  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    const root = dialogRef.current;
    const first = initialFocus?.current ?? (root ? focusablesIn(root)[0] : undefined);
    first?.focus();
    return () => {
      if (closeFocusTarget(opener) === "opener") opener?.focus();
    };
    // Mount and unmount only: the opener is whatever had focus when it opened.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <KeyPortal>
      <div
        className="modal-backdrop"
        onMouseDown={(ev) => {
          if (ev.target === ev.currentTarget) close();
        }}
        onKeyDown={(ev) => {
          if (ev.key === "Escape") {
            ev.stopPropagation();
            close();
            return;
          }
          if (ev.key === "Tab") {
            ev.stopPropagation();
            trapTab(dialogRef.current, ev);
          }
        }}
      >
        <div
          className={`sheet key-dialog${className ? ` ${className}` : ""}`}
          role="dialog"
          aria-modal="true"
          aria-labelledby={headingId}
          ref={dialogRef}
        >
          <header className="sheet-header">
            <h2 id={headingId}>{title}</h2>
            {dismissable ? (
              <button
                type="button"
                className="icon-button"
                aria-label={t("action.close")}
                disabled={busy}
                onClick={close}
              >
                ✕
              </button>
            ) : null}
          </header>
          <div className="sheet-body">{children}</div>
        </div>
      </div>
    </KeyPortal>
  );
}

/**
 * A dialog's layer above the settings page: a portal into <body> (see the
 * header for why), stacked over the page, and a stop for Tab so the page's
 * own trap never acts on a key the dialog inside already handled. Also used
 * around ImportKeyDialog, which has its own trap but does not stop Tab.
 */
export function KeyPortal({ children }: { children: ReactNode }): JSX.Element {
  return createPortal(
    <div
      className="key-portal"
      onKeyDown={(ev) => {
        if (ev.key === "Tab") ev.stopPropagation();
      }}
    >
      {children}
    </div>,
    document.body,
  );
}

/** A yes/no question in the same shell: the body says what happens, the
 * confirm button names the action, and Cancel has focus first. */
export function KeyConfirmDialog({
  title,
  paragraphs,
  confirmLabel,
  busyLabel,
  busy,
  error,
  onConfirm,
  onClose,
}: {
  title: string;
  paragraphs: string[];
  confirmLabel: string;
  busyLabel: string;
  busy: boolean;
  error: string | null;
  onConfirm: () => void;
  onClose: () => void;
}): JSX.Element {
  const cancelRef = useRef<HTMLButtonElement | null>(null);
  return (
    <KeyDialog title={title} busy={busy} onClose={onClose} initialFocus={cancelRef}>
      {paragraphs.map((p) => (
        <p key={p}>{p}</p>
      ))}
      {error ? (
        <p className="field-error" role="alert">
          {error}
        </p>
      ) : null}
      {busy ? (
        <p className="field-hint" role="status">
          {busyLabel}
        </p>
      ) : null}
      <div className="sheet-actions">
        <button type="button" className="button" onClick={onClose} disabled={busy} ref={cancelRef}>
          {t("action.cancel")}
        </button>
        <button type="button" className="button button-danger" disabled={busy} onClick={onConfirm}>
          {confirmLabel}
        </button>
      </div>
    </KeyDialog>
  );
}
