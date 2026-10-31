// SPDX-License-Identifier: AGPL-3.0-only

// THE BROWSER HALF OF ROUTING (#373). core/routes.ts knows the shapes; this
// module is the only place that touches `location`, `history` and per-device
// storage — so the route table stays testable without a DOM and a future native
// shell replaces this file and nothing else (spec §12).
//
// Everything here that makes a DECISION is a pure function taking its inputs as
// arguments (decideEscape, rememberPosition/recallPosition, applyRemembered),
// and everything that touches the browser is a thin wrapper around one. That
// split is deliberate: the hard parts of routing are "which of three things
// should Escape do" and "is this the list I was on", and both are answerable
// without a browser.

import { useSyncExternalStore } from "react";

import type { Id } from "../core/jmap/types";
import type { InboxTab, Route } from "../core/routes";
import { INBOX_ROUTE, escapeTarget, formatRoute, isListRoute, parseRoute } from "../core/routes";

// -- storage -----------------------------------------------------------------
//
// Read through helpers that fail soft: a browser with storage disabled, or a
// private window, must lose the memory and not the app (searchHistory.ts makes
// the same promise for the same reason).

const DEPTH_KEY = "mailroom.route.depth";
const LAST_LIST_KEY = "mailroom.route.lastList";
const POSITIONS_KEY = "mailroom.route.positions";
const SCOPE_KEY = "mailroom.route.scope";

function sessionStore(): Storage | null {
  try {
    return typeof window === "undefined" ? null : window.sessionStorage;
  } catch {
    return null;
  }
}

function localStore(): Storage | null {
  try {
    return typeof window === "undefined" ? null : window.localStorage;
  } catch {
    return null;
  }
}

function readJson<T>(store: Storage | null, key: string): T | null {
  try {
    const raw = store?.getItem(key);
    if (!raw) return null;
    return JSON.parse(raw) as T;
  } catch {
    return null;
  }
}

function writeJson(store: Storage | null, key: string, value: unknown): void {
  try {
    store?.setItem(key, JSON.stringify(value));
  } catch {
    /* storage full or unavailable — the memory is a convenience, never a gate */
  }
}

// -- the route store ---------------------------------------------------------
//
// useSyncExternalStore over `hashchange`, the same primitive state.tsx uses for
// the client-core. getSnapshot MUST return a stable reference or React loops,
// so the parsed route is cached against the hash it was parsed from.

interface RouteSnapshot {
  route: Route;
  /** The list underneath: the route itself, or — while settings is open, which
   * overlays the list rather than replacing it — the last one that was a list. */
  listRoute: Route;
}

let snapshot: RouteSnapshot | null = null;
let snapshotHash: string | null = null;
const listeners = new Set<() => void>();

/** Set while WE are the cause of the next hashchange, so a genuine browser
 * back/forward can be told apart from our own navigation. */
let selfNavigating = false;

function currentHash(): string {
  return typeof window === "undefined" ? "" : window.location.hash;
}

function loadLastList(): Route {
  const stored = readJson<Route>(sessionStore(), LAST_LIST_KEY);
  return stored && isListRoute(stored) ? stored : INBOX_ROUTE;
}

function refresh(): void {
  const hash = currentHash();
  const route = parseRoute(hash);
  let listRoute: Route;
  if (isListRoute(route)) {
    listRoute = route;
    // Session-scoped, so a reload straight onto #/settings/rules still knows
    // which list to draw beneath it and which one Escape returns to.
    writeJson(sessionStore(), LAST_LIST_KEY, route);
  } else {
    listRoute = loadLastList();
  }
  snapshot = { route, listRoute };
  snapshotHash = hash;
}

function readSnapshot(): RouteSnapshot {
  if (snapshot === null || snapshotHash !== currentHash()) refresh();
  return snapshot as RouteSnapshot;
}

function emit(): void {
  for (const listener of [...listeners]) listener();
}

function onHashChange(): void {
  if (selfNavigating) {
    selfNavigating = false;
  } else {
    // A navigation we did not make (the browser's own back/forward, or a
    // hand-edited hash) makes the pushed-entry count untrustworthy. Zeroing it
    // FAILS CLOSED: the worst outcome of an over-count is history.back() taking
    // the user out of the app entirely, and the worst outcome of an under-count
    // is an Escape that replaces instead of going back — which stays put.
    setOwnDepth(0);
  }
  escapeConsumed = false;
  refresh();
  emit();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  if (listeners.size === 1 && typeof window !== "undefined") {
    window.addEventListener("hashchange", onHashChange);
  }
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0 && typeof window !== "undefined") {
      window.removeEventListener("hashchange", onHashChange);
    }
  };
}

/** The current route. */
export function useRoute(): Route {
  return useSyncExternalStore(subscribe, () => readSnapshot().route);
}

/** The current route, read NOW — for code that runs after an await, where a
 * route captured by the render that started the work may no longer be where
 * the user is (#612: a save lands, and the user has since moved). */
export function currentRoute(): Route {
  return readSnapshot().route;
}

/** The list route underneath the current one — the same thing while the current
 * route IS a list, and the remembered one while settings is open. */
export function useListRoute(): Route {
  return useSyncExternalStore(subscribe, () => readSnapshot().listRoute);
}

// -- navigating --------------------------------------------------------------

/**
 * Go to a route. A push (the default) writes `location.hash`, which is what
 * makes back work and what a hand-edited URL is indistinguishable from;
 * `replace` rewrites the current entry instead, for the navigations that are
 * corrections rather than moves (applying a remembered scope, advancing the
 * reading pane, backing out when there is nothing of ours to go back to).
 *
 * Navigating to where you already are is a no-op, deliberately: a URL-driven
 * app has no way to distinguish "clicked Inbox again" from a re-render, and a
 * no-op is the honest reading of a navigation that does not move.
 */
export function navigate(route: Route, opts: { replace?: boolean } = {}): void {
  if (typeof window === "undefined") return;
  const hash = formatRoute(route);
  if (window.location.hash === hash) return;
  if (opts.replace) {
    window.history.replaceState(window.history.state, "", hash);
    // replaceState fires no event, so this store has to say so itself.
    refresh();
    emit();
    return;
  }
  setOwnDepth(ownDepth() + 1);
  selfNavigating = true;
  window.location.hash = hash;
}

/** One step back in the browser's own history. */
export function back(): void {
  if (typeof window === "undefined") return;
  setOwnDepth(ownDepth() - 1);
  selfNavigating = true;
  window.history.back();
}

// -- how many history entries are ours ---------------------------------------

/** How many entries this app has pushed on top of the one the tab arrived on.
 * Per tab (sessionStorage), and it survives a reload because the history does. */
export function ownDepth(): number {
  const raw = sessionStore()?.getItem(DEPTH_KEY);
  const n = raw === null || raw === undefined ? 0 : Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : 0;
}

function setOwnDepth(n: number): void {
  try {
    sessionStore()?.setItem(DEPTH_KEY, String(Math.max(0, n)));
  } catch {
    /* see writeJson */
  }
}

// -- escape ------------------------------------------------------------------

export type EscapeDecision = "back" | { replace: Route } | "none";

/**
 * ESCAPE AND THE BACK BUTTON MUST AGREE (#373).
 *
 * If the previous history entry is one we pushed, Escape is `history.back()` —
 * so the two controls do the same thing and the forward button still works.
 * If it is not (a deep link, a fresh tab, a hash someone pasted), going back
 * would leave the app; the target is written over the current entry instead, so
 * Escape still peels a layer without stranding anyone on another site.
 *
 * Pure so the decision can be tested without a history stack, which is the only
 * way to test it at all — a real one cannot be inspected from script.
 */
export function decideEscape(route: Route, depth: number, previous?: Route): EscapeDecision {
  const target = escapeTarget(route, previous);
  if (!target) return "none";
  return depth > 0 ? "back" : { replace: target };
}

/**
 * True while an Escape has already been acted on in the CURRENT event.
 *
 * Several overlays close themselves on Escape through their own onKeyDown (the
 * `.modal-backdrop` in AddAccountWizard, Compose's snippet and send-later
 * pickers, ThreadPane's confirm, desk/ConfirmDialog), and the shell has a
 * window-level handler as well. That was harmless when closing was
 * `setState(false)` twice; now that closing is a navigation, doing it twice
 * would go back two entries. React's handler runs first (its root is inside
 * window), so the shell asks this before acting.
 *
 * #374 shortened that list: the four settings sheets that used to be on it are
 * ROWS of the settings page now, with no backdrop and no Escape handler of their
 * own — the row's heading opens them and this module's escapeTarget peels the
 * route. The settings search field is the one place on that page that still
 * stops the keystroke itself, and it does it by stopping propagation rather than
 * by navigating (ui/routes/SettingsRoute.tsx says why).
 */
let escapeConsumed = false;

export function consumeEscape(): boolean {
  if (!escapeConsumed) return false;
  escapeConsumed = false;
  return true;
}

function markEscapeConsumed(): void {
  escapeConsumed = true;
  // A stale flag must never outlive its event: the ✕ button calls the same
  // close path with no keystroke behind it at all.
  queueMicrotask(() => {
    escapeConsumed = false;
  });
}

/** Back out one layer. Returns false when there was nothing to back out of, so
 * the caller can fall through to whatever it does next (clear the selection,
 * blur the field). */
export function escapeNow(route: Route, previous?: Route): boolean {
  const decision = decideEscape(route, ownDepth(), previous);
  if (decision === "none") return false;
  markEscapeConsumed();
  if (decision === "back") back();
  else navigate(decision.replace, { replace: true });
  return true;
}

/** Leave for a specific route with the same back-or-replace decision Escape
 * makes: `u` (close the reading pane), a sheet's ✕, the reading pane's ✕. It
 * marks the keystroke consumed for the same reason escapeNow does — a sheet
 * closing ITSELF on Escape must not then be closed again by the shell. */
export function leaveTo(target: Route): void {
  markEscapeConsumed();
  if (ownDepth() > 0) back();
  else navigate(target, { replace: true });
}

// -- where the list was ------------------------------------------------------

export interface ListPosition {
  cursor: Id | null;
  scrollTop: number;
}

export type PositionMap = Record<string, ListPosition>;

/** How many lists to remember. A cap rather than none because this is mirrored
 * into sessionStorage on every navigation, and an unbounded map of every search
 * anyone ever ran is a quota exhaustion waiting for a long session. */
export const POSITION_LIMIT = 20;

/**
 * Remember where a list was, keyed by `routeKey` (which ignores the open
 * message, so opening one does not look like arriving at a different list).
 *
 * Returns a NEW map, oldest-first, with the named key moved to the end — so the
 * eviction below drops the list you have not looked at in longest rather than
 * the one you are about to return to.
 */
export function rememberPosition(map: PositionMap, key: string, pos: ListPosition): PositionMap {
  const next: PositionMap = {};
  for (const [k, v] of Object.entries(map)) if (k !== key) next[k] = v;
  next[key] = pos;
  const keys = Object.keys(next);
  for (const stale of keys.slice(0, Math.max(0, keys.length - POSITION_LIMIT))) delete next[stale];
  return next;
}

/** Where that list was, or null if we have never been there. */
export function recallPosition(map: PositionMap, key: string): ListPosition | null {
  return map[key] ?? null;
}

export function savePosition(key: string, pos: ListPosition): void {
  const map = readJson<PositionMap>(sessionStore(), POSITIONS_KEY) ?? {};
  writeJson(sessionStore(), POSITIONS_KEY, rememberPosition(map, key, pos));
}

export function loadPosition(key: string): ListPosition | null {
  const map = readJson<PositionMap>(sessionStore(), POSITIONS_KEY) ?? {};
  const pos = recallPosition(map, key);
  if (!pos) return null;
  // Validated rather than trusted: sessionStorage is editable by hand and a
  // string where a number belongs would be handed straight to scrollTop.
  const scrollTop = typeof pos.scrollTop === "number" && Number.isFinite(pos.scrollTop)
    ? pos.scrollTop
    : 0;
  const cursor = typeof pos.cursor === "string" ? pos.cursor : null;
  return { cursor, scrollTop };
}

// -- the last scope and tab, per device --------------------------------------

/** What this device was last looking at, when the URL does not say (prefs.ts
 * keeps theme and density the same way, and for the same reason: it is a
 * property of the device, not of the account). */
export interface RememberedScope {
  tab?: InboxTab;
  acct?: Id;
}

/**
 * The route to REPLACE the given one with so it carries the remembered scope,
 * or null when it already says everything or there is nothing to add.
 *
 * THE DESK'S OWN THREE PAGES — the inbox, the tray and all mail. Since #375
 * `acct` narrows every list route (core/routes.ts viewForRoute), so the limit
 * here is no longer "where it would do anything"; it is where the scope is a
 * property of the SCREEN rather than of the link. These three are the surfaces
 * the account switcher sits above and scopes as a unit. A legacy view hash
 * (`#/starred`, `#/label/<id>`, a search) is left exactly as written: those are
 * links people paste, the rail and the `g` shortcuts carry the current scope
 * onto them explicitly, and silently rewriting someone's URL to add a filter
 * they did not ask for is the one thing scope memory must not do.
 *
 * A route that names its own scope is never overridden either — the URL always
 * wins over the memory, or a pasted link would not mean what it says.
 */
export function applyRemembered(route: Route, scope: RememberedScope): Route | null {
  if (route.page === "inbox") {
    const acct = route.acct ?? scope.acct;
    // The sort queue is not one of the inbox's two halves, so it never adopts a
    // tab (and `tab` is null on it by construction).
    const tab = route.tab ?? (route.sort ? null : scope.tab ?? null);
    if (acct === route.acct && tab === route.tab) return null;
    return { ...route, tab, ...(acct ? { acct } : {}) };
  }
  if (route.page === "all" || route.page === "tray") {
    const acct = route.acct ?? scope.acct;
    if (acct === route.acct) return null;
    return { ...route, ...(acct ? { acct } : {}) };
  }
  return null;
}

/** What a route contributes to the memory. Only what it SAYS: a route with no
 * `acct` is "wherever I was", not "all accounts", so it does not clear the
 * memory — that would fight `applyRemembered`, which puts the scope straight
 * back on the next render. "All accounts" is an explicit choice and is written
 * by the explicit setter below. */
export function rememberedFrom(route: Route, current: RememberedScope): RememberedScope {
  const next: RememberedScope = { ...current };
  if (route.acct) next.acct = route.acct;
  if (route.page === "inbox" && route.tab !== null) next.tab = route.tab;
  return next;
}

/**
 * Remember (or forget) the account scope, said outright — what the account
 * switcher writes when a scope is CHOSEN (#375).
 *
 * It exists because `rememberedFrom` cannot express "all accounts": a route
 * with no `acct` is indistinguishable from one that simply does not carry the
 * scope, so clearing the memory from a route would clear it on every navigation
 * that happened not to mention an account. Picking "All accounts" is a
 * decision, and the only way a decision reaches durable memory is by being
 * stated. Written BEFORE the navigation, so the redirect `applyRemembered`
 * would otherwise perform on the new route reads the new memory rather than
 * putting the old account straight back.
 */
export function setRememberedAccount(acct: Id | null): void {
  const next: RememberedScope = { ...loadScope() };
  if (acct) next.acct = acct;
  else delete next.acct;
  writeJson(localStore(), SCOPE_KEY, next);
}

export function loadScope(): RememberedScope {
  const raw = readJson<RememberedScope>(localStore(), SCOPE_KEY);
  if (!raw || typeof raw !== "object") return {};
  const scope: RememberedScope = {};
  if (raw.tab === "review" || raw.tab === "rest") scope.tab = raw.tab;
  if (typeof raw.acct === "string") scope.acct = raw.acct;
  return scope;
}

export function saveScope(route: Route): void {
  const current = loadScope();
  const next = rememberedFrom(route, current);
  if (next.tab === current.tab && next.acct === current.acct) return;
  writeJson(localStore(), SCOPE_KEY, next);
}
