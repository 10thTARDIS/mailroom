// SPDX-License-Identifier: AGPL-3.0-only

// The seam between the shell and whatever route is inside it (#373).
//
// App.tsx used to be 1,179 lines because everything that two surfaces both
// needed had to live in the one component that contained them both. Splitting
// the shell from the route targets does not make that need go away — the rail
// still lights up for a drag that starts in the message list, the header still
// renders the splitter whose size the list pane obeys, and the keyboard still
// has to be ONE ordered chain rather than two listeners racing. So the seam is
// declared here, deliberately small, instead of being re-created as prop
// drilling through three files.
//
// WHY THE KEYBOARD IS A REF AND NOT PROPS. There is exactly one window keydown
// listener, and it lives in the shell (which is the thing that is always
// mounted and knows the route). The list publishes its half of the chain here
// on every render; the shell reads it inside the event, so it always sees the
// current closure without re-registering a listener whose dependency list would
// otherwise be the twenty-odd values today's handler closes over. Two
// listeners would be the alternative, and their order would depend on React's
// effect ordering — which changes the moment either component's dependencies
// change. Escape would then sometimes back out of a route before the palette on
// top of it had a chance to close.

import { createContext, useContext } from "react";
import type { MutableRefObject } from "react";

import type { Id } from "../../core/jmap/types";
import type { PaneLayout } from "../prefs";
import type { RuleDraft } from "./RuleEditor";

/** The list surface's half of the key chain. Each method reports whether it
 * consumed the keystroke, so the shell's chain can fall through in the same
 * order today's single handler did. */
export interface ListKeys {
  /** Ctrl/⌘+K. */
  openPalette: () => void;
  /** Escape, innermost first: the command palette, then the label menu (which
   * also abandons an armed queue batch, #176), then the snooze menu. */
  closeTopPopover: () => boolean;
  /** Escape, after the route has had its turn. */
  clearSelection: () => boolean;
  /** True while something the list owns should swallow the plain verb keys —
   * the same set today's handler guards on, no more. */
  blocksVerbs: () => boolean;
  /** j/k/o/e/#/s/p/b/!/m/x/r/l/-/I/U — everything that needs the list. */
  handleVerb: (ev: KeyboardEvent) => boolean;
}

/**
 * The RAIL's half of the key chain (#375). Small, and for the same reason the
 * list's is a ref rather than props: there is one window keydown listener, it
 * lives in the shell, and `g g` has to reach a popover the rail owns without
 * the shell re-registering its listener whenever the rail re-renders.
 */
export interface RailKeys {
  /** `g g`, and the switcher line's own click. */
  openSwitcher: () => void;
  /** Escape, in the shell's one chain — so the switcher is not also closing
   * itself on the same keystroke and peeling two layers for one press. */
  closeSwitcher: () => boolean;
  /** True while the open switcher should swallow the plain verb keys. Its
   * listbox is a div, so `isTyping` does not cover it and `e` would otherwise
   * archive the selection behind it. */
  blocksVerbs: () => boolean;
}

/**
 * ONE OPEN ATTACHMENT VIEWER (#449), named by the message it belongs to rather
 * than by the bytes it shows.
 *
 * ⚠ IT CARRIES IDS, NOT PARTS, and that is deliberate: the viewer reads the
 * attachments and the scan verdicts LIVE out of the store. Releasing a held file
 * reloads the whole thread — the server's decision is keyed on the part's content
 * hash and can affect every message sharing those bytes — and that reload is what
 * turns the held panel into the file. A request that carried a frozen copy of the
 * parts would leave the explanation on screen after the release that lifted it.
 *
 * NOT A ROUTE, unlike the composer, and for the reason routes.ts gives about what
 * a URL may hold: `?attachment=` would name a stranger's file in the user's
 * browser history, and it is not a place you can meaningfully be after a reload —
 * it is a look at something you are already reading.
 */
export interface ViewerRequest {
  messageId: Id;
  /** The thread to reload after a release (core/actions.ts releaseAttachment). */
  threadId: Id;
  /** Which of the message's attachments to open on. */
  index: number;
  /**
   * Called when the viewer has closed, so the surface that opened it can put focus
   * back on the tile (#447).
   *
   * ⚠ THE CALLER MUST DEFER THE ACTUAL focus() TO AN EFFECT. In the pane placement
   * the reading pane is `display: none` while the viewer holds its column, and an
   * element that is not displayed cannot take focus — so focusing synchronously
   * from here lands on nothing and the user is dropped at the top of the document.
   * ui/ThreadPane.tsx does it as a state bump plus an effect for exactly this.
   */
  onClosed?: () => void;
}

export interface ShellApi {
  /** Run a search from anywhere (the box, the command palette). One place, so
   * the history in #57 records every search rather than only the typed ones. */
  submitSearch: (query: string) => void;
  /** Reading-pane layout and size (#61). Lifted to the shell because the shell
   * renders the splitter, exactly as they were lifted to App before. */
  paneLayout: PaneLayout;
  paneSplit: number;
  setPaneLayout: (v: PaneLayout) => void;
  resizePane: (px: number) => number;
  /**
   * A splitter drag is in flight / has ended (#375 increment 5).
   *
   * The shell needs to know because `paneSplitPct` is SYNCED now: a change
   * arriving from another device mid-drag would yank the divider out from under
   * the pointer, so it is held until the gesture finishes and applied then.
   * Props rather than the splitter reading this context itself, so the splitter
   * stays a component that is given its inputs.
   */
  beginPaneDrag: () => void;
  endPaneDrag: () => void;
  /** The ids a message-list drag is carrying (#62), so the rail can light up
   * only the targets that would actually do something. */
  setDragIds: (ids: Id[]) => void;
  /** A rule pre-filled from a message (#155). NOT a route: it carries a whole
   * draft rule, and a URL that claimed to hold one would have to put it in the
   * user's history. Setting it navigates to #/settings/rules, which is where it
   * is rendered. */
  ruleDraft: RuleDraft | null;
  openRuleDraft: (draft: RuleDraft) => void;
  clearRuleDraft: () => void;
  /**
   * The open attachment viewer (#449), or null.
   *
   * IT LIVES IN THE SHELL BECAUSE IT TAKES A GRID TRACK. app/compose.css states the
   * rule this follows: `.workspace` is the grid, its children are the only things
   * that can be given one of its columns, and a surface rendered deep inside a route
   * (this one is opened from a tile in the reading pane) cannot be a child of it. So
   * the request travels up here, the shell puts `data-viewer` on `.app` and renders
   * the viewer beside the route — the same shape the composer has, for the same
   * reason.
   */
  viewer: ViewerRequest | null;
  openViewer: (request: ViewerRequest) => void;
  closeViewer: () => void;
  /** Where the list publishes its half of the key chain. */
  listKeys: MutableRefObject<ListKeys | null>;
  /** Where the rail publishes its half — see RailKeys. */
  railKeys: MutableRefObject<RailKeys | null>;
}

export const ShellContext = createContext<ShellApi | null>(null);

export function useShell(): ShellApi {
  const ctx = useContext(ShellContext);
  if (!ctx) throw new Error("mailroom: a route was rendered outside <Shell>");
  return ctx;
}
