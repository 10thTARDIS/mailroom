// SPDX-License-Identifier: AGPL-3.0-only

// THE DESK (#375 increment 3): what `#/inbox` is now.
//
// The message list answers "what arrived". The desk answers "what is waiting on
// me" — the same mail, partitioned the way the user arranged it: every message
// in exactly one AREA and on exactly one side of the LINE, with the in-tray
// (things you said you would come back to) stated permanently above it and the
// drawers (what the line kept rather than showed) one tab across.
//
// WHAT THIS ROUTE IS, AND WHAT IT IS NOT. It is composition and state: which
// tab, which scope, what is selected, where the cursor is, and the wiring of
// the verbs. It is NOT arithmetic — every number on screen comes from
// core/desk.ts (headerCounts, areaCounts, trayStats, drawerPeriodDays,
// syncedAgoSeconds, virtualWindow), which is DOM-free and tested; not ordering
// — ui/desk/flatten.ts turns the sections into the one flat list that is drawn,
// scrolled and walked; and not sentences — ui/desk/format.ts owns those. The
// desk's whole claim is that its numbers are the server's, and that claim is
// only checkable if the numbers live somewhere a test can reach.
//
// THE VERBS ARE SHARED WITH ListRoute (ui/useListVerbs.ts). Archiving,
// starring, pinning, snoozing, muting and the reading-pane advance behave
// identically here because they ARE the same callbacks; a second copy would be
// a second set of behaviours to learn.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";

import { effectiveEmail, sectionIds } from "../../../core/store";
import type { EffectiveEmail } from "../../../core/store";
import {
  areaCounts,
  deskSections,
  drawerPeriodDays,
  headerCounts,
  sectionKey,
  syncedAgoSeconds,
  trayStats,
} from "../../../core/desk";
import type { DeskTab } from "../../../core/desk";
import { KEYWORD_FLAGGED } from "../../../core/jmap/types";
import type { Area, Id } from "../../../core/jmap/types";
import { PAGE_SIZE } from "../../../core/sync";
import { invertChanges, refileChanges } from "../../../core/refile";
import { offerRuleSuggestion } from "../ruleSuggestion";
import { withCompose } from "../../../core/routes";
import type { Route } from "../../../core/routes";
import { navigate, useListRoute } from "../../routing";
import {
  loadCollapsedSections,
  loadTrayOpen,
  saveCollapsedSections,
  saveTrayOpen,
} from "../../prefs";
import { useApp, useMailState } from "../../state";
import { t } from "../../i18n";
import { ThreadPane } from "../ThreadPane";
import { CommandPalette } from "../CommandPalette";
import { LabelMenu } from "../LabelMenu";
import { RefilePicker } from "../RefilePicker";
import { SnoozeMenu } from "../SnoozeMenu";
import { PaneSplitter } from "../PaneSplitter";
import { useShell } from "../shellContext";
import { useListVerbs } from "../useListVerbs";
import { useOpenDwell } from "../useOpenDwell";
import { useAccounts } from "../useAccounts";
import { useHeaderState } from "../useHeaderState";
import { useShadowNotices } from "../useShadowNotices";
import { PHONE_QUERY, REDUCED_MOTION_QUERY, useMediaQuery } from "../useMediaQuery";
import { crossedLineText, filedText } from "../undoText";
import { draftRuleFrom } from "../ruleDraft";
import { ConfirmDialog } from "../desk/ConfirmDialog";
import { DeskHeader } from "../desk/DeskHeader";
import { DeskRow } from "../desk/DeskRow";
import type { RowAccount } from "../desk/DeskRow";
import { DESK_GRID_ID, DeskTabs, deskTabId } from "../desk/DeskTabs";
import { DrawerRow } from "../desk/DrawerRow";
import { ShadowBanners } from "../desk/ShadowBanner";
import { KeyExpiryBanners, useOwnKeysForBanner } from "../desk/KeyExpiryBanner";
import { SectionHeading } from "../desk/SectionHeading";
import { SelectionToolbar } from "../desk/SelectionToolbar";
import type { RefileTarget } from "../desk/SelectionToolbar";
import { LINE_STEP, TheLine } from "../desk/TheLine";
import { TRAY_PREVIEW_ROWS, TrayStrip } from "../desk/TrayStrip";
import { FollowUpStrip } from "../desk/FollowUpStrip";
import { useFollowUps } from "../desk/useFollowUps";
import type { FollowUpItem } from "../../../core/jmap/types";
import { VirtualList } from "../desk/VirtualList";
import type { GridKeys, RowAria } from "../desk/VirtualList";
import { rowNameFor } from "../desk/rowName";
import {
  collapseKey,
  cursorAfterRemoval,
  flatten,
  nextRowId,
  positionOf,
  rowIds,
  rowIndexOf,
} from "../desk/flatten";
import type { DeskItem, DrawerView, SectionView } from "../desk/flatten";
import type { SwipeAction } from "../desk/swipe";
import { moreText } from "../desk/format";

/** How often the header sentence re-states how long ago the last sync was. Ten
 * seconds is the granularity the sentence itself has ("just now" below that),
 * so anything finer would re-render for a number that cannot change. */
const TICK_MS = 10_000;

/** How long a click on `stricter`/`looser` waits before asking the server what
 * that line would show. Long enough that four clicks are one question, short
 * enough that one click feels answered. */
const PREVIEW_DEBOUNCE_MS = 250;

function clampLine(v: number): number {
  return Math.min(1, Math.max(0, Math.round(v * 100) / 100));
}

export function DeskRoute(): JSX.Element {
  const { actions, sync, store, pushToast, offerUndo, client } = useApp();
  const state = useMailState();
  const shell = useShell();
  // FOLLOW UP (#58): the nudges due now, under the tray strip.
  const followUps = useFollowUps(
    client,
    state.accountId,
    state.deskPreferences?.followUp,
    state.lastSyncAt,
  );
  const [followUpSnooze, setFollowUpSnooze] = useState<FollowUpItem | null>(null);
  // The LIST route, not the current one: settings sits on top of the desk
  // rather than replacing it (core/routes.ts listRouteOf).
  const listRoute: Route = useListRoute();

  /** The tab is the URL's. A bare `#/inbox` is Review first: the half that asks
   * for a decision is the half worth opening on. */
  const tab: DeskTab = listRoute.page === "inbox" && listRoute.tab === "rest" ? "rest" : "review";
  const scope: Id | null = listRoute.acct ?? null;
  const openId: Id | null = listRoute.m ?? null;

  const paneRef = useRef<HTMLElement | null>(null);
  const chromeRef = useRef<HTMLDivElement | null>(null);
  const [selected, setSelected] = useState<Set<Id>>(new Set());
  const [anchor, setAnchor] = useState<Id | null>(null);
  const [cursor, setCursor] = useState<Id | null>(null);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [labelMenuOpen, setLabelMenuOpen] = useState(false);
  const [refileOpen, setRefileOpen] = useState(false);
  // The reading pane's area picker (#921): the desk's own re-file menu is
  // `refileOpen` above, and this is the one the pane's More menu opens.
  const [refilePickerOpen, setRefilePickerOpen] = useState(false);
  const [snoozeIds, setSnoozeIds] = useState<Id[] | null>(null);
  const [trayOpen, setTrayOpen] = useState<boolean>(() => loadTrayOpen());
  /** The sections folded to their headings (#375 increment 7) — remembered per
   * device, because folding is what makes a dozen areas fit a phone and would
   * be an odd thing to inflict on the monitor that had room for them. */
  const [collapsed, setCollapsed] = useState<Set<string>>(() => loadCollapsedSections());
  const [openDrawers, setOpenDrawers] = useState<Set<Id>>(new Set());
  const [confirm, setConfirm] = useState<{ areaId: Id; name: string; count: number } | null>(null);
  const [archiving, setArchiving] = useState(false);
  // The line being considered, and what the server says it would show. Nothing
  // moves until Apply — see ui/desk/TheLine.tsx.
  const [pendingLine, setPendingLine] = useState<number | null>(null);
  const [preview, setPreview] = useState<{ review: number; drawers: number } | null>(null);
  const [previewFailed, setPreviewFailed] = useState(false);
  const [lineBusy, setLineBusy] = useState(false);

  // "synced 40 s ago" has to keep being true while the tab sits open.
  const [now, setNow] = useState<number>(() => Date.now());
  useEffect(() => {
    const handle = setInterval(() => setNow(Date.now()), TICK_MS);
    return () => clearInterval(handle);
  }, []);

  // -- what the store currently says ------------------------------------------

  /** Areas in the order they are drawn — the SAME total order deskSections
   * uses, so the rail, the sections and the `1`–`9` shortcut cannot disagree
   * about which area is third. */
  const areas: Area[] = useMemo(
    () =>
      Object.values(state.areas).sort((a, b) =>
        a.position !== b.position ? a.position - b.position : a.id.localeCompare(b.id),
      ),
    [state.areas],
  );

  /** Offline, stale, an error, or fine — ONE decision (core/headerState.ts),
   * assembled once and shared with the tray page (ui/useHeaderState.ts) so the
   * two never describe the same failure differently. */
  const header = useHeaderState(now);

  /** Every account IN VIEW running ingest-only right now (ADR-028 §5,
   * #521) — a fact orthogonal to `header` above and rendered separately for
   * that reason (ui/desk/ShadowBanner.tsx's own comment says why). */
  const shadowNotices = useShadowNotices(scope);

  /** Your own keys, for the expiry banner (#795): a key about to expire, or a
   * bound one that has. Login-wide, so not narrowed by `scope`. */
  const ownKeys = useOwnKeysForBanner();

  /** Below 760px the desk is ONE COLUMN and three of its parts are different
   * components rather than the same ones restyled: the header is a scope line,
   * the in-tray strip is a link, and the rows carry the two swipes
   * (ui/useMediaQuery.ts says why this cannot be CSS). */
  const phone = useMediaQuery(PHONE_QUERY);
  const reducedMotion = useMediaQuery(REDUCED_MOTION_QUERY);

  /**
   * The summary, but ONLY if it was taken at this scope.
   *
   * `deskSummary` carries the scope it was measured at on purpose (core/store
   * .ts): after an account switch the previous scope's counts are still in the
   * store until the new ones land, and printing them beside this scope's
   * sections would be a header sentence describing a different desk.
   */
  const summary = state.deskSummary && state.deskSummary.scope === scope ? state.deskSummary : null;
  const summaryRows = useMemo(() => summary?.rows ?? [], [summary]);
  const counts = useMemo(() => headerCounts(summaryRows), [summaryRows]);

  const trayKey = sectionKey("tray", null, scope);
  const traySection = state.sections[trayKey];
  const trayIds = useMemo(
    () =>
      sectionIds(state, trayKey, {
        move: "inbox",
        // Unflagging from the strip empties that row now, not at the next poll.
        keyword: { atom: KEYWORD_FLAGGED, present: true },
        // The tray's query carries NO snooze condition, so neither does its
        // overlay filter — hiding rows the query returned would make the strip
        // disagree with the count printed on it.
        snoozed: null,
      }),
    [state, trayKey],
  );
  const trayRows = useMemo(
    () =>
      trayIds
        .map((id) => effectiveEmail(state, id))
        .filter((e): e is EffectiveEmail => e !== null),
    [trayIds, state],
  );
  const tray = useMemo(() => trayStats(trayRows, now), [trayRows, now]);
  const trayTotal = traySection?.total ?? 0;

  const sections: SectionView[] = useMemo(() => {
    if (tab !== "review") return [];
    return deskSections("review", areas).map((section) => {
      const key = sectionKey(section.kind, section.areaId, scope);
      const stored = state.sections[key];
      return {
        key,
        areaId: section.areaId,
        ids: sectionIds(state, key, { move: "inbox" }),
        total: stored?.total ?? 0,
        // The heading prints what the SUMMARY says the area holds, never the
        // number of rows fetched.
        count: areaCounts(summaryRows, section.areaId).review,
        error: stored?.error ?? null,
      };
    });
  }, [tab, areas, scope, state, summaryRows]);

  const drawers: DrawerView[] = useMemo(() => {
    if (tab !== "rest") return [];
    return areas.map((area) => {
      const key = sectionKey("drawer", area.id, scope);
      const stored = state.sections[key];
      const open = openDrawers.has(area.id);
      return {
        areaId: area.id,
        key,
        open,
        ids: open ? sectionIds(state, key, { move: "inbox" }) : [],
        total: stored?.total ?? 0,
        error: stored?.error ?? null,
      };
    });
  }, [tab, areas, scope, state, openDrawers]);

  /** key → area, so a "show more" on the rest tab knows which drawer to grow. */
  const drawerByKey = useMemo(() => {
    const out = new Map<string, Id>();
    for (const drawer of drawers) out.set(drawer.key, drawer.areaId);
    return out;
  }, [drawers]);

  const items: DeskItem[] = useMemo(
    () => flatten({ tab, sections, drawers, trayCount: trayIds.length, collapsed }),
    [tab, sections, drawers, trayIds.length, collapsed],
  );
  /** The rows, flattened across every section — what `j`/`k` walk and what the
   * reading pane advances through. */
  const ids = useMemo(() => rowIds(items), [items]);

  // -- accounts ---------------------------------------------------------------

  /**
   * One entry per account — name, dot tone and state — from the ONE derivation
   * the rail's switcher also reads (ui/useAccounts.ts). A message showing one
   * name for its account while the switcher above it shows another would be the
   * same account described twice.
   *
   * The `--acct-off` tone now means what it says: `Account.syncEnabled` is
   * false. Increment 3 stood in "no identity for this account is enabled",
   * which is a different fact that merely correlated with it; the switcher is
   * what loads the Accounts, so the real flag is available here now and the
   * stand-in is gone.
   */
  const { byId: accountsById, accounts } = useAccounts();

  /** The dot appears only when it could distinguish anything: several accounts,
   * and a desk that is showing all of them. */
  const showAccountDot = scope === null && accounts.length > 1;
  const scopeName = scope === null ? null : accountsById.get(scope)?.name ?? scope;

  // -- loading ----------------------------------------------------------------

  /**
   * THE DESK ON SCREEN MUST BE THE DESK THE ENGINE BELIEVES IN.
   *
   * Deliberately without a dependency list, and this is not laziness. Loading
   * an ordinary view CLEARS `SyncEngine.desk` (sync.ts loadView) — correctly,
   * because with a desk still recorded every Email change would re-run a dozen
   * queries for a screen nobody is looking at. But the boot sequence loads a
   * view, and so does anything else that navigates through `loadView`, and if
   * that lands after this route's own load then `currentDesk` is null while the
   * desk is plainly on screen: opening a drawer would do nothing, `refreshDesk`
   * would return early, and archive-all would refuse with "open the desk
   * first". So the check is re-run on every render and re-asserts the desk
   * whenever the engine has stopped agreeing. It cannot loop: `loadDesk` sets
   * the desk synchronously before it awaits anything, so the next render finds
   * a match and does nothing.
   */
  //
  // NOT BEFORE THE AREAS ARE KNOWN (#673). The session, and its account id,
  // land before the reference data does, and a desk loaded in that gap plans
  // only the tray and Needs sorting. Those rows seed the cursor, which then
  // stays on the BOTTOM section once the areas arrive and the real top of the
  // desk appears above it. `cursor.Area` is set by the same step that stores
  // the areas (sync.loadReferenceData), so waiting for it waits for them.
  useEffect(() => {
    if (!state.accountId) return;
    if (state.cursor.Area === null) return;
    const current = sync.currentDesk;
    if (current && current.tab === tab && current.scope === scope) return;
    void sync.loadDesk(tab, scope);
  });

  // A tab or scope change forgets what was selected on the other one: the rows
  // are gone, and a selection the user cannot see is one they cannot undo.
  useEffect(() => {
    setSelected(new Set());
    setAnchor(null);
    setCursor(null);
    setRefileOpen(false);
    setOpenDrawers(new Set());
  }, [tab, scope]);

  // Keep the cursor pointing at a row that still exists — and when its row
  // leaves, on the row that took its place rather than the first one, or
  // archive-and-advance scrolls the list twice (#685, desk/flatten.ts
  // cursorAfterRemoval). `idsBefore` is the list the cursor was last checked
  // against: by the time this runs the row is already gone from `ids`.
  const idsBefore = useRef<Id[]>(ids);
  useEffect(() => {
    const before = idsBefore.current;
    idsBefore.current = ids;
    if (cursor && ids.includes(cursor)) return;
    setCursor(cursorAfterRemoval(before, ids, cursor));
  }, [ids, cursor]);

  const open = state.emails[openId ?? ""] ? effectiveEmail(state, openId as Id) : null;

  // -- selection --------------------------------------------------------------

  const targets = useCallback(
    (id?: Id | null): Id[] => {
      if (selected.size > 0) return [...selected];
      const one = id ?? cursor;
      return one ? [one] : [];
    },
    [selected, cursor],
  );

  const toggleSelect = useCallback(
    (id: Id, shift: boolean) => {
      setSelected((prev) => {
        const next = new Set(prev);
        if (shift && anchor) {
          const from = ids.indexOf(anchor);
          const to = ids.indexOf(id);
          if (from >= 0 && to >= 0) {
            const [lo, hi] = from < to ? [from, to] : [to, from];
            for (let i = lo; i <= hi; i += 1) {
              const rid = ids[i];
              if (rid) next.add(rid);
            }
            return next;
          }
        }
        if (next.has(id)) next.delete(id);
        else next.add(id);
        return next;
      });
      setAnchor(id);
    },
    [anchor, ids],
  );

  const clearSelection = useCallback(() => {
    setSelected(new Set());
    setRefileOpen(false);
  }, []);

  // -- the verbs --------------------------------------------------------------

  const {
    openMessage,
    closeMessage,
    afterRemoval,
    doArchive,
    doTrash,
    doNotJunk,
    doStar,
    doPin,
    doSnooze,
    doImportant,
    allImportant,
    doMute,
    doRead,
    replyTo,
    replyAllTo,
    forwardOn,
  } = useListVerbs({
    ids,
    openId,
    route: listRoute,
    targets,
    setCursor,
    clearSelection,
    openSnoozeMenu: setSnoozeIds,
    // The desk never lists snoozed mail, so `b` always means "put this away"
    // rather than "bring it back".
    unsnoozeInstead: false,
    // On the desk the flag is THE TRAY — the word every other control here uses
    // for it (SelectionToolbar's button, the row's ⚑ label).
    flagWord: "tray",
  });

  /**
   * A PLAIN CLICK ON A ROW'S MESSAGE CELL: open it, and drop the ticked selection.
   *
   * The file-manager convention, and a reported defect: a Ctrl/Shift-click
   * selection used to survive a plain click on another message, so the next verb
   * acted on rows the user believed they had left behind (`targets` prefers the
   * selection over the open message). A MODIFIED click never reaches here — the
   * row's capture handler turns it into a selection first (ui/desk/DeskRow.tsx)
   * — and neither does the keyboard: Enter, `o`, `j`/`k` open through
   * `openMessage` and leave a selection built with `x`/Space alone.
   *
   * The SENDER cell is not this either. A plain click there still toggles that
   * row (#375): it is the one selection gesture that needs no modifier, so it has
   * to add to a selection rather than replace it. The clicked row becomes the
   * anchor, so a Shift-click after it ranges from the message just opened.
   */
  const openFromRow = useCallback(
    (id: Id) => {
      clearSelection();
      setAnchor(id);
      openMessage(id);
    },
    [clearSelection, openMessage],
  );

  /**
   * "None of these" (#176) — wired here rather than shared, because it belongs
   * to the needs-a-category workflow and the desk needs it only so the reading
   * pane's own control is not a button that does nothing. The desk's OWN answer
   * to an unsorted message is re-filing it (`1`–`9`), which is a label write and
   * a different gesture.
   */
  const doDismiss = useCallback(
    (id?: Id) => {
      const list = targets(id);
      if (list.length === 0) return;
      void (async () => {
        const changed = await actions.dismissCategory(list);
        if (changed.length === 0) return;
        pushToast({
          kind: "info",
          message: t("queue.dismissed", { count: changed.length }),
          action: { label: t("action.undo"), run: () => void actions.undoDismissCategory(changed) },
        });
      })();
      afterRemoval(list);
    },
    [actions, targets, afterRemoval, pushToast],
  );

  /**
   * Opening a message marks it read — instantly, locally (spec §12/§4a) — and
   * fetches its thread. Driven by the ROUTE rather than by the click, so a
   * message opened by a pasted link, a reload or the back button is treated
   * exactly like one opened from a row.
   */
  const opened = useRef<Id | null>(null);
  useEffect(() => {
    if (!openId) {
      opened.current = null;
      return;
    }
    if (opened.current === openId) return;
    const e = effectiveEmail(state, openId);
    if (!e) return;
    opened.current = openId;
    setCursor(openId);
    if (!e.isSeen) void actions.setSeen([openId], true);
    if (e.threadId) void sync.loadThread(e.threadId, undefined, { open: true });
  }, [openId, state, actions, sync]);
  // The mark-read above is NOT the read signal — it fires on a flick past a
  // message too. This is: shown for the dwell, once per message (#842).
  useOpenDwell(openId, (id) => void actions.recordOpen(id));

  // -- navigation -------------------------------------------------------------

  const goToTab = useCallback(
    (next: DeskTab) => {
      navigate({ page: "inbox", tab: next, ...(scope ? { acct: scope } : {}) });
    },
    [scope],
  );

  const openTray = useCallback(() => {
    navigate({ page: "tray", ...(scope ? { acct: scope } : {}) });
  }, [scope]);

  const toggleTray = useCallback(() => {
    setTrayOpen((v) => {
      saveTrayOpen(!v);
      return !v;
    });
  }, []);

  /** Fold or unfold one section, and remember it on this device. The write is
   * inside the updater so the stored value is always the one that was actually
   * applied rather than a second computation of it. */
  const toggleSection = useCallback((areaId: Id | null) => {
    const key = collapseKey(areaId);
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      saveCollapsedSections(next);
      return next;
    });
  }, []);

  const toggleDrawer = useCallback(
    (areaId: Id) => {
      setOpenDrawers((prev) => {
        const next = new Set(prev);
        if (next.has(areaId)) {
          next.delete(areaId);
          return next;
        }
        next.add(areaId);
        // Opening is what fetches it — nothing is queried for a closed drawer.
        void sync.loadDrawer(areaId);
        return next;
      });
    },
    [sync],
  );

  const loadMore = useCallback(
    (sectionKeyName: string) => {
      const areaId = drawerByKey.get(sectionKeyName);
      if (areaId !== undefined) {
        void sync.loadDrawer(areaId, { more: true });
        return;
      }
      void sync.loadDesk(tab, scope, { more: sectionKeyName });
    },
    [drawerByKey, sync, tab, scope],
  );

  // -- the line ---------------------------------------------------------------

  const threshold = state.deskPreferences?.lineThreshold ?? summary?.threshold ?? 0;
  /** The line cannot be moved from a number we have not read: without the
   * preferences, "±0.05" would be relative to a value this client invented. */
  const lineUnknown = state.deskPreferences === null;

  useEffect(() => {
    if (pendingLine === null) {
      setPreview(null);
      setPreviewFailed(false);
      return;
    }
    setPreview(null);
    setPreviewFailed(false);
    let cancelled = false;
    const handle = setTimeout(() => {
      void (async () => {
        const rows = await sync.previewLine(pendingLine);
        if (cancelled) return;
        if (!rows) {
          setPreviewFailed(true);
          return;
        }
        setPreview(headerCounts(rows));
      })();
    }, PREVIEW_DEBOUNCE_MS);
    return () => {
      cancelled = true;
      clearTimeout(handle);
    };
  }, [pendingLine, sync]);

  const moveLine = useCallback(
    (delta: number) => {
      setPendingLine((prev) => clampLine((prev ?? threshold) + delta));
    },
    [threshold],
  );

  const applyLine = useCallback(() => {
    if (pendingLine === null) return;
    setLineBusy(true);
    void (async () => {
      const ok = await actions.setLineThreshold(pendingLine);
      setLineBusy(false);
      if (ok) setPendingLine(null);
    })();
  }, [actions, pendingLine]);

  // -- re-filing and drawers --------------------------------------------------

  /** The areas mail can be re-filed into. An area OWNS a label, so filing IS
   * applying that label — local-only or mirrored alike since #704 (a local one
   * rides `labelIds/<id>` and stays in Mailroom). The one thing that still
   * blocks a target is its label not being loaded: there is then nothing to
   * apply, and a button that did nothing would be worse than a disabled one. */
  const refileTargets: RefileTarget[] = useMemo(
    () =>
      areas.map((area) => ({
        id: area.id,
        name: area.name,
        blocked: state.labels[area.labelId] ? null : t("desk.refile.labelMissing", { area: area.name }),
      })),
    [areas, state.labels],
  );

  const refile = useCallback(
    (areaId: Id, ids?: Id[]) => {
      const area = state.areas[areaId];
      if (!area) return;
      // `ids` is the reading pane's picker re-filing THE OPEN MESSAGE ONLY
      // (#921); the desk's own menu and the 1–9 keys pass nothing and take the
      // selection, exactly as before.
      const list = ids ?? targets();
      if (list.length === 0) return;
      if (!state.labels[area.labelId]) {
        pushToast({ kind: "error", message: t("desk.refile.labelMissing", { area: area.name }) });
        return;
      }
      // A SWAP, not an add (#704, core/refile.ts): the other areas' own labels
      // come off and the target's goes on, per message — otherwise a message
      // re-filed to a LATER area stays in the earlier one, because the
      // resolver picks the label-fed area by position.
      const changes = refileChanges(state, list, areaId);
      void actions
        .relabel(changes, filedText(list.length, area.name))
        .then(() =>
          offerRuleSuggestion({ store, sync, actions, offerUndo, pushToast }, list, areaId, changes.length),
        );
      // The undo is the exact inverse of what changed for each message, so it
      // restores the prior label set rather than guessing at one: what was
      // removed goes back, the target comes off only where this added it.
      const inverse = invertChanges(changes);
      offerUndo(filedText(list.length, area.name), async () => {
        await actions.relabel(inverse, "Re-file undone");
        return true;
      });
      // The pane's picker (`ids` given) re-files the message being read and
      // leaves the selection and the toolbar's menu as they were (#921).
      if (!ids) {
        setRefileOpen(false);
        clearSelection();
      }
    },
    [actions, sync, store, state, targets, pushToast, clearSelection, offerUndo],
  );

  /** Ask again, now, rather than waiting out the poll interval — and re-load
   * the desk itself, because a poll only advances the cursors and the desk's
   * sections are separate queries (core/sync.ts loadDesk). */
  const retry = useCallback(() => {
    void (async () => {
      await sync.poll();
      await sync.refreshDesk();
    })();
  }, [sync]);

  /**
   * Opens the account switcher — which is TWO affordances answered by one
   * place, deliberately. The stale sentence's `Details` wants the health panel;
   * the phone header's `Personal ▾` wants the scope chooser; both live in the
   * same disclosure (ui/AccountSwitcher.tsx), so both come here.
   */
  const openSwitcher = useCallback(() => {
    shell.railKeys.current?.openSwitcher();
  }, [shell]);

  const askArchiveDrawer = useCallback(
    (areaId: Id) => {
      const area = state.areas[areaId];
      if (!area) return;
      setConfirm({ areaId, name: area.name, count: areaCounts(summaryRows, areaId).drawer });
    },
    [state.areas, summaryRows],
  );

  const runArchiveDrawer = useCallback(() => {
    const target = confirm;
    if (!target) return;
    setArchiving(true);
    void (async () => {
      const result = await actions.archiveDrawer(target.areaId);
      setArchiving(false);
      setConfirm(null);
      // Nothing moved and nothing failed: `archiveDrawer` already said why
      // (no Archive folder, offline, a read that failed), and a second,
      // cheerier toast on top of its error would be the client contradicting
      // itself.
      if (result.archived === 0 && result.failed === 0) return;
      const base = result.bound
        ? t("desk.drawer.archivedBound", {
            n: result.archived,
            total: target.count,
            area: target.name,
          })
        : t("desk.drawer.archived", { n: result.archived, area: target.name });
      const message =
        result.failed > 0
          ? `${base} — ${t("desk.drawer.archivedFailed", { n: result.failed })}`
          : base;
      if (result.failed > 0) {
        // A partial sweep keeps its plain error toast. An undo offer beside it
        // would be a button whose scope the sentence does not state — "put back
        // the 4,812 that worked" is not what "Undo" reads as next to "188 could
        // not be archived".
        pushToast({ kind: "error", message });
        return;
      }
      // The inverse is the SAME WALK BACKWARDS: restoreToInbox over exactly the
      // ids that moved, in the same 200-message chunks and the same batches
      // (core/actions.ts moveInChunks, which both directions share). Which is
      // why archiveDrawer returns its ids — a count could not build this.
      offerUndo(message, () => actions.restoreToInbox(result.ids, { direct: true }));
    })();
  }, [actions, confirm, pushToast, offerUndo]);

  /**
   * CROSS THE LINE — Shift+↑ "this belongs on the surface", Shift+↓ "this
   * belongs in the drawer" (#375 increment 7, #464).
   *
   * IT IS THE SAME CORRECTION THE `!` KEY MAKES, said in the desk's words. The
   * line is a threshold over the model's score, and `Triage/correct` is how a
   * user overrules it: since #464 the server records that as a PLACEMENT
   * (`messages.line_side`, COALESCEd over the score comparison), so the row
   * genuinely moves to the other tab's section and an `Email` change row is
   * appended for every client that is listening.
   *
   * ⚠ ABSOLUTE, NOT A TOGGLE, and that is what makes the undo honest. These
   * two keys each say ONE side outright, so the inverse is simply the other
   * side, over the same ids, exactly as `doStar`'s is. (`doImportant` was once
   * the counter-example here: its first-row toggle made "the opposite" over a
   * selection unsendable, so it offered no undo. #921 pointed it the way this
   * does — mark important unless every row already is — and it now registers
   * its undo the same way, ui/useListVerbs.ts.)
   *
   * The refresh does NOT wait: `setImportant` already polls, and the desk's
   * sections are separate queries (core/sync.ts loadDesk), so this is what moves
   * the row from one tab to the other. Fired and not awaited — the keystroke has
   * already been answered by the toast.
   */
  const crossLine = useCallback(
    async (toSurface: boolean) => {
      const list = targets();
      if (list.length === 0) return;
      // Only on a correction the server accepted: an undo offer for a write
      // that was refused is the worst kind of button (the same rule doArchive
      // follows).
      if (!(await actions.setImportant(list, toSurface))) return;
      offerUndo(crossedLineText(list.length, toSurface), async () => {
        // No refreshDesk here: setImportant polls, and a correction appends an
        // Email change row, so that poll re-runs the desk (#665).
        return actions.setImportant(list, !toSurface);
      });
      // `clearSelection`, deliberately NOT `afterRemoval`. The rows do leave
      // this tab, but only the ones that were on the OTHER side of the line to
      // begin with — pressing Shift+↑ on something already above it is a
      // correction that changes nothing on screen, and advancing the reading
      // pane past the message you are reading because you agreed with where it
      // already was would be the app punishing you for confirming it.
      clearSelection();
    },
    [actions, targets, offerUndo, clearSelection],
  );

  /**
   * THE PHONE'S TWO SWIPES (#375 increment 7): right archives, left is the
   * tray.
   *
   * They go through the SAME verbs the buttons and the keys do, which is what
   * makes them undoable at all — `doArchive` and `doStar` register their own
   * inverses (ui/useListVerbs.ts), so a swipe raises exactly the toast a
   * keystroke would. A second implementation here would be a gesture with no
   * undo, on the surface where a mis-press is most likely.
   *
   * `doArchive(id)`/`doStar(id)` resolve their targets through `targets(id)`,
   * which prefers a SELECTION when there is one — so this is only ever handed
   * to a row while nothing is selected (see where it is passed), and the id it
   * is given is therefore the row that was swiped.
   */
  const onSwipe = useCallback(
    (id: Id, action: SwipeAction) => {
      if (action === "archive") void doArchive(id);
      else void doStar(id);
    },
    [doArchive, doStar],
  );

  // -- the cursor ------------------------------------------------------------

  /**
   * Move the cursor `rows` message rows — the ONE implementation of that move.
   *
   * `j`/`k` call it with ±1, the grid's own arrows call it with ±1, and
   * PageUp/PageDown call it with a screenful (ui/desk/VirtualList.tsx measures
   * that). Which matters because moving the cursor is not only moving the cursor:
   * with a message open the reading pane FOLLOWS it, and a second copy of this in
   * the grid would be arrows that walk the list while the pane stays behind.
   *
   * `nextRowId` skips everything that is not a row and clamps at both ends rather
   * than wrapping (ui/desk/flatten.ts), so this loop stops of its own accord at
   * the end of the list — the `after === next` guard is that clamp observed.
   */
  const moveCursor = useCallback(
    (rows: number) => {
      if (rows === 0) return;
      const step = rows > 0 ? 1 : -1;
      let next = cursor;
      for (let i = 0; i < Math.abs(rows); i += 1) {
        const after = nextRowId(items, next, step);
        if (after === null || after === next) break;
        next = after;
      }
      if (!next || next === cursor) return;
      setCursor(next);
      if (openId) openMessage(next, { replace: true });
    },
    [items, cursor, openId, openMessage],
  );

  /** Home and End. A null cursor is what `nextRowId` reads as "start from the
   * near end", so the two edges are the same call with the two directions. */
  const jumpCursor = useCallback(
    (to: "first" | "last") => {
      const target = nextRowId(items, null, to === "first" ? 1 : -1);
      if (!target || target === cursor) return;
      setCursor(target);
      if (openId) openMessage(target, { replace: true });
    },
    [items, cursor, openId, openMessage],
  );

  /** What the grid does with the keys it owns (ui/desk/VirtualList.tsx). Every
   * one of them is a verb this route already had — the grid supplies the arrows,
   * Home/End, PageUp/PageDown and Space that a `role="grid"` owes the keyboard,
   * and hands them straight back here. */
  const gridKeys: GridKeys = useMemo(
    () => ({
      move: moveCursor,
      edge: jumpCursor,
      open: () => {
        if (cursor) openMessage(cursor);
      },
      select: (extend: boolean) => {
        if (cursor) toggleSelect(cursor, extend);
      },
    }),
    [moveCursor, jumpCursor, cursor, openMessage, toggleSelect],
  );

  // -- the keyboard, desk half (the chain lives in Shell) ---------------------

  const handleVerb = useCallback(
    (ev: KeyboardEvent): boolean => {
      switch (ev.key) {
        // CROSS THE LINE (#464). Shift+↑ is "this belongs on the surface",
        // Shift+↓ is "this belongs in the drawer" — over the cursor, or over
        // the whole selection.
        //
        // ⚠ THE UNMODIFIED ARROWS ARE THE GRID'S NOW (#447), which SUPERSEDES the
        // decision that used to be written here: "unmodified arrows are left
        // alone — they are the browser's own scrolling, which is the only way to
        // move a long list without a pointer". That was true of a `<ul>` of
        // links. The list is a `role="grid"` with one tab stop and a current row
        // named by `aria-activedescendant`, and a grid that says that owes the
        // keyboard its arrows: the thing being moved is the active descendant,
        // which the browser cannot scroll to because it does not know it exists.
        // So ui/desk/VirtualList.tsx consumes them WHILE FOCUS IS IN THE LIST and
        // this chain still returns false for them, which leaves the browser's own
        // scrolling exactly where it was from everywhere else on the route.
        case "ArrowUp":
          if (!ev.shiftKey) return false;
          ev.preventDefault();
          void crossLine(true);
          return true;
        case "ArrowDown":
          if (!ev.shiftKey) return false;
          ev.preventDefault();
          void crossLine(false);
          return true;
        // `j`/`k` work from anywhere on the desk, not only in the list — that is
        // why they stay here rather than moving into the grid, and why the grid
        // calls this same mover rather than reimplementing it.
        case "j":
          ev.preventDefault();
          moveCursor(1);
          return true;
        case "k":
          ev.preventDefault();
          moveCursor(-1);
          return true;
        case "Enter":
        case "o":
          if (cursor) {
            ev.preventDefault();
            openMessage(cursor);
          }
          return true;
        case "e":
          ev.preventDefault();
          void doArchive();
          return true;
        case "#":
          ev.preventDefault();
          void doTrash();
          return true;
        // `s` is the tray's own key here — the tray IS the flag (#444). It used
        // to share this case with `f`, which is Forward's key everywhere else
        // (#582) and made Forward's own `case "f":` below unreachable: the
        // first matching case in a switch wins, so every `f` on the desk ran
        // this toggle and the later Forward case was dead code the compiler
        // never flagged (`noFallthroughCasesInSwitch` checks for a missing
        // break, not a repeated label). #624 §1 gave `f` back to Forward
        // everywhere; `s` alone is now this surface's toggle.
        case "s":
          ev.preventDefault();
          void doStar();
          return true;
        case "p":
          ev.preventDefault();
          void doPin();
          return true;
        case "b":
          ev.preventDefault();
          doSnooze();
          return true;
        case "m":
          ev.preventDefault();
          void doMute();
          return true;
        case "x":
          if (cursor) {
            ev.preventDefault();
            toggleSelect(cursor, ev.shiftKey);
          }
          return true;
        case "r": {
          const target = open ?? (cursor ? effectiveEmail(state, cursor) : null);
          if (target) {
            ev.preventDefault();
            replyTo(target);
          }
          return true;
        }
        // Reply all (#646), beside Reply — see ListRoute's case for why `a` and
        // not shift-r, and why it cannot collide with `g a` (Archive).
        case "a": {
          const target = open ?? (cursor ? effectiveEmail(state, cursor) : null);
          if (target) {
            ev.preventDefault();
            replyAllTo(target);
          }
          return true;
        }
        case "f": {
          // Forward (#582), next to reply: the composer takes ONE parent, so this
          // acts on the open message or the cursor row, never the selection.
          const target = open ?? (cursor ? effectiveEmail(state, cursor) : null);
          if (target) {
            ev.preventDefault();
            forwardOn(target);
          }
          return true;
        }
        case "l":
          ev.preventDefault();
          setLabelMenuOpen(true);
          return true;
        case "I":
          ev.preventDefault();
          void doRead(true);
          return true;
        case "U":
          ev.preventDefault();
          void doRead(false);
          return true;
        default: {
          // `1`–`9` re-file to the Nth area in position order. In Needs sorting
          // this IS placing: the same label write, because an area owns a label
          // and putting mail in one is applying it. (The rule the placement
          // implies is offered in #375 increment 5.) It goes through the SAME
          // `refile` as the menu, so the keyboard swaps labels exactly as the
          // menu does (#704) — local area labels included.
          if (!/^[1-9]$/.test(ev.key)) return false;
          const area = areas[Number(ev.key) - 1];
          if (!area) return true;
          ev.preventDefault();
          refile(area.id);
          return true;
        }
      }
    },
    [
      items,
      cursor,
      openId,
      open,
      state,
      areas,
      moveCursor,
      openMessage,
      replyTo,
      replyAllTo,
      forwardOn,
      toggleSelect,
      doArchive,
      doTrash,
      doStar,
      doPin,
      doSnooze,
      doMute,
      doRead,
      refile,
      crossLine,
    ],
  );

  // Published on EVERY render (no dependency list), so the shell's one listener
  // always calls the current closures without re-registering itself — the same
  // contract ListRoute has (ui/shellContext.ts says why it is a ref).
  const { listKeys } = shell;
  useEffect(() => {
    listKeys.current = {
      openPalette: () => setPaletteOpen(true),
      closeTopPopover: () => {
        // Innermost first. The confirm dialog is modal and traps focus, so it
        // normally consumes its own Escape; this is the fallback for the case
        // where focus is somehow outside it.
        if (confirm) {
          setConfirm(null);
          return true;
        }
        if (paletteOpen) {
          setPaletteOpen(false);
          return true;
        }
        if (refileOpen) {
          setRefileOpen(false);
          return true;
        }
        if (labelMenuOpen) {
          setLabelMenuOpen(false);
          return true;
        }
        if (snoozeIds) {
          setSnoozeIds(null);
          return true;
        }
        if (followUpSnooze) {
          setFollowUpSnooze(null);
          return true;
        }
        return false;
      },
      clearSelection: () => {
        if (selected.size === 0) return false;
        clearSelection();
        return true;
      },
      blocksVerbs: () => paletteOpen || snoozeIds !== null || followUpSnooze !== null || confirm !== null,
      handleVerb,
    };
    return () => {
      listKeys.current = null;
    };
  });

  // -- rendering --------------------------------------------------------------

  const areaName = (areaId: Id | null): string =>
    areaId === null ? t("desk.section.needsSorting") : state.areas[areaId]?.name ?? "";

  /** The account whose dot a row draws, or null when it draws none — several
   * accounts and an unscoped desk (`showAccountDot`). One derivation, read by the
   * dot and by the row's name. */
  const accountNameOf = (email: EffectiveEmail): string | null => {
    if (!showAccountDot) return null;
    const accountId = email.identityId ? state.identities[email.identityId]?.accountId : undefined;
    return accountId ? accountsById.get(accountId)?.name ?? null : null;
  };

  const renderRow = (id: Id): JSX.Element | null => {
    const email = effectiveEmail(state, id);
    // THE SECTION SAID THIS ID WAS HERE AND THE STORE HAS NO MESSAGE FOR IT.
    // Returning null left a row-height hole in a virtualised list — every item
    // is placed at `index * height` (tokens.css --desk-row-h), so the gap stays
    // whatever we render — and a silent gap is the one thing worse than saying
    // so. It is what "showing what was loaded" means one row at a time: offline
    // after a partial load, or a section fetched while its Email/get failed.
    if (!email) {
      // `role="gridcell"` because this REPLACES the row's three cells: a
      // `role="row"` with no cell in it is a row a screen reader cannot enter.
      return (
        <p role="gridcell" className="desk-row is-not-loaded">
          {t("row.notLoaded")}
        </p>
      );
    }
    const thread = state.threads[email.threadId];
    const threadCount = thread && thread.emailIds.length > 1 ? thread.emailIds.length : null;
    const accountId = email.identityId ? state.identities[email.identityId]?.accountId : undefined;
    const entry = showAccountDot && accountId ? accountsById.get(accountId) ?? null : null;
    const account: RowAccount | null = entry
      ? { name: entry.name, tone: entry.tone, color: entry.color }
      : null;
    return (
      <DeskRow
        email={email}
        selected={selected.has(id)}
        isCursor={cursor === id}
        isOpen={openId === id}
        threadCount={threadCount}
        account={account}
        queued={state.overlay[id] !== undefined && state.queue.length > 0}
        now={now}
        onOpen={openFromRow}
        onToggleSelect={toggleSelect}
        // GESTURES ONLY ON THE PHONE, AND ONLY WITH NOTHING SELECTED. The
        // second half is not caution, it is the only correct answer: the verbs
        // resolve their targets through `targets()`, which prefers the
        // selection, so a swipe with four rows selected would archive four rows
        // — a gesture meaning something other than the row under the finger.
        // With a selection on screen the toolbar is the path.
        {...(phone && selected.size === 0 ? { onSwipe } : {})}
        swipeFlagLabel={t("desk.selection.tray")}
        reducedMotion={reducedMotion}
      />
    );
  };

  const renderItem = (item: DeskItem): JSX.Element | null => {
    switch (item.kind) {
      case "heading":
        return (
          <SectionHeading
            name={areaName(item.areaId)}
            count={item.count}
            needsSorting={item.areaId === null}
            collapsed={item.collapsed}
            onToggle={() => toggleSection(item.areaId)}
          />
        );
      case "row":
        return renderRow(item.id);
      case "more":
        return (
          <button
            type="button"
            className="button desk-more"
            onClick={() => loadMore(item.section)}
          >
            {moreText(item.remaining, PAGE_SIZE, sectionTotalOf(item.section))}
          </button>
        );
      case "error":
        // Deliberately NOT role="alert": the header sentence is the desk's one
        // live region (ui/desk/DeskHeader.tsx), and an assertive announcement
        // per failed section would interrupt the reader once per area on a
        // connection that is having a bad minute. The sentence itself already
        // carries "cannot reach the server".
        return (
          <p className="desk-section-error">{t("desk.section.error", { error: item.message })}</p>
        );
      case "line":
        return (
          <TheLine
            reviewCount={counts.review}
            drawerCount={counts.drawers}
            threshold={threshold}
            pending={pendingLine}
            preview={preview}
            previewFailed={previewFailed}
            onStricter={() => moveLine(LINE_STEP)}
            onLooser={() => moveLine(-LINE_STEP)}
            onApply={applyLine}
            onCancel={() => setPendingLine(null)}
            busy={lineBusy || lineUnknown}
          />
        );
      case "drawer": {
        const area = state.areas[item.areaId];
        if (!area) return null;
        return (
          <DrawerRow
            name={area.name}
            count={areaCounts(summaryRows, item.areaId).drawer}
            periodDays={drawerPeriodDays(area, state.deskPreferences)}
            // drawerPeriodDays deliberately answers only "what is the period";
            // whether the sweep RUNS is a login-level switch, and it defaults
            // to off (#443). The row consults it here so it never promises an
            // archive that nothing performs.
            sweepEnabled={state.deskPreferences?.drawerSweepEnabled ?? false}
            open={item.open}
            busy={archiving}
            onToggle={() => toggleDrawer(item.areaId)}
            onArchiveAll={() => askArchiveDrawer(item.areaId)}
          />
        );
      }
      case "empty":
        return <p className="desk-nothing">{t("desk.empty", { n: counts.drawers })}</p>;
    }
  };

  /** Which area's section a row was drawn in — the row's name says it in words
   * ("in Bills"), and Needs sorting names itself.
   *
   * FROM THE SECTION, not from `Email.area`: that property is body-only and
   * opt-in (jmap/types.ts), so on a row fetched as part of a list it is simply
   * absent — the desk's lists learn a message's area from the QUERY that returned
   * it, and this is that query, read back. */
  function areaNameOfSection(key: string): string | null {
    const surface = sections.find((sec) => sec.key === key);
    if (surface) return areaName(surface.areaId);
    const drawer = drawers.find((d) => d.key === key);
    return drawer ? areaName(drawer.areaId) : null;
  }

  /**
   * WHAT THE ACCESSIBILITY TREE IS TOLD ABOUT ONE ITEM (#447).
   *
   * A message row gets the whole sentence (ui/desk/rowName.ts) and its selection
   * state; a heading gets `rowheader` so its `<h2>` sits in a header cell rather
   * than in an ordinary one; everything else takes the defaults — one `gridcell`
   * and no label, because the line, a drawer and a "show more" already say what
   * they are and a label would be heard INSTEAD of them.
   */
  const rowAriaOf = (item: DeskItem): RowAria => {
    switch (item.kind) {
      case "row": {
        const email = effectiveEmail(state, item.id);
        // Nothing loaded for this id: the row says so in its own cell, and a
        // label built from a message we do not have would be an invention.
        if (!email) return { cell: "own" };
        const thread = state.threads[email.threadId];
        return {
          label: rowNameFor(email, {
            areaName: areaNameOfSection(item.section),
            // The same condition the DOT is drawn under, so the word appears on
            // exactly the rows that carry the colour and nowhere else.
            accountName: accountNameOf(email),
            threadCount: thread && thread.emailIds.length > 1 ? thread.emailIds.length : null,
            queued: state.overlay[item.id] !== undefined && state.queue.length > 0,
            now,
          }),
          selected: selected.has(item.id),
          current: openId === item.id,
          cell: "own",
        };
      }
      case "heading":
        return { cell: "rowheader" };
      default:
        return {};
    }
  };

  /** What the server says a section holds — the "of 218" the load-more button
   * is a bound against. */
  function sectionTotalOf(key: string): number {
    const surface = sections.find((s) => s.key === key);
    if (surface) return surface.total;
    const drawer = drawers.find((d) => d.key === key);
    return drawer ? drawer.total : 0;
  }

  const itemKey = (item: DeskItem, index: number): string => {
    switch (item.kind) {
      case "heading":
        return `h:${item.section}`;
      case "row":
        return `r:${item.id}`;
      case "more":
        return `m:${item.section}`;
      case "error":
        return `e:${item.section}`;
      case "line":
        return "line";
      case "drawer":
        return `d:${item.areaId}`;
      case "empty":
        return `empty:${index}`;
    }
  };

  return (
    <>
      <main className="content" id="messages" tabIndex={-1}>
        {/* The desk's list pane IS the first grid column of `.content`, exactly
            as MessageList's is — `.content` is a grid whose columns are the
            reading-pane layout, so anything else here would shove the panes one
            column across (ui/TabStrip.tsx's ⚠ says what that looks like). It
            keeps the `.list-pane` class for the same reason: the narrow
            breakpoints hide THAT selector when a message is open. */}
        <section className="list-pane desk-pane" ref={paneRef} aria-label={t("desk.pane")}>
          <div className="desk-chrome" ref={chromeRef}>
            <DeskHeader
              scopeName={scopeName}
              loading={summary === null}
              trayCount={trayTotal}
              reviewCount={counts.review}
              drawerCount={counts.drawers}
              syncedSeconds={syncedAgoSeconds(state.lastSyncAt, now)}
              state={header}
              now={now}
              onRetry={retry}
              onDetails={openSwitcher}
              onScope={openSwitcher}
              phone={phone}
            />
            <ShadowBanners notices={shadowNotices} />
            <KeyExpiryBanners keys={ownKeys} />
            <TrayStrip
              phone={phone}
              open={trayOpen}
              onToggle={toggleTray}
              onOpenTray={openTray}
              count={trayIds.length}
              total={trayTotal}
              oldestWaitMs={tray.oldestWaitMs}
              overWeek={tray.overWeek}
              // Offline, the strip's counts are a snapshot and it says so in the
              // short form the line has room for. Null the rest of the time: an
              // "as of" on a live number is noise.
              asOfSeconds={
                header.kind === "offline" ? syncedAgoSeconds(state.lastSyncAt, now) : undefined
              }
            >
              {/* The phone's strip is one tappable line and shows no preview
                  rows at all, so building them there would be five rows of
                  work for a branch that never renders them. */}
              {phone
                ? null
                : trayIds.slice(0, TRAY_PREVIEW_ROWS).map((id) => (
                    <div className="desk-tray-row" key={`tray-${id}`}>
                      {renderRow(id)}
                    </div>
                  ))}
            </TrayStrip>
            <FollowUpStrip
              items={followUps.items}
              now={now}
              pending={followUps.pending}
              onOpen={(item) => {
                // A nudge is often for mail the desk has not loaded (a sent
                // message, ten days old), so its conversation is fetched as it
                // opens; the pane appears when the message lands.
                if (item.threadId) void sync.loadThread(item.threadId, item.emailId, { open: true });
                openMessage(item.emailId);
              }}
              onDone={(item) => {
                void followUps.dismiss(item).then((ok) => {
                  if (!ok) pushToast({ kind: "error", message: t("followUp.doneFailed") });
                });
              }}
              onSnooze={(item) => setFollowUpSnooze(item)}
            />
            <DeskTabs
              tab={tab}
              reviewCount={counts.review}
              restCount={counts.drawers}
              onSelect={goToTab}
            />
            {selected.size > 0 ? (
              <SelectionToolbar
                count={selected.size}
                areas={refileTargets}
                refileOpen={refileOpen}
                onArchive={() => void doArchive()}
                onTray={() => void doStar()}
                onMarkRead={() => void doRead(true)}
                onMarkUnread={() => void doRead(false)}
                onImportant={() => void doImportant()}
                allImportant={allImportant()}
                onLabel={() => setLabelMenuOpen(true)}
                onToggleRefile={() => setRefileOpen((v) => !v)}
                // The toolbar's menu takes the selection: the picker's second,
                // single-id argument is the pane's only (#921).
                onRefile={(areaId) => refile(areaId)}
                onClear={clearSelection}
              />
            ) : null}
          </div>

          <VirtualList
            items={items}
            scrollRef={paneRef}
            chromeRef={chromeRef}
            cursorIndex={rowIndexOf(items, cursor)}
            renderItem={renderItem}
            itemKey={itemKey}
            rowAria={rowAriaOf}
            keys={gridKeys}
            gridId={DESK_GRID_ID}
            // NAMED BY THE SELECTED TAB, which is also the tab that points its
            // `aria-controls` back here (ui/desk/DeskTabs.tsx). A grid cannot also
            // be a `tabpanel`, so the grid takes the panel's place and its name:
            // "Review first 6" says which half you are in, which a fixed label
            // for the list could not.
            labelledBy={deskTabId(tab)}
          />
        </section>

        {open ? (
          <>
            <PaneSplitter
              layout={shell.paneLayout}
              size={shell.paneSplit}
              onResize={shell.resizePane}
              onDragStart={shell.beginPaneDrag}
              onDragEnd={shell.endPaneDrag}
            />
            <ThreadPane
              email={open}
              // "← Inbox · 3 of 6" on the phone: where this message sits among
              // the rows the list is actually showing, which is what the reader
              // is about to swipe or press `j` through.
              position={positionOf(items, open.id)}
              backLabel={t("desk.header.inbox")}
              onClose={closeMessage}
              onReply={() => replyTo(open)}
              onReplyAll={() => replyAllTo(open)}
              onForward={() => forwardOn(open)}
              onArchive={() => void doArchive(open.id)}
              onNotJunk={() => void doNotJunk(open.id)}
              onTrash={() => void doTrash(open.id)}
              onTray={() => void doStar(open.id)}
              onPin={() => void doPin(open.id)}
              onPrint={() => window.print()}
              onMute={() => void doMute(open.id)}
              onSnooze={() => doSnooze(open.id)}
              onImportant={() => void doImportant(open.id)}
              onCreateRule={() => shell.openRuleDraft(draftRuleFrom(open))}
              onLabel={() => setLabelMenuOpen(true)}
              // THE PANE'S RE-FILE (#921), offered only when there is something
              // to file into — `refileTargets` is the very list the toolbar's
              // menu shows, so the two cannot disagree about the areas.
              onRefile={refileTargets.length > 0 ? () => setRefilePickerOpen(true) : undefined}
              onDismiss={() => doDismiss(open.id)}
            />
          </>
        ) : null}
      </main>

      {labelMenuOpen ? (
        <LabelMenu ids={targets()} onClose={() => setLabelMenuOpen(false)} />
      ) : null}
      {refilePickerOpen && open ? (
        // THE OPEN MESSAGE ONLY (#921): the picker moves the message being
        // read, never the selection, and the pane stays on it — no advance.
        <RefilePicker
          areas={refileTargets}
          onPick={(areaId) => refile(areaId, [open.id])}
          onClose={() => setRefilePickerOpen(false)}
        />
      ) : null}
      {snoozeIds ? (
        <SnoozeMenu
          ids={snoozeIds}
          onClose={() => setSnoozeIds(null)}
          // The desk hides snoozed mail, so a snoozed row leaves it and the
          // reading pane advances — the callback closes over THIS render's
          // list, which is the pre-action one advancePastOpen needs.
          onSnoozed={() => afterRemoval(snoozeIds)}
        />
      ) : null}
      {followUpSnooze ? (
        <SnoozeMenu
          ids={[followUpSnooze.emailId]}
          onClose={() => setFollowUpSnooze(null)}
          // A snoozed message is not a candidate until it wakes (the server
          // leaves it out), so its row leaves the strip now.
          onSnoozed={() => followUps.remove(followUpSnooze.emailId)}
        />
      ) : null}
      {paletteOpen ? (
        <CommandPalette
          onClose={() => setPaletteOpen(false)}
          onCompose={() => navigate(withCompose(listRoute, "new"))}
          onShowShortcuts={() => navigate({ page: "settings", section: "shortcuts" })}
          onLabel={() => setLabelMenuOpen(true)}
          onArchive={() => void doArchive()}
          onTrash={() => void doTrash()}
          onMarkRead={() => void doRead(true)}
          onMarkUnread={() => void doRead(false)}
          onPin={() => void doPin()}
          onSnooze={() => doSnooze()}
          onImportant={() => void doImportant()}
          onMute={() => void doMute()}
          onSearch={shell.submitSearch}
          hasSelection={selected.size > 0 || cursor !== null}
          onPrint={open ? () => window.print() : undefined}
        />
      ) : null}
      {confirm ? (
        <ConfirmDialog
          title={t("desk.drawer.confirmTitle", { n: confirm.count, area: confirm.name })}
          body={t("desk.drawer.confirmBody")}
          confirmLabel={t("desk.drawer.archiveAll")}
          busy={archiving}
          onConfirm={runArchiveDrawer}
          onCancel={() => setConfirm(null)}
        />
      ) : null}
    </>
  );
}
