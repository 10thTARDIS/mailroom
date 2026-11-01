// SPDX-License-Identifier: AGPL-3.0-only

// The React binding for the client-core (spec §12: core/ is framework-agnostic,
// so ALL of the framework knowledge lives here and in ui/).
//
// The core exposes a plain subscribe/getState store, which is exactly the
// contract React 18's useSyncExternalStore wants — no adapter, no duplicate
// cache, no query library sitting between the app and a protocol that already
// has its own state/changes machinery (see web/README.md for why TanStack Query
// is deliberately not used here).

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import type { ReactNode } from "react";
import type { JSX } from "react";

import { JmapClient } from "../core/jmap/client";
import { reconcilePush } from "./push";
import { MailStore } from "../core/store";
import type { MailState } from "../core/store";
import { StorageActionQueue } from "../core/queue";
import { SyncEngine } from "../core/sync";
import { MailActions } from "../core/actions";
import { UNDO_TTL_MS, UndoLog } from "../core/undo";
import type { View } from "../core/views";
import { viewKey } from "../core/views";
import { viewForRoute, viewRoute } from "../core/routes";
import { navigate, useListRoute, useRoute } from "./routing";
import { SESSION_URL, redirectToLogin } from "./auth";

export interface Toast {
  id: number;
  kind: "info" | "error";
  message: string;
  action?: { label: string; run: () => void };
  /** An optional second button beside `action` (for example "Not now"). Rendered
   * wherever `action` is, as a real button, and dismisses the toast the same way. */
  secondary?: { label: string; run: () => void };
  /**
   * Remove an info toast after this long even though it carries an action.
   * An offer (the rule suggestion) should go when the moment has passed, like
   * the move confirmation beside it, rather than wait for an answer forever.
   */
  ttlMs?: number;
  /**
   * The undo offer this toast carries (#375 increment 6): which entry in the
   * log it is about, and when that offer runs out.
   *
   * `expiresAt` is DUPLICATED here rather than read off the log on every frame
   * because the toast is what renders the countdown, and a component that
   * reached into a mutable object for a number it re-renders once a second
   * would have no way to be told the number moved (the pause moves it). Both
   * copies are written by the same two calls — `offerUndo` and `extendUndo` —
   * so they cannot drift.
   */
  undo?: { entryId: number; expiresAt: number };
}

interface AppContextValue {
  store: MailStore;
  /** The JMAP client itself — the UI needs it for the SESSION-supplied blob URLs
   * (attachment downloads, compose uploads); everything else goes through
   * actions/sync. */
  client: JmapClient;
  sync: SyncEngine;
  actions: MailActions;
  toasts: Toast[];
  pushToast: (t: Omit<Toast, "id">) => number;
  dismissToast: (id: number) => void;
  /**
   * Records an inverse and shows the toast that offers it (#375 increment 6).
   *
   * ONE UNDO TOAST AT A TIME: a new offer replaces the visible one. The
   * replaced ENTRY stays in the log until it expires, so Ctrl/Cmd+Z can still
   * reach it — what is bounded is the screen, not the memory. Two stacked
   * countdowns would be two deadlines to read and the wrong one to press.
   *
   * `ttlMs` is for the offers whose deadline is not ours to choose: a send's
   * undo window is the server's, and it is `sendAt` (core/actions.ts
   * SendOutcome).
   */
  offerUndo: (label: string, inverse: () => Promise<boolean>, ttlMs?: number) => void;
  /** Runs one offer, once. Both the button and the shortcut come through here,
   * because `UndoLog.take` is what makes "once" true. */
  runUndo: (entryId: number) => void;
  /** Ctrl/Cmd+Z. Returns whether there was anything to undo, so the key handler
   * can leave the keystroke alone when there was not. */
  undoLatest: () => boolean;
  /** THE PAUSE. Holds an offer open with `ttlMs` still on its clock, so a toast
   * being read or hovered does not expire mid-sentence (ui/Toasts.tsx). */
  extendUndo: (entryId: number, ttlMs: number) => void;
  view: View;
  setView: (v: View) => void;
}

const AppContext = createContext<AppContextValue | null>(null);

export function AppProvider({ children }: { children: ReactNode }): JSX.Element {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const toastId = useRef(0);

  const pushToast = useCallback((t: Omit<Toast, "id">): number => {
    toastId.current += 1;
    const id = toastId.current;
    setToasts((prev) => [...prev, { ...t, id }]);
    // Errors stay until dismissed; info is transient. An UNDO toast is neither:
    // it lives exactly as long as the offer behind it, and Toasts.tsx dismisses
    // it when the countdown reaches zero — a four-second timer over an
    // eight-second offer would take the button away while the offer stood.
    if (t.kind === "info" && !t.action && !t.undo) {
      setTimeout(() => setToasts((prev) => prev.filter((x) => x.id !== id)), 4000);
    } else if (t.kind === "info" && t.ttlMs && t.ttlMs > 0) {
      setTimeout(() => setToasts((prev) => prev.filter((x) => x.id !== id)), t.ttlMs);
    }
    return id;
  }, []);

  const dismissToast = useCallback((id: number) => {
    setToasts((prev) => prev.filter((x) => x.id !== id));
  }, []);

  // -- undo (#375 increment 6) ------------------------------------------------
  //
  // One log for the app, alongside the core singletons below. It is not part of
  // `core` the way the store is because nothing in core/ ever reads it: the
  // verbs register inverses and the toasts run them, and both of those are UI.
  const undoLog = useMemo(() => new UndoLog(), []);

  const offerUndo = useCallback(
    (label: string, inverse: () => Promise<boolean>, ttlMs: number = UNDO_TTL_MS) => {
      const entry = undoLog.push(label, inverse, Date.now(), ttlMs);
      // Both updates land in one React batch, so the previous undo toast is
      // never on screen beside the new one for a frame.
      setToasts((prev) => prev.filter((x) => !x.undo));
      pushToast({
        kind: "info",
        message: label,
        undo: { entryId: entry.id, expiresAt: entry.expiresAt },
      });
    },
    [undoLog, pushToast],
  );

  const runUndo = useCallback(
    (entryId: number) => {
      // TAKE FIRST. It removes the entry, which is what stops a second press —
      // or the shortcut arriving after the button — from applying the same
      // inverse twice.
      const entry = undoLog.take(entryId);
      setToasts((prev) => prev.filter((x) => x.undo?.entryId !== entryId));
      if (!entry) return;
      // A failed inverse reports itself: every one of them is a MailActions
      // call, and MailActions surfaces a refusal through `onError` above. A
      // second toast here would either duplicate the server's own words or
      // replace them with a vaguer sentence.
      void entry.inverse();
    },
    [undoLog],
  );

  const undoLatest = useCallback((): boolean => {
    const entry = undoLog.latest(Date.now());
    if (!entry) return false;
    runUndo(entry.id);
    return true;
  }, [undoLog, runUndo]);

  const extendUndo = useCallback(
    (entryId: number, ttlMs: number) => {
      const now = Date.now();
      undoLog.extend(entryId, now, ttlMs);
      setToasts((prev) =>
        prev.map((x) =>
          x.undo?.entryId === entryId ? { ...x, undo: { entryId, expiresAt: now + ttlMs } } : x,
        ),
      );
    },
    [undoLog],
  );

  // One set of singletons for the life of the app. The refs are needed because
  // MailActions wants callbacks that can reach the toast state.
  const toastRef = useRef({ pushToast });
  toastRef.current = { pushToast };

  const core = useMemo(() => {
    const client = new JmapClient({ sessionUrl: SESSION_URL });
    const store = new MailStore(window.localStorage);
    const queue = new StorageActionQueue(window.localStorage);
    const sync = new SyncEngine(client, store);
    const actions = new MailActions(client, store, queue, sync, {
      onError: (m) => toastRef.current.pushToast({ kind: "error", message: m }),
      onInfo: (m) => toastRef.current.pushToast({ kind: "info", message: m }),
    });
    return { client, store, queue, sync, actions };
  }, []);

  // THE VIEW IS DERIVED FROM THE ROUTE NOW (#373). The URL is the single source
  // of truth for what is on screen; this reads it through routing.ts's store
  // (which owns the hashchange listener that used to live here) and maps it
  // with the one table in core/routes.ts. `view` and the query layer beneath it
  // are unchanged — routes sit ON TOP of views, they do not replace them.
  const route = useRoute();
  const listRoute = useListRoute();
  const derivedView = viewForRoute(route, { previousListRoute: listRoute });

  // ...but its IDENTITY must change only when the view really changes.
  // Components key effects off `view` ("changing view resets everything that was
  // about the old one" — ui/routes/ListRoute.tsx), and routes move for reasons
  // the list does not care about: opening a message writes `?m=`, opening
  // settings changes the page entirely while the list beneath it stays put. A
  // fresh object for those would clear the selection and close the reading pane
  // behind the settings sheet. Adjusting a ref during render is the cheap,
  // idempotent form of derived state — no extra render, and StrictMode's double
  // invocation reaches the same answer.
  const viewRef = useRef(derivedView);
  if (viewKey(viewRef.current) !== viewKey(derivedView)) viewRef.current = derivedView;
  const view = viewRef.current;

  /** The compatibility shim (#373): every component that still speaks Views —
   * the rail, the tab strip, the search box — navigates through here, so they
   * did not have to change with the routing layer underneath them. */
  const setView = useCallback((v: View) => {
    navigate(viewRoute(v));
  }, []);

  // Boot: connect, load, start the changes loop, resume the offline queue.
  const bootView = useRef(view);
  useEffect(() => {
    void core.sync
      .start(bootView.current)
      .then(() => core.actions.resume())
      // Session established: put back a push subscription the server lost (push.ts).
      .then(() => reconcilePush(core.client));
    return () => core.sync.stop();
  }, [core]);

  // Every later view — from a click, a shortcut, the back button or a pasted
  // hash, which are now all the same thing. Compared by identity against the
  // one sync.start already loaded, so booting does not fetch the first view
  // twice.
  //
  // ⚠ EXCEPT THE DESK AND THE TRAY, WHICH LOAD THEMSELVES (#375). `#/inbox` is
  // not one list any more — it is a screenful of them (the tray, an area per
  // section, a drawer) — and `#/tray` is the tray section plus the counts
  // beside it; both ask through `sync.loadDesk`. Running `loadView` for those
  // routes as well would be two things at once: a whole inbox (or starred)
  // query per navigation for a list nothing renders, and — because `loadView`
  // CLEARS `SyncEngine.desk`, correctly, so an ordinary view does not re-run a
  // dozen desk queries on every change — a race that leaves the engine
  // believing no desk is on screen while one plainly is. Both routes re-assert
  // it if that happens anyway; this is what stops it happening on every tab
  // switch.
  const loadedView = useRef<View>(view);
  useEffect(() => {
    if (loadedView.current === view) return;
    loadedView.current = view;
    if (route.page === "tray") return;
    if (route.page === "inbox" && !route.sort) return;
    void core.sync.loadView(view);
  }, [core, view, route]);

  // Coming back to the tab (or back online) syncs immediately rather than
  // waiting out the poll interval, and flushes anything the queue is holding.
  useEffect(() => {
    const wake = (): void => {
      if (document.visibilityState !== "visible") return;
      void core.sync.poll();
      void core.actions.drain();
    };
    document.addEventListener("visibilitychange", wake);
    window.addEventListener("online", wake);
    return () => {
      document.removeEventListener("visibilitychange", wake);
      window.removeEventListener("online", wake);
    };
  }, [core]);

  const value: AppContextValue = {
    store: core.store,
    client: core.client,
    sync: core.sync,
    actions: core.actions,
    toasts,
    pushToast,
    dismissToast,
    offerUndo,
    runUndo,
    undoLatest,
    extendUndo,
    view,
    setView,
  };

  return <AppContext.Provider value={value}>{children}</AppContext.Provider>;
}

export function useApp(): AppContextValue {
  const ctx = useContext(AppContext);
  if (!ctx) throw new Error("useApp must be used inside <AppProvider>");
  return ctx;
}

/** Subscribes a component to the client-core store. */
export function useMailState(): MailState {
  const { store } = useApp();
  return useSyncExternalStore(store.subscribe, store.getState, store.getState);
}

/** Bounces to the login page when the server says the session is gone. */
export function useAuthGuard(conn: MailState["conn"]): void {
  useEffect(() => {
    if (conn === "unauthenticated") redirectToLogin();
  }, [conn]);
}
