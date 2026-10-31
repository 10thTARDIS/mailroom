// SPDX-License-Identifier: AGPL-3.0-only

// THE LIST ROUTE (#373): the message list, the reading pane, and everything
// that is about the mail rather than about the app — selection, the keyboard
// cursor, drag state, and the transient popovers that are not places you can
// link to (the label menu, the snooze menu, the command palette).
//
// This was the bottom two-thirds of App.tsx and is transplanted, not rewritten:
// the behaviour of every verb here is the behaviour it had. Two things did
// change, and both are the point of the issue:
//
//   * WHICH MESSAGE IS OPEN IS NOW THE URL (`?m=<id>`), not local state. So a
//     refresh lands on the same message, a link to one can be pasted, and the
//     back button closes the pane. Walking the list with j/k while the pane is
//     open REPLACES that entry rather than pushing: moving the cursor is not a
//     navigation, and fifty history entries for one pass down the inbox would
//     make the back button useless.
//   * WHERE THE LIST WAS is remembered per route key, so coming back from a
//     message — or from settings — puts the cursor and the scroll position back
//     where they were rather than at the top.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";

import { followSelection, latestRevision, repairCursor } from "../../../core/draftLineage";
import { effectiveEmail, visibleIds } from "../../../core/store";
import type { EffectiveEmail, ListOrder } from "../../../core/store";
import { layoutQueue } from "../../../core/needsCategory";
import type { QueueGroup } from "../../../core/needsCategory";
import type { QueueGroupOutcome } from "../NeedsCategory";
import type { Id } from "../../../core/jmap/types";
import { planFor } from "../../../core/views";
import { routeKey, withCompose } from "../../../core/routes";
import { loadPosition, navigate, savePosition, useListRoute } from "../../routing";
import type { ListPosition } from "../../routing";
import { useApp, useMailState } from "../../state";
import { t } from "../../i18n";
import { MessageList } from "../MessageList";
import { TabStrip } from "../TabStrip";
import { ThreadPane } from "../ThreadPane";
import { CommandPalette } from "../CommandPalette";
import { LabelMenu } from "../LabelMenu";
import { RefilePicker } from "../RefilePicker";
import { importantTarget } from "../importantVerb";
import { moveToArea } from "../moveToArea";
import type { RefileTarget } from "../desk/SelectionToolbar";
import { areasInDeskOrder } from "../../../core/refile";
import { SnoozeMenu } from "../SnoozeMenu";
import { PaneSplitter } from "../PaneSplitter";
import { useShell } from "../shellContext";
import { useListVerbs } from "../useListVerbs";
import { useOpenDwell } from "../useOpenDwell";
import { draftRuleFrom } from "../ruleDraft";
import { SearchSort } from "../SearchSort";

/** How many a "select all matching" takes at once, and how many each "select the
 * next" takes: allMatchingIds' own cap. */
const BULK_BATCH = 1000;

export function ListRoute(): JSX.Element {
  const { actions, sync, store, view, setView, pushToast, offerUndo } = useApp();
  const state = useMailState();
  const shell = useShell();
  // The LIST route, not the current one: settings sits on top of the list
  // rather than replacing it, so while a settings sheet is open the message
  // beneath it must stay open and every list navigation must land back on the
  // list it came from (core/routes.ts listRouteOf).
  const listRoute = useListRoute();

  const plan = useMemo(
    () => planFor(view, Object.values(state.mailboxes), state.labels),
    [view, state.mailboxes, state.labels],
  );
  const inPriority = view.kind === "priority";
  const inNeedsCategory = view.kind === "needsCategory";
  // Best match (#724) is the server's order and nothing else: the client holds
  // no score to re-sort by.
  const byRelevance = view.kind === "search" && view.sort === "relevance";
  const order: ListOrder = inPriority
    ? "priority"
    : inNeedsCategory
      ? "confidence"
      : byRelevance
        ? "server"
        : "arrival";
  const queried = useMemo(
    () => visibleIds(state, plan.role, plan.snoozed, order),
    [state, plan.role, plan.snoozed, order],
  );
  // THE QUEUE'S GROUPING DECIDES THE ORDER, and the shell has to be the one
  // holding it (#183). `ids` is what `j`/`k`, shift-range and select-all walk;
  // when MessageList reordered rows for display only, the cursor moved somewhere
  // other than where the eye was. One list, derived once, used by both.
  const queueLayout = useMemo(() => {
    if (!inNeedsCategory) return null;
    const rows = queried
      .map((id) => effectiveEmail(state, id))
      .filter((e): e is EffectiveEmail => !!e);
    return layoutQueue(rows);
  }, [inNeedsCategory, queried, state]);
  const ids = queueLayout?.ids ?? queried;

  const [selected, setSelected] = useState<Set<Id>>(new Set());
  const [anchor, setAnchor] = useState<Id | null>(null);
  const [cursor, setCursor] = useState<Id | null>(null);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [labelMenuOpen, setLabelMenuOpen] = useState(false);
  // The reading pane's area picker (#921): the list's own "Move to area" acts
  // on the selection, this opens on the message being read.
  const [refilePickerOpen, setRefilePickerOpen] = useState(false);
  // The needs-a-category batch in flight (#176): the group whose messages the
  // label menu is about to act on, and what each answered group turned out to
  // do. Held as state rather than derived because the group is chosen BEFORE the
  // label is, and the answer has to find its way back to the header the user
  // clicked — which by then may not be the first group on screen any more.
  const [queueBatch, setQueueBatch] = useState<QueueGroup | null>(null);
  // Newest first: the thing the user just did is the thing they are looking for.
  const [groupOutcomes, setGroupOutcomes] = useState<QueueGroupOutcome[]>([]);
  // The messages the snooze menu is acting on, captured when it opens (null =
  // closed). Held rather than re-derived on render so the menu cannot change
  // targets underneath itself if the selection moves while it is open.
  const [snoozeIds, setSnoozeIds] = useState<Id[] | null>(null);
  const [allMatching, setAllMatching] = useState<Id[] | null>(null);
  // How many the "select all matching" query said matched in all: more than
  // `allMatching.length` when the cap truncated it (#708 says so when moving).
  const [allMatchingTotal, setAllMatchingTotal] = useState(0);
  // The bulk run a TRUNCATED "select all matching" started (#769): the ids a verb
  // has already been applied to, and how many matched when it began. Kept apart
  // from `allMatching` because most verbs clear the selection (and with it
  // `allMatching`) the moment they finish, which is exactly when the offer to
  // carry on has to appear. Cleared when the list changes or the user selects by
  // hand: either way it is no longer a run over one query.
  const [bulkRun, setBulkRun] = useState<{ done: Set<Id>; total: number } | null>(null);
  // Labels already offered a suggestion this session. Asking twice about the
  // same label is nagging: the user has seen the offer and moved on, and a
  // prompt that reappears every time they file another message is one they learn
  // to dismiss without reading.
  const suggestedFor = useRef<Set<Id>>(new Set());

  /** The open message is the URL's, not a piece of state (#373). */
  const openId: Id | null = listRoute.m ?? null;

  // -- where this list was ---------------------------------------------------

  /** The list's identity WITHOUT the open message or the composer, so opening
   * either does not read as arriving at a different list — and so settings,
   * which sits on top of the list, keeps the list it sits on. */
  const listKey = routeKey(listRoute);
  const paneRef = useRef<HTMLElement | null>(null);
  const cursorRef = useRef<Id | null>(cursor);
  cursorRef.current = cursor;
  const recalled = useRef<ListPosition | null>(null);

  // Changing list resets everything that was about the old one. Declared BEFORE
  // the recall below so the recalled cursor is applied after this has cleared
  // the old one, not before.
  useEffect(() => {
    setSelected(new Set());
    setAnchor(null);
    setAllMatching(null);
    setBulkRun(null);
    setCursor(null);
    // The batch-triage results belong to the queue that was on screen (#176).
    // Keeping them would show "24 labelled Information" above a group in some
    // other view that happens to share a key.
    setQueueBatch(null);
    setGroupOutcomes([]);
  }, [listKey]);

  useEffect(() => {
    recalled.current = loadPosition(listKey);
    return () => {
      // On the way out — a different list, or the whole app closing. The cursor
      // is read from a ref because this cleanup runs BEFORE the reset above has
      // cleared it, which is exactly the value worth saving.
      savePosition(listKey, {
        cursor: cursorRef.current,
        scrollTop: paneRef.current?.scrollTop ?? 0,
      });
    };
  }, [listKey]);

  // Keep the keyboard cursor pointing at something that still exists — and, on
  // the first list that arrives after a return, put it back where it was.
  useEffect(() => {
    const want = recalled.current;
    if (want) {
      // Nothing to point at yet: the query has not come back. Hold the recalled
      // position rather than spending it on an empty list.
      if (ids.length === 0) return;
      recalled.current = null;
      if (paneRef.current) paneRef.current.scrollTop = want.scrollTop;
      // The remembered cursor may name a draft revision an autosave has since
      // replaced; the same succession applies to it as to the live cursor below.
      const recalledCursor = want.cursor ? latestRevision(state.draftSuccessors, want.cursor) : null;
      if (recalledCursor && ids.includes(recalledCursor)) {
        setCursor(recalledCursor);
        return;
      }
    }
    // A draft replaced by its own autosave is followed rather than abandoned
    // (#610 M2): the rule this replaced sent the cursor to the row BELOW the
    // draft, and the next keyboard archive or trash went with it.
    const next = repairCursor(ids, cursor, state.draftSuccessors);
    if (next !== cursor) setCursor(next);
  }, [ids, cursor, state.draftSuccessors]);

  // The checked set and the shift-click anchor hold ids too, and are carried
  // across an autosave for the same reason (core/draftLineage.ts).
  useEffect(() => {
    setSelected((prev) => {
      const next = followSelection(prev, state.draftSuccessors);
      return next === prev ? prev : new Set(next);
    });
    setAnchor((prev) => (prev ? latestRevision(state.draftSuccessors, prev) : prev));
  }, [state.draftSuccessors]);

  // Never sit on a tab that is switched off (#228). The server REFUSES an
  // `inTab` query naming a disabled tab — answering with an empty list would
  // claim the tab holds nothing when its mail actually folded into Primary — so
  // this view has no valid query behind it. It happens two ways: switching the
  // tab off in settings while looking at it, and opening a pasted `#/tab/forums`
  // for a tab this account has off.
  //
  // Guarded on the map being POPULATED, not just on the lookup: it is empty
  // until the reference data lands, and treating "not loaded yet" as "disabled"
  // would bounce every deep link to a tab on startup.
  useEffect(() => {
    if (view.kind !== "tab") return;
    if (Object.keys(state.categoryTabs).length === 0) return;
    if (state.categoryTabs[view.tab]?.enabled) return;
    setView({ kind: "role", role: "inbox" });
  }, [view, state.categoryTabs, setView]);

  const open = state.emails[openId ?? ""] ? effectiveEmail(state, openId as Id) : null;

  // -- selection -------------------------------------------------------------

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
      setAllMatching(null);
      setBulkRun(null);
    },
    [anchor, ids],
  );

  const selectAllOnPage = useCallback(() => {
    setSelected((prev) => (prev.size === ids.length ? new Set() : new Set(ids)));
    setAllMatching(null);
    setBulkRun(null);
  }, [ids]);

  /** "Select all N matching this search" (spec §12). Asks the server for every
   * id the current view's query matches — not just the loaded page. */
  const selectAllMatching = useCallback(async () => {
    let found: { ids: Id[]; total: number };
    try {
      found = await sync.allMatchingIds(view);
    } catch {
      // Never a partial selection presented as "all matching".
      pushToast({ kind: "error", message: "Could not select every matching conversation." });
      return;
    }
    const { ids: all, total } = found;
    setSelected(new Set(all));
    setAllMatching(all);
    setAllMatchingTotal(total);
    setBulkRun(null);
    if (all.length < total) {
      pushToast({
        kind: "info",
        message: `Selected the first ${all.length} of ${total} conversations.`,
      });
    }
  }, [sync, view, pushToast]);

  /** Note that a verb was just applied to these ids, when they are the
   * selection a TRUNCATED "select all matching" made (#769). This is the one
   * hook for the continuation: the verbs reach their ids through `targets`, so
   * recording there covers every verb (archive, trash, read, star, snooze, …)
   * without each knowing about bulk runs. It deliberately does nothing for a
   * hand-made selection or an untruncated one. */
  const recordProcessed = useCallback(
    (list: Id[]) => {
      // The LAST batch of a run is not itself truncated, but it still finishes the
      // run, so a run already in progress records it too.
      if (allMatching === null || (allMatching.length >= allMatchingTotal && bulkRun === null)) return;
      // Only the whole all-matching selection counts; one row acted on by the
      // cursor while it is selected is not "a batch done".
      if (list.length !== allMatching.length) return;
      setBulkRun((prev) => {
        const done = new Set(prev?.done ?? []);
        for (const id of list) done.add(id);
        return { done, total: prev?.total ?? allMatchingTotal };
      });
    },
    [allMatching, allMatchingTotal, bulkRun],
  );

  /** `targets` as the verbs see it: the same ids, noted as processed. Event
   * handlers only — never called while rendering. */
  const verbTargets = useCallback(
    (id?: Id | null): Id[] => {
      const list = targets(id);
      recordProcessed(list);
      return list;
    },
    [targets, recordProcessed],
  );

  /** "Done N of M — select the next K": carry a truncated bulk run on (#769).
   *
   * The next batch is found by EXCLUSION, not by position. A verb either took the
   * mail out of this view (trash, archive under `in:inbox`) or left it there
   * (mark read), and position 0 is right for the first and wrong for the second;
   * asking for processed + cap from the start and dropping what was already done
   * is right for both. */
  const selectNextBatch = useCallback(async () => {
    if (!bulkRun) return;
    if (!(await actions.settle())) {
      pushToast({ kind: "error", message: t("senderBulk.notSettled") });
      return;
    }
    let found: { ids: Id[]; total: number };
    try {
      found = await sync.allMatchingIds(view, bulkRun.done.size + BULK_BATCH, 0);
    } catch {
      pushToast({ kind: "error", message: "Could not select every matching conversation." });
      return;
    }
    const fresh = found.ids.filter((id) => !bulkRun.done.has(id)).slice(0, BULK_BATCH);
    if (fresh.length === 0) {
      pushToast({ kind: "info", message: t("list.nextBatchNone") });
      setBulkRun(null);
      return;
    }
    // What is left: the total counts the done ones too when the verb left them in
    // view, and not when it took them out; subtract the ones the answer repeats.
    const repeated = found.ids.filter((id) => bulkRun.done.has(id)).length;
    setSelected(new Set(fresh));
    setAllMatching(fresh);
    setAllMatchingTotal(Math.max(found.total - repeated, fresh.length));
  }, [bulkRun, actions, sync, view, pushToast]);

  const clearSelection = useCallback(() => {
    setSelected(new Set());
    setAllMatching(null);
  }, []);

  /** The Cancel button: the user is done with this selection, and with the run. */
  const cancelSelection = useCallback(() => {
    clearSelection();
    setBulkRun(null);
  }, [clearSelection]);

  /** The areas "Move to area" offers: the desk's own order and names, and the
   * same single block (an unloaded label has nothing to apply). */
  const areaTargets: RefileTarget[] = useMemo(
    () =>
      areasInDeskOrder(state.areas).map((area) => ({
        id: area.id,
        name: area.name,
        blocked: state.labels[area.labelId] ? null : t("desk.refile.labelMissing", { area: area.name }),
      })),
    [state.areas, state.labels],
  );

  /** Re-file the whole selection through the desk's own path
   * (ui/moveToArea.ts -> core/refile.ts), one Undo. An "all matching"
   * selection the cap truncated carries its real total, so the toast cannot
   * read as complete. */
  const moveSelectionToArea = useCallback(
    (areaId: Id) => {
      const list = verbTargets();
      if (list.length === 0) return;
      const total =
        allMatching !== null && allMatching.length === list.length
          ? Math.max(allMatchingTotal, list.length)
          : list.length;
      void moveToArea({ store, sync, actions, offerUndo, pushToast }, list, areaId, total).then((r) => {
        if (r) clearSelection();
      });
    },
    [verbTargets, allMatching, allMatchingTotal, store, sync, actions, offerUndo, pushToast, clearSelection],
  );

  // -- opening and the verbs --------------------------------------------------
  //
  // The verbs themselves live in ui/useListVerbs.ts (#375): they are the same
  // callbacks this route always had, moved so the desk can have the SAME ones
  // rather than a second implementation that drifts. Everything below is what
  // is specific to this route — the queue's answers, and the effects that are
  // about this list rather than about the mail.

  const {
    openMessage,
    closeMessage,
    afterRemoval,
    doArchive,
    doTrash,
    doUntrash,
    doNotJunk,
    doStar,
    doPin,
    doSnooze,
    doImportant,
    doMute,
    doRead,
    doToggleRead,
    replyTo,
    replyAllTo,
    forwardOn,
  } = useListVerbs({
    ids,
    openId,
    route: listRoute,
    targets: verbTargets,
    setCursor,
    clearSelection,
    // Opening the snooze menu is not snoozing: undo the note `verbTargets` just
    // made by putting back the run as it stood before this event (the render's
    // value), so a cancelled menu neither counts a batch done nor forgets one a
    // previous verb really did. onSnoozed below records it when it happens.
    openSnoozeMenu: (list) => {
      setBulkRun(bulkRun);
      setSnoozeIds(list);
    },
    unsnoozeInstead: view.kind === "snoozed",
    // In the message list the flag is THE STAR — the word this surface's own
    // controls use for it, and the one the Starred view is named after.
    flagWord: "star",
  });

  /**
   * OPENING A ROW IN DRAFTS RESUMES IT IN COMPOSE (#448).
   *
   * A draft is not something to read — it is something you were writing — so its
   * row goes to `?compose=draft:<id>` rather than to the reading pane. Wrapped
   * around `openMessage` rather than replacing it, because `j`/`k` still walk the
   * list through the ordinary path: they only open anything when a message is
   * ALREADY open beside the list, which in Drafts never happens (nothing here ever
   * writes `?m=`).
   */
  const inDrafts = view.kind === "role" && view.role === "drafts";
  const openRow = useCallback(
    (id: Id, opts: { replace?: boolean } = {}) => {
      if (!inDrafts) {
        openMessage(id, opts);
        return;
      }
      setCursor(id);
      navigate(withCompose(listRoute, { draft: id }), opts);
    },
    [inDrafts, openMessage, listRoute],
  );

  /**
   * Opening a message marks it read — instantly, locally (spec §12/§4a) — and
   * fetches its thread.
   *
   * Driven by the ROUTE rather than by the click, so a message opened by a
   * pasted link, a reload or the back button is treated exactly like one opened
   * from the list. Guarded by a ref rather than by the dependency list because
   * the message may not be in the store yet on a deep link: the effect re-runs
   * as the store fills and acts on the first render where the message exists.
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

  // -- actions ---------------------------------------------------------------

  /**
   * Take back a batch answer (#182).
   *
   * The card is marked undone FIRST, which also withdraws its own Undo button —
   * the write is not instant, and a second click would ask the server to remove
   * a label that is already going. If the removal does not reach the server the
   * mark is rolled back, so the card returns to offering an undo rather than
   * claiming one that did not happen.
   *
   * Declared ABOVE suggestRule, which closes over it: a dependency array is
   * evaluated during render, so a `const` referenced there must already exist.
   */
  const undoBatch = useCallback(
    (key: string, batchIds: Id[], atom: string) => {
      setGroupOutcomes((prev) =>
        prev.map((o) => (o.key === key ? { ...o, undone: true, onUndo: undefined } : o)),
      );
      void (async () => {
        const ok = await actions.undoBatchLabel(batchIds, atom);
        if (ok) return;
        setGroupOutcomes((prev) =>
          prev.map((o) =>
            o.key === key
              ? { ...o, undone: false, onUndo: () => undoBatch(key, batchIds, atom) }
              : o,
          ),
        );
        pushToast({ kind: "error", message: t("queue.group.undoFailed") });
      })();
    },
    [actions, pushToast],
  );

  /**
   * After a label is applied, ask whether it now implies a rule (#168).
   *
   * SPECULATIVE AND SILENT ON FAILURE. The user asked to label a message; they
   * did not ask for a suggestion. So too few examples, no usable candidate, or a
   * failed call all produce NOTHING — no toast, no error. The only visible
   * outcome is a real proposal, which is the only outcome worth an interruption.
   *
   * The toast offers "Review rule" rather than applying anything: a candidate is
   * a proposal, and the numbers behind it belong in front of the user before it
   * starts labelling their mail.
   */
  const suggestRule = useCallback(
    (labelId: Id) => {
      // A BATCH ANSWER REPORTS IN PLACE, not as a toast (#176). The group header
      // the user just acted on is still on screen and is the natural home for
      // "24 labelled Information, and a rule for alias = bank@ would catch
      // 26 more" — a toast would put the blast radius somewhere transient, and
      // this is the number the whole epic promises to show before anything
      // automatic starts.
      //
      // It also bypasses the once-per-label nag guard below, on purpose: the
      // guard exists because nobody asked for a suggestion after labelling one
      // message. Here they asked, explicitly, by labelling a group — and the
      // next group's answer implies a DIFFERENT rule, so suppressing it would
      // hide the useful half of the workflow.
      const batch = queueBatch;
      if (batch) {
        setQueueBatch(null);
        setLabelMenuOpen(false);
        clearSelection();
        const label = state.labels[labelId];
        // Answering the same group twice REPLACES its card rather than stacking
        // a second one: the group is gone either way, and two cards about it
        // would be two claims about what happened to the same messages.
        const outcome: QueueGroupOutcome = {
          key: batch.key,
          count: batch.ids.length,
          labelName: label?.name ?? "",
          rule: null,
          verdict: "pending",
          // UNDO closes over the ids and the atom this batch actually used
          // (#182), not over anything re-derived later: the group is gone from
          // the queue by the time the card is on screen, and a label can be
          // renamed, so re-deriving either would be undoing something else.
          //
          // A local-only label has no atom and cannot be un-applied through the
          // keyword path, so no undo is offered rather than one that does
          // nothing — the same reason LabelMenu shows those disabled.
          ...(label?.keyword
            ? { onUndo: () => undoBatch(batch.key, batch.ids, label.keyword as string) }
            : {}),
        };
        setGroupOutcomes((prev) => [outcome, ...prev.filter((o) => o.key !== batch.key)]);
        void (async () => {
          // WAIT FOR THE LABELS TO LAND FIRST. Rule/induce learns from the
          // PERSISTED hand-applied labels, so asking it before the queue has
          // drained means asking about a batch the server has not seen: it finds
          // too few examples and returns no candidates, which would be rendered
          // as "these have too little in common" — a false diagnosis of the one
          // number this whole flow exists to produce.
          const settled = await actions.settle();
          const res = settled ? await actions.induceRule(labelId) : null;
          const best = res?.candidates[0];
          setGroupOutcomes((prev) =>
            prev.map((o) => {
              if (o.key !== batch.key) return o;
              // Four distinguishable outcomes, because they call for four
              // different sentences and the old code collapsed all of them into
              // "no rule fits these yet".
              const verdict: QueueGroupOutcome["verdict"] = !settled
                ? "unsettled"
                : !res
                  ? "unavailable"
                  : best
                    ? "rule"
                    : res.examples < res.required
                      ? "tooFewExamples"
                      : "noRule";
              return {
                ...o,
                verdict,
                // `complete` travels with the numbers: covers/collateral are
                // measured over the window the server scanned, and calling a
                // windowed count "nothing else you have" would be a claim the
                // server explicitly declined to make.
                rule: best
                  ? {
                      covers: best.covers,
                      collateral: best.collateral,
                      complete: res?.complete ?? false,
                    }
                  : null,
                needed: res ? res.required - res.examples : 0,
                onReview:
                  best && label
                    ? () =>
                        shell.openRuleDraft({
                          name: t("rules.suggest.defaultName", { label: label.name }),
                          priority: 100,
                          conditions: best.conditions,
                          actions: [{ type: "addLabel", label: labelId }],
                          enabled: true,
                        })
                    : undefined,
              };
            }),
          );
        })();
        return;
      }
      if (suggestedFor.current.has(labelId)) return;
      suggestedFor.current.add(labelId);
      void (async () => {
        const res = await actions.induceRule(labelId);
        const best = res?.candidates[0];
        if (!res || !best) return;
        const label = state.labels[labelId];
        if (!label) return;
        pushToast({
          kind: "info",
          message:
            t("rules.suggest.body", { count: res.examples, label: label.name }) +
            " " +
            (best.collateral > 0
              ? t("rules.suggest.effect", { collateral: best.collateral })
              : t("rules.suggest.effectNone")),
          action: {
            label: t("rules.suggest.review"),
            run: () =>
              shell.openRuleDraft({
                name: t("rules.suggest.defaultName", { label: label.name }),
                priority: 100,
                conditions: best.conditions,
                actions: [{ type: "addLabel", label: labelId }],
                enabled: true,
              }),
          },
        });
      })();
    },
    [actions, pushToast, state.labels, queueBatch, clearSelection, undoBatch, shell],
  );

  /**
   * "None of these" (#176) — close a message's open questions without filing it.
   *
   * The row leaves the queue at once (the overlay's `answered` flag), and the
   * pane advances the same way archiving does, because from the queue's point of
   * view the message is dealt with either way.
   */
  const doDismiss = useCallback(
    (id?: Id) => {
      const list = targets(id);
      if (list.length === 0) return;
      void (async () => {
        const changed = await actions.dismissCategory(list);
        if (changed.length === 0) return;
        // UNDO, because a dismissal is otherwise the one permanent, silent verb
        // on this surface: archive, trash, snooze and labels are all reversible,
        // and this removes a message from the only list that would ever ask
        // about it again. Offered over what actually CHANGED, not over what was
        // asked about, so "Undo" cannot restore something it did not remove.
        pushToast({
          kind: "info",
          message: t("queue.dismissed", { count: changed.length }),
          action: {
            label: t("action.undo"),
            run: () => void actions.undoDismissCategory(changed),
          },
        });
      })();
      afterRemoval(list);
    },
    [actions, targets, afterRemoval, pushToast],
  );

  /** Answer for a whole alias/domain group at once — the supervision multiplier
   * this surface exists for. It selects the group and opens the ordinary label
   * menu, so `l` then `1` labels twenty-four messages with the same two
   * keystrokes that label one. */
  const labelGroup = useCallback((group: QueueGroup) => {
    setQueueBatch(group);
    setSelected(new Set(group.ids));
    setAnchor(group.ids[0] ?? null);
    setLabelMenuOpen(true);
  }, []);

  const dismissGroup = useCallback(
    (group: QueueGroup) => {
      void (async () => {
        const changed = await actions.dismissCategory(group.ids);
        if (changed.length === 0) return;
        pushToast({
          kind: "info",
          message: t("queue.dismissed", { count: changed.length }),
          action: {
            label: t("action.undo"),
            run: () => void actions.undoDismissCategory(changed),
          },
        });
      })();
    },
    [actions, pushToast],
  );

  // -- the keyboard, list half (the chain lives in Shell) --------------------

  const handleVerb = useCallback(
    (ev: KeyboardEvent): boolean => {
      switch (ev.key) {
        case "j": {
          ev.preventDefault();
          const i = cursor ? ids.indexOf(cursor) : -1;
          const next = ids[Math.min(i + 1, ids.length - 1)];
          if (next) {
            setCursor(next);
            if (openId) openMessage(next, { replace: true });
          }
          return true;
        }
        case "k": {
          ev.preventDefault();
          const i = cursor ? ids.indexOf(cursor) : 0;
          const prev = ids[Math.max(i - 1, 0)];
          if (prev) {
            setCursor(prev);
            if (openId) openMessage(prev, { replace: true });
          }
          return true;
        }
        case "Enter":
        case "o":
          if (cursor) {
            ev.preventDefault();
            openRow(cursor);
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
        case "!":
          // Shift+1. The importance correction, next to the other one-key verbs.
          ev.preventDefault();
          void doImportant();
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
        // Reply all (#646), beside Reply. `a` rather than shift-r (Gmail's own
        // choice) because this app's shift-key verbs are corrections (`!` for
        // importance), not alternates — and `a` is free: `g a` (Archive) is a
        // two-key sequence Shell consumes entirely before this switch ever sees
        // the second keystroke (ui/goto.ts).
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
        case "-":
          // "This does not need a category" (#176), next to `l`: the queue's two
          // answers are "it is this" and "it is nothing", and only one of them
          // had a key. A punctuation key rather than a letter, so it does not
          // compete with the navigation verbs and reads as subtraction.
          //
          // ONLY IN THE QUEUE. Everywhere else it would be a global keystroke
          // that permanently removes whatever is selected from the only surface
          // that would ever ask about it again — and unlike every neighbouring
          // verb, one the user never went looking for.
          if (!inNeedsCategory) return false;
          ev.preventDefault();
          doDismiss();
          return true;
        case "I":
          ev.preventDefault();
          void doRead(true);
          return true;
        case "U":
          ev.preventDefault();
          void doRead(false);
          return true;
        default:
          return false;
      }
    },
    [
      cursor,
      ids,
      openId,
      open,
      state,
      inNeedsCategory,
      openMessage,
      openRow,
      replyTo,
      replyAllTo,
      forwardOn,
      toggleSelect,
      doArchive,
      doTrash,
      doStar,
      doPin,
      doSnooze,
      doImportant,
      doMute,
      doDismiss,
      doRead,
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
        if (labelMenuOpen) {
          setLabelMenuOpen(false);
          // Escape closes the menu HERE rather than through LabelMenu's onClose,
          // so the batch has to be abandoned here too (#176). Without this, a
          // group whose label menu was escaped stays armed, and the next label
          // the user applies — to any message, from any view — is reported as
          // having filed that group.
          setQueueBatch(null);
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

  const inTrash = plan.role === "trash";
  const inJunk = plan.role === "junk";

  // The selection bar's importance label (#921), computed from the PLAIN
  // targets rather than the hook's `allImportant`: the hook's targets here is
  // `verbTargets`, which notes the selection as processed for the bulk run
  // (#769) and is "event handlers only — never called while rendering". A
  // label is read on every render; one that recorded the batch would
  // setBulkRun on every render of a truncated all-matching selection.
  const importantAll = !importantTarget(
    targets().map((id) => effectiveEmail(state, id)?.isImportant ?? false),
  );

  return (
    <>
      <main className="content" id="messages" tabIndex={-1}>
        <MessageList
          paneRef={paneRef}
          header={
            <>
              <TabStrip
                tabs={state.categoryTabs}
                view={view}
                setView={setView}
                label={t("tabs.strip")}
                tabLabel={(tab) => t(`tabs.${tab}` as Parameters<typeof t>[0])}
              />
              {view.kind === "search" ? <SearchSort view={view} setView={setView} /> : null}
            </>
          }
          ids={ids}
          selected={selected}
          cursor={cursor}
          openId={openId}
          inTrash={inTrash}
          inJunk={inJunk}
          allMatchingCount={allMatching?.length ?? 0}
          nextBatch={
            // Shown once a verb has run on the run's current batch, whether the verb
            // cleared the selection (archive) or left it standing (mark read) — and
            // not while the batch it just selected is still untouched.
            bulkRun && (allMatching === null || bulkRun.done.has(allMatching[0] as Id))
              ? { done: bulkRun.done.size, total: bulkRun.total, next: Math.min(BULK_BATCH, bulkRun.total - bulkRun.done.size) }
              : null
          }
          onSelectNextBatch={() => void selectNextBatch()}
          onToggleSelect={toggleSelect}
          onDragIds={shell.setDragIds}
          onSelectAllOnPage={selectAllOnPage}
          onSelectAllMatching={() => void selectAllMatching()}
          onClearSelection={cancelSelection}
          onOpen={openRow}
          onPin={(id) => void doPin(id)}
          onSnooze={(id) => doSnooze(id)}
          inSnoozed={view.kind === "snoozed"}
          onImportant={(id) => void doImportant(id)}
          importantAll={importantAll}
          showPriorityBuckets={inPriority}
          inNeedsCategory={inNeedsCategory}
          queueLayout={queueLayout}
          groupOutcomes={groupOutcomes}
          onLabelGroup={labelGroup}
          onDismissGroup={dismissGroup}
          onDismiss={(id) => doDismiss(id)}
          onArchive={(id) => void doArchive(id)}
          onTrash={(id) => void doTrash(id)}
          onUntrash={(id) => void doUntrash(id)}
          onNotJunk={(id) => void doNotJunk(id)}
          onStar={(id) => void doStar(id)}
          onToggleRead={(id) => void doToggleRead(id)}
          onMarkRead={() => void doRead(true)}
          onMarkUnread={() => void doRead(false)}
          onLabel={() => setLabelMenuOpen(true)}
          areaTargets={areaTargets}
          onMoveToArea={moveSelectionToArea}
          onLoadMore={() => void sync.loadView(view, { more: true })}
        />

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
              // to file into — `areaTargets` is the very list the list's own
              // "Move to area" offers, so the two cannot disagree about the
              // areas.
              onRefile={areaTargets.length > 0 ? () => setRefilePickerOpen(true) : undefined}
              onDismiss={() => doDismiss(open.id)}
            />
          </>
        ) : null}
      </main>

      {/* NOT ROUTES, deliberately: a menu that acts on the current selection has
          no meaning without it, so a URL that reopened one would be a URL that
          lies about what it is about to do. */}
      {labelMenuOpen ? (
        <LabelMenu
          ids={targets()}
          // A batch answer is add-only: "these are Shopping". Without this, a
          // group whose messages already carry the chosen atom would read as
          // "on" and the same click would strip the label off all of them.
          forceAdd={queueBatch !== null}
          onClose={() => {
            setLabelMenuOpen(false);
            // Closing without answering abandons the batch. Leaving it armed
            // would attach the NEXT label the user applies — to anything, from
            // anywhere — to a group they walked away from.
            setQueueBatch(null);
          }}
          onSuggestRule={suggestRule}
          // Recorded when a label is actually applied, not when the menu closes:
          // a menu dismissed untouched has done nothing to the batch.
          onApplied={() => recordProcessed(targets())}
        />
      ) : null}
      {refilePickerOpen && open ? (
        // THE OPEN MESSAGE ONLY (#921): the picker moves the message being
        // read through the list's own single path (ui/moveToArea.ts), one Undo,
        // and the pane stays on it — no advance, no selection touched.
        <RefilePicker
          areas={areaTargets}
          onPick={(areaId) =>
            void moveToArea({ store, sync, actions, offerUndo, pushToast }, [open.id], areaId, 1)
          }
          onClose={() => setRefilePickerOpen(false)}
        />
      ) : null}
      {snoozeIds ? (
        <SnoozeMenu
          ids={snoozeIds}
          onClose={() => setSnoozeIds(null)}
          // Advance only from a view that hides snoozed mail (the inbox). The
          // callback closes over THIS render's list, which is the pre-action one
          // advancePastOpen needs.
          onSnoozed={() => {
            recordProcessed(snoozeIds);
            if (plan.snoozed === false) afterRemoval(snoozeIds);
          }}
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
