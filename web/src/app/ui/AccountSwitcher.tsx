// SPDX-License-Identifier: AGPL-3.0-only

// THE FOLDED ACCOUNT SWITCHER (#375), at the top of the rail.
//
//   All accounts ▾ · 1 failing
//
// ONE LINE, CLOSED. The rail used to carry an "Accounts" heading with a row per
// account, which spent a permanent block of the navigation column on a question
// asked a few times a week — and which said nothing about the accounts while it
// sat there. This says the two things worth knowing at a glance (which scope
// you are in, and whether any account has stopped working) and opens to the
// rest.
//
// CHOOSING A SCOPE DOES NOT NAVIGATE ANYWHERE. It rewrites the CURRENT list's
// route with a different `acct`, so the desk you were on reloads as that
// account's desk and the tray, all mail and every count follow — the scope is a
// property of the screen, not a place. It is also written to the per-device
// memory outright, because "all accounts" cannot be inferred from a route that
// simply does not mention one (app/routing.ts setRememberedAccount).
//
// THE HEALTH PANEL IS INSIDE IT. Account health used to be a ♥ button beside
// the accounts heading opening a modal sheet. Both halves of "which accounts do
// I have, and how are they doing" now live in the same disclosure, which is
// also the only way the second half stays reachable now the heading is gone.

import { useCallback, useEffect, useRef, useState } from "react";
import type { JSX } from "react";

import type { Id } from "../../core/jmap/types";
import { withAcct } from "../../core/routes";
import type { Route } from "../../core/routes";
import { navigate, setRememberedAccount, useListRoute } from "../routing";
import { t } from "../i18n";
import { AccountHealthContent } from "./AccountHealthPanel";
import { useShell } from "./shellContext";
import { useAccounts } from "./useAccounts";
import type { AccountEntry, AccountStatus } from "./useAccounts";

/** The word beside an account that is not ok, or null. Nothing is said about a
 * healthy one: a list where every row carries a status word is a list where the
 * one row that matters does not stand out. */
function statusWord(status: AccountStatus): string | null {
  if (status === "paused") return t("switcher.statusPaused");
  if (status === "failing") return t("switcher.statusFailing");
  return null;
}

/**
 * Where this switcher is drawn (#375 increment 7).
 *
 *   `rail`  — the folded line at the top of the navigation rail, which is the
 *             trigger AND the panel, pushing the rail down rather than
 *             covering it (spec §12: nothing is layered over anything).
 *   `sheet` — the phone. NO TRIGGER AT ALL: the desk's own header is the
 *             account name with a `▾` (ui/desk/ScopeLine.tsx), because on a
 *             phone the rail is a pane away behind ☰ and a switcher nobody can
 *             reach is a switcher that does not exist. The panel becomes a
 *             bottom sheet — the one place this design does layer, because a
 *             chooser has to be over the thing it is choosing FOR.
 *
 * ⚠ EXACTLY ONE OF THE TWO IS EVER MOUNTED (ui/Shell.tsx picks by breakpoint),
 * and that is a correctness requirement, not tidiness: both publish
 * `shell.railKeys`, so two mounted copies would leave `g g` and Escape talking
 * to whichever rendered last — which on a `display: none` rail means opening a
 * switcher nobody can see.
 */
export type SwitcherVariant = "rail" | "sheet";

export function AccountSwitcher({
  variant = "rail",
}: { variant?: SwitcherVariant } = {}): JSX.Element | null {
  const { accounts, paused, failing } = useAccounts();
  const listRoute: Route = useListRoute();
  const shell = useShell();
  const scope: Id | null = listRoute.acct ?? null;

  const [open, setOpen] = useState(false);
  const buttonRef = useRef<HTMLButtonElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);
  /** Which row the keyboard is on. Index 0 is "All accounts"; the accounts
   * follow, so this indexes `options` below rather than `accounts`. */
  const [active, setActive] = useState(0);

  /** All accounts first, then each account: the widening is a choice like any
   * other, so it is a row in the same list rather than a "clear" affordance
   * somewhere else. */
  const options: { acct: Id | null; entry: AccountEntry | null }[] = [
    { acct: null, entry: null },
    ...accounts.map((a) => ({ acct: a.id as Id | null, entry: a })),
  ];
  const currentIndex = Math.max(
    0,
    options.findIndex((o) => o.acct === scope),
  );

  /** What had focus when the SHEET was opened — its trigger lives in another
   * component (the desk's header), so there is no `buttonRef` to go back to and
   * closing would otherwise drop a keyboard user on <body>. */
  const returnFocus = useRef<HTMLElement | null>(null);

  const close = useCallback((focusLine: boolean) => {
    setOpen(false);
    // Focus goes back where it came from. A listbox that closes leaving focus
    // on <body> drops a keyboard user at the top of the document, which on this
    // page means tabbing past the whole rail to get back.
    if (!focusLine) return;
    if (buttonRef.current) buttonRef.current.focus();
    else returnFocus.current?.focus();
  }, []);

  const choose = useCallback(
    (acct: Id | null) => {
      // The memory FIRST: navigating fires the route effect that would
      // otherwise re-apply the previous account to a route that deliberately
      // carries none (app/routing.ts applyRemembered).
      setRememberedAccount(acct);
      navigate(withAcct(listRoute, acct));
      close(true);
    },
    [listRoute, close],
  );

  // Opened from the keyboard (`g g`) as well as from the line. Published on
  // every render like the list's own key half, so the shell's single listener
  // always calls the current closures (ui/shellContext.ts says why a ref).
  const { railKeys } = shell;
  useEffect(() => {
    railKeys.current = {
      openSwitcher: () => {
        // Recorded BEFORE the open, because the effect below moves focus into
        // the list the moment it renders.
        returnFocus.current =
          document.activeElement instanceof HTMLElement ? document.activeElement : null;
        setActive(currentIndex);
        setOpen(true);
      },
      closeSwitcher: () => {
        if (!open) return false;
        close(true);
        return true;
      },
      blocksVerbs: () => open,
    };
    return () => {
      railKeys.current = null;
    };
  });

  // Focus moves INTO the list when it opens, so the arrow keys reach it without
  // a Tab, and `aria-activedescendant` reports which row they are on.
  useEffect(() => {
    if (open) listRef.current?.focus();
  }, [open]);

  const onKeyDown = (ev: React.KeyboardEvent): void => {
    switch (ev.key) {
      case "ArrowDown":
        ev.preventDefault();
        setActive((i) => Math.min(options.length - 1, i + 1));
        return;
      case "ArrowUp":
        ev.preventDefault();
        setActive((i) => Math.max(0, i - 1));
        return;
      case "Home":
        ev.preventDefault();
        setActive(0);
        return;
      case "End":
        ev.preventDefault();
        setActive(options.length - 1);
        return;
      case "Enter":
      case " ": {
        ev.preventDefault();
        const option = options[active];
        if (option) choose(option.acct);
        return;
      }
      // Escape is NOT handled here. The shell owns one Escape chain (Shell.tsx),
      // and a second handler closing this as well would peel two layers for one
      // press — the bug routing.ts's consumeEscape exists because of.
      default:
        return;
    }
  };

  const sheet = variant === "sheet";
  const current = scope === null ? null : accounts.find((a) => a.id === scope) ?? null;
  const line = current ? current.name : t("switcher.allAccounts");
  // The trailing clause names ONE problem, worst first, rather than adding up
  // two kinds of trouble into a number that means neither.
  const trouble =
    failing > 0
      ? t("switcher.failing", { n: failing })
      : paused > 0
        ? t("switcher.paused", { n: paused })
        : null;

  const panel = open ? (
    <div className={sheet ? "rail-switcher-panel is-sheet" : "rail-switcher-panel"}>
      <div
        className="rail-switcher-list"
        role="listbox"
        aria-label={t("switcher.label")}
        aria-activedescendant={`acct-opt-${active}`}
        tabIndex={0}
        ref={listRef}
        onKeyDown={onKeyDown}
      >
        {options.map((option, i) => {
          const word = option.entry ? statusWord(option.entry.status) : null;
          return (
            <div
              key={option.acct ?? "all"}
              id={`acct-opt-${i}`}
              role="option"
              aria-selected={option.acct === scope}
              className={`rail-switcher-option${i === active ? " is-active" : ""}`}
              onClick={() => choose(option.acct)}
            >
              {option.entry ? (
                // The chosen colour (#374) overrides the ordinal tone, and the
                // class stays underneath it — see desk/DeskRow.tsx RowAccount.color.
                // `aria-hidden` is right here and only here: this row prints the
                // account's NAME beside the dot and its status word after it, so the
                // dot carries nothing a reader needs.
                <span
                  className={`desk-acct-dot is-acct-${option.entry.tone}`}
                  aria-hidden="true"
                  style={
                    option.entry.color === null ? undefined : { background: option.entry.color }
                  }
                />
              ) : (
                <span className="rail-switcher-nodot" aria-hidden="true" />
              )}
              <span className="rail-switcher-option-name">
                {option.entry ? option.entry.name : t("switcher.allAccounts")}
              </span>
              {/* The word, not only the dot's colour: `--acct-off` is a tone
                  and a tone is never the only carrier of a fact
                  (tokens.css). */}
              {word ? <span className="rail-switcher-status">{word}</span> : null}
            </div>
          );
        })}
      </div>
      <div className="rail-switcher-health">
        <h2 className="nav-heading">{t("switcher.health")}</h2>
        <AccountHealthContent />
      </div>
    </div>
  ) : null;

  // THE SHEET HAS NO TRIGGER OF ITS OWN and renders NOTHING while it is closed
  // — not an empty div, nothing. A bottom sheet is a fixed-position surface;
  // leaving an empty one in the tree is how a stray element ends up eating taps
  // along the bottom edge of a screen that has a bottom bar there.
  if (sheet) {
    if (!open) return null;
    return (
      // `dialog` rather than a bare div: it covers what it is choosing for, so
      // a reader has to be told it is a layer, and Escape already closes it
      // through the shell's one chain (railKeys.closeSwitcher).
      <div className="switcher-sheet" role="dialog" aria-modal="true" aria-label={t("switcher.label")}>
        <div className="switcher-sheet-head">
          <span className="switcher-sheet-title">{line}</span>
          <button
            type="button"
            className="icon-button"
            aria-label={t("action.close")}
            onClick={() => close(true)}
          >
            ✕
          </button>
        </div>
        {panel}
      </div>
    );
  }

  return (
    <div className="rail-switcher">
      <button
        type="button"
        ref={buttonRef}
        className="rail-switcher-line"
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => {
          returnFocus.current = null;
          setActive(currentIndex);
          setOpen((v) => !v);
        }}
      >
        <span className="rail-switcher-name">{line}</span>
        <span className="rail-switcher-caret" aria-hidden="true">
          ▾
        </span>
        {trouble ? <span className="rail-switcher-trouble">· {trouble}</span> : null}
      </button>
      {panel}
    </div>
  );
}
