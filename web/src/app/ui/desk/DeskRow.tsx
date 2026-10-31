// SPDX-License-Identifier: AGPL-3.0-only

// ONE ROW OF THE DESK (#375 increment 3).
//
// The message list's row is a control panel: a checkbox, a star, and six hover
// buttons that appear under the pointer. The desk's row is deliberately not.
// There are NO ROW ACTIONS AND NO ROW BUTTONS here beyond the two things a
// click can mean, and hover only highlights — because the desk's premise is
// that you act on mail with the keyboard or with a selection, and a row that
// grows six targets when the pointer crosses it is a row you cannot read while
// the pointer is anywhere near it.
//
// CLICKING THE SENDER SELECTS; CLICKING ANYTHING ELSE OPENS. That replaces the
// checkbox column: selection is a frequent, deliberate gesture on this surface
// (re-file these four, archive these nine), and a 16-pixel target for it is how
// you get people reaching for the mouse. The sender column is the widest stable
// target on the row that is not the subject.
//
// EVERY PIECE OF SENDER-CONTROLLED TEXT GOES THROUGH THE HOSTILE-TEXT LAYER
// (#307): the name is `senderIdentity`, which decides whether a display name is
// confusable enough that the address has to be shown beside it, and the subject
// and preview are bidi-stripped. A row is one line box, which is exactly the
// shape a directional override reorders.

import { useCallback, useRef, useState } from "react";
import type { JSX } from "react";

import { senderIdentity, senderText, stripBidiControls } from "../../../core/hostileText";
import { senderWarningsList } from "../../../core/senderWarnings";
import type { EffectiveEmail } from "../../../core/store";
import type { Id } from "../../../core/jmap/types";
import { t } from "../../i18n";
import { aliasOf, rowTime } from "./format";
import { warningText } from "./rowName";
import { swipeDecision } from "./swipe";
import type { SwipeAction } from "./swipe";

/** How long a finger has to rest on a row before it selects it (#375 increment
 * 7). Half a second is the platform convention on both mobile platforms; below
 * it a slow tap selects, above it the gesture stops feeling connected to the
 * press. */
export const LONG_PRESS_MS = 500;

/** How far the pointer may move before a press stops being a long press. Small,
 * because this only has to survive the wobble of a finger holding still — a
 * genuine drag is a swipe or a scroll, and both must cancel the timer. */
const LONG_PRESS_SLOP_PX = 8;

/** What one in-flight gesture is, while it is in flight. A ref rather than
 * state: every pointermove would otherwise re-render a virtualised row. */
interface Drag {
  pointerId: number;
  startX: number;
  startY: number;
  rowWidth: number;
  /** Set once the pointer has moved past the slop — the long press is off and
   * this is a drag now. */
  moved: boolean;
  /**
   * The FURTHEST the pointer ever strayed off the horizontal, not how far off
   * it happened to end.
   *
   * A finger that went 90px down the list and came back to where it started
   * finishes at `dy = 0`, and judging the gesture on that number alone would
   * read a scroll-and-return as a clean swipe — which archives a message the
   * user was scrolling past. The veto is about the PATH, so the path is what is
   * remembered.
   */
  maxDy: number;
}

/** Which account tone a row's dot carries. Three, not one per account: colour
 * is an identity CUE beside a name (tokens.css "never colour alone"), and a
 * picker with a hue per account and nothing else is a matching game. */
export type AccountTone = "1" | "2" | "off";

export interface RowAccount {
  name: string;
  tone: AccountTone;
  /**
   * The account's CHOSEN colour (#374), or null to use the tone.
   *
   * ⚠ AN INLINE STYLE, DELIBERATELY, and it is the one colour in this app that is
   * not a token. `Account.color` is a single stored `#RRGGBB` — it cannot be a
   * token, because a token resolves per theme and this value does not. The token
   * check scans CSS files, so a server value in a `style` prop is not a token
   * violation; the CLASS is still rendered underneath it, so the dot has a colour
   * either way.
   *
   * The dot keeps its `title`/`aria-label` regardless: this is a cue, never the
   * carrier of the fact (tokens.css "never colour alone").
   */
  color: string | null;
}

export function DeskRow({
  email,
  selected,
  isCursor,
  isOpen,
  threadCount,
  account,
  queued,
  now,
  onOpen,
  onToggleSelect,
  onSwipe,
  swipeFlagLabel,
  reducedMotion = false,
}: {
  email: EffectiveEmail;
  selected: boolean;
  isCursor: boolean;
  isOpen: boolean;
  /** How many messages are in this conversation, when the thread is known and
   * holds more than one. Null otherwise — a "1" pill on every row would be
   * noise, and a pill on a thread we have not loaded would be a guess. */
  threadCount: number | null;
  /** The account cue, or null when the desk is scoped to one account or the
   * user only has one — a dot that is always the same colour says nothing. */
  account: RowAccount | null;
  /**
   * This row's change has not reached the server yet (#375 increment 6): there
   * is a local intent for it AND the queue is holding something.
   *
   * Both halves are needed and neither is sufficient. An overlay entry alone is
   * the NORMAL state of an optimistic action for the second or two before the
   * server agrees — tagging every one of those would put "waiting to send" on
   * half the desk every time you press a key. A non-empty queue alone says
   * nothing about WHICH row. Together they are the honest claim: something is
   * unsent, and this row is one of the things it could be.
   */
  queued: boolean;
  now: number;
  /** A PLAIN click on the message cell — never a modified one, which the row's
   * capture handler has already turned into `onToggleSelect`. The routes clear
   * the ticked selection here as well as opening (DeskRoute `openFromRow`). */
  onOpen: (id: Id) => void;
  onToggleSelect: (id: Id, shift: boolean) => void;
  /**
   * The phone's two gestures (#375 increment 7), or absent.
   *
   * ITS PRESENCE IS THE SWITCH. When it is not supplied the row attaches no
   * pointer handlers at all — not disabled ones, none — so the desktop row is
   * byte-for-byte the interaction it always was, and a mouse cannot
   * accidentally archive by dragging across a list. The route supplies it only
   * below the phone breakpoint (ui/useMediaQuery.ts), and only while nothing
   * is selected: with a selection on screen the toolbar is the path, and a
   * swipe would have to guess whether it meant the row or the selection.
   *
   * Right is archive, left is flag — what "flag" means is the surface's
   * business (the desk puts mail IN the tray, the tray page takes it out).
   */
  onSwipe?: (id: Id, action: SwipeAction) => void;
  /** What a left swipe is called ON THIS SURFACE, for the word revealed behind
   * the row: the desk puts mail in the tray, the tray page takes it out. The
   * same keyword, two vocabularies — the argument ui/undoText.ts makes about
   * the toast, applied to the gesture that raises it. */
  swipeFlagLabel?: string;
  /** Someone who asked for less movement: the reveal behind the row is not
   * drawn, and the row does not follow the finger. The GESTURE still works and
   * still acts — what is dropped is the animation, not the ability. */
  reducedMotion?: boolean;
}): JSX.Element {
  // -- the phone's gestures (#375 increment 7) --------------------------------
  //
  // POINTER EVENTS, NO LIBRARY. One `pointerdown`, the moves that follow it and
  // the release, all of which reach this element by bubbling — a touch pointer
  // is implicitly captured to the element it went down on, which is why a
  // handler on the row sees the whole sequence without asking for capture (and
  // why asking for it would break the tap; see onPointerDown).
  //
  // `touch-action: pan-y` in the stylesheet is the other half: it tells the
  // browser this row scrolls VERTICALLY and leaves the horizontal axis to
  // JavaScript, so the two gestures do not fight over the same finger. It is
  // also the first line of defence against a scroll being read as a swipe — the
  // browser fires `pointercancel` the moment it takes the gesture over — and
  // `maxDy` below is the second, for the moments before it decides.
  const drag = useRef<Drag | null>(null);
  const longPress = useRef<ReturnType<typeof setTimeout> | null>(null);
  /**
   * ⚠ A GESTURE MUST NOT ALSO BE A TAP. The row's two buttons are what a tap
   * lands on, and the browser fires their `click` after the pointer sequence
   * whatever else happened in between — so without this, a swipe would archive
   * the message AND open it, and a long press would select it AND open it. The
   * flag is set by whichever gesture completed and consumed by the very next
   * click, in the CAPTURE phase, before the button's own handler sees it.
   */
  const swallowClick = useRef(false);
  const [dx, setDx] = useState(0);

  const cancelLongPress = useCallback(() => {
    if (longPress.current !== null) clearTimeout(longPress.current);
    longPress.current = null;
  }, []);

  const endDrag = useCallback(() => {
    cancelLongPress();
    drag.current = null;
    setDx(0);
  }, [cancelLongPress]);

  const onPointerDown = (ev: React.PointerEvent<HTMLDivElement>): void => {
    if (!onSwipe) return;
    // Secondary buttons and a second finger are not gestures this row has.
    if (ev.button !== 0 || drag.current !== null) return;
    const box = ev.currentTarget.getBoundingClientRect();
    drag.current = {
      pointerId: ev.pointerId,
      startX: ev.clientX,
      startY: ev.clientY,
      rowWidth: box.width,
      moved: false,
      maxDy: 0,
    };
    // ⚠ DELIBERATELY NO `setPointerCapture`. Capturing on THIS element would
    // retarget the whole sequence to it, and the browser fires `click` at the
    // capture element rather than at the button under the finger — so every
    // ordinary tap would stop opening the message, on the surface where a tap
    // is the only way to open one. Touch input is implicitly captured to the
    // pointerdown target anyway (Pointer Events §"implicit pointer capture"),
    // so the moves and the release still bubble here through the button; the
    // mouse case is covered by `onPointerLeave` below, and the click a finished
    // gesture would otherwise leave behind by `swallowClick`.
    //
    // LONG PRESS SELECTS. Not on the sender column only: clicking the sender is
    // a MOUSE affordance for selecting, and on a phone the gesture that means
    // the same thing has the whole row as its target.
    longPress.current = setTimeout(() => {
      longPress.current = null;
      drag.current = null;
      setDx(0);
      swallowClick.current = true;
      onToggleSelect(email.id, false);
    }, LONG_PRESS_MS);
  };

  const onPointerMove = (ev: React.PointerEvent<HTMLDivElement>): void => {
    const d = drag.current;
    if (!d || d.pointerId !== ev.pointerId) return;
    const moveX = ev.clientX - d.startX;
    const moveY = ev.clientY - d.startY;
    d.maxDy = Math.max(d.maxDy, Math.abs(moveY));
    if (!d.moved && Math.hypot(moveX, moveY) > LONG_PRESS_SLOP_PX) {
      d.moved = true;
      cancelLongPress();
    }
    if (d.moved && !reducedMotion) setDx(moveX);
  };

  const onPointerUp = (ev: React.PointerEvent<HTMLDivElement>): void => {
    const d = drag.current;
    if (!d || d.pointerId !== ev.pointerId) {
      endDrag();
      return;
    }
    const action = d.moved
      ? swipeDecision(
          ev.clientX - d.startX,
          // The worst drift on the way, not the drift at the end — see `maxDy`.
          Math.max(d.maxDy, Math.abs(ev.clientY - d.startY)),
          d.rowWidth,
        )
      : null;
    if (action) swallowClick.current = true;
    endDrag();
    // AFTER the state reset, so the row is back under the finger before
    // anything re-renders it away — and only for a decided gesture: an
    // undecided one is a tap, which the buttons below already handle.
    if (action && onSwipe) onSwipe(email.id, action);
  };

  const sender = senderIdentity(email.from?.[0]);
  const senderLabel = senderText(sender, t("list.noSender"));
  const subject = stripBidiControls(email.subject) || t("list.noSubject");
  const preview = stripBidiControls(email.preview);
  const alias = aliasOf(email.deliveredTo);
  // `senderWarnings` is a BODY property (jmap/types.ts EMAIL_BODY_PROPERTIES),
  // so on a row that has only ever been fetched as part of a list this is
  // empty and the mark is simply absent. That is the honest behaviour for a
  // warning: never shown when it was not asked for, never invented.
  const [warning] = senderWarningsList(email.senderWarnings);

  const className = [
    "desk-row",
    email.isSeen ? "is-read" : "is-unread",
    selected ? "is-selected" : "",
    isCursor ? "is-cursor" : "",
    isOpen ? "is-open" : "",
    onSwipe ? "is-swipeable" : "",
    dx !== 0 ? "is-swiping" : "",
  ]
    .filter(Boolean)
    .join(" ");

  // What the row is currently ABOUT to do, so the reveal names the action
  // rather than being a coloured shape. Null until the travel would actually
  // commit, so a hesitant drag says nothing rather than promising an archive
  // it will not perform.
  const pending: SwipeAction | null =
    dx === 0 ? null : swipeDecision(dx, 0, drag.current?.rowWidth ?? 0);

  const row = (
    <div
      className={className}
      // The grid's row is the `li` around this; this is the box that lays the
      // three cells out, so it is removed from the accessibility tree rather than
      // sitting between a row and its cells as an unnamed group.
      role="presentation"
      {...(onSwipe
        ? {
            onPointerDown,
            onPointerMove,
            onPointerUp,
            onPointerCancel: endDrag,
            // The mouse's escape hatch: a touch pointer is implicitly captured
            // and cannot leave mid-gesture, but a mouse dragged off the row
            // would never deliver its pointerup here and the row would stay
            // translated with a drag that never ends.
            onPointerLeave: endDrag,
          }
        : {})}
      // A MODIFIED CLICK ANYWHERE ON THE ROW SELECTS (#708). Shift extends the
      // selection from the anchor; Ctrl (Cmd on macOS) toggles just this row, so
      // a selection need not be contiguous. Without a modifier the cells keep
      // their own meaning: the sender selects, everything else opens. Users
      // found the sender-only gesture by being told about it, which is
      // the evidence that one target was not enough.
      //
      // CAPTURE, so it runs before the main cell's open handler and can stop it.
      // The swipe's swallowed click (a drag that ended over the row) is checked
      // first: a drag is never a selection either.
      onClickCapture={(ev: React.MouseEvent<HTMLDivElement>) => {
        if (onSwipe && swallowClick.current) {
          swallowClick.current = false;
          ev.preventDefault();
          ev.stopPropagation();
          return;
        }
        if (!(ev.shiftKey || ev.ctrlKey || ev.metaKey)) return;
        ev.preventDefault();
        ev.stopPropagation();
        onToggleSelect(email.id, ev.shiftKey);
      }}
      // Shift+mousedown would otherwise start a TEXT selection from the last
      // click to here, painting half the list blue on every range select.
      onMouseDown={(ev: React.MouseEvent<HTMLDivElement>) => {
        if (ev.shiftKey) ev.preventDefault();
      }}
      style={dx === 0 ? undefined : { transform: `translateX(${dx}px)` }}
    >
      {/* THE ROW'S THREE CELLS (#447). The `li` above this is the grid ROW
          (ui/desk/VirtualList.tsx) and these are its cells, so a screen reader
          walking a row column by column hears the sender, then the subject, then
          the marks — the way a mail list has always been read.

          The wrappers are real boxes, not `display: contents`: an element that
          generates no box is missing from the accessibility tree in some engines,
          which would silently delete the one thing they exist for. So each cell
          IS the grid item now (app/desk.css .desk-cell-*) and the control inside
          it fills it, which is the same rectangle the control was before.

          `.desk-row` itself is `role="presentation"`: it is the flex/grid
          container, and a generic box between a row and its cells is exactly what
          that role removes from the tree. */}
      <span role="gridcell" className="desk-cell desk-cell-sender">
        <button
          type="button"
          className="desk-sender"
          aria-pressed={selected}
          aria-label={t("desk.row.select", { sender: senderLabel })}
          // NOT IN THE TAB ORDER (#447). The grid is one tab stop and the keyboard
          // acts on the CURSOR row (Space selects, Enter opens); two tab stops per
          // row meant Tab took forty presses to cross a screenful, and every one of
          // them announced the same message twice. A click still focuses it, which
          // is what a pointer user expects.
          tabIndex={-1}
          onClick={(ev) => onToggleSelect(email.id, ev.shiftKey)}
        >
          {account ? (
            <span
              className={`desk-acct-dot is-acct-${account.tone}`}
              // `role="img"` because ARIA does not require an `aria-label` on a bare
              // `<span>` to be exposed at all — which made the account a fact
              // carried by hue alone. The row's own name says it too
              // (ui/desk/rowName.ts); this is what makes the DOT sayable.
              role="img"
              title={account.name}
              aria-label={account.name}
              style={account.color === null ? undefined : { background: account.color }}
            />
          ) : null}
          <span className="desk-sender-name">
            {sender.name || sender.address || t("list.noSender")}
            {sender.showAddress ? (
              <span className="desk-sender-address" title={t("list.senderAddressShown")}>
                {" "}
                &lt;{sender.address}&gt;
              </span>
            ) : null}
          </span>
          {warning ? (
            <span
              className="desk-warning"
              role="img"
              title={warningText(warning)}
              aria-label={warningText(warning)}
            >
              ⚠
            </span>
          ) : null}
        </button>
      </span>

      <span role="gridcell" className="desk-cell desk-cell-main">
        <button
          type="button"
          className="desk-main"
          tabIndex={-1}
          onClick={() => onOpen(email.id)}
          // The ACTION and the subject, not the sender and the subject: the row's
          // own name already said who it is from (ui/desk/rowName.ts), and a reader
          // who tabs into this cell should hear what the button does.
          //
          // ⚠ AND IT MUST CONTAIN THE VISIBLE TEXT (WCAG 2.5.3). The snippet is
          // inside this button, so a name of "Open {subject}" leaves out half of
          // what a voice-control user can see and say. axe-core 4.14 enforces
          // this (label-content-name-mismatch); it compares the name against the
          // visible text, so aria-hidden on the snippet would not satisfy it.
          aria-label={
            preview
              ? t("desk.row.openWithPreview", { subject, preview })
              : t("desk.row.open", { subject })
          }
        >
          <span className="desk-subject">{subject}</span>
          {/* A real space between the two boxes. It renders nothing (whitespace
              between flex or grid items is dropped), but text read off the DOM
              would otherwise join the last word of the subject to the first
              of the snippet, which is how axe saw "photographssee". */}
          {" "}
          {/* The snippet trails the subject on a wide list and is hidden by a
              container query when the reading pane narrows this column — it is
              the first thing worth losing, and losing it is better than
              squeezing the subject to four characters. */}
          <span className="desk-preview">{preview}</span>
        </button>
      </span>

      <span role="gridcell" className="desk-meta">
        {/* Muted and wordy rather than a glyph: it is not a property of the
            message, it is a statement about this client's connection, and the
            header sentence above is already carrying the reason. */}
        {queued ? (
          <span className="desk-tag is-queued" title={t("row.waitingToSend")}>
            {t("row.waitingToSend")}
          </span>
        ) : null}
        {alias ? (
          <span className="desk-alias" title={t("desk.row.alias", { alias })}>
            {alias}
          </span>
        ) : null}
        {threadCount !== null ? (
          <span className="desk-thread" role="img" aria-label={t("desk.row.thread", { n: threadCount })}>
            {threadCount}
          </span>
        ) : null}
        {email.isPinned ? (
          <span className="desk-glyph" role="img" title={t("desk.row.pinned")} aria-label={t("desk.row.pinned")}>
            📌
          </span>
        ) : null}
        {email.isFlagged ? (
          <span className="desk-glyph is-flag" role="img" title={t("desk.row.flagged")} aria-label={t("desk.row.flagged")}>
            ⚑
          </span>
        ) : null}
        {email.hasAttachment ? (
          <span className="desk-glyph" role="img" title={t("list.attachment")} aria-label={t("list.attachment")}>
            📎
          </span>
        ) : null}
        {email.pending ? (
          <span className="desk-glyph" role="img" title={t("list.pending")} aria-label={t("list.pending")}>
            ◌
          </span>
        ) : null}
        <time className="desk-time" dateTime={email.receivedAt}>
          {rowTime(email.receivedAt, now)}
        </time>
      </span>
    </div>
  );

  if (!onSwipe) return row;

  // THE REVEAL IS BEHIND THE ROW, not a decoration on it: the row slides and
  // the word underneath is what it is sliding to reveal, which is how the
  // gesture explains itself the first time. `aria-hidden` because the ACTION
  // announces itself — every one of these registers an undo toast, and that
  // toast is a live region saying what happened in a whole sentence.
  return (
    <div className="desk-row-swipe" role="presentation">
      <span className="desk-row-reveal" aria-hidden="true">
        <span className="desk-row-reveal-left">
          {pending === "archive" ? t("action.archive") : ""}
        </span>
        <span className="desk-row-reveal-right">
          {pending === "flag" ? swipeFlagLabel ?? t("desk.selection.tray") : ""}
        </span>
      </span>
      {row}
    </div>
  );
}
