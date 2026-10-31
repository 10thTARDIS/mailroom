// SPDX-License-Identifier: AGPL-3.0-only

// THE SHELL (#373): the chrome that is on screen whatever route you are on —
// the header, the connection banners, the navigation rail, the toasts, the
// reading-pane geometry, and the one keyboard chain.
//
// Layout (spec §12: responsive, proper margins, no overlapping visuals):
//   ≥1100px  sidebar │ message list │ reading pane   (three panes)
//   ≥760px   sidebar │ message list or reading pane  (two panes)
//   <760px   ONE pane at a time (nav ▸ list ▸ thread), with a back affordance.
// Nothing is ever layered on top of anything else, and no surface is
// translucent — panels are solid colours, separated by borders and (only for
// genuinely floating things) an elevation shadow.
//
// This file was the top half of App.tsx. The route target goes inside it as
// `children`; overlays that sit above the workspace (settings, compose) go in
// `overlays`, which is where the modal sheets rendered before.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import type { JSX } from "react";

import type { ClientPreferences, Id } from "../../core/jmap/types";
import { viewRoute, withAcct, withCompose, withM } from "../../core/routes";
import type { Route } from "../../core/routes";
import {
  consumeEscape,
  escapeNow,
  leaveTo,
  navigate,
  setRememberedAccount,
  useListRoute,
  useRoute,
} from "../routing";
import { useApp, useAuthGuard, useMailState } from "../state";
import { t } from "../i18n";
import {
  applyDensity,
  applyPaneLayout,
  applyPaneSplit,
  applyRowLayout,
  applyTextSize,
  applyTheme,
  contentSizePx,
  densityFromServer,
  loadBesideLayout,
  loadPaneLayout,
  loadPaneSplit,
  pctToPx,
  pxToPct,
  rowLayoutFromServer,
  saveBesideLayout,
  textSizeFromServer,
} from "../prefs";
import type { PaneLayout } from "../prefs";
import { rememberSearch } from "../searchHistory";
import { AccountSwitcher } from "./AccountSwitcher";
import { AddAccountWizard } from "./AddAccountWizard";
import { PendingApprovalsBanner } from "./PendingApprovalsBanner";
import { OAuthReturnBanner } from "./OAuthReturnBanner";
import { ReconnectBanner } from "./ReconnectBanner";
import { BottomBar } from "./BottomBar";
import { PaneSplitter } from "./PaneSplitter";
import { SearchBox } from "./SearchBox";
import { searchBoxTextFor } from "./searchBoxText";
import { Sidebar } from "./Sidebar";
import { Toasts, focusUndoToast } from "./Toasts";
import { ShellContext } from "./shellContext";
import type { ListKeys, RailKeys, ShellApi, ViewerRequest } from "./shellContext";
import { Viewer } from "./viewer/Viewer";
import type { RuleDraft } from "./RuleEditor";
import { gotoTarget } from "./goto";
import { useAccounts } from "./useAccounts";
import { PHONE_QUERY, useMediaQuery } from "./useMediaQuery";
import { composePlacement as composeFor } from "./compose/placement";
import { shouldOpenSetup } from "./setup/openSetup";
import { alreadyOffered, markOffered } from "./setup/setupStorage";

/** How long the splitter's writes coalesce for (#375 increment 5). A drag
 * fires on every pointermove; without this, dragging across the window would be
 * a `ClientPreferences/set` per pixel. Half a second is comfortably longer than
 * the pauses inside one gesture and shorter than the pause before the next
 * thing the user does. */
const SPLIT_SAVE_MS = 500;

function isTyping(el: EventTarget | null): boolean {
  const node = el as HTMLElement | null;
  if (!node) return false;
  const tag = node.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || node.isContentEditable;
}

export function Shell({
  children,
  overlays,
  composer,
}: {
  children: ReactNode;
  overlays?: ReactNode;
  /**
   * THE COMPOSER, WHICH IS NOT AN OVERLAY ANY MORE (#448).
   *
   * It used to live in `overlays` beside the settings page, because it was a fixed
   * panel in the bottom-right corner and it did not matter where in the tree it
   * hung. It now takes the reading-pane COLUMN, which means it has to be a child
   * of the workspace GRID — a `position: fixed` element cannot occupy a grid
   * track, and nothing outside `.workspace` can be given one.
   *
   * So it is its own prop rather than more of `overlays`: the two differ in what
   * they ARE, not only in where they render. A sheet sits ON TOP of the workspace
   * (`.sheet` is fixed and out of flow, so it is not a grid item at all); the
   * composer sits INSIDE it, and `data-compose` on `.app` below is what tells the
   * grid which track to give it.
   */
  composer?: ReactNode;
}): JSX.Element {
  const { actions, sync, view, undoLatest } = useApp();
  const state = useMailState();
  useAuthGuard(state.conn);

  const route: Route = useRoute();
  const listRoute: Route = useListRoute();

  const [navOpen, setNavOpen] = useState(false);
  /**
   * The phone (#375 increment 7) — a DECISION here, not a stylesheet, because
   * the bottom bar and the account switcher's sheet are components that do or
   * do not exist rather than components that are or are not visible.
   *
   * ⚠ EXACTLY ONE SWITCHER IS MOUNTED, and that is why this is a boolean rather
   * than rendering both and hiding one: both copies publish
   * `shell.railKeys.current`, so a hidden second copy would silently become the
   * one `g g` and Escape talk to (ui/AccountSwitcher.tsx says the same thing
   * from the other side).
   */
  const phone = useMediaQuery(PHONE_QUERY);
  const [dragIds, setDragIds] = useState<Id[]>([]);
  const [searchText, setSearchText] = useState(view.kind === "search" ? view.query : "");
  const [ruleDraft, setRuleDraft] = useState<RuleDraft | null>(null);
  /** The open attachment viewer (#449), or null. Here rather than in the reading
   *  pane that opens it because it takes a grid track — shellContext.ts says why
   *  that forces it up to the one component that owns `.workspace`. */
  const [viewer, setViewer] = useState<ViewerRequest | null>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const listKeys = useRef<ListKeys | null>(null);
  const railKeys = useRef<RailKeys | null>(null);
  /** The accounts, in the switcher's own order — so `g 1`–`g 9` and the Nth row
   * of the open switcher are the same account by construction (ui/goto.ts). */
  const { accounts } = useAccounts();
  const accountIds = useMemo(() => accounts.map((a) => a.id), [accounts]);

  // Reading-pane layout + split size (#61). Lifted here rather than owned by
  // the settings page because the shell renders the splitter and needs both.
  const [paneLayout, setPaneLayoutState] = useState<PaneLayout>(() => loadPaneLayout());
  const [paneSplit, setPaneSplit] = useState<number>(() => loadPaneSplit(loadPaneLayout()));

  /**
   * WHERE THE COMPOSER IS, when there is one (#448).
   *
   * The same decision ComposeRoute makes, from the same pure function
   * (ui/compose/placement.ts), so the attribute on `.app` and the class on the
   * panel cannot disagree about which track the composer is in — the failure that
   * would produce is a composer drawn in a column the grid never made.
   *
   * `composeDocked` is the PANE placement specifically: it is the only one that
   * takes a column away from the reading pane.
   */
  const composePlacement = route.compose
    ? composeFor({ phone, paneLayout })
    : null;
  const composeDocked = composePlacement === "pane";

  /**
   * WHERE THE ATTACHMENT VIEWER IS (#449) — the same three cases as the composer,
   * from the same function.
   *
   * Not a coincidence and not copied prose: both surfaces answer "what happens to
   * the reading-pane column when something else needs the main area", so answering
   * it twice would be two rules to keep in step. `pane` takes the reading pane's
   * column with the list still beside it; `main` takes the content area where there
   * is no second column; `full` is the phone's one screen.
   */
  const viewerPlacement = viewer ? composeFor({ phone, paneLayout }) : null;
  const viewerDocked = viewerPlacement === "pane";

  /** Closes the viewer and hands the focus back — see ViewerRequest.onClosed for
   *  why the caller must defer the actual focus() to an effect. */
  const closeViewer = useCallback(() => {
    if (!viewer) return;
    setViewer(null);
    viewer.onClosed?.();
  }, [viewer]);

  /**
   * A viewer never outlives the message it belongs to.
   *
   * Closing the reading pane, or opening a different message, leaves the viewer
   * showing a file from a message nobody is reading — and in the `pane` placement it
   * would be holding the column that message's replacement wants. The viewer closes
   * itself when the part it is on disappears (ui/viewer/Viewer.tsx); this covers the
   * other direction, where the store still has everything and the ROUTE moved.
   */
  useEffect(() => {
    setViewer(null);
  }, [listRoute.m, route.page]);

  /** Applies the stored layout on mount, so the very first paint is already in
   *  the chosen shape rather than flashing the default and correcting. */
  useEffect(() => {
    applyPaneLayout(paneLayout);
    // Intentionally mount-only: every later change goes through setPaneLayout,
    // which applies as it sets.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  /** Applies a layout to THIS DEVICE and nothing else — the half of
   *  setPaneLayout that must not write to the server, because it is also how a
   *  layout that ARRIVED from the server is put on screen. Without the split,
   *  one settings click would echo back as a change to apply. */
  const applyLayoutLocally = useCallback((v: PaneLayout) => {
    setPaneLayoutState(v);
    applyPaneLayout(v);
    // Remembered so "Beside the list" can restore the orientation the user
    // actually chose rather than defaulting to vertical every time.
    saveBesideLayout(v);
    // Each orientation stores its own size; adopt the one we just switched to.
    setPaneSplit(loadPaneSplit(v));
  }, []);

  const setPaneLayout = useCallback(
    (v: PaneLayout) => {
      applyLayoutLocally(v);
      // ONLY the beside/focused half is synced. The ORIENTATION is this
      // device's business — a width that reads well on a 27-inch screen is not
      // a height on a laptop — and `ClientPreferences` has no field for it,
      // which is the server agreeing.
      void actions.saveClientPreferences({ messageOpens: v === "off" ? "focused" : "beside" });
    },
    [actions, applyLayoutLocally],
  );

  /** Whether a splitter drag is in flight, and the server value held back
   *  because of it. A ref rather than state: nothing renders differently for
   *  it, and a re-render per pointermove is exactly what the pointer capture in
   *  PaneSplitter exists to avoid. */
  const dragging = useRef(false);
  const deferredPrefs = useRef<ClientPreferences | null>(null);

  /** Commits a splitter drag. Returns the clamped value so the splitter can
   *  report the position it actually landed on to assistive tech. */
  const resizePane = useCallback(
    (px: number): number => {
      const applied = applyPaneSplit(paneLayout, px);
      setPaneSplit(applied);
      // The SERVER stores a percent of the content area, because the two
      // devices sharing it have different windows (app/prefs.ts). A container
      // we cannot measure yields null, and null is not saved: writing a percent
      // taken against an unknown width would put the divider somewhere nobody
      // chose on whichever device syncs next.
      const container = contentSizePx(paneLayout);
      const pct = container === null ? null : pxToPct(applied, container);
      if (pct !== null) {
        void actions.saveClientPreferences({ paneSplitPct: pct }, { debounceMs: SPLIT_SAVE_MS });
      }
      return applied;
    },
    [paneLayout, actions],
  );

  /**
   * Puts a server-side appearance on this device (#375 increment 5).
   *
   * ⚠ SERVER WINS AT LOAD, and that is the whole feature: a theme chosen on the
   * desktop is worth nothing if the laptop keeps overriding it with whatever
   * localStorage last held. Every local change writes upward as it is made, so
   * the two only diverge while a write is in flight or refused — and a refused
   * write deliberately leaves the device on the user's value.
   *
   * ⚠ "EVERY RECORD" IS SAFE ONLY BECAUSE THE STORE VETS IT (#562). A record
   * from a read the server answered before this device's latest write
   * committed carries the EARLIER value; applied here, it undid the user's
   * newer choice (Focused, then Beside, stayed Focused). The store lays this
   * device's unanswered writes over any read that cannot be shown to have seen
   * them (core/prefsLedger.ts), so what arrives here is safe to apply whole.
   *
   * The density VOCABULARIES differ between the two sides and the mapping lives
   * in prefs.ts; handing the server's word straight to applyDensity would set
   * `data-density="relaxed"`, which matches no rule in tokens.css and silently
   * leaves the app at whatever the previous density was.
   *
   * The text size and the row layout (#514) ARE spelled the same on both sides,
   * so there is no mapping — but they still go through prefs.ts's guards, for a
   * different reason. `res.list as ClientPreferences[]` (core/sync.ts) is a
   * blind cast: a server that predates migration 00052 sends no `textSize` at
   * all, and `data-text-size="undefined"` matches no rule in tokens.css. The
   * guard turns that into the default, which is the size the app already draws
   * at, so an old server is simply an app that never changes size rather than
   * an app whose text control does nothing visible.
   */
  const applyServerPrefs = useCallback(
    (p: ClientPreferences) => {
      applyTheme(p.theme);
      applyDensity(densityFromServer(p.density));
      applyTextSize(textSizeFromServer(p.textSize));
      applyRowLayout(rowLayoutFromServer(p.rowLayout));
      const layout: PaneLayout = p.messageOpens === "focused" ? "off" : loadBesideLayout();
      applyLayoutLocally(layout);
      // The split belongs to the orientation it is VISIBLE in: with the pane
      // focused there is no divider on screen, and storing the percent against
      // "off" would lose it the moment the user goes back to Beside.
      const splitLayout: PaneLayout = layout === "off" ? loadBesideLayout() : layout;
      const container = contentSizePx(splitLayout);
      const px = container === null ? null : pctToPx(p.paneSplitPct, container, splitLayout);
      // A percent that already DESCRIBES what is on screen is not a change to
      // apply. Without this, the record that comes back from this device's own
      // splitter write would be converted back to pixels and re-applied — and
      // the round trip through a whole percent moves the divider a few pixels
      // the instant the user lets go of it, which reads as the drag not having
      // taken. Compared through the same pure conversion, so the two sides
      // cannot disagree about rounding.
      const shown = container === null ? null : pxToPct(loadPaneSplit(splitLayout), container);
      if (px !== null && p.paneSplitPct !== shown) setPaneSplit(applyPaneSplit(splitLayout, px));
    },
    [applyLayoutLocally],
  );

  const web = state.clientPreferences["web"] ?? null;
  const appliedPrefs = useRef<ClientPreferences | null>(null);
  useEffect(() => {
    if (!web || appliedPrefs.current === web) return;
    // Mid-drag, hold it: yanking the divider out from under the pointer is the
    // one way a synced setting can make the app feel broken rather than
    // helpful. `endPaneDrag` applies whatever was held.
    if (dragging.current) {
      deferredPrefs.current = web;
      return;
    }
    appliedPrefs.current = web;
    applyServerPrefs(web);
  }, [web, applyServerPrefs]);

  /**
   * DAY-ONE SETUP OPENS ITSELF, ONCE PER TAB (#446).
   *
   * The DECISION is pure and tested (ui/setup/openSetup.ts); what lives here is
   * the two things it cannot be — the navigation itself, and the memory that
   * keeps it to ONE offer. Without that mark, somebody who skipped the wizard by
   * navigating away is dragged straight back: the gate is still true until the
   * `setupCompletedAt` write lands, and it is re-evaluated on every render.
   *
   * `mailboxesLoaded` is read as "the folder map is not empty", which for
   * mailboxes IS "the reference-data load has answered": every account has an
   * INBOX, so an empty map can only be a load in flight. The store has no
   * `loaded` marker and inventing one to serve this effect would be a second
   * source of truth for it.
   *
   * The mark is written BEFORE the navigate, so a re-render between the two
   * cannot offer it twice.
   */
  useEffect(() => {
    if (alreadyOffered()) return;
    if (
      !shouldOpenSetup({
        prefs: state.deskPreferences,
        accounts: accounts.length,
        mailboxesLoaded: Object.keys(state.mailboxes).length > 0,
        route,
      })
    ) {
      return;
    }
    markOffered();
    navigate({ page: "setup", step: 1, ...(route.acct ? { acct: route.acct } : {}) });
  }, [state.deskPreferences, state.mailboxes, accounts.length, route]);

  const beginPaneDrag = useCallback(() => {
    dragging.current = true;
  }, []);

  const endPaneDrag = useCallback(() => {
    dragging.current = false;
    const held = deferredPrefs.current;
    deferredPrefs.current = null;
    if (!held) return;
    appliedPrefs.current = held;
    applyServerPrefs(held);
  }, [applyServerPrefs]);

  // The search field mirrors the view, so leaving a search empties the box and
  // arriving at one (from a pasted URL, the rail's saved searches, or the back
  // button) fills it.
  //
  // ⚠ EXCEPT A SEARCH THE BOX ITSELF STARTED WHILE TYPING (#724). Live results
  // navigate after a pause, and by the time that view arrives the user may have
  // typed more: writing the view's query back into the box would eat those
  // keystrokes. `liveQuery` is the query the box last asked for; arriving at it
  // leaves the box alone.
  const liveQuery = useRef<string | null>(null);
  useEffect(() => {
    const next = searchBoxTextFor(view, liveQuery.current);
    if (next === null) return;
    liveQuery.current = null;
    setSearchText(next);
  }, [view]);

  /** The sort a search keeps when its text changes: refining a best-match
   * search should not silently flip it back to newest first. */
  const currentSort = view.kind === "search" ? view.sort : undefined;
  const onSearchRoute = view.kind === "search";
  const onLiveRoute = view.kind === "search" && view.prefix === true;

  const submitSearch = useCallback(
    (q: string) => {
      const query = q.trim();
      // Remembered HERE rather than in the search box (#57), because this is the one
      // place every search goes through — the box, the command palette, and anything
      // added later. Recording it in the box instead would silently forget every
      // search made from the palette.
      if (query) rememberSearch(query);
      liveQuery.current = null;
      const sort = currentSort ? { sort: currentSort } : {};
      // Enter on a list the box was already showing live REPLACES that entry
      // (#724): Back should leave the search, not step through what was typed.
      navigate(viewRoute(query ? { kind: "search", query, ...sort } : { kind: "role", role: "inbox" }), {
        replace: onLiveRoute,
      });
    },
    [currentSort, onLiveRoute],
  );

  /** Live results (#724): the box calls this after SEARCH_DEBOUNCE_MS without a
   * keystroke. The first one from another list PUSHES (Back returns there);
   * every later one REPLACES, so a typed word is one history entry, not ten. It
   * records no history — only a search someone actually ran (Enter) is
   * remembered. */
  const liveSearch = useCallback(
    (query: string, prefix: boolean) => {
      liveQuery.current = query;
      const sort = currentSort ? { sort: currentSort } : {};
      navigate(viewRoute({ kind: "search", query, ...(prefix ? { prefix: true as const } : {}), ...sort }), {
        replace: onSearchRoute,
      });
    },
    [currentSort, onSearchRoute],
  );

  const openRuleDraft = useCallback((draft: RuleDraft) => {
    setRuleDraft(draft);
    navigate({ page: "settings", section: "rules" });
  }, []);

  const clearRuleDraft = useCallback(() => setRuleDraft(null), []);

  // -- keyboard (spec §12: keyboard-driven power use) ------------------------
  //
  // ONE listener, one order. The chain below is the one App.tsx had; the halves
  // that need the message list are called through listKeys (shellContext.ts
  // says why it is a ref), and the layers that are now routes — the composer,
  // the open message, the settings sheets — go through the escape decision in
  // routing.ts so that Escape and the browser's back button do the same thing.
  const gotoRef = useRef(false);
  useEffect(() => {
    const onKey = (ev: KeyboardEvent): void => {
      const list = listKeys.current;

      if ((ev.ctrlKey || ev.metaKey) && ev.key.toLowerCase() === "k") {
        ev.preventDefault();
        list?.openPalette();
        return;
      }

      // UNDO (#375 increment 6). Before the `isTyping` guard below, because
      // that guard also drops every modified key — and after its own check, so
      // Ctrl/⌘+Z inside the composer is still the editor's own undo. Shift is
      // excluded: Ctrl/⌘+Shift+Z is redo everywhere else, and there is no redo
      // here, so swallowing it would be this app quietly meaning something else
      // by a keystroke people already know.
      //
      // The key is NOT printed on the toast's button (the sheet is the one
      // place a shortcut is written down, ui/ShortcutsHelp.tsx), and it is the
      // reason the toast is reachable at all without a tab-hunt.
      if ((ev.ctrlKey || ev.metaKey) && !ev.shiftKey && ev.key.toLowerCase() === "z") {
        if (isTyping(ev.target)) return;
        // Only prevented when something was actually undone: with an empty log
        // the browser's own Ctrl+Z should still reach whatever it would have.
        if (undoLatest()) ev.preventDefault();
        return;
      }

      if (ev.key === "Escape") {
        // A sheet that closed itself on this very keystroke has already
        // navigated; acting again would back out two layers for one press.
        if (consumeEscape()) return;
        if (list?.closeTopPopover()) return;
        // The rail's switcher is a popover like any other, and it closes before
        // the route peels a layer — otherwise Escape out of an open switcher
        // would also leave the message you had open behind it.
        if (railKeys.current?.closeSwitcher()) return;
        if (escapeNow(route, listRoute)) return;
        if (list?.clearSelection()) return;
        if (isTyping(ev.target)) (ev.target as HTMLElement).blur();
        return;
      }

      if (isTyping(ev.target) || ev.ctrlKey || ev.metaKey || ev.altKey) return;
      // A route-level overlay swallows the plain verbs, which is what each of
      // the sheet booleans used to do one at a time. It now covers the rules
      // sheet too: that one was missing from the old guard, so `e` behind an
      // open rules editor archived whatever the list had selected — and the
      // day-one wizard (#446), which takes the whole content area: `e` while
      // choosing what to do with 4,000 messages must not archive the selection on
      // a list that is not even drawn.
      if (route.compose || route.page === "settings" || route.page === "setup") return;
      if (list?.blocksVerbs()) return;
      if (railKeys.current?.blocksVerbs()) return;

      // Two-key "go to" sequences (Gmail: g then i). The second key is consumed
      // whatever it is — a mistyped sequence must not also do something — and
      // `g 7` with two accounts resolves to nothing rather than to whatever the
      // seventh of something else is (ui/goto.ts).
      if (gotoRef.current) {
        gotoRef.current = false;
        const target = gotoTarget(ev.key, accountIds);
        if (!target) return;
        if (target.kind === "switcher") {
          railKeys.current?.openSwitcher();
          return;
        }
        // `g u` — focus the undo offer rather than running it (#447). Ctrl/⌘+Z
        // above is the fast path; this is the one for a reader who wants to hear
        // what happened first, and it pauses the countdown while they do
        // (ui/Toasts.tsx). Nothing on screen means nothing happens: a key that
        // reported success with no offer to reach would be a lie.
        if (target.kind === "undo") {
          focusUndoToast();
          return;
        }
        if (target.kind === "scope") {
          // A SCOPE CHANGE IS NOT A NAVIGATION. It rewrites the list you are
          // already on, exactly as the switcher does — and it says so to the
          // per-device memory, because "all accounts" cannot be read back off a
          // route that simply carries no account (routing.ts).
          setRememberedAccount(target.acct);
          navigate(withAcct(listRoute, target.acct));
          return;
        }
        // A view destination keeps the scope you were in: `g s` from one
        // account's desk means that account's tray, not everybody's.
        navigate(withAcct(viewRoute(target.view), listRoute.acct ?? null));
        return;
      }

      switch (ev.key) {
        case "g":
          gotoRef.current = true;
          return;
        case "c":
          ev.preventDefault();
          navigate(withCompose(route, "new"));
          return;
        case "u":
          // Close the reading pane. The same one-layer decision Escape makes,
          // so `u` and the back button agree too.
          if (route.m) leaveTo(withM(route, null));
          return;
        case "/":
          ev.preventDefault();
          searchRef.current?.focus();
          return;
        case "?":
          ev.preventDefault();
          navigate({ page: "settings", section: "shortcuts" });
          return;
        default:
          list?.handleVerb(ev);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [route, listRoute, accountIds, undoLatest]);

  // Memoised so the seam is stable: the route inside re-renders when the mail
  // state moves, which is often, and a fresh api object each time would
  // invalidate every callback that closes over it for no reason.
  const api = useMemo<ShellApi>(
    () => ({
      submitSearch,
      paneLayout,
      paneSplit,
      setPaneLayout,
      resizePane,
      beginPaneDrag,
      endPaneDrag,
      setDragIds,
      ruleDraft,
      openRuleDraft,
      clearRuleDraft,
      viewer,
      openViewer: setViewer,
      closeViewer,
      listKeys,
      railKeys,
    }),
    [
      submitSearch,
      paneLayout,
      paneSplit,
      setPaneLayout,
      resizePane,
      beginPaneDrag,
      endPaneDrag,
      ruleDraft,
      openRuleDraft,
      clearRuleDraft,
      viewer,
      closeViewer,
    ],
  );

  return (
    <ShellContext.Provider value={api}>
      {/* The narrow-screen pane state follows the LIST route: a settings sheet
          sits on top of the message that is open, it does not close it.

          ⚠ A DOCKED COMPOSER TAKES THE READING-PANE COLUMN, so while one is open
          `data-pane` is NOT "thread" even with a message open: the column has one
          occupant, and leaving it "thread" would split `.content` into
          list + splitter + pane inside a track the composer is already using. The
          message stays in the URL (`?m=`), so closing the composer puts the reader
          back exactly where they were — and app/compose.css hides the reading pane
          off the same one attribute, which is what keeps a still-mounted ThreadPane
          from being drawn under the list. Mounted rather than unmounted on purpose:
          the message body lives in a sandboxed iframe, and unmounting it would
          re-fetch and re-render the whole document when the composer closes. */}
      <div
        className="app"
        // ⚠ A DOCKED VIEWER TAKES THE READING-PANE COLUMN TOO, so it drops
        //   `data-pane="thread"` exactly as a docked composer does: one column has
        //   one occupant, and leaving it "thread" would split `.content` into
        //   list + splitter + pane inside a track the viewer is already using.
        data-pane={
          // Contacts (#816) draws its own list and page; the mail list's open
          // message underneath must not split the content area.
          listRoute.m && !composeDocked && !viewerDocked && route.page !== "contacts"
            ? "thread"
            : navOpen
              ? "nav"
              : "list"
        }
        {...(composePlacement ? { "data-compose": composePlacement } : {})}
        {...(viewerPlacement ? { "data-viewer": viewerPlacement } : {})}
      >
        <a className="skip-link" href="#messages" onClick={(e) => { e.preventDefault(); document.getElementById("messages")?.focus(); }}>
          {t("app.skipToContent")}
        </a>

        <header className="topbar">
          <button
            type="button"
            className="icon-button nav-toggle"
            aria-label={t("nav.mailboxes")}
            aria-expanded={navOpen}
            onClick={() => setNavOpen((v) => !v)}
          >
            ☰
          </button>
          <span className="brand">{t("app.name")}</span>

          {/* #57: the operator syntax was reachable only by knowing it existed. The
              suggestions are now the documentation for it. */}
          <SearchBox
            value={searchText}
            onChange={setSearchText}
            onSubmit={submitSearch}
            onLive={liveSearch}
            onClear={() => navigate(viewRoute({ kind: "role", role: "inbox" }))}
            inputRef={searchRef}
          />

          <div className="topbar-actions">
            {state.queue.length > 0 ? (
              <span className="pill" title={state.queue.map((q) => q.label).join(", ")}>
                {t("conn.pending", { count: state.queue.length })}
              </span>
            ) : null}
            <button
              type="button"
              className="icon-button"
              aria-label={t("action.refresh")}
              onClick={() => void sync.poll()}
            >
              ⟳
            </button>
            <AddAccountWizard />
            {/* Settings is a PLACE now, not a popover (#373): it has a URL, it
                survives a reload, and every section in it can be linked to. */}
            <button
              type="button"
              className="icon-button"
              aria-label={t("settings.title")}
              aria-current={route.page === "settings" ? "page" : undefined}
              onClick={() => navigate({ page: "settings", section: null })}
            >
              ⚙
            </button>
          </div>
        </header>

        {state.conn === "offline" ? (
          <div className="banner banner-warn" role="status">
            {t("conn.offline")}
          </div>
        ) : null}
        {state.conn === "error" && state.fatal ? (
          <div className="banner banner-error" role="alert">
            {t("conn.error", { error: state.fatal })}
          </div>
        ) : null}
        {/* Agent requests waiting for the user (#49): over every mail view, beside
            the connection banners, linking to Settings → API keys. */}
        <PendingApprovalsBanner />
        {/* Mail-provider sign-in (#52): what the provider's sign-in came back
            with (once, then off the URL), and the reminder that an account
            needs signing in again. */}
        <OAuthReturnBanner />
        <ReconnectBanner />

        <div className="workspace">
          <Sidebar
            view={view}
            onNavigate={(r: Route) => {
              navigate(r);
              setNavOpen(false);
            }}
            onCompose={() => navigate(withCompose(route, "new"))}
            dragIds={dragIds}
            showSwitcher={!phone}
          />
          {children}
          {/* THE DOCKED SPLITTER (#617): between the list and a composer or viewer
              that has taken the reading pane's column. Owned HERE rather than by
              each route — the list, the desk and the tray each render their own
              splitter inside `.content`, but only while a message is open, and a
              draft opens in the composer, not the reading pane — so one place
              covers every route. The CSS shows it only where the list track IS
              `--pane-split` (vertical, >=1100px); elsewhere it is display:none. */}
          {composeDocked || viewerDocked ? (
            <PaneSplitter
              docked
              layout={paneLayout}
              size={paneSplit}
              onResize={resizePane}
              onDragStart={beginPaneDrag}
              onDragEnd={endPaneDrag}
            />
          ) : null}
          {/* Inside the grid, after the content, so `.app[data-compose]`'s explicit
              track placement (app/compose.css) has something to place. */}
          {composer}
          {/* And the attachment viewer, for the same reason and in the same place
              (app/viewer.css). Not in `overlays`: a sheet sits ON TOP of the
              workspace and is not a grid item at all, whereas this occupies a
              track — spec §12's "nothing is layered on top of anything else". */}
          {viewer && viewerPlacement ? (
            <Viewer
              // Keyed on the message and the attachment it opened on, so opening a
              // different file is a different viewer rather than the previous one
              // with new props (the fit/zoom state belongs to the file on screen).
              key={`${viewer.messageId}:${viewer.index}`}
              request={viewer}
              placement={viewerPlacement}
              onClose={closeViewer}
            />
          ) : null}
        </div>

        {/* THE BAR AND THE RAIL COEXIST BY COVERING DIFFERENT THINGS. The five
            journeys made constantly are here, on the thumb, on every pane
            INCLUDING the rail — so the rail is a place one tap gets you out of
            — while the rail itself keeps the whole map (labels, areas, folders,
            settings) behind the header's ☰. It is a flex child of a `100dvh`
            column, not a fixed overlay, so it can never cover a list, a rail or
            a reading pane (ui/BottomBar.tsx). */}
        {phone ? (
          <BottomBar
            route={listRoute}
            // The SCOPE follows you: Inbox from one account's desk is that
            // account's inbox, exactly as `g i` is (the goto chain above).
            onNavigate={(to) => navigate(withAcct(to, listRoute.acct ?? null))}
            onSearch={() => searchRef.current?.focus()}
            onCompose={() => navigate(withCompose(route, "new"))}
          />
        ) : null}

        {overlays}
        {/* The phone's switcher: no trigger of its own (the desk's header is
            the account name with a ▾) and nothing in the tree until it opens. */}
        {phone ? <AccountSwitcher variant="sheet" /> : null}
        <Toasts />
      </div>
    </ShellContext.Provider>
  );
}
