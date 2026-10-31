// SPDX-License-Identifier: AGPL-3.0-only

// THE TRAY (#375 increment 4): what `#/tray` is.
//
// The desk's strip states the tray's SHAPE in one line and shows five rows.
// This is the whole thing, and it is arranged by the only question the tray
// raises: how long has each of these been waiting? Four groups — Today, This
// week, Over a week, Over a month — because "what have I been ignoring" is a
// shape, not a list of forty exact durations (core/desk.ts waitBucket).
//
// A SEPARATE ROUTE, NOT A MODE OF DeskRoute. The two surfaces share their
// COMPONENTS (DeskRow, SelectionToolbar, VirtualList) and their VERBS
// (ui/useListVerbs.ts) — which is where behaviour lives, and where a second
// copy would actually hurt — but almost nothing else. DeskRoute is a thousand
// lines of tabs, the line, drawers, per-area sections, the preview debounce and
// archive-all, none of which the tray has; the tray is groups by wait, which
// the desk has not. Folding them together would mean a `mode` test inside a
// dozen memos and a `flatten` that produced two unrelated item vocabularies —
// more branching than either surface has today, to avoid duplicating a shell.
//
// IT COSTS ONE ROUND TRIP. `loadDesk("rest", scope)` is the counts, the
// all-mail total and the tray, and on `rest` it issues no per-area query at all
// (core/sync.ts) — so opening the tray is exactly one request however many
// areas the user has. It is also what keeps the engine refreshing THIS screen
// when mail changes: `loadDesk` records the desk on screen, and the effect
// below re-asserts it for the same reason DeskRoute's does.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";

import { effectiveEmail, sectionIds } from "../../../core/store";
import { invertChanges, refileChanges } from "../../../core/refile";
import type { EffectiveEmail } from "../../../core/store";
import { groupByWait, sectionKey, syncedAgoSeconds, trayStats } from "../../../core/desk";
import type { WaitBucket } from "../../../core/desk";
import { KEYWORD_FLAGGED } from "../../../core/jmap/types";
import type { Area, Id } from "../../../core/jmap/types";
import { TRAY_PAGE } from "../../../core/sync";
import { withCompose } from "../../../core/routes";
import type { Route } from "../../../core/routes";
import { navigate, useListRoute } from "../../routing";
import { useApp, useMailState } from "../../state";
import { t } from "../../i18n";
import type { MessageKey } from "../../i18n";
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
import { filedText, flaggedText } from "../undoText";
import { draftRuleFrom } from "../ruleDraft";
import { DeskRow } from "../desk/DeskRow";
import type { RowAccount } from "../desk/DeskRow";
import { ShadowBanners } from "../desk/ShadowBanner";
import { SelectionToolbar } from "../desk/SelectionToolbar";
import type { RefileTarget } from "../desk/SelectionToolbar";
import { TrayHeader } from "../desk/TrayHeader";
import { VirtualList } from "../desk/VirtualList";
import type { GridKeys, RowAria } from "../desk/VirtualList";
import { rowNameFor } from "../desk/rowName";
import { cursorAfterRemoval, nextRowId, positionOf, rowIds, rowIndexOf } from "../desk/flatten";
import type { SwipeAction } from "../desk/swipe";
import { moreText } from "../desk/format";

/** How often the header sentence re-states how long ago the last sync was —
 * the same cadence the desk ticks at, and for the same reason: the sentence's
 * own granularity is ten seconds, so anything finer re-renders for a number
 * that cannot have changed. */
const TICK_MS = 10_000;

/** One drawn row of the tray page. EVERY KIND IS ONE ROW TALL — the same
 * contract the desk's items keep, because the same `virtualWindow` places them
 * at `index * rowHeight` and an item that rendered taller would draw the rows
 * after it on top of one another, silently. */
type TrayItem =
  | { kind: "heading"; bucket: WaitBucket; count: number }
  | { kind: "row"; id: Id }
  | { kind: "more"; remaining: number }
  | { kind: "error"; message: string }
  | { kind: "empty" };

const BUCKET_LABEL: Record<WaitBucket, MessageKey> = {
  today: "tray.group.today",
  thisWeek: "tray.group.thisWeek",
  overWeek: "tray.group.overWeek",
  overMonth: "tray.group.overMonth",
};

export function TrayRoute(): JSX.Element {
  const { actions, sync, pushToast, offerUndo } = useApp();
  const state = useMailState();
  const shell = useShell();
  const { byId: accountsById, accounts } = useAccounts();
  // The LIST route, not the current one: settings sits on top of the tray
  // rather than replacing it (core/routes.ts listRouteOf).
  const listRoute: Route = useListRoute();

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
  // The reading pane's area picker (#921): the tray's own re-file menu is
  // `refileOpen` above, and this is the one the pane's More menu opens.
  const [refilePickerOpen, setRefilePickerOpen] = useState(false);
  const [snoozeIds, setSnoozeIds] = useState<Id[] | null>(null);

  const [now, setNow] = useState<number>(() => Date.now());
  useEffect(() => {
    const handle = setInterval(() => setNow(Date.now()), TICK_MS);
    return () => clearInterval(handle);
  }, []);

  // -- what the store currently says ------------------------------------------

  const trayKey = sectionKey("tray", null, scope);
  const traySection = state.sections[trayKey];
  const trayIds = useMemo(
    () =>
      sectionIds(state, trayKey, {
        move: "inbox",
        // Unflagging is what "remove from the tray" IS, so the row leaves at the
        // keystroke rather than at the next poll.
        keyword: { atom: KEYWORD_FLAGGED, present: true },
        // The tray's query carries NO snooze condition, so neither does its
        // overlay filter — hiding rows the query returned would make the list
        // disagree with the total printed above it.
        snoozed: null,
      }),
    [state, trayKey],
  );
  const rows = useMemo(
    () => trayIds.map((id) => effectiveEmail(state, id)).filter((e): e is EffectiveEmail => e !== null),
    [trayIds, state],
  );
  const stats = useMemo(() => trayStats(rows, now), [rows, now]);
  const total = traySection?.total ?? 0;
  const loaded = traySection?.loadedAt != null;

  /** The groups, in order, empty ones omitted (core/desk.ts groupByWait) — then
   * flattened into the one array the window, the cursor and `j`/`k` all index
   * into. */
  const items: TrayItem[] = useMemo(() => {
    const out: TrayItem[] = [];
    if (traySection?.error) {
      // AN ERROR IS NOT AN EMPTY TRAY. "Nothing is waiting on you" is the best
      // news this screen can give and it must never be shown for a query that
      // failed.
      out.push({ kind: "error", message: traySection.error });
      return out;
    }
    if (rows.length === 0) {
      out.push({ kind: "empty" });
      return out;
    }
    for (const group of groupByWait(rows, now)) {
      out.push({ kind: "heading", bucket: group.bucket, count: group.emails.length });
      for (const email of group.emails) out.push({ kind: "row", id: email.id });
    }
    // A BOUND, REPORTED: the window is a slice of the tray, not the tray, so
    // the button says how much is missing rather than the list quietly ending.
    if (trayIds.length < total) {
      out.push({ kind: "more", remaining: total - trayIds.length });
    }
    return out;
  }, [rows, now, traySection?.error, trayIds.length, total]);

  const ids = useMemo(() => rowIds(items), [items]);

  /** Areas in the desk's own draw order, so the `1`–`9` re-file keys mean the
   * same area here as they do on the desk. */
  const areas: Area[] = useMemo(
    () =>
      Object.values(state.areas).sort((a, b) =>
        a.position !== b.position ? a.position - b.position : a.id.localeCompare(b.id),
      ),
    [state.areas],
  );

  /** The dot appears only when it could distinguish anything: several accounts,
   * and a tray showing all of them. */
  const showAccountDot = scope === null && accounts.length > 1;
  const scopeName = scope === null ? null : accountsById.get(scope)?.name ?? scope;

  /** The same one decision the desk shows (ui/useHeaderState.ts): two pages,
   * one sentence about the connection. */
  const header = useHeaderState(now);

  /** The same ingest-only facts the desk shows (ui/useShadowNotices.ts):
   * ADR-028 §5's banner must not read differently one click away. */
  const shadowNotices = useShadowNotices(scope);

  /** Below 760px this page is one column too: the header becomes the scope line
   * the desk uses, and the rows carry the two swipes. */
  const phone = useMediaQuery(PHONE_QUERY);
  const reducedMotion = useMediaQuery(REDUCED_MOTION_QUERY);

  // -- loading ----------------------------------------------------------------

  /**
   * THE TRAY ON SCREEN MUST BE THE DESK THE ENGINE BELIEVES IN — the same
   * re-assertion DeskRoute makes, deliberately without a dependency list.
   * Loading an ordinary view clears `SyncEngine.desk`, and the boot sequence
   * loads one; if that lands after this route's own load, the engine believes
   * no desk is on screen and an Email change refreshes nothing here. The check
   * cannot loop: `loadDesk` records the desk synchronously before it awaits, so
   * the next render finds a match and does nothing.
   *
   * `rest` rather than `review`: on that tab loadDesk fetches the counts and
   * the tray and NO per-area sections, which is exactly this page.
   */
  useEffect(() => {
    if (!state.accountId) return;
    const current = sync.currentDesk;
    if (current && current.tab === "rest" && current.scope === scope) return;
    void sync.loadDesk("rest", scope);
  });

  // A scope change forgets what was selected under the other one: those rows
  // are gone, and a selection the user cannot see is one they cannot undo.
  useEffect(() => {
    setSelected(new Set());
    setAnchor(null);
    setCursor(null);
    setRefileOpen(false);
  }, [scope]);

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
    // The tray lists snoozed mail — its query has no snooze condition — so `b`
    // means "put this away", the same as everywhere except the Snoozed view.
    unsnoozeInstead: false,
    // This page IS the tray, so the flag is the tray.
    flagWord: "tray",
  });

  /** A plain click on a row's message cell opens it and drops the ticked
   * selection — the desk's rule, for the same row (ui/routes/DeskRoute.tsx
   * `openFromRow` says why, and why the sender cell and the keyboard differ). */
  const openFromRow = useCallback(
    (id: Id) => {
      clearSelection();
      setAnchor(id);
      openMessage(id);
    },
    [clearSelection, openMessage],
  );

  /**
   * REMOVE FROM THE TRAY — the page's own verb, and deliberately NOT the shared
   * `doStar`.
   *
   * `doStar` toggles, taking its direction from the first target so a
   * multi-select acts as one gesture. Here the direction is never in question:
   * every row on this screen is flagged, that is what put it here, and a toggle
   * would re-flag anything that had somehow been unflagged already — turning
   * "take these out of my tray" into "put one back". So this writes `false`
   * outright, then advances the reading pane, because unlike a star anywhere
   * else the row genuinely leaves this list.
   */
  const removeFromTray = useCallback(
    async (id?: Id) => {
      const list = targets(id);
      if (list.length === 0) return;
      await actions.setFlagged(list, false);
      afterRemoval(list);
      // The page's own verb needs the page's own undo: `doStar` is not what ran,
      // so the offer registered inside it is not what is on offer here.
      offerUndo(flaggedText(list.length, false, "tray"), async () => {
        await actions.setFlagged(list, true);
        return true;
      });
    },
    [actions, targets, afterRemoval, offerUndo],
  );

  /**
   * "None of these" (#176), wired for one reason only: the reading pane renders
   * a suggestion panel for any message that still has open questions, and its
   * dismiss button must not be a control that does nothing. The tray's own
   * answer to "what is this?" is re-filing it (`1`–`9`), which is a different
   * gesture with a different write behind it.
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
      // NOT afterRemoval: answering a question does not take a message out of
      // the tray. It is still something you said you would come back to.
    },
    [actions, targets, pushToast],
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

  // -- re-filing --------------------------------------------------------------

  /** The areas mail can be re-filed into — the same rule the desk applies
   * (ui/routes/DeskRoute.tsx refileTargets): an area OWNS a label, so filing IS
   * applying that label, local-only or not (#704). Only an unloaded label
   * blocks a target. */
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
      // (#921); the tray's own menu and the 1–9 keys pass nothing and take the
      // selection, exactly as before.
      const list = ids ?? targets();
      if (list.length === 0 || !state.labels[area.labelId]) return;
      // The same SWAP as the desk's (core/refile.ts): the other areas' own
      // labels come off and the target's goes on, and the undo is the exact
      // per-message inverse — so it restores the prior label set.
      const changes = refileChanges(state, list, areaId);
      void actions.relabel(changes, filedText(list.length, area.name));
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
    [actions, state, targets, clearSelection, offerUndo],
  );

  /**
   * THE PHONE'S TWO SWIPES, in the tray's vocabulary (#375 increment 7).
   *
   * Right is archive, as everywhere. LEFT IS "TAKE IT OUT", not "toggle the
   * flag": every row on this page is flagged — that is what put it here — so
   * the page's own verb is what a left swipe means, exactly as `f` does
   * (removeFromTray's comment says why a toggle would be wrong here).
   *
   * Handed to a row only while nothing is selected, because the verbs resolve
   * their targets through `targets()` and that prefers the selection.
   */
  const onSwipe = useCallback(
    (id: Id, action: SwipeAction) => {
      if (action === "archive") void doArchive(id);
      else void removeFromTray(id);
    },
    [doArchive, removeFromTray],
  );

  const loadMore = useCallback(() => {
    void sync.loadDesk("rest", scope, { more: trayKey });
  }, [sync, scope, trayKey]);

  /** Ask again now rather than waiting out the poll interval. `refreshDesk`
   * too: the tray's rows are a desk section, not the poll's cursors. */
  const retry = useCallback(() => {
    void (async () => {
      await sync.poll();
      await sync.refreshDesk();
    })();
  }, [sync]);

  /**
   * Opens the account switcher — the health panel the stale sentence
   * summarises, and the scope chooser the phone's header line is, are the same
   * disclosure (ui/AccountSwitcher.tsx).
   */
  const openSwitcher = useCallback(() => {
    shell.railKeys.current?.openSwitcher();
  }, [shell]);

  // -- the cursor ------------------------------------------------------------

  /** Move the cursor `rows` rows — the one implementation, called by `j`/`k`, by
   * the grid's arrows and by PageUp/PageDown with a screenful (the desk's own
   * routes/DeskRoute.tsx says at length why this must not exist twice: with a
   * message open the reading pane FOLLOWS the cursor). */
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

  /** Home and End: a null cursor is "start from the near end" (ui/desk/flatten
   * .ts nextRowId), so the two edges are one call with two directions. */
  const jumpCursor = useCallback(
    (to: "first" | "last") => {
      const target = nextRowId(items, null, to === "first" ? 1 : -1);
      if (!target || target === cursor) return;
      setCursor(target);
      if (openId) openMessage(target, { replace: true });
    },
    [items, cursor, openId, openMessage],
  );

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

  // -- the keyboard, tray half (the chain lives in Shell) ---------------------

  const handleVerb = useCallback(
    (ev: KeyboardEvent): boolean => {
      switch (ev.key) {
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
        // `s` is the tray's own key here — on the desk it puts mail in; here it
        // takes mail out. It used to share this case with `f`, which is
        // Forward's key everywhere else (#582) and made Forward's own
        // `case "f":` below unreachable: the first matching case in a switch
        // wins, so every `f` in the tray ran this removal and the later
        // Forward case was dead code the compiler never flagged
        // (`noFallthroughCasesInSwitch` checks for a missing break, not a
        // repeated label). #624 §1 gave `f` back to Forward everywhere; `s`
        // alone is now this surface's toggle.
        case "s":
          ev.preventDefault();
          void removeFromTray();
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
          // `1`–`9` re-file to the Nth area in position order, as on the desk —
          // the same key for the same write, so the shortcut does not have to be
          // relearned per screen. It does NOT remove the row: mail can be in the
          // tray and in an area at once, and those are different statements.
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
      openMessage,
      replyTo,
      replyAllTo,
      forwardOn,
      toggleSelect,
      doArchive,
      doTrash,
      removeFromTray,
      doPin,
      doSnooze,
      doMute,
      doRead,
      refile,
    ],
  );

  // Published on EVERY render (no dependency list), so the shell's one listener
  // always calls the current closures without re-registering itself.
  const { listKeys } = shell;
  useEffect(() => {
    listKeys.current = {
      openPalette: () => setPaletteOpen(true),
      closeTopPopover: () => {
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
        return false;
      },
      clearSelection: () => {
        if (selected.size === 0) return false;
        clearSelection();
        return true;
      },
      blocksVerbs: () => paletteOpen || snoozeIds !== null,
      handleVerb,
    };
    return () => {
      listKeys.current = null;
    };
  });

  // -- rendering --------------------------------------------------------------

  const renderRow = (id: Id): JSX.Element | null => {
    const email = effectiveEmail(state, id);
    // The section listed this id and the store has no message for it. Said out
    // loud rather than left as a row-height hole in a virtualised list — the
    // desk's renderRow carries the full reasoning.
    if (!email) {
      // `role="gridcell"` because this REPLACES the row's three cells: a
      // `role="row"` with no cell in it is a row a reader cannot enter.
      return (
        <p role="gridcell" className="desk-row is-not-loaded">{t("row.notLoaded")}</p>
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
        {...(phone && selected.size === 0 ? { onSwipe } : {})}
        // The same keyword, this page's word for it: here a left swipe takes
        // the message OUT of the tray.
        swipeFlagLabel={t("tray.remove")}
        reducedMotion={reducedMotion}
      />
    );
  };

  const renderItem = (item: TrayItem): JSX.Element | null => {
    switch (item.kind) {
      case "heading":
        return (
          <h2 className="desk-heading">
            <span className="desk-heading-name">{t(BUCKET_LABEL[item.bucket])}</span>
            <span className="desk-heading-count">{item.count}</span>
          </h2>
        );
      case "row":
        return renderRow(item.id);
      case "more":
        return (
          <button type="button" className="button desk-more" onClick={loadMore}>
            {moreText(item.remaining, TRAY_PAGE, total)}
          </button>
        );
      case "error":
        // Not role="alert": the header sentence is this page's one live region,
        // and it already carries "cannot reach the server".
        return <p className="desk-section-error">{t("desk.section.error", { error: item.message })}</p>;
      case "empty":
        // Only once the tray has actually come back. Before that the header
        // says "loading…" and this would be the good news stated early.
        return loaded ? <p className="desk-nothing">{t("desk.tray.empty")}</p> : null;
    }
  };

  /**
   * WHAT THE ACCESSIBILITY TREE IS TOLD ABOUT ONE ITEM (#447) — the desk's rule,
   * with one difference stated rather than hidden: NO AREA NAME.
   *
   * This page's sections are how long something has waited, not which area claims
   * it, and a message's area is a body-only property (jmap/types.ts) that a tray
   * row has never been fetched with. So the clause is absent because the fact is,
   * rather than being guessed from a field that is `undefined` here.
   */
  const rowAriaOf = (item: TrayItem): RowAria => {
    switch (item.kind) {
      case "row": {
        const email = effectiveEmail(state, item.id);
        if (!email) return { cell: "own" };
        const thread = state.threads[email.threadId];
        const accountId = email.identityId
          ? state.identities[email.identityId]?.accountId
          : undefined;
        return {
          label: rowNameFor(email, {
            areaName: null,
            // The same condition the DOT is drawn under (`showAccountDot`), so the
            // word appears on exactly the rows that carry the colour.
            accountName:
              showAccountDot && accountId ? accountsById.get(accountId)?.name ?? null : null,
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

  const itemKey = (item: TrayItem, index: number): string => {
    switch (item.kind) {
      case "heading":
        return `h:${item.bucket}`;
      case "row":
        return `r:${item.id}`;
      case "more":
        return "more";
      case "error":
        return "error";
      case "empty":
        return `empty:${index}`;
    }
  };

  return (
    <>
      <main className="content" id="messages" tabIndex={-1}>
        {/* The same grid column the desk's list pane and the message list are,
            and the same `.list-pane` class — that is the selector the narrow
            breakpoints hide when a message is open. */}
        <section className="list-pane desk-pane" ref={paneRef} aria-label={t("tray.pane")}>
          <div className="desk-chrome" ref={chromeRef}>
            <TrayHeader
              scopeName={scopeName}
              loading={!loaded}
              count={trayIds.length}
              total={total}
              oldestWaitMs={stats.oldestWaitMs}
              syncedSeconds={syncedAgoSeconds(state.lastSyncAt, now)}
              state={header}
              now={now}
              onRetry={retry}
              onDetails={openSwitcher}
              onScope={openSwitcher}
              phone={phone}
            />
            <ShadowBanners notices={shadowNotices} />
            {selected.size > 0 ? (
              <SelectionToolbar
                count={selected.size}
                areas={refileTargets}
                refileOpen={refileOpen}
                trayAction="remove"
                onArchive={() => void doArchive()}
                onTray={() => void removeFromTray()}
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
            // No tabs on this page, so the grid carries its own name rather than
            // borrowing a tab's (ui/desk/VirtualList.tsx).
            gridId="tray-grid"
            label={t("tray.list.label")}
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
              position={positionOf(items, open.id)}
              backLabel={t("tray.title")}
              onClose={closeMessage}
              onReply={() => replyTo(open)}
              onReplyAll={() => replyAllTo(open)}
              onForward={() => forwardOn(open)}
              onArchive={() => void doArchive(open.id)}
              onNotJunk={() => void doNotJunk(open.id)}
              onTrash={() => void doTrash(open.id)}
              onTray={() => void removeFromTray(open.id)}
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

      {labelMenuOpen ? <LabelMenu ids={targets()} onClose={() => setLabelMenuOpen(false)} /> : null}
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
          // The tray LISTS snoozed mail (its query has no snooze condition), so
          // a snoozed row stays and the reading pane does not advance — the row
          // simply gains its "returns …" cue.
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
    </>
  );
}
