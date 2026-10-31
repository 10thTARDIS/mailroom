// SPDX-License-Identifier: AGPL-3.0-only

// SyncEngine — session, initial load, view queries, and the incremental
// `Foo/changes` loop. DOM-free (spec §12).
//
// The sync contract with this server, precisely:
//
//   * Every read method returns an opaque per-type `state` string backed by the
//     per-user change log (spec §13/§14). We keep one cursor per type and ask
//     `Foo/changes` what moved — we never re-fetch the world on a timer.
//   * This client never calls `Email/queryChanges` at all: every filter it
//     issues (onDesk/onSurface, inTab, deskArchived, text search, …) is one
//     the server itself cannot compute a real delta for — see
//     `deltaStableConditions` in internal/api/email.go, which DOES return a
//     genuine RFC 8620 §5.6 delta for a narrower, structured filter set
//     (inMailbox/hasKeyword/notKeyword/before/after/empty, immutable sort)
//     that this client happens not to use. So when — and only when — the
//     Email state moves, the current view's `Email/query` is re-run. That is
//     the documented `cannotCalculateChanges` fallback the RFC prescribes for
//     a volatile filter, not a poll-everything loop.
//   * PUSH, not poll (#85). The server now serves `eventSourceUrl` (RFC 8620
//     §7.3): one EventSource connection delivers a `StateChange` the moment a
//     type's state moves, and we answer it with the same `Foo/changes` call the
//     timer used to make. So the timer is not what detects change any more — it
//     is the FALLBACK, and it runs only while the push channel is down (no
//     EventSource in this runtime, a proxy that ate the stream, an offline tab).
//     Both paths converge on doPoll(): there is exactly one piece of logic that
//     decides what changed, and push only decides WHEN to run it.
//   * Mailbox unread counts move when EMAILS change, but the server appends no
//     Mailbox change_log row for that — so an Email change also refreshes the
//     (cheap, handful-of-rows) Mailbox list.

import type { Call, CallResult, JmapClient } from "./jmap/client";
import { JmapAuthError, JmapMethodError, JmapTransportError, outboxPageCalls } from "./jmap/client";
import type {
  Area,
  AreaGetDeskResult,
  AreaSummaryResult,
  AreaSummaryRow,
  ChangesResult,
  ClientPreferences,
  DeskPreferences,
  Email,
  EmailSubmission,
  GetResult,
  Id,
  Identity,
  ComposeSnippet,
  CategoryTabState,
  SavedSearch,
  Label,
  Mailbox,
  RemoteAllow,
  QueryResult,
  SearchSnippet,
  Thread,
} from "./jmap/types";
import {
  EMAIL_BODY_PROPERTIES,
  EMAIL_LIST_PROPERTIES,
  MAX_BODY_VALUE_BYTES,
} from "./jmap/types";
import type { MailState, MailStore, SectionState, SyncErrorKind } from "./store";
import type { ChangeType, SyncCursor } from "./session";
import { CHANGE_TYPES, PUSH_TYPES, hasDeskSnapshot, maxCallsInRequest, maxObjectsInGet } from "./session";
import type { View } from "./views";
import { planFor, snippetFilter, viewKey } from "./views";
import type { DeskSection, DeskTab } from "./desk";
import { deskSections, sectionFilter, sectionKey, sectionKeyPrefix } from "./desk";

/** How many messages a page of a list holds. */
export const PAGE_SIZE = 50;

/**
 * The in-tray strip's page (#375), four times a list page.
 *
 * The tray is not scrolled a page at a time like a mailbox: it is BOUNDED work
 * — everything the user has said they will come back to — and the strip groups
 * it by how long each item has waited. A window that cut the oldest items off
 * would drop exactly the ones the grouping exists to surface. 200 is generous
 * for a tray that is behaving and still a bound; when it is not enough the
 * section reports `ids.length < total` and the UI says so rather than
 * presenting the window as the count.
 */
export const TRAY_PAGE = 200;

/**
 * The needs-a-category queue's page (#176), an order of magnitude larger.
 *
 * Every other list is READ a page at a time, so a page is the right unit. This
 * one is GROUPED and the groups are counted: "24 messages arrived at bank@"
 * has to be true of the mailbox, not of the window, because the user answers it
 * as a statement about a service. On a 50-row page a large real
 * group (120 messages at one address) would be reported as 50, "Label all 50"
 * would file 50, and the remaining 70 would reappear as the same group — reading
 * exactly like the batch not having worked.
 *
 * It is also what makes the homogeneity number honest: senderCount is counted
 * over the group's members, so on a page-sized sample a group that spans dozens
 * of senders can easily show one, and the guard against sweeping a mixed group
 * under one label silently fails open.
 *
 * Affordable because this list is cheap: no bodies, no snippets — the same
 * projection columns every other list fetches, over a set bounded by how much
 * mail the user has not categorised. When it is still not enough, the header
 * says so rather than asserting the count (MessageList `complete`).
 */
export const QUEUE_PAGE_SIZE = 500;

function pageSizeFor(view: View): number {
  return view.kind === "needsCategory" ? QUEUE_PAGE_SIZE : PAGE_SIZE;
}

/** The highest open-suggestion confidence on a message, or -1 when it has none
 * — the merge key for a confidence-ordered plan. Reads the raw Email rather than
 * the overlay on purpose: this decides which of the SERVER'S rows are in the
 * window, and the server has not seen the overlay. */
function maxConfidence(e: Email): number {
  const suggestions = e.suggestedLabels;
  if (!suggestions || suggestions.length === 0) return -1;
  return suggestions.reduce((best, s) => (s.confidence > best ? s.confidence : best), -1);
}

/** How often the FALLBACK changes loop runs — i.e. only while the push channel
 * is down. With push up, nothing is polled at all. */
const DEFAULT_POLL_MS = 10_000;

/** Ask the server to ping the push stream this often (seconds). It keeps a proxy
 * from silently dropping an idle stream, which would otherwise look to us like a
 * connection that is up and simply has nothing to say. */
const PUSH_PING_SECONDS = 30;

/** SearchSnippet/get is capped server-side (internal/api/search.go maxSnippets). */
const MAX_SNIPPETS = 100;

/** The order every view gets unless it asks for another (#152/#154).
 *
 * `returnedAt`, not `receivedAt`: mail coming back from a snooze sorts by when
 * it RETURNED, so it lands at the top instead of wherever its age puts it. The
 * server coalesces, so for anything never snoozed this IS a receivedAt sort.
 *
 * It has to be the SERVER's order — the window a view pages through is chosen
 * there, and a client can only reorder what it already holds. */
const DEFAULT_SORT = [{ property: "returnedAt" as const, isAscending: false }];

/** An EmailSubmission/query answer, or null when it is not one this client can
 * page on (#653): no `ids` array means nothing to hydrate or count. A missing
 * position reads as 0 and a missing total as "this page is all of it", so a
 * short answer never starts a paging loop. */
function outboxQueryOf(value: unknown): QueryResult | null {
  const q = value as Partial<QueryResult> | null;
  if (!q || !Array.isArray(q.ids)) return null;
  const position = typeof q.position === "number" ? q.position : 0;
  return {
    accountId: q.accountId ?? "",
    queryState: typeof q.queryState === "string" ? q.queryState : "",
    canCalculateChanges: q.canCalculateChanges === true,
    position,
    total: typeof q.total === "number" ? q.total : position + q.ids.length,
    ids: q.ids,
  };
}

/** The push channel, abstracted so this module stays DOM-free and testable: a
 * browser supplies EventSource, a test supplies a fake, a runtime with neither
 * supplies nothing and gets the polling fallback. */
export interface PushChannel {
  close(): void;
}

export interface PushHandlers {
  /** The stream is live: the fallback poll can stand down. */
  onOpen(): void;
  /** An RFC 8620 §7.3 StateChange arrived (the raw `data` of a `state` event). */
  onState(data: string): void;
  /** The stream is down. EventSource retries on its own; we resume polling in
   * the meantime. */
  onError(): void;
}

export type PushFactory = (url: string, handlers: PushHandlers) => PushChannel | null;

/** The default push factory: EventSource, when the runtime has one. Cookies ride
 * along automatically on a same-origin EventSource, which is exactly how every
 * other request in this client authenticates. */
export const defaultPushFactory: PushFactory = (url, handlers) => {
  const ES = (globalThis as { EventSource?: typeof EventSource }).EventSource;
  if (!ES) return null;
  const es = new ES(url);
  es.onopen = () => handlers.onOpen();
  es.onerror = () => handlers.onError();
  es.addEventListener("state", (ev) => handlers.onState((ev as MessageEvent<string>).data));
  return { close: () => es.close() };
};

export interface SyncOptions {
  pollMs?: number;
  /** Injectable timers keep this module testable and DOM-free. */
  setTimer?: (fn: () => void, ms: number) => unknown;
  clearTimer?: (handle: unknown) => void;
  /** Injectable push channel. `null` disables push entirely (pure polling). */
  push?: PushFactory | null;
}

/** The `searchLimit` Email/query is given when enumerating every match for a bulk
 * action (#769): the server's search.MaxLimit. Without it a text or `from:`
 * search can name at most 500 messages, whatever `position` says. */
export const BULK_SEARCH_LIMIT = 5000;

export class SyncEngine {
  private readonly client: JmapClient;
  private readonly store: MailStore;
  private readonly pollMs: number;
  private readonly setTimer: (fn: () => void, ms: number) => unknown;
  private readonly clearTimer: (handle: unknown) => void;
  private readonly pushFactory: PushFactory | null;
  private timer: unknown = null;
  private push: PushChannel | null = null;
  private pushOpen = false;
  private running = false;
  private view: View = { kind: "role", role: "inbox" };
  /** The desk currently on screen (#375), or null when an ordinary view is.
   * It is what decides which of the two refresh paths a change takes: the desk
   * is several lists at once and `refreshView` knows nothing about them. */
  private desk: { tab: DeskTab; scope: Id | null } | null = null;
  private inFlight: Promise<void> | null = null;
  /**
   * The desk load in progress, if any (#665). Desk loads are single-flight with
   * one trailing re-run, the way poll() is: `again` says another was asked for
   * while this one ran, `superseded` says a different desk (tab or scope) took
   * over and this chain must not run its trailing load.
   */
  private deskChain: {
    promise: Promise<void>;
    again: boolean;
    superseded: boolean;
    abort: AbortController | null;
  } | null = null;
  /** Bumped at the start of every desk request; only the latest may be applied. */
  private deskGen = 0;
  /** "Show more" requests not yet sent: they survive being coalesced into one
   * trailing load, so a request that arrives mid-flight is not lost when a later
   * plain refresh replaces it. */
  private deskMore = new Set<string>();
  /** Set when a server that advertised `Area/getDesk` answered `unknownMethod`
   * (#665): every later desk load takes the per-section path. */
  private noDeskSnapshot = false;
  /** The open-drawer pass in progress, if any — see refreshOpenDrawers(). */
  private drawerPass: { promise: Promise<void>; again: boolean } | null = null;
  /** A poll was asked for while one was running — see poll(). */
  private pollAgain = false;
  /**
   * THE LATEST LIST LOAD WINS (#724). Bumped by every `loadView`; a response
   * whose generation is no longer the latest is dropped, and the request behind
   * it is aborted when the next one starts. Live search makes this load-bearing:
   * one load per pause in typing, and a slow answer for "inv" arriving after the
   * answer for "invoice" must not put the older list back on screen. It also
   * covers ordinary navigation, which had the same race at human speed.
   */
  private listGen = 0;
  private listAbort: AbortController | null = null;

  constructor(client: JmapClient, store: MailStore, opts: SyncOptions = {}) {
    this.client = client;
    this.store = store;
    this.pollMs = opts.pollMs ?? DEFAULT_POLL_MS;
    this.setTimer = opts.setTimer ?? ((fn, ms) => setTimeout(fn, ms));
    this.clearTimer = opts.clearTimer ?? ((h) => clearTimeout(h as ReturnType<typeof setTimeout>));
    this.pushFactory = opts.push === undefined ? defaultPushFactory : opts.push;
  }

  get currentView(): View {
    return this.view;
  }

  /** The desk on screen, or null. Read by archive-all, which needs the scope
   * the user is actually looking at — a bulk archive computed against the wrong
   * account's drawer would archive mail nobody asked about. */
  get currentDesk(): { tab: DeskTab; scope: Id | null } | null {
    return this.desk;
  }

  /** True while the EventSource stream is live (the UI can say "live" rather
   * than "polling"). */
  get pushing(): boolean {
    return this.pushOpen;
  }

  /** Connects, loads the reference data (mailboxes/labels/identities), loads the
   * initial view, and subscribes to the change stream (falling back to the timer
   * if push is unavailable). */
  async start(view: View): Promise<void> {
    this.view = view;
    this.store.setConn("connecting");
    try {
      const session = await this.client.connect();
      this.store.setSession(session, this.client.accountId);
      this.store.setConn("online");
    } catch (err) {
      this.handleFatal(err);
      return;
    }
    await this.loadReferenceData();
    await this.loadView(view);
    this.running = true;
    this.subscribe();
  }

  stop(): void {
    this.running = false;
    this.clearPoll();
    this.push?.close();
    this.push = null;
    this.pushOpen = false;
  }

  // -- push (RFC 8620 §7.3) --------------------------------------------------

  /** Opens the EventSource stream. If the runtime has no EventSource — or the
   * factory declines — the timer takes over immediately, so a client that cannot
   * be pushed to is never a client that stops syncing. */
  private subscribe(): void {
    if (!this.running) return;
    if (!this.pushFactory) {
      this.schedule();
      return;
    }
    let url: string;
    try {
      // PUSH_TYPES, not CHANGE_TYPES: the stream refuses an unknown type with a
      // request-level 400 and the refusal costs the WHOLE connection, so only
      // types this server pushes may be named here. The two lists are the same
      // set today (#474 added the last one that was missing); they are still
      // two names, because "what this client tracks" and "what this server
      // pushes" are two facts. See session.ts PUSH_TYPES.
      url = this.client.pushUrl({ types: PUSH_TYPES, ping: PUSH_PING_SECONDS });
    } catch {
      this.schedule();
      return;
    }
    const channel = this.pushFactory(url, {
      onOpen: () => {
        this.pushOpen = true;
        // Push is live: stop polling. Run one poll first — anything that moved
        // while we were connecting is not going to be announced again.
        this.clearPoll();
        void this.poll();
      },
      onState: (data) => this.onStateChange(data),
      onError: () => {
        // The browser's EventSource reconnects by itself; until it does, the
        // timer is what keeps the mailbox fresh.
        this.pushOpen = false;
        this.schedule();
      },
    });
    if (!channel) {
      this.schedule();
      return;
    }
    this.push = channel;
  }

  /** Handles one StateChange: if any type's state differs from our cursor, ask
   * the server what actually changed. The push event is a TRIGGER, never a source
   * of truth — `Foo/changes` remains the only thing that says what moved. */
  private onStateChange(data: string): void {
    let changed: Record<string, Record<string, string>> = {};
    try {
      const parsed = JSON.parse(data) as { changed?: Record<string, Record<string, string>> };
      changed = parsed.changed ?? {};
    } catch {
      return; // a malformed event is not a reason to do anything
    }
    const st = this.store.getState();
    const states = changed[st.accountId];
    if (!states) return;
    const moved = CHANGE_TYPES.some((t) => {
      const seen = st.cursor[t];
      const now = states[t];
      return !!now && !!seen && now !== seen;
    });
    if (moved) void this.poll();
  }

  private clearPoll(): void {
    if (this.timer !== null) this.clearTimer(this.timer);
    this.timer = null;
  }

  private schedule(): void {
    if (!this.running || this.pushOpen || this.timer !== null) return;
    this.timer = this.setTimer(() => {
      this.timer = null;
      void this.tick();
    }, this.pollMs);
  }

  private async tick(): Promise<void> {
    await this.poll();
    this.schedule();
  }

  /**
   * Runs the changes loop once, now (used on focus/after an action lands).
   *
   * ⚠ A REQUEST THAT ARRIVES MID-POLL RUNS AGAIN AFTERWARDS (#562). It used to
   * be handed the in-flight promise and nothing more — but that poll may already
   * have read `/changes`, so whatever moved since (the push that asked, the
   * action that just landed) went unread until something ELSE changed. With push
   * live there is no timer to catch it. That is how a stale appearance record
   * stayed on screen for the accessibility lane's full twenty seconds rather
   * than one round trip. Requests during one poll collapse into ONE re-run.
   */
  async poll(): Promise<void> {
    if (this.inFlight) {
      this.pollAgain = true;
      return this.inFlight;
    }
    this.inFlight = (async () => {
      do {
        this.pollAgain = false;
        await this.doPoll();
      } while (this.pollAgain);
    })().finally(() => {
      this.inFlight = null;
    });
    return this.inFlight;
  }

  /**
   * Whether a failure was the NETWORK or the SERVER saying no (#375 increment
   * 6).
   *
   * The header sentence turns these into opposite words — "Offline. Showing
   * what was loaded" against "cannot reach the server" — and the only place
   * that distinction is still available is HERE, where the exception object
   * still exists. One line down it is a string, and a string cannot be asked
   * what kind of error it was (core/store.ts SyncErrorKind).
   */
  private static errorKind(err: unknown): SyncErrorKind {
    return err instanceof JmapTransportError ? "transport" : "server";
  }

  private handleFatal(err: unknown): void {
    if (err instanceof JmapAuthError) {
      this.store.setConn("unauthenticated");
      return;
    }
    if (err instanceof JmapTransportError) {
      this.store.setConn("offline", err.message);
      return;
    }
    this.store.setConn("error", err instanceof Error ? err.message : String(err));
  }

  // -- reference data --------------------------------------------------------

  /** Mailboxes, labels and identities in one batch. Small, bounded sets: the
   * whole list is cheaper to re-read than to diff. */
  async loadReferenceData(): Promise<void> {
    const accountId = this.store.getState().accountId;
    // Taken as the read is SENT (#562): a write of this device's answered
    // after this point may not be in what comes back (core/prefsLedger.ts).
    const prefsReadAt = this.store.beginClientPreferencesRead();
    try {
      const results = await this.client.request([
        { name: "Mailbox/get", args: { accountId, ids: null }, id: "mb" },
        { name: "Label/get", args: { accountId, ids: null }, id: "lb" },
        { name: "Identity/get", args: { accountId, ids: null }, id: "id" },
        // Remembered remote-content grants (#202). In the SAME batch as the other
        // reference data because it is the same kind of thing: a small bounded set
        // the reading pane needs before it renders anything, cheaper to re-read
        // whole than to diff.
        { name: "RemoteAllow/get", args: { accountId, ids: null }, id: "ra" },
        // Saved signatures and templates (#56): compose needs them before it can
        // offer a picker, and they are the same kind of small bounded set.
        { name: "ComposeSnippet/get", args: { accountId, ids: null }, id: "cs" },
        // Saved searches / smart folders (#57): the SIDEBAR needs the pinned ones
        // before it renders, so this belongs with the reference data rather than
        // being fetched when someone opens a menu.
        { name: "SavedSearch/get", args: { accountId, ids: null }, id: "ss" },
        // The category-tab strip (#228): the message list cannot render its tabs
        // until it knows which are enabled and in what order, so it belongs with
        // the reference data rather than being fetched on first click.
        { name: "CategoryTab/get", args: { accountId, ids: null }, id: "ct" },
        // The desk's areas (#375/#442). Reference data in the strictest sense:
        // a handful of rows, every desk section is one of them, and the rail
        // cannot render before it knows them. Unlike the tabs these DO get a
        // cursor — an area can be renamed, reordered or destroyed from another
        // device, and each of those changes what this desk draws.
        { name: "Area/get", args: { accountId, ids: null }, id: "ar" },
        // Where the LINE is (#375). One row, and every section's contents
        // depend on it, so it is loaded with the areas rather than fetched when
        // something needs to render a threshold control.
        { name: "DeskPreferences/get", args: { accountId, ids: null }, id: "dp" },
        // How this device should LOOK (#375 increment 5). Reference data for
        // the same reason the areas are: the shell applies theme, density,
        // message-opens and the pane split before the first list is drawn, and
        // fetching it later would mean booting in one appearance and changing
        // to another in front of the user. `ids: null` returns all three
        // families and MATERIALISES them server-side, which is what guarantees
        // the web row exists before anything tries to update it.
        { name: "ClientPreferences/get", args: { accountId, ids: null }, id: "cp" },
        // THE OUTBOX (#653). Submissions used to reach the store only from this
        // tab's own sends and from `/changes` — and `/changes` needs a cursor,
        // which only a send in this tab ever set. So a reload emptied the Outbox
        // and the rail's count: a failed send vanished without the user acting
        // on it, and a Send Later could no longer be cancelled from the Outbox.
        // The query asks for exactly what the Outbox shows (`inOutbox`), the
        // get hydrates it through `#ids`, and the rest is paged below.
        ...outboxPageCalls(accountId, 0, "ob"),
      ]);
      const cursor: SyncCursor = { ...this.store.getState().cursor };
      /** The Outbox's first page: how far it reached, of how many, and the
       * state its query was answered at. Null until both calls succeeded. */
      let outbox: { reached: number; total: number; state: string } | null = null;
      let outboxQuery: QueryResult | null = null;
      for (const r of results) {
        if (r.error || !r.value) continue;
        if (r.id === "obq") {
          // Only a well-formed answer counts. A malformed one must not throw
          // here: that would take the mailboxes, labels and the rest of this
          // batch down with the Outbox.
          outboxQuery = outboxQueryOf(r.value);
          continue;
        }
        const res = r.value as unknown as GetResult<unknown>;
        if (r.id === "obg") {
          this.store.putSubmissions((res.list ?? []) as EmailSubmission[]);
          if (outboxQuery) {
            outbox = {
              reached: outboxQuery.position + outboxQuery.ids.length,
              total: outboxQuery.total,
              // The QUERY's state, not the get's: the query ran first, so a
              // submission made between the two is after this state and the
              // change loop will fetch it. Starting from the get's state would
              // step over it. (The server's queryState IS the EmailSubmission
              // state string — internal/api/submission.go submissionQuery.)
              state: outboxQuery.queryState || res.state,
            };
          }
          continue;
        }
        switch (r.id) {
          case "mb":
            this.store.putMailboxes(res.list as Mailbox[], true);
            cursor.Mailbox = res.state;
            break;
          case "lb":
            this.store.putLabels(res.list as Label[], true);
            cursor.Label = res.state;
            break;
          case "id":
            this.store.putIdentities(res.list as Identity[], true);
            cursor.Identity = res.state;
            break;
          case "ra":
            this.store.putRemoteAllows(res.list as RemoteAllow[]);
            break;
          case "cs":
            this.store.putComposeSnippets(res.list as ComposeSnippet[]);
            break;
          case "ss":
            this.store.putSavedSearches(res.list as SavedSearch[]);
            break;
          case "ct":
            // No cursor entry: CategoryTab is deliberately NOT in CHANGE_TYPES.
            // The strip is five rows that only this user edits, and a full
            // reference-data reload already refreshes it — adding it to the
            // change poller would be a per-tick round trip to observe something
            // that changes when the user clicks a setting in this very client.
            this.store.putCategoryTabs(res.list as CategoryTabState[]);
            break;
          case "ar":
            this.store.putAreas(res.list as Area[], true);
            cursor.Area = res.state;
            break;
          case "dp": {
            // A singleton: the list is one row, or empty on a server that has
            // never materialised it. Absent leaves deskPreferences null, which
            // is what stops the desk from inventing a line.
            const prefs = (res.list as DeskPreferences[])[0];
            if (prefs) this.store.setDeskPreferences(prefs);
            cursor.DeskPreferences = res.state;
            break;
          }
          case "cp":
            // Three rows (web/phone/desktop). The cursor IS established here:
            // without one the poll skips the type silently, and the poll is
            // still the fallback whenever the event stream is not connected.
            this.store.putClientPreferences((res.list as ClientPreferences[]) ?? [], prefsReadAt);
            cursor.ClientPreferences = res.state;
            break;
        }
      }
      // Only when unset: a cursor this tab already holds came from its own
      // send or from the change loop, and is at least as far along.
      if (outbox && !cursor.EmailSubmission) cursor.EmailSubmission = outbox.state;
      this.store.setCursor(cursor);
      this.store.setSyncError(null);
      // After the cursor is stored, so nothing the pages take time over can be
      // written back over a cursor that moved meanwhile.
      if (outbox && outbox.total > outbox.reached) {
        await this.loadOutboxRest(outbox.reached, outbox.total);
      }
    } catch (err) {
      this.handleFatal(err);
    }
  }

  /**
   * The Outbox beyond its first page (#653). The server caps a query page (and
   * says so with `limit`), so the bootstrap's `total` is the only thing that
   * can say there is more — this reads it rather than trusting one page to be
   * everything. In practice the Outbox is a handful of rows and this never
   * runs; it exists so that a large one is complete rather than silently cut.
   *
   * Stops when it has reached the total, when a page fails (what it has is
   * kept; the change loop carries on from the cursor), or when a page comes
   * back empty — the result shrank while it was being read, and asking again at
   * the same position would loop.
   */
  private async loadOutboxRest(position: number, total: number): Promise<void> {
    const accountId = this.store.getState().accountId;
    let at = position;
    let of = total;
    while (at < of) {
      const results = await this.client.request(outboxPageCalls(accountId, at, "ob"));
      const q = results.find((r) => r.id === "obq");
      const g = results.find((r) => r.id === "obg");
      const page = outboxQueryOf(q?.value ?? null);
      if (!page || !g?.value) return;
      this.store.putSubmissions((g.value as unknown as GetResult<EmailSubmission>).list ?? []);
      if (page.ids.length === 0) return;
      at = page.position + page.ids.length;
      of = page.total;
    }
  }

  // -- views -----------------------------------------------------------------

  /** Loads (or extends) the list for a view.
   *  - `more: true` grows the window by one page (the "load more" button);
   *  - `limit` pins the window explicitly (a refresh preserves what the user
   *    has already scrolled to);
   *  - `refresh: true` re-runs a list that is ALREADY ON SCREEN, so it does not
   *    raise `loading` (see refreshView). */
  async loadView(
    view: View,
    opts: { more?: boolean; limit?: number; refresh?: boolean } = {},
  ): Promise<void> {
    this.view = view;
    // Navigating to an ordinary view leaves the desk. It matters for the change
    // loop, not for this call: with a desk still recorded, every Email change
    // would re-run the desk's dozen queries for a screen nobody is looking at.
    this.desk = null;
    const st = this.store.getState();
    const key = viewKey(view);
    const plan = planFor(view, Object.values(st.mailboxes), st.labels);
    const sameView = st.list.key === key;
    const limit =
      opts.limit ??
      (opts.more && sameView
        ? st.list.limit + pageSizeFor(view)
        : sameView
          ? Math.max(st.list.limit, pageSizeFor(view))
          : pageSizeFor(view));

    if (plan.filters.length === 0) {
      this.store.setList({
        key,
        ids: [],
        total: 0,
        limit,
        loading: false,
        error: null,
        searchUnavailable: false,
        epoch: this.store.moveEpoch(),
      });
      return;
    }

    // Quiet only when the list being refreshed IS the one on screen: a refresh
    // that lands on a different key starts from an empty list, and an empty list
    // with `loading` unset draws "Nothing here" rather than "Loading…".
    const quiet = opts.refresh === true && sameView;
    // ONE SEARCH REPLACING ANOTHER KEEPS THE OLD ROWS UNTIL THE NEW ONES LAND
    // (#724). A fresh key starts from an empty list, which is right for "I went
    // somewhere else" and wrong for live search: every pause in typing is a new
    // query, and blanking the list for each would make it flicker to empty on
    // every keystroke. The rows shown are the previous search's, with `loading`
    // raised, until this one answers.
    const prev = st.list;
    const carry =
      !sameView && view.kind === "search" && prev.key.startsWith("search:")
        ? { ids: prev.ids, total: prev.total, searchCapped: prev.searchCapped, epoch: prev.epoch }
        : {};
    this.store.setList({ key, error: null, limit, ...carry, ...(quiet ? {} : { loading: true }) });
    const gen = ++this.listGen;
    this.listAbort?.abort();
    const abort = new AbortController();
    this.listAbort = abort;
    const stale = (): boolean => gen !== this.listGen;
    // Stamped BEFORE the request goes out, and handed to setList with the result
    // (#176): it is what decides whether an "answered" flag is older than the
    // query that came back, and therefore whether the query should already have
    // reflected it. Taken after the await it would always look newer, and every
    // optimistic answer would survive a result that disagreed with it.
    const queriedAt = Date.now();
    // The same instant, for moves (#685): a move that settles after this is
    // newer than the ids that come back, and the store keeps hiding it from them.
    const epoch = this.store.moveEpoch();

    // One batch: every Email/query, each immediately followed by the Email/get
    // that hydrates its ids through an RFC 8620 §3.7 result reference. A
    // three-account unified inbox is therefore ONE round trip, not six.
    const calls: Call[] = [];
    plan.filters.forEach((filter, i) => {
      calls.push({
        name: "Email/query",
        args: {
          accountId: st.accountId,
          filter,
          sort: plan.sort ?? DEFAULT_SORT,
          position: 0,
          limit,
          calculateTotal: true,
        },
        id: `q${i}`,
      });
      calls.push({
        name: "Email/get",
        args: {
          accountId: st.accountId,
          "#ids": { resultOf: `q${i}`, name: "Email/query", path: "/ids" },
          // Name the properties: omitting them selects RFC 8621 §4.2's DEFAULT
          // set, which includes the body — and a body costs the server a blob
          // decrypt + MIME parse PER MESSAGE. A list view shows none of it.
          properties: EMAIL_LIST_PROPERTIES,
        },
        id: `g${i}`,
      });
    });

    try {
      const results = await this.client.request(calls, undefined, abort.signal);
      if (stale()) return;
      const ids: Id[] = [];
      const emails: Email[] = [];
      let total = 0;
      let searchCapped: number | null = null;
      let searchUnavailable = false;
      let error: string | null = null;

      for (const r of results) {
        if (r.error) {
          if (r.error.type === "unsupportedFilter") searchUnavailable = true;
          else if (r.error.type !== "invalidResultReference") error = r.error.message;
          continue;
        }
        if (!r.value) continue;
        if (r.name === "Email/query") {
          const q = r.value as unknown as QueryResult;
          ids.push(...q.ids);
          total += q.total ?? q.ids.length;
          // The index's hit limit cut this search short (#722): `total` is a
          // lower bound, and the list has to say so rather than print it as
          // the number of matches.
          if (typeof q.searchCapped === "number") searchCapped = Math.max(searchCapped ?? 0, q.searchCapped);
          // The Email cursor rides the query state: both are the Email change
          // sequence (internal/api/email.go), so the first list establishes it.
          const cursor = this.store.getState().cursor;
          if (!cursor.Email) this.store.setCursor({ ...cursor, Email: q.queryState });
        } else if (r.name === "Email/get") {
          emails.push(...((r.value as unknown as GetResult<Email>).list ?? []));
        }
      }

      this.store.putEmails(emails);
      const st2 = this.store.getState();
      const unique = [...new Set(ids)];
      // MERGE IN THE ORDER THE VIEW ASKED FOR, because the next line WINDOWS the
      // result. A plan with N filters is N server-side queries, each correctly
      // ordered, and merging them by an unrelated key before slicing throws away
      // the server's choice of which rows belong on the page — the one part of
      // the ordering a client cannot reconstruct.
      //
      // It was harmless while every plan sorted by arrival, and the needs-a-
      // category queue is the first that does not: merged by receivedAt and cut
      // to `limit`, a two-account queue would keep the NEWEST N questions and
      // drop the most confident ones the server had gone to the trouble of
      // selecting.
      const byConfidence = plan.sort?.[0]?.property === "suggestionConfidence";
      // Best match (#724) is an order only the server can produce — it has no
      // client-side key to re-sort by — so the server's order is kept as is.
      const byRelevance = plan.sort?.[0]?.property === "relevance";
      if (!byRelevance) unique.sort((a, b) => {
        const ea = st2.emails[a];
        const eb = st2.emails[b];
        if (!ea || !eb) return 0;
        if (byConfidence) {
          // Mirrors store.topConfidence: -1 for "no open question", so mail the
          // system has said nothing about sorts last, as `NULLS LAST` does
          // server-side.
          const ca = maxConfidence(ea);
          const cb = maxConfidence(eb);
          if (ca !== cb) return cb - ca;
        }
        return eb.receivedAt.localeCompare(ea.receivedAt);
      });
      const windowed = unique.slice(0, limit);

      this.store.setList(
        {
          key,
          ids: windowed,
          total,
          limit,
          loading: false,
          error,
          searchUnavailable,
          searchCapped,
          epoch,
        },
        queriedAt,
      );
      this.store.setSyncError(null);
      await this.loadSnippets(plan, windowed, stale);
    } catch (err) {
      // Superseded: the newer load owns the list, and an abort is not an error.
      if (abort.signal.aborted || stale()) return;
      if (err instanceof JmapAuthError) {
        this.handleFatal(err);
        return;
      }
      this.store.setList({
        key,
        loading: false,
        error: err instanceof Error ? err.message : String(err),
      });
      this.store.setSyncError(
        err instanceof Error ? err.message : String(err),
        SyncEngine.errorKind(err),
      );
    }
  }

  /** Re-runs the current view's query, preserving the window the user has
   * scrolled to. This is the `cannotCalculateChanges` fallback (RFC 8620 §5.6),
   * and it runs only when the Email state has actually moved.
   *
   * ⚠ IT DOES NOT RAISE `loading` (#610 M4). A refresh is the list the user is
   * already looking at, re-asked; MessageList draws "Loading…" UNDER the rows
   * whenever `loading` is set, so every autosave of a draft — one Email change,
   * so one refresh — flashed that line beneath a list that had never stopped
   * being current. What the rows show until the answer lands is still true. */
  async refreshView(): Promise<void> {
    const st = this.store.getState();
    await this.loadView(this.view, {
      limit: Math.max(st.list.limit, pageSizeFor(this.view)),
      refresh: true,
    });
  }

  // -- the desk (#375) --------------------------------------------------------
  //
  // The desk is SEVERAL LISTS AT ONCE — the tray, an area per section, a drawer
  // when one is opened — so it cannot ride `list`, and each of them pages,
  // loads and fails on its own. What it must not become is a request per
  // section: the whole desk is ONE batch (RFC 8620 §3.2 + §3.7 result
  // references), exactly as a unified inbox is, so opening it is one round trip
  // however many areas the user has.
  //
  // None of the desk's filters is delta-stable — `Email/queryChanges` refuses
  // `inArea`/`unsorted`/`onDesk`/`onSurface`, because an area's claim or a
  // score can move with no Email change behind it — so the desk re-queries when
  // the Email or Area state moves. That is the same RFC 8620 §5.6 fallback
  // `refreshView` is, applied to a screenful of lists instead of one.

  /** The order the tray asks for. `receivedAt` rather than the default
   * `returnedAt`: this only decides WHICH rows are in the window; the strip
   * itself orders and groups by `flaggedAt`, which is not a server sort. */
  private static readonly TRAY_SORT = [{ property: "receivedAt" as const, isAscending: false }];

  /**
   * How many rows to ask for in one section: never fewer than it already holds,
   * so a refresh does not shrink a window the user has grown, and one page more
   * when `more` names this section.
   */
  private sectionLimit(key: string, page: number, more?: string | ReadonlySet<string>): number {
    const cur = this.store.getState().sections[key];
    const base = Math.max(cur?.limit ?? 0, page);
    const grow = typeof more === "string" ? more === key : (more?.has(key) ?? false);
    return grow ? base + page : base;
  }

  /**
   * Loads a desk tab: the counts, the ALL-MAIL count, the tray, and — on Review
   * first — every area's surface section plus Needs sorting.
   *
   * `rest` deliberately issues NO per-drawer query. A drawer is drawn closed,
   * from the summary's own count and nothing else; its contents are fetched when
   * someone opens it (loadDrawer). Fetching a page for every drawer up front
   * would be a dozen queries to render a dozen closed rows.
   *
   * That is also what makes this the TRAY PAGE's load (#375): `rest` at any
   * scope is the summary, the all-mail count and the tray, and nothing else —
   * one round trip for the whole screen.
   *
   * `opts.more` names the SECTION KEY to grow by one page — the desk has many
   * lists, so "load more" has to say which one.
   */
  async loadDesk(tab: DeskTab, scope: Id | null, opts: { more?: string } = {}): Promise<void> {
    // SINGLE-FLIGHT WITH ONE TRAILING RE-RUN (#665). Every caller used to start
    // its own request, so a burst of changes put a dozen overlapping desk loads
    // on the server, each slower than the last. Requests during a flight
    // collapse into one more load afterwards, with the LATEST tab and scope.
    const prev = this.desk;
    const sameDesk = prev !== null && prev.tab === tab && prev.scope === scope;
    // Keys carry the scope, so a pending "more" for another scope is meaningless.
    if (prev === null || prev.scope !== scope) this.deskMore.clear();
    if (opts.more) this.deskMore.add(opts.more);
    this.desk = { tab, scope };

    const live = this.deskChain;
    if (live && sameDesk) {
      live.again = true;
      return live.promise;
    }
    // A different desk: the old request is for a screen nobody is looking at.
    if (live) {
      live.superseded = true;
      live.abort?.abort();
    }
    const chain: NonNullable<typeof this.deskChain> = {
      promise: Promise.resolve(),
      again: false,
      superseded: false,
      abort: null,
    };
    chain.promise = (async () => {
      do {
        chain.again = false;
        const want = this.desk;
        if (!want) return;
        const more = new Set(this.deskMore);
        this.deskMore.clear();
        const abort = new AbortController();
        chain.abort = abort;
        await this.runDesk(want.tab, want.scope, more, ++this.deskGen, abort.signal);
      } while (chain.again && !chain.superseded);
    })().finally(() => {
      if (this.deskChain === chain) this.deskChain = null;
    });
    this.deskChain = chain;
    return chain.promise;
  }

  /** One desk load. `gen` is its generation: a response is applied only while
   * it is still the latest and the desk it was for is still the desk on screen. */
  private async runDesk(
    tab: DeskTab,
    scope: Id | null,
    moreKeys: ReadonlySet<string>,
    gen: number,
    signal: AbortSignal,
  ): Promise<void> {
    const stale = (): boolean =>
      gen !== this.deskGen ||
      this.desk === null ||
      this.desk.tab !== tab ||
      this.desk.scope !== scope;
    const st = this.store.getState();
    const accountId = st.accountId;

    // Forget every section belonging to another scope. Switching accounts must
    // not leave the previous account's rows on screen under the same area name
    // — that is mail attributed to an account it is not in.
    const prefix = sectionKeyPrefix(scope);
    this.store.dropSections((k) => !k.startsWith(prefix));

    const planned: { key: string; section: DeskSection; limit: number }[] = [];
    const tray: DeskSection = { kind: "tray", areaId: null };
    const trayKey = sectionKey(tray.kind, tray.areaId, scope);
    planned.push({ key: trayKey, section: tray, limit: this.sectionLimit(trayKey, TRAY_PAGE, moreKeys) });
    if (tab === "review") {
      for (const section of deskSections("review", Object.values(st.areas))) {
        const key = sectionKey(section.kind, section.areaId, scope);
        planned.push({ key, section, limit: this.sectionLimit(key, PAGE_SIZE, moreKeys) });
      }
    }

    // ONE PASS OR PER SECTION (#665). A server with `Area/getDesk` answers the
    // counts and every Review-first section from ONE resolution of every
    // message's area; asking it per section, as below, made it resolve them
    // again for each one — about a second apiece on a large mailbox. An older
    // server (no `getDesk` in its desk capability, or one that refused the
    // method) gets the per-section batch it always got.
    const snapshot = hasDeskSnapshot(st.session) && !this.noDeskSnapshot;
    // The sections Area/getDesk answers: everything planned except the tray,
    // which stays its own query — it is per MESSAGE, not collapsed, and cheap.
    const deskPlanned = snapshot ? planned.slice(1) : [];
    // One Email/get hydrates every section when their windows fit in what this
    // server takes in one /get; otherwise the ids are fetched in chunks after.
    const getMax = maxObjectsInGet(st.session);
    const oneGet = deskPlanned.reduce((n, p) => n + p.limit, 0) <= getMax;

    // THE BATCH, AS UNITS RATHER THAN A FLAT LIST. A unit is a group of calls
    // that must travel together, because a result reference can only name a
    // call in its own request (RFC 8620 §3.7): the two standalone counts are
    // one call each, every section is a query/get PAIR. Building it this way is
    // what lets sendDeskBatch split a big desk without having to re-derive
    // where the pair boundaries are from an index.
    const allMailUnit: Call[] = [
      {
        // THE ALL-MAIL COUNT (#375), for the rail's badge. It rides this
        // batch rather than being its own request because it is the same
        // question at the same scope and at the same instant as the summary
        // beside it — a count fetched separately would be a number from
        // another moment printed under the same heading.
        //
        // `limit: 1`, NOT 0. This server treats a limit of zero as NO LIMIT
        // (internal/store/jmap_email.go: `if q.Limit > 0` is what appends the
        // LIMIT clause), so `limit: 0` would ask it to return the id of every
        // message the user has. One row is the smallest window that is
        // certainly bounded; the number we want is `total`, which Email/query
        // computes either way.
        name: "Email/query",
        args: {
          accountId,
          // No `inMailbox`, so the server excludes the trash and nothing
          // else (QueryEmails only lifts that when the query names the trash
          // mailbox itself) — which is exactly what "all mail" means.
          filter: scope ? { inAccount: scope } : {},
          sort: DEFAULT_SORT,
          position: 0,
          limit: 1,
          calculateTotal: true,
        },
        id: "all",
      },
    ];
    const pairUnit = (p: (typeof planned)[number], i: number): Call[] => [
      {
        name: "Email/query",
        args: {
          accountId,
          filter: sectionFilter(p.section, scope),
          sort: p.section.kind === "tray" ? SyncEngine.TRAY_SORT : DEFAULT_SORT,
          position: 0,
          limit: p.limit,
          calculateTotal: true,
          // ROWS ARE THREADS (#375 increment 5, RFC 8621 §4.4.4): one row per
          // conversation, the newest message representing it, and `total` is
          // the collapsed count so the section's "showing N of M" counts the
          // same things the rows do.
          //
          // ⚠ EXCEPT THE TRAY, and that is not an oversight. A tray item is
          // the `$flagged` KEYWORD, which is per MESSAGE (#444) — collapsing
          // would hide a flagged reply behind an unflagged newer sibling and
          // the strip would age the wrong one. `flaggedAt` is what the strip
          // orders by, and it belongs to the message that was picked up.
          //
          // The server treats this as NOT delta-stable
          // (internal/api/email.go: `if q.CollapseThreads { stable = false }`),
          // which the desk already accepts — `Email/queryChanges` is
          // cannotCalculateChanges here by design and every section is
          // re-queried when the Email state moves.
          ...(p.section.kind === "tray" ? {} : { collapseThreads: true }),
        },
        id: `q${i}`,
      },
      {
        name: "Email/get",
        args: {
          accountId,
          "#ids": { resultOf: `q${i}`, name: "Email/query", path: "/ids" },
          properties: EMAIL_LIST_PROPERTIES,
        },
        id: `g${i}`,
      },
    ];

    // Marked loading BEFORE the request, so each section shows it is working
    // rather than showing its previous contents as if they were current.
    for (const p of planned) {
      this.store.setSection({ key: p.key, loading: true, error: null, limit: p.limit });
    }
    let units: Call[][];
    if (snapshot) {
      const deskUnit: Call[] = [
        {
          name: "Area/getDesk",
          args: {
            accountId,
            ...(scope ? { mailAccountId: scope } : {}),
            // No `threshold`: the STORED line, as for Area/getSummary below.
            // The sections ask in the order they are drawn; each window is the
            // one this section has grown to (sectionLimit), so `deskMore`
            // works exactly as it does per section.
            sections: deskPlanned.map((p) => ({ areaId: p.section.areaId, limit: p.limit })),
          },
          id: "desk",
        },
      ];
      if (deskPlanned.length > 0 && oneGet) {
        deskUnit.push({
          name: "Email/get",
          args: {
            accountId,
            // `*` maps over the sections and flattens their ids (RFC 8620
            // §3.7), so one call hydrates them all.
            "#ids": { resultOf: "desk", name: "Area/getDesk", path: "/sections/*/ids" },
            properties: EMAIL_LIST_PROPERTIES,
          },
          id: "dg",
        });
      }
      units = [deskUnit, allMailUnit, pairUnit(planned[0]!, 0)];
    } else {
      units = [
        [
          {
            name: "Area/getSummary",
            // No `threshold`: this asks for the STORED line. Supplying one would
            // make the answer a preview of a desk nobody is looking at.
            args: { accountId, ...(scope ? { mailAccountId: scope } : {}) },
            id: "sum",
          },
        ],
        allMailUnit,
        ...planned.map(pairUnit),
      ];
    }

    // Taken BEFORE the request leaves (#685). A move that settles while it is
    // out is newer than every id it brings back — the generation guard cannot
    // see that, because this load is still the latest when it lands.
    const epoch = this.store.moveEpoch();
    let results: CallResult[];
    try {
      results = await this.sendDeskBatch(units, signal);
    } catch (err) {
      // A superseded load is not an error, whatever it threw: the abort that
      // cancelled it, or a failure from a request nobody is waiting on.
      if (signal.aborted || stale()) return;
      if (err instanceof JmapAuthError) {
        this.handleFatal(err);
        return;
      }
      const message = err instanceof Error ? err.message : String(err);
      for (const p of planned) this.store.setSection({ key: p.key, loading: false, error: message });
      this.store.setSyncError(message, SyncEngine.errorKind(err));
      return;
    }

    // THE GENERATION GUARD (#665): a response that is no longer the latest is
    // dropped without touching the store, whatever order responses arrive in.
    if (signal.aborted || stale()) return;

    const loadedAt = Date.now();
    const emails: Email[] = [];
    const patches: (Partial<SectionState> & { key: string })[] = [];
    // A per-section error, so one failing area does not blank the rest of the
    // desk. `unsupportedFilter` is included on purpose: against a server that
    // predates the desk filters, an empty section would read as "no mail here",
    // which is the same wrong answer #20 shipped when a dropped condition
    // returned a superset. It must say it could not ask.
    let summaryError: string | null = null;
    // BOTH COUNTS ARE HELD UNTIL THE WHOLE RESPONSE IS READ, then written once.
    // They arrive in call order — the summary first, the all-mail total second
    // — and they are stored in ONE record (store.ts deskSummary), so writing it
    // the moment the summary was seen would file it with an all-mail count that
    // had not been read yet: a permanent `null` on the rail's badge for a call
    // that succeeded.
    let allMail: number | null = null;
    let summary: MailState["deskSummary"] = null;
    let desk: AreaGetDeskResult | null = null;
    let deskGetError: string | null = null;
    for (const r of results) {
      if (r.id === "all") {
        // A failed count is left null. The badge goes blank; nothing else on
        // the desk depends on it, so it is not worth an error of its own.
        if (r.error || !r.value) continue;
        const q = r.value as unknown as QueryResult;
        allMail = q.total ?? null;
        continue;
      }
      if (r.id === "sum" || r.id === "desk") {
        if (r.error || !r.value) {
          if (r.id === "desk" && r.error?.type === "unknownMethod") {
            // A server that advertised getDesk and then did not have it (a
            // proxy, a rollback). Stop asking, and load this desk the old way.
            this.noDeskSnapshot = true;
            return this.runDesk(tab, scope, moreKeys, gen, signal);
          }
          summaryError = r.error?.message ?? "the desk's counts did not come back";
          // Cleared rather than kept: the previous counts were taken at another
          // moment, and a header sentence is only worth anything if its numbers
          // are the ones the sections below it are showing.
          if (r.id === "desk") {
            for (const p of deskPlanned) patches.push({ key: p.key, loading: false, error: summaryError });
          }
          continue;
        }
        const res = r.value as unknown as AreaSummaryResult;
        summary = { scope, threshold: res.threshold, rows: res.list ?? [], state: res.state, allMail: null };
        if (r.id === "desk") desk = r.value as unknown as AreaGetDeskResult;
        continue;
      }
      if (r.id === "dg") {
        // `invalidResultReference` only ever means the getDesk beside it
        // failed, and that has already reported the real reason.
        if (r.error) {
          if (r.error.type !== "invalidResultReference") deskGetError = r.error.message;
        } else if (r.value) {
          emails.push(...((r.value as unknown as GetResult<Email>).list ?? []));
        }
        continue;
      }
      const target = planned[Number(r.id.slice(1))];
      if (!target) continue;
      if (r.name === "Email/query") {
        if (r.error) {
          patches.push({ key: target.key, loading: false, error: r.error.message });
          continue;
        }
        if (!r.value) continue;
        const q = r.value as unknown as QueryResult;
        patches.push({
          key: target.key,
          ids: q.ids,
          total: q.total ?? q.ids.length,
          loading: false,
          error: null,
          loadedAt,
          epoch,
        });
        // The Email cursor rides the query state, same rule as loadView: the
        // first list to come back establishes it.
        const cursor = this.store.getState().cursor;
        if (!cursor.Email) this.store.setCursor({ ...cursor, Email: q.queryState });
        continue;
      }
      if (r.error) {
        // `invalidResultReference` only ever means the query beside it failed,
        // and that query has already reported the real reason.
        if (r.error.type !== "invalidResultReference") {
          patches.push({ key: target.key, loading: false, error: r.error.message });
        }
        continue;
      }
      if (r.value) emails.push(...((r.value as unknown as GetResult<Email>).list ?? []));
    }

    if (desk) {
      // The windows that did not fit one Email/get, fetched now in chunks the
      // server takes. Only a desk grown far past its first page gets here.
      if (deskPlanned.length > 0 && !oneGet && deskGetError === null) {
        try {
          emails.push(...(await this.getEmailsChunked(desk.sections.flatMap((s) => s.ids), getMax, signal)));
        } catch (err) {
          if (signal.aborted || stale()) return;
          if (err instanceof JmapAuthError) {
            this.handleFatal(err);
            return;
          }
          deskGetError = err instanceof Error ? err.message : String(err);
        }
        if (signal.aborted || stale()) return;
      }
      const answered = new Map(desk.sections.map((s) => [s.areaId ?? "", s]));
      const gone = new Set(desk.notFound ?? []);
      for (const p of deskPlanned) {
        const s = answered.get(p.section.areaId ?? "");
        if (!s) {
          // Destroyed on another device since the areas were loaded: the area
          // change will redraw the desk without it. Until then the section
          // says why it is empty rather than claiming to be.
          const why = p.section.areaId !== null && gone.has(p.section.areaId)
            ? "This area no longer exists."
            : "The desk did not return this section.";
          patches.push({ key: p.key, loading: false, error: why });
          continue;
        }
        if (deskGetError !== null) {
          patches.push({ key: p.key, loading: false, error: deskGetError });
          continue;
        }
        patches.push({ key: p.key, ids: s.ids, total: s.total, loading: false, error: null, loadedAt, epoch });
      }
      // The Email cursor rides the sections' query state, as it does a query's.
      const cursor = this.store.getState().cursor;
      if (!cursor.Email && desk.queryState) this.store.setCursor({ ...cursor, Email: desk.queryState });
    }

    // Emails first, then the ids that point at them: a listener woken between
    // the two would otherwise be handed a section of ids the store cannot
    // render yet.
    this.store.putEmails(emails);
    for (const patch of patches) this.store.setSection(patch);
    this.store.setDeskSummary(summary === null ? null : { ...summary, allMail });
    this.store.setLastSyncAt(loadedAt);
    // The server ANSWERED and one call in the batch failed: a refusal, not a
    // dropped connection (a dropped one threw, above).
    this.store.setSyncError(summaryError, "server");
  }

  /**
   * Email/get for a known list of ids, at most `perGet` ids a call and at most
   * maxCallsInRequest calls a request (#665). Used only when a desk's windows
   * together exceed one Email/get; the order of the result is not meaningful.
   */
  private async getEmailsChunked(ids: Id[], perGet: number, signal: AbortSignal): Promise<Email[]> {
    const accountId = this.store.getState().accountId;
    const calls: Call[] = [];
    for (let i = 0; i < ids.length; i += perGet) {
      calls.push({
        name: "Email/get",
        args: { accountId, ids: ids.slice(i, i + perGet), properties: EMAIL_LIST_PROPERTIES },
        id: `c${calls.length}`,
      });
    }
    const out: Email[] = [];
    for (const r of await this.sendDeskBatch(calls.map((c) => [c]), signal)) {
      if (r.error) throw r.error;
      if (r.value) out.push(...((r.value as unknown as GetResult<Email>).list ?? []));
    }
    return out;
  }

  /**
   * Sends the desk's calls — as ONE request whenever the server will take them,
   * which is every realistic desk.
   *
   * It has to be a bound rather than an assumption. A tab is `2 + 2 calls per
   * section`, and this server refuses a request over `maxCallsInRequest` (64)
   * WHOLE, with a request-level 400 — so a user with more than about thirty
   * areas would not get a slow desk, they would get no desk at all, and every
   * section would report a transport error that named none of this.
   *
   * The input is the list of UNITS, not a flat call list, because the split has
   * to fall between them: an Email/get's result reference can only name a call
   * in its own batch (RFC 8620 §3.7), so a pair that straddled the boundary
   * would produce `invalidResultReference` for a query that succeeded.
   */
  private async sendDeskBatch(units: Call[][], signal?: AbortSignal): Promise<CallResult[]> {
    const calls = units.flat();
    // Never below 3: the summary plus one query/get pair is the smallest batch
    // that can say anything, and a server advertising less than that could not
    // serve an ordinary list view either.
    const max = Math.max(3, maxCallsInRequest(this.store.getState().session));
    if (calls.length <= max) return this.client.request(calls, undefined, signal);
    const batches: Call[][] = [];
    let current: Call[] = [];
    for (const unit of units) {
      if (current.length > 0 && current.length + unit.length > max) {
        batches.push(current);
        current = [];
      }
      current.push(...unit);
    }
    if (current.length > 0) batches.push(current);
    const out: CallResult[] = [];
    for (const batch of batches) {
      if (signal?.aborted) break;
      out.push(...(await this.client.request(batch, undefined, signal)));
    }
    return out;
  }

  /**
   * Loads (or extends) ONE drawer's contents — what opening a closed drawer
   * does. Requires a desk to be on screen: a drawer is scoped by the account
   * switcher, and querying one without knowing the scope would list mail from
   * accounts the user is not looking at.
   */
  async loadDrawer(areaId: Id, opts: { more?: boolean } = {}): Promise<void> {
    const desk = this.desk;
    if (!desk) return;
    const accountId = this.store.getState().accountId;
    const section: DeskSection = { kind: "drawer", areaId };
    const key = sectionKey(section.kind, section.areaId, desk.scope);
    const limit = this.sectionLimit(key, PAGE_SIZE, opts.more ? key : undefined);
    this.store.setSection({ key, loading: true, error: null, limit });
    const epoch = this.store.moveEpoch(); // before the request leaves (#685)

    let results;
    try {
      results = await this.client.request([
        {
          name: "Email/query",
          args: {
            accountId,
            filter: sectionFilter(section, desk.scope),
            sort: DEFAULT_SORT,
            position: 0,
            limit,
            calculateTotal: true,
            // A drawer's rows are threads too — see loadDesk's note. A drawer
            // is where a conversation is KEPT, and keeping it as eleven rows
            // was the single loudest thing wrong with a drawer full of a
            // mailing list.
            collapseThreads: true,
          },
          id: "q0",
        },
        {
          name: "Email/get",
          args: {
            accountId,
            "#ids": { resultOf: "q0", name: "Email/query", path: "/ids" },
            properties: EMAIL_LIST_PROPERTIES,
          },
          id: "g0",
        },
      ]);
    } catch (err) {
      if (err instanceof JmapAuthError) {
        this.handleFatal(err);
        return;
      }
      const message = err instanceof Error ? err.message : String(err);
      this.store.setSection({ key, loading: false, error: message });
      this.store.setSyncError(message, SyncEngine.errorKind(err));
      return;
    }

    const loadedAt = Date.now();
    let patch: (Partial<SectionState> & { key: string }) | null = null;
    const emails: Email[] = [];
    for (const r of results) {
      if (r.name === "Email/query") {
        if (r.error) {
          patch = { key, loading: false, error: r.error.message };
          continue;
        }
        if (!r.value) continue;
        const q = r.value as unknown as QueryResult;
        patch = {
          key,
          ids: q.ids,
          total: q.total ?? q.ids.length,
          loading: false,
          error: null,
          loadedAt,
          epoch,
        };
        continue;
      }
      if (r.error) {
        if (r.error.type !== "invalidResultReference" && !patch) {
          patch = { key, loading: false, error: r.error.message };
        }
        continue;
      }
      if (r.value) emails.push(...((r.value as unknown as GetResult<Email>).list ?? []));
    }
    this.store.putEmails(emails);
    if (patch) this.store.setSection(patch);
    this.store.setLastSyncAt(loadedAt);
  }

  /**
   * Re-runs the desk that is on screen, keeping every window the user has
   * grown and re-loading every drawer that is actually OPEN.
   *
   * A closed drawer is deliberately left alone: its row shows a count from the
   * summary, which this call refreshes anyway, so fetching its rows would be a
   * query per drawer to update something nobody can see. `loadedAt !== null` is
   * what tells the two apart — an empty open drawer and a never-opened one look
   * identical by ids alone.
   */
  async refreshDesk(): Promise<void> {
    const desk = this.desk;
    if (!desk) return;
    await this.loadDesk(desk.tab, desk.scope);
    await this.refreshOpenDrawers();
  }

  /**
   * Re-queries every open drawer, once for a burst of refreshes (#665).
   * loadDesk coalesces a burst into one trailing load, and every caller then
   * resumes at the same moment; without this each of them would walk the open
   * drawers again, one serial query per drawer per caller. Same single-flight
   * shape as loadDesk and poll(): callers during a pass get one trailing pass.
   */
  private refreshOpenDrawers(): Promise<void> {
    const live = this.drawerPass;
    if (live) {
      live.again = true;
      return live.promise;
    }
    const pass: { promise: Promise<void>; again: boolean } = { promise: Promise.resolve(), again: false };
    pass.promise = (async () => {
      do {
        pass.again = false;
        const desk = this.desk;
        if (!desk) return;
        const st = this.store.getState();
        const open = Object.values(st.areas)
          .map((a) => a.id)
          .filter((id) => st.sections[sectionKey("drawer", id, desk.scope)]?.loadedAt != null);
        for (const areaId of open) await this.loadDrawer(areaId);
      } while (pass.again);
    })().finally(() => {
      if (this.drawerPass === pass) this.drawerPass = null;
    });
    this.drawerPass = pass;
    return pass.promise;
  }

  /**
   * What the desk WOULD look like with the line somewhere else (#375): one
   * `Area/getSummary` at a hypothetical threshold.
   *
   * It STORES NOTHING. A preview is a question about a desk that does not exist
   * yet, and writing it into `deskSummary` would leave the header sentence
   * describing a threshold nobody has agreed to. The caller renders the returned
   * rows beside the real ones and throws them away.
   */
  async previewLine(threshold: number): Promise<AreaSummaryRow[] | null> {
    const scope = this.desk?.scope ?? null;
    try {
      const [r] = await this.client.request([
        {
          name: "Area/getSummary",
          args: {
            accountId: this.store.getState().accountId,
            ...(scope ? { mailAccountId: scope } : {}),
            threshold,
          },
          id: "sum",
        },
      ]);
      if (!r || r.error || !r.value) return null;
      return (r.value as unknown as AreaSummaryResult).list ?? [];
    } catch (err) {
      // A preview is disposable, but being logged out is not: surface that the
      // same way every other call does, then answer "no preview".
      if (err instanceof JmapAuthError) this.handleFatal(err);
      return null;
    }
  }

  /**
   * WHAT THE CURRENT LINE HAS ACTUALLY DONE OVER THE LAST WEEK (#374): how much
   * of the week's mail it put on the surface, and how much it kept.
   *
   * Two `Email/query` counts IN ONE REQUEST, over the identical window, differing
   * only in `onSurface`. One request rather than two because the pair is a single
   * sentence: two round trips could straddle a message arriving, and the sentence
   * would then add up to a week that never happened.
   *
   * `limit: 0` + `calculateTotal: true` is the count WITHOUT the ids — the row
   * prints two numbers, and a page of ids would be a bigger answer than the
   * question and a WRONG one, since a page's length is bounded by the page (#155's
   * "a lower bound reads as a total"). `total` is read as `unknown` because
   * `QueryResult` declares it required while `calculateTotal` is what makes a
   * server send it; an absent one yields null rather than NaN.
   *
   * NULL IS THE WHOLE PAIR FAILING, and it is all-or-nothing on purpose: one half
   * of the split is not a fact about the line, it is half a fact.
   *
   * SCOPED TO THE DESK ON SCREEN, like previewLine — an account-scoped desk asks
   * about that account's week, so the sentence describes the desk the user is
   * looking at rather than the login as a whole.
   */
  async lineWeekCounts(
    after: string,
  ): Promise<{ review: number; drawers: number } | null> {
    const accountId = this.store.getState().accountId;
    const scope = this.desk?.scope ?? null;
    const base = {
      onDesk: true,
      after,
      ...(scope ? { inAccount: scope } : {}),
    };
    try {
      const results = await this.client.request([
        {
          name: "Email/query",
          args: {
            accountId,
            filter: { ...base, onSurface: true },
            limit: 0,
            calculateTotal: true,
          },
          id: "surface",
        },
        {
          name: "Email/query",
          args: {
            accountId,
            filter: { ...base, onSurface: false },
            limit: 0,
            calculateTotal: true,
          },
          id: "drawer",
        },
      ]);
      const total = (id: string): number | null => {
        const r = results.find((x) => x.id === id);
        if (!r || r.error || !r.value) return null;
        const n = (r.value as { total?: unknown }).total;
        return typeof n === "number" ? n : null;
      };
      const review = total("surface");
      const drawers = total("drawer");
      if (review === null || drawers === null) return null;
      return { review, drawers };
    } catch (err) {
      if (err instanceof JmapAuthError) this.handleFatal(err);
      return null;
    }
  }

  /** Highlighted fragments for the page just loaded (RFC 8621 §5). */
  private async loadSnippets(
    plan: ReturnType<typeof planFor>,
    ids: Id[],
    stale: () => boolean = () => false,
  ): Promise<void> {
    const filter = snippetFilter(plan);
    if (!filter || ids.length === 0) return;
    try {
      const res = await this.client.searchSnippetGet({
        accountId: this.store.getState().accountId,
        filter,
        emailIds: ids.slice(0, MAX_SNIPPETS),
      });
      // A later search's highlights for the same message must not be replaced
      // by this older query's (#724).
      if (stale()) return;
      this.store.putSnippets((res.list ?? []) as SearchSnippet[]);
    } catch {
      // Snippets are decoration; a server without search must not break the list.
    }
  }

  /**
   * Loads a thread and every Email in it (the reading pane).
   *
   * `openedId` is the message the pane is showing. Thread/get leaves trashed
   * messages out of a conversation, so a message opened from Trash (or from an
   * `is:trashed` search) is not among the ids the Email/get below asks for,
   * and its body never arrived: the pane showed its siblings, or nothing
   * (#630). When the batch did not return it, it is fetched by id, with its
   * body. That second request is only ever paid for a message the conversation
   * left out.
   */
  async loadThread(threadId: Id, openedId?: Id, opts: { open?: boolean } = {}): Promise<void> {
    // ONE DECRYPT PER OPEN (#47). Opening a message mounts the list route AND the
    // reading pane, and each used to ask for the thread's body on its own: two
    // identical Email/get calls, and for an encrypted message two decryptions
    // (two `body` audit rows a few ms apart). An OPEN load joins one already in
    // flight for the same thread instead of issuing another. Only open-initiated
    // loads share: a reload after a mutation (release, key decision, invitation
    // reply) must see the server's state AFTER the mutation, so it always asks.
    // Nothing is retained once the request settles: the map holds the promise
    // only while it is in flight, never a result.
    let got: Set<Id> | null;
    const shared = opts.open ? this.openLoads.get(threadId) : undefined;
    if (shared) {
      got = await shared;
    } else {
      const load = this.fetchThread(threadId);
      if (opts.open) {
        this.openLoads.set(threadId, load);
        void load.finally(() => {
          if (this.openLoads.get(threadId) === load) this.openLoads.delete(threadId);
        });
      }
      got = await load;
    }
    if (got !== null && openedId !== undefined && !got.has(openedId)) {
      await this.loadEmails([openedId], { body: true });
    }
  }

  /** Open-initiated thread loads in flight, by thread id (see loadThread). */
  private readonly openLoads = new Map<Id, Promise<Set<Id> | null>>();

  /** The single Thread/get + Email/get batch. The ids the Email/get returned, or
   * null when the request itself failed. Never rejects. */
  private async fetchThread(threadId: Id): Promise<Set<Id> | null> {
    const accountId = this.store.getState().accountId;
    try {
      const results = await this.client.request([
        { name: "Thread/get", args: { accountId, ids: [threadId] }, id: "t" },
        {
          name: "Email/get",
          args: {
            accountId,
            "#ids": { resultOf: "t", name: "Thread/get", path: "/list/*/emailIds" },
            // The reading pane: the full body and the recipient lists a
            // reply/reply-all needs. This is the ONE call that pays for the
            // server's blob reads, and it pays for exactly the thread it opened.
            properties: EMAIL_BODY_PROPERTIES,
            fetchTextBodyValues: true,
            fetchHTMLBodyValues: true,
            maxBodyValueBytes: MAX_BODY_VALUE_BYTES,
          },
          id: "e",
        },
      ]);
      const got = new Set<Id>();
      for (const r of results) {
        if (r.error || !r.value) continue;
        if (r.name === "Thread/get") {
          this.store.putThreads((r.value as unknown as GetResult<Thread>).list ?? []);
        } else {
          const list = (r.value as unknown as GetResult<Email>).list ?? [];
          for (const e of list) got.add(e.id);
          this.store.putEmails(list);
        }
      }
      return got;
    } catch (err) {
      if (err instanceof JmapAuthError) this.handleFatal(err);
      return null;
    }
  }

  /**
   * Fetches specific messages by id, in ONE batch (#448).
   *
   * WHO NEEDS THIS AND WHY IT IS NOT `loadView`. Two surfaces name messages that
   * no query returned:
   *
   *   * the OUTBOX lists `EmailSubmission` objects and has to print each one's
   *     recipients and subject — the submission carries an `emailId` and nothing
   *     else, and `to` is not even in EMAIL_LIST_PROPERTIES, so the row cannot
   *     name anybody until this has run;
   *   * RESUMING A DRAFT (`?compose=draft:<id>`) needs the body and the
   *     attachments of one message the composer was handed by id.
   *
   * ONE REQUEST FOR THE WHOLE LIST, never one per row: the Outbox is small but a
   * request per row is the shape that stops being small quietly.
   *
   * `body: true` asks for the reading-pane property set plus the decoded body
   * values — what an editable draft needs. Without it the call takes the list
   * properties plus the recipient lists, which is the cheapest thing that can
   * answer "who is this going to".
   *
   * It does NOT touch `this.view` or `this.desk`: these messages are not a list,
   * and clearing the desk here would make the change loop believe no desk is on
   * screen while one plainly is (see loadView's own note).
   */
  /**
   * Resolves with the ids the server said it does NOT have — only what it said —
   * and whether the read FAILED. The two are different facts and the composer
   * acts on each (#612): "no such message" is final and becomes "this draft is
   * gone"; "we could not ask" (no session yet, the network) is asked again.
   */
  async loadEmails(
    ids: Id[],
    opts: { body?: boolean } = {},
  ): Promise<{ notFound: Id[]; failed: boolean }> {
    if (ids.length === 0) return { notFound: [], failed: false };
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.emailGet({
        accountId,
        ids,
        properties: opts.body
          ? EMAIL_BODY_PROPERTIES
          : // `to`/`cc`/`bcc` are body-only server-side (they need the blob
            // parsed), and the Outbox's whole row is who a message is going to,
            // so they are named explicitly rather than by taking the full body
            // set and paying for the bodies as well.
            [...EMAIL_LIST_PROPERTIES, "to", "cc", "bcc"],
        ...(opts.body
          ? {
              fetchTextBodyValues: true,
              fetchHTMLBodyValues: true,
              maxBodyValueBytes: MAX_BODY_VALUE_BYTES,
            }
          : {}),
      });
      this.store.putEmails(res.list ?? []);
      return { notFound: res.notFound ?? [], failed: false };
    } catch (err) {
      if (err instanceof JmapAuthError) this.handleFatal(err);
      // Anything else is left to the caller's own emptiness: the Outbox row says
      // "not loaded", which is true, rather than a toast about a read it will
      // retry on the next change.
      return { notFound: [], failed: true };
    }
  }

  /** Fetches every id matching the current view's query — what "select all
   * matching this search" needs (spec §12). Capped: a bulk action over an
   * unbounded set is a foot-gun, so the caller is told when it truncated. */
  async allMatchingIds(
    view: View,
    cap = 1000,
    // Where in the query's order to start (#708). Only meaningful for a view
    // that is ONE query (a search): a view fanned out over several accounts
    // merges its answers, so a shared offset into them would skip or repeat.
    position = 0,
  ): Promise<{ ids: Id[]; total: number }> {
    const st = this.store.getState();
    const plan = planFor(view, Object.values(st.mailboxes), st.labels);
    if (plan.filters.length === 0) return { ids: [], total: 0 };
    const calls: Call[] = plan.filters.map((filter, i) => ({
      name: "Email/query",
      args: {
        accountId: st.accountId,
        filter,
        // returnedAt, not receivedAt (#152): mail coming back from a snooze sorts
        sort: DEFAULT_SORT,
        position,
        limit: cap,
        calculateTotal: true,
        // Bulk enumeration must reach every match, not the search index's first
        // 500 hits (#769). Ordinary list queries leave this off: a larger index
        // limit costs time that a page of results does not need.
        searchLimit: BULK_SEARCH_LIMIT,
      },
      id: `q${i}`,
    }));
    const results = await this.client.request(calls);
    const ids: Id[] = [];
    let total = 0;
    for (const r of results) {
      // A refused query is not "nothing matches": the caller would report an
      // empty set (or select a part of one) as if it were the answer. Search
      // turned off answers `unsupportedFilter` (config.SearchDisabled).
      if (r.error) throw r.error;
      if (!r.value) continue;
      const q = r.value as unknown as QueryResult;
      ids.push(...q.ids);
      total += q.total ?? q.ids.length;
    }
    return { ids: [...new Set(ids)].slice(0, cap), total };
  }

  // -- the changes loop ------------------------------------------------------

  private async doPoll(): Promise<void> {
    const st = this.store.getState();
    if (!st.accountId || st.conn === "unauthenticated") return;

    const types: ChangeType[] = [
      "Email",
      "Mailbox",
      "Label",
      "Identity",
      "EmailSubmission",
      // The desk (#375). Both are polled like everything else rather than being
      // refreshed only when this client edits them: an area renamed or the line
      // moved on another device changes what this desk draws, and the change
      // log is the only thing that says so.
      "Area",
      "DeskPreferences",
      // Appearance synced between devices (#375 increment 5). Pushed since #474
      // as well as polled — the poll is what keeps it arriving while the event
      // stream is down, exactly as for every type above it.
      "ClientPreferences",
    ];
    const calls: Call[] = [];
    for (const t of types) {
      const since = st.cursor[t];
      if (!since) continue; // no cursor yet: the next full load establishes one
      calls.push({
        name: `${t}/changes`,
        args: { accountId: st.accountId, sinceState: since },
        id: t,
      });
    }
    if (calls.length === 0) return;

    let results;
    try {
      results = await this.client.request(calls);
      if (st.conn !== "online") this.store.setConn("online");
      this.store.setSyncError(null);
      // A ROUND TRIP HAPPENED, whatever it found. "Synced 40 s ago" is a
      // statement about contact with the server, not about mail moving — a
      // quiet mailbox must not look like a stalled one.
      this.store.setLastSyncAt(Date.now());
    } catch (err) {
      if (err instanceof JmapAuthError) {
        this.handleFatal(err);
        return;
      }
      // A transport failure is not fatal: stay on the cached data, mark offline,
      // and try again on the next tick.
      this.store.setConn("offline");
      this.store.setSyncError(
        err instanceof Error ? err.message : String(err),
        SyncEngine.errorKind(err),
      );
      return;
    }

    const cursor: SyncCursor = { ...this.store.getState().cursor };
    const emailIds = new Set<Id>();
    let emailsMoved = false;
    let refreshLabels = false;
    let refreshIdentities = false;
    let refreshMailboxes = false;
    /** An area was created, renamed, reordered or destroyed — which changes
     * which sections the desk has and which mail each one holds. */
    let areasMoved = false;
    /** The desk record moved — in practice, THE LINE. It decides which side of
     * itself every message is on, so a line moved on another device has to
     * re-split the sections here, not merely update the stored value. */
    let prefsMoved = false;
    /** This client family's appearance moved — theme, density, how a message
     * opens, the pane split. It changes nothing about which mail is where, so
     * unlike `prefsMoved` it must NOT re-run the desk: it is re-read and the
     * shell applies it. */
    let clientPrefsMoved = false;
    const submissionIds = new Set<Id>();
    /** Destroyed Email ids, removed only AFTER the list has been re-run (#610). */
    const destroyedEmails: Id[] = [];

    for (const r of results) {
      const type = r.id as ChangeType;
      if (r.error) {
        if (r.error instanceof JmapMethodError && r.error.type === "cannotCalculateChanges") {
          // The designed fallback: drop the cursor and re-establish it from a
          // full read of that type.
          cursor[type] = null;
          if (type === "Email") emailsMoved = true;
          if (type === "Mailbox") refreshMailboxes = true;
          if (type === "Label") refreshLabels = true;
          if (type === "Identity") refreshIdentities = true;
          if (type === "Area") areasMoved = true;
          if (type === "DeskPreferences") prefsMoved = true;
          if (type === "ClientPreferences") clientPrefsMoved = true;
        }
        continue;
      }
      if (!r.value) continue;
      const ch = r.value as unknown as ChangesResult;
      cursor[type] = ch.newState;
      const touched = [...ch.created, ...ch.updated];
      switch (type) {
        case "Email":
          for (const id of touched) emailIds.add(id);
          destroyedEmails.push(...ch.destroyed);
          if (touched.length > 0 || ch.destroyed.length > 0) {
            emailsMoved = true;
            // Unread counts hang off emails, but the server appends no Mailbox
            // change row for a flag flip — so refresh the (tiny) mailbox list.
            refreshMailboxes = true;
          }
          break;
        case "Mailbox":
          if (touched.length > 0 || ch.destroyed.length > 0) refreshMailboxes = true;
          if (ch.destroyed.length > 0) this.store.removeMailboxes(ch.destroyed);
          break;
        case "Label":
          if (touched.length > 0 || ch.destroyed.length > 0) refreshLabels = true;
          break;
        case "Identity":
          if (touched.length > 0 || ch.destroyed.length > 0) refreshIdentities = true;
          break;
        case "EmailSubmission":
          for (const id of touched) submissionIds.add(id);
          break;
        case "Area":
          // Re-read the WHOLE list rather than the touched ids: it is a handful
          // of rows, positions move together when one area is reordered, and a
          // partial merge would leave the desk drawing the old order.
          if (touched.length > 0 || ch.destroyed.length > 0) areasMoved = true;
          if (ch.destroyed.length > 0) this.store.removeAreas(ch.destroyed);
          break;
        case "DeskPreferences":
          if (touched.length > 0 || ch.destroyed.length > 0) prefsMoved = true;
          break;
        case "ClientPreferences":
          if (touched.length > 0 || ch.destroyed.length > 0) clientPrefsMoved = true;
          break;
        default:
          break;
      }
    }
    this.store.setCursor(cursor);

    const follow: Call[] = [];
    const accountId = this.store.getState().accountId;
    if (emailIds.size > 0) {
      follow.push({
        name: "Email/get",
        args: {
          accountId,
          ids: [...emailIds].slice(0, 500),
          // Re-hydrating up to 500 changed messages on every poll: this MUST be
          // the cheap property set (see loadView).
          properties: EMAIL_LIST_PROPERTIES,
        },
        id: "email",
      });
    }
    if (submissionIds.size > 0) {
      follow.push({
        name: "EmailSubmission/get",
        args: { accountId, ids: [...submissionIds] },
        id: "sub",
      });
    }
    if (refreshMailboxes) {
      follow.push({ name: "Mailbox/get", args: { accountId, ids: null }, id: "mb" });
    }
    if (refreshLabels) {
      follow.push({ name: "Label/get", args: { accountId, ids: null }, id: "lb" });
    }
    if (refreshIdentities) {
      follow.push({ name: "Identity/get", args: { accountId, ids: null }, id: "idn" });
    }
    if (areasMoved) {
      follow.push({ name: "Area/get", args: { accountId, ids: null }, id: "ar" });
    }
    if (prefsMoved) {
      follow.push({ name: "DeskPreferences/get", args: { accountId, ids: null }, id: "dp" });
    }
    if (clientPrefsMoved) {
      follow.push({ name: "ClientPreferences/get", args: { accountId, ids: null }, id: "cp" });
    }

    if (follow.length > 0) {
      // The tick for THIS request, not the /changes before it: the follow-up
      // `/get` is what reads the record, and a write of this device's answered
      // after it is sent may not be in it (#562, core/prefsLedger.ts). This is
      // the read the accessibility lane caught — started by the push for the
      // user's first choice, answered after their second.
      const prefsReadAt = this.store.beginClientPreferencesRead();
      try {
        const results2 = await this.client.request(follow);
        const cursor2: SyncCursor = { ...this.store.getState().cursor };
        for (const r of results2) {
          if (r.error || !r.value) continue;
          const res = r.value as unknown as GetResult<unknown>;
          switch (r.id) {
            case "email":
              this.store.putEmails(res.list as Email[]);
              if (!cursor2.Email) cursor2.Email = res.state;
              break;
            case "sub":
              this.store.putSubmissions(res.list as EmailSubmission[]);
              if (!cursor2.EmailSubmission) cursor2.EmailSubmission = res.state;
              break;
            case "mb":
              this.store.putMailboxes(res.list as Mailbox[], true);
              cursor2.Mailbox = res.state;
              break;
            case "lb":
              this.store.putLabels(res.list as Label[], true);
              cursor2.Label = res.state;
              break;
            case "idn":
              this.store.putIdentities(res.list as Identity[], true);
              cursor2.Identity = res.state;
              break;
            case "ar":
              this.store.putAreas(res.list as Area[], true);
              cursor2.Area = res.state;
              break;
            case "dp": {
              const prefs = (res.list as DeskPreferences[])[0];
              if (prefs) this.store.setDeskPreferences(prefs);
              cursor2.DeskPreferences = res.state;
              break;
            }
            case "cp":
              this.store.putClientPreferences(
                (res.list as ClientPreferences[]) ?? [],
                prefsReadAt,
              );
              cursor2.ClientPreferences = res.state;
              break;
          }
        }
        this.store.setCursor(cursor2);
      } catch (err) {
        if (err instanceof JmapAuthError) this.handleFatal(err);
      }
    }

    // This client's filters are never delta-stable (see the header comment),
    // so Email/queryChanges would always answer cannotCalculateChanges and the
    // list is re-run instead — but ONLY when the Email state actually moved.
    //
    // An AREA or DESK-PREFERENCES change re-runs the desk too, and only the
    // desk: none of the desk's filters is delta-stable, and either one moves
    // mail between sections with no Email change behind it — an area gaining a
    // feed, or the LINE moving on another device, which re-splits every section
    // at once. Neither is a reason to re-run an ordinary view, which has no idea
    // areas or the line exist.
    //
    // ⚠ DESTROYED IDS LEAVE THE LIST AFTER THE RE-RUN, NOT BEFORE IT (#610 M1).
    // Editing a draft is a create AND a destroy in one Email/set (RFC 8621 §4.6
    // — see MailActions.saveDraft), so every autosave arrives here as one
    // `created` and one `destroyed` in the same response. Only a query result
    // (setList) ever puts an id INTO a list; removing the old id up front left
    // the draft's row absent for two round trips — this Email/get, then the
    // query — which is the row the user watched vanish and reappear every
    // fifteen seconds while typing, and the gap the cursor repair (ListRoute)
    // then fell through. Removed after the re-run, the old row is replaced by
    // the new one in a single render. The cost is that a message destroyed
    // elsewhere lingers for those same round trips, still showing what it was.
    //
    // `finally`, because a re-run that fails must still let the destroyed go:
    // the alternative is a row naming a message that no longer exists.
    try {
      if (emailsMoved || areasMoved || prefsMoved) {
        if (this.desk) await this.refreshDesk();
        else if (emailsMoved) await this.refreshView();
      }
    } finally {
      if (destroyedEmails.length > 0) this.store.removeEmails(destroyedEmails);
    }
  }
}
