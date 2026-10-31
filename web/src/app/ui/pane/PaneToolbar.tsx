// SPDX-License-Identifier: AGPL-3.0-only

// THE READING PANE'S ACTION ROW (#375 increment 5; Reply all added by #646).
//
//   ← · Archive · In-tray · Snooze · Mute · [Reply] · Reply all · More ▾
//
// WORDS, NOT GLYPHS, AND NO KEY LEGENDS ANYWHERE. The row this replaces mixed
// six word buttons with a bare ★ and printed nothing about what any of them
// did; the desk's own toolbar (desk/SelectionToolbar.tsx) settled that argument
// already and this is the same answer, for the same reasons. A control
// annotated with its keystroke teaches shortcuts to the people already using a
// mouse and clutters the row for everyone else — the Keys sheet is the ONE
// place keys are listed.
//
// ONE PRIMARY BUTTON, AND IT IS REPLY. A toolbar where seven controls look
// identical has no primary action, so the eye has to read all seven every time.
// Reply is what a person opens a message to do; everything destructive or
// irreversible stays an ordinary button, and the rest is behind More.
//
// WHAT IS IN "MORE" AND WHY: Trash, Pin, Important, Label, Create rule, Print,
// and — only for a message the classifier still has a question about — Dismiss.
// They are the verbs used once a week rather than once a minute. Dismiss is
// CONDITIONAL because "does not need a category" is meaningless for a message
// nobody asked a question about, and a permanently-disabled item is worse than
// an absent one.

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { JSX } from "react";

import { t } from "../../i18n";

/** One entry of the More menu. `key` is React's, and the identity a test would
 * name it by. */
export interface PaneMenuItem {
  key: string;
  label: string;
  onSelect: () => void;
  /** Rendered as aria-pressed for the toggles (pin, important), so a screen
   * reader hears the state rather than inferring it from a changing word. */
  pressed?: boolean;
}

export interface PaneToolbarProps {
  /** In-tray: the `$flagged` keyword (#444). "Remove from tray" when it is
   * already there — the button says what it will DO, not what is true. */
  inTray: boolean;
  muted: boolean;
  snoozed: boolean;
  /**
   * The phone's form (#375 increment 7): the row docks to the bottom at the
   * thumb and carries FOUR buttons — Archive · In-tray · Snooze · Reply — with
   * Reply all (#646, moved by #798) and Mute moved into More.
   *
   * MOVED, NOT HIDDEN. A `display: none` button is still a control the user
   * cannot reach and a menu that is missing an entry it should have; making it
   * the first item of More is the same verb one tap further away, which is what
   * "the rest under More ▾" means. Which is also why this is a prop and not a
   * media query in the stylesheet: it changes what the menu CONTAINS.
   */
  compact?: boolean;
  onBack: () => void;
  onArchive: () => void;
  /**
   * Set ONLY for a message in Junk (#528, spec §6): the row's first verb becomes
   * "Not junk", in the slot Archive has for every other message.
   *
   * REPLACED, NOT ADDED. Archiving spam is not a rescue — it files the message
   * somewhere nobody looks and sends the provider no ham signal — so offering it
   * beside the one action §6 makes mandatory would be offering the wrong one
   * first. It is the same trade the message list's selection toolbar makes for
   * Trash, where Restore takes Archive's place. Delete is still under More.
   */
  onNotJunk?: (() => void) | undefined;
  onTray: () => void;
  onSnooze: () => void;
  onMute: () => void;
  onReply: () => void;
  /** Reply all (#646), beside Reply — see ui/compose/recipients.ts for who it
   * addresses and why that differs from Reply. */
  onReplyAll: () => void;
  /** Everything behind More ▾, in the order it is shown. */
  menu: PaneMenuItem[];
}

/**
 * The More menu: a real `role="menu"` with arrow keys, Home/End, Escape and a
 * click-outside, rather than a div that happens to contain buttons.
 *
 * ESCAPE IS STOPPED HERE, ON PURPOSE. The shell owns one window-level keydown
 * chain (ui/Shell.tsx), and its Escape peels a ROUTE — closing the open
 * message. A menu that closed itself and let the keystroke through would peel
 * two layers for one press, which is the exact bug routing.ts's
 * `consumeEscape` exists to prevent for sheets. This menu does not navigate, so
 * it stops the event instead.
 */
function MoreMenu({ items, onClose }: { items: PaneMenuItem[]; onClose: () => void }): JSX.Element {
  const ref = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    // The first item, so the menu is usable from the keyboard the instant it
    // opens — a menu you have to Tab into is a menu the keyboard user pays for.
    const first = ref.current?.querySelector<HTMLButtonElement>("[role=menuitem]");
    first?.focus();
  }, []);

  useEffect(() => {
    const onPointerDown = (ev: PointerEvent): void => {
      if (!ref.current?.contains(ev.target as Node)) onClose();
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [onClose]);

  const move = useCallback((delta: number | "first" | "last") => {
    const el = ref.current;
    if (!el) return;
    const all = [...el.querySelectorAll<HTMLButtonElement>("[role=menuitem]")];
    if (all.length === 0) return;
    const here = all.indexOf(document.activeElement as HTMLButtonElement);
    const next =
      delta === "first"
        ? 0
        : delta === "last"
          ? all.length - 1
          : (here + delta + all.length) % all.length;
    all[next]?.focus();
  }, []);

  return (
    <div
      ref={ref}
      className="pane-menu"
      role="menu"
      aria-label={t("pane.more.label")}
      onKeyDown={(ev) => {
        switch (ev.key) {
          case "ArrowDown":
            ev.preventDefault();
            move(1);
            return;
          case "ArrowUp":
            ev.preventDefault();
            move(-1);
            return;
          case "Home":
            ev.preventDefault();
            move("first");
            return;
          case "End":
            ev.preventDefault();
            move("last");
            return;
          case "Escape":
            ev.preventDefault();
            // See the note above: the shell's Escape would ALSO close the open
            // message.
            ev.stopPropagation();
            onClose();
            return;
          default:
        }
      }}
    >
      {items.map((item) => (
        <button
          key={item.key}
          type="button"
          role="menuitem"
          tabIndex={-1}
          className="pane-menu-item"
          {...(item.pressed === undefined ? {} : { "aria-pressed": item.pressed })}
          onClick={() => {
            onClose();
            item.onSelect();
          }}
        >
          {item.label}
        </button>
      ))}
    </div>
  );
}

export function PaneToolbar({
  inTray,
  muted,
  snoozed,
  compact: compactProp = false,
  onBack,
  onArchive,
  onNotJunk,
  onTray,
  onSnooze,
  onMute,
  onReply,
  onReplyAll,
  menu,
}: PaneToolbarProps): JSX.Element {
  const [menuOpen, setMenuOpen] = useState(false);
  const moreRef = useRef<HTMLButtonElement | null>(null);

  // THE ROW FITS ITS PANE, WHATEVER THE SPLIT (#803). The eight-button row needs
  // about 574 px; at 1440 px the default split gives the pane about 493, and the
  // row ran under the pane's edge, so the WHOLE pane scrolled sideways
  // (`.thread-pane`'s `overflow-y: auto` makes x `auto` too, the #798 mechanism).
  // A fixed breakpoint cannot know the pane's width, which is the user's split,
  // so the row measures itself: when its buttons need more than the row has, it
  // takes the phone's form (Reply all and Mute move into More, the same verbs
  // one tap further away), and it takes the full form back once the row is at
  // least as wide as the full form was measured to need. The width the full row
  // needed is remembered, because a squeezed row cannot say how wide the full
  // one would be.
  const rowRef = useRef<HTMLDivElement | null>(null);
  const [squeezed, setSqueezed] = useState(false);
  const fullNeeds = useRef(0);
  useLayoutEffect(() => {
    const row = rowRef.current;
    if (!row) return;
    const check = (): void => {
      if (!squeezed) {
        if (row.scrollWidth > row.clientWidth + 1) {
          fullNeeds.current = row.scrollWidth;
          setSqueezed(true);
        }
      } else if (row.clientWidth >= fullNeeds.current) {
        setSqueezed(false);
      }
    };
    check();
    if (typeof ResizeObserver !== "function") return;
    const ro = new ResizeObserver(check);
    ro.observe(row);
    return () => ro.disconnect();
  }, [squeezed, compactProp]);
  const compact = compactProp || squeezed;

  const close = useCallback(() => {
    setMenuOpen(false);
    // Focus goes back where it came from: a menu that closes into nowhere
    // leaves a keyboard user at the top of the document.
    moreRef.current?.focus();
  }, []);

  // The verbs that came OUT of the row go first in More, because those are the
  // ones a hand reaching for them will look for first. Reply all leads: it is
  // the one used daily. It left the phone's row in #798. #646 had added it to
  // both forms, and a sixth button needed 416 px. On a 375 px phone that made
  // the whole reading pane scroll sideways (`.thread-pane`'s `overflow-y: auto`
  // makes x `auto` too), with the subject and body cut off at the left.
  const items: PaneMenuItem[] = compact
    ? [
        { key: "replyAll", label: t("action.replyAll"), onSelect: onReplyAll },
        {
          key: "mute",
          label: muted ? t("action.unmute") : t("action.mute"),
          onSelect: onMute,
          pressed: muted,
        },
        ...menu,
      ]
    : menu;

  return (
    <div ref={rowRef} className={squeezed ? "thread-toolbar is-squeezed" : "thread-toolbar"} role="toolbar" aria-label={t("pane.toolbar")}>
      {/* Hidden on the phone by the stylesheet, where the pane's TOP LINE
          carries the back affordance ("← Inbox · 3 of 6") and this row has
          docked to the other end of the screen. Two backs, one at each end,
          would be the same action twice. */}
      <button type="button" className="icon-button pane-back" aria-label={t("action.back")} onClick={onBack}>
        ←
      </button>
      {onNotJunk ? (
        <button type="button" className="button" onClick={onNotJunk}>
          {t("action.notJunk")}
        </button>
      ) : (
        <button type="button" className="button" onClick={onArchive}>
          {t("action.archive")}
        </button>
      )}
      <button type="button" className="button" aria-pressed={inTray} onClick={onTray}>
        {inTray ? t("tray.remove") : t("desk.selection.tray")}
      </button>
      <button
        type="button"
        className={`button ${snoozed ? "is-on" : ""}`}
        aria-pressed={snoozed}
        onClick={onSnooze}
      >
        {t("action.snooze")}
      </button>
      {compact ? null : (
        <button
          type="button"
          className={`button ${muted ? "is-on" : ""}`}
          aria-pressed={muted}
          onClick={onMute}
        >
          {muted ? t("action.unmute") : t("action.mute")}
        </button>
      )}
      <button type="button" className="button button-primary" onClick={onReply}>
        {t("action.reply")}
      </button>
      {compact ? null : (
        <button type="button" className="button" onClick={onReplyAll}>
          {t("action.replyAll")}
        </button>
      )}
      <span className="pane-more">
        <button
          ref={moreRef}
          type="button"
          className="button"
          aria-expanded={menuOpen}
          aria-haspopup="menu"
          onClick={() => setMenuOpen((v) => !v)}
        >
          {t("pane.more")} ▾
        </button>
        {menuOpen ? <MoreMenu items={items} onClose={close} /> : null}
      </span>
    </div>
  );
}
