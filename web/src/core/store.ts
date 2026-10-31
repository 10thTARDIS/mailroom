// SPDX-License-Identifier: AGPL-3.0-only

// MailStore — the client-core's normalized cache and the home of OPTIMISTIC
// LOCAL STATE. DOM-free and framework-agnostic (spec §12): the web app binds to
// it with useSyncExternalStore, and a native shell can bind to it however it
// likes.
//
// ---------------------------------------------------------------------------
// Why there is an "overlay" and not just a mutable copy of the server's data
// ---------------------------------------------------------------------------
// Star/read/archive/trash/label must apply INSTANTLY and survive a refetch.
//
// ⚠ THIS USED TO SAY Email/query and Email/get kept showing the OLD placement
// (inbox) until the IMAP writeback actually landed, because provenance rows
// don't move synchronously. That is no longer true of this server and may
// never have been true of the exact mechanism described: a move/trash writes
// `messages.moved_mailbox_id` in the SAME transaction as Email/set, and both
// `Email/get`'s mailboxIds and `Email/query`'s `inMailbox` filter
// (internal/store/jmap_email.go, `inMailboxSQL`/`effectiveMembershipSQL`)
// treat `moved_mailbox_id` as canonical and override provenance with it —
// specifically to stop an archived message reappearing in the inbox. Query
// also applies `trashed_at IS NULL` outside a Trash-scoped query
// (`QueryEmails`). So a refetch after the server has actually answered the
// Email/set call already reflects the move/trash correctly.
//
// What overwrite-on-refetch still cannot survive is the gap BEFORE that
// answer comes back: the UI must not wait a network round trip to show star/
// read/archive/trash/label, and a subsequent sync pass's remote-wins keyword
// merge (spec §4a: OR across provenances) can still momentarily disagree with
// a keyword the user just cleared until the writeback intent actually reaches
// the provider. That is what the overlay is for.
//
// So the store keeps two layers:
//
//   * `emails` — exactly what the server said (never edited locally).
//   * `overlay` — per-message local intent: keyword deltas and a local move.
//     The UI always reads through `effectiveEmail()`, which applies the overlay.
//
// An overlay entry is retired only when the SERVER AGREES with it (keyword
// present/absent as desired; the message actually gone from the source folder).
// That is self-healing in both directions: a successful action eventually
// disappears from the overlay; an action the server rejected is rolled back
// explicitly by the action queue; and an action whose IMAP writeback is still
// pending stays applied locally, which is exactly what the user expects.
//
// The overlay is persisted (KeyValueStorage) so a reload does not resurrect
// archived mail.

import type {
  Area,
  AreaSummaryRow,
  ClientPreferences,
  DeskPreferences,
  Email,
  EmailSubmission,
  Id,
  Identity,
  ComposeSnippet,
  Label,
  Mailbox,
  RemoteAllow,
  CategoryTabState,
  SavedSearch,
  SuggestedLabel,
  Thread,
} from "./jmap/types";
import { KEYWORD_FLAGGED, KEYWORD_SEEN } from "./jmap/types";
import type { JmapSession, SyncCursor } from "./session";
import { emptySyncCursor } from "./session";
import type { KeyValueStorage } from "./storage";
import type { QueuedAction } from "./queue";
import type { Successors } from "./draftLineage";
import { recordSuccessor } from "./draftLineage";
import type { PrefsPatch } from "./prefsLedger";
import { PrefsLedger } from "./prefsLedger";

/** A local move: the mailbox ROLE the user moved this message to. Mailroom's
 * Email/set expresses archive/trash as a role move, so the overlay does too. */
/** "junk" joined with #59: blocking a sender can move their mail INTO Junk, an
 * ordinary move to the junk-role folder (MailActions.moveToJunk). */
export type LocalMove = "archive" | "trash" | "inbox" | "junk";

export interface EmailOverlay {
  /** Desired keyword presence, e.g. {"$seen": true, "$flagged": false}. */
  keywords: Record<string, boolean>;
  /** The role this message was locally moved to, if any. */
  move: LocalMove | null;
  /** Desired pin state (#51), or undefined when this overlay says nothing about
   * the pin. Pin is NOT a keyword — it is its own Email property — so it cannot
   * ride the keywords map and needs its own slot. */
  pinned?: boolean | undefined;
  /** Desired wake time (#51): an ISO timestamp to snooze until, `null` to
   * un-snooze, `undefined` when this overlay says nothing about the snooze.
   * Three states, so it cannot collapse into a boolean the way the pin does. */
  snoozedUntil?: string | null | undefined;
  /** An importance correction the user made that the server has not reflected
   * back yet (#154). Unlike the others this is not a property patch — it goes
   * through Triage/correct, and what returns is a RE-SCORED model rather than
   * the value we sent, so it is retired on the next score rather than by
   * equality. */
  important?: boolean | undefined;
  /** True once the user has ANSWERED this message's open questions (#176) —
   * either by labelling it or by dismissing them — and the server has not
   * reflected that back yet.
   *
   * One flag for both gestures, because the queue only cares about one thing:
   * is this message still asking? A label and a dismissal clear it identically
   * server-side (ApplyEmailUpdate → ClearLabelSuggestions, and
   * DismissCategoryQuestions), so splitting it here would be a distinction with
   * no consequence. Retired when the server's own suggestedLabels comes back
   * empty — the same "pending until the server agrees" rule as the pin and the
   * snooze. */
  answered?: boolean | undefined;
  /** Desired presence of labels applied BY ID (#704): `{labelId: true}` to
   * add, `false` to remove. This is how a LOCAL-ONLY label — every area's own
   * label (ADR-031) — is applied, because it has no keyword atom to ride the
   * `keywords` map above. Retired, like the keywords, the moment the server's
   * own `labelIds` agrees. Absent (or empty) when nothing is pending. */
  labels?: Record<Id, boolean> | undefined;
  updatedAt: number;
}

/** Whether an overlay entry still carries a label-by-id intent (#704). One
 * helper because every "is this entry empty now?" check has to ask it, and a
 * check that forgot would drop an in-flight re-file along with whatever else
 * it was retiring. */
function hasLabelIntent(ov: EmailOverlay): boolean {
  return !!ov.labels && Object.keys(ov.labels).length > 0;
}

export interface ListState {
  /** The QueryPlan key this list belongs to. */
  key: string;
  ids: Id[];
  total: number;
  /** How many messages the client has asked the server for so far. */
  limit: number;
  loading: boolean;
  error: string | null;
  /** True when the view needed the search index and the server has none. */
  searchUnavailable: boolean;
  /** The search index's hit limit when it cut this search short (#722, #724),
   * else null. While set, `total` is a LOWER BOUND — "at least N" — and must
   * never be shown as the number of matches. */
  searchCapped: number | null;
  /** The store's `moveEpoch` when the query behind `ids` was SENT (#685) —
   * what decides whether a settled move is newer than these ids. */
  epoch: number;
}

/**
 * One of the desk's named lists (#375): the in-tray strip, an area's surface
 * section, or an open drawer.
 *
 * A SECTION IS NOT `list`. The desk shows several of them at once, so they
 * cannot share the single-list slot every other view uses — and each one pages
 * and fails on its own, which is why `limit`, `loading` and `error` are
 * per-section rather than per-desk.
 *
 * `total` is the server's `calculateTotal` for that section's own query. When
 * `ids.length < total` the window is a BOUND, not the whole section, and the UI
 * has to say so rather than presenting the window as the count (CLAUDE.md: a
 * bounded result must report its bound).
 */
export interface SectionState {
  /** desk.sectionKey(kind, areaId, scope) — stable and scope-qualified. */
  key: string;
  ids: Id[];
  total: number;
  /** How many rows the client has asked the server for so far. */
  limit: number;
  loading: boolean;
  error: string | null;
  /** When this section last came back from the server, or null if it never
   * has. It is what distinguishes "this drawer is empty" from "this drawer has
   * never been opened" — refreshDesk reloads only the drawers that are actually
   * open, and an empty-looking closed drawer must not be mistaken for one. */
  loadedAt: number | null;
  /** The store's `moveEpoch` when the query behind `ids` was SENT (#685); see
   * `settledMoves`. */
  epoch: number;
}

export type ConnState = "connecting" | "online" | "offline" | "unauthenticated" | "error";

/**
 * WHY the last sync failed, which the message alone cannot say (#375 increment
 * 6).
 *
 * `transport` — the request never produced a JMAP response: the network, a
 * 5xx, unparseable bytes (JmapTransportError). Nothing was refused; we simply
 * did not reach the server, and the honest sentence is "offline".
 * `server` — the server answered and said no (a method error, a summary that
 * came back with an error beside it). We are connected; one thing failed.
 *
 * Kept as a FIELD rather than sniffed out of the message text later, because
 * the two lead to opposite sentences in the header (core/headerState.ts) and
 * "does this string look like a network error" is exactly the kind of guess
 * that reads correctly for a year and then does not.
 */
export type SyncErrorKind = "transport" | "server";

export interface MailState {
  conn: ConnState;
  fatal: string | null;
  session: JmapSession | null;
  accountId: string;
  cursor: SyncCursor;

  mailboxes: Record<Id, Mailbox>;
  labels: Record<Id, Label>;
  /** Remembered per-sender remote-content grants (#202), keyed by id. Loaded once
   * on connect and kept current through /changes, because the reading pane consults
   * it on every message it renders — a per-render fetch would be a request per
   * message to answer a question about a handful of rows. */
  remoteAllows: Record<Id, RemoteAllow>;
  /** Saved signatures and templates (#56), keyed by id. Loaded with the other
   * reference data because compose needs them before it renders. */
  composeSnippets: Record<Id, ComposeSnippet>;
  /** Saved searches / smart folders (#57), keyed by id. Loaded with the other
   * reference data because the sidebar needs the pinned ones before it renders. */
  savedSearches: Record<Id, SavedSearch>;
  /** The category-tab strip (#228), keyed by tab name. Sorted on each row's own
   * `position` when rendered — NOT on the vocabulary order, which is only the
   * default a fresh account starts from. */
  categoryTabs: Record<string, CategoryTabState>;
  identities: Record<Id, Identity>;
  emails: Record<Id, Email>;
  threads: Record<Id, Thread>;
  submissions: Record<Id, EmailSubmission>;
  snippets: Record<Id, { subject: string | null; preview: string | null }>;

  /** The desk's areas (#375/#442), keyed by id. Reference data: a handful of
   * rows loaded with the mailboxes, kept current through `/changes`, and read
   * on every render of the desk. Render them in `position` order. */
  areas: Record<Id, Area>;
  /** The one-per-login desk record — where the LINE is — or null before the
   * first load. Null is not a default: a client that guessed a threshold would
   * be drawing a different desk from the one the server counts. */
  deskPreferences: DeskPreferences | null;
  /**
   * Display preferences per CLIENT FAMILY (#375 increment 5), keyed by family.
   *
   * The web app is family `"web"` and reads only that row, but all three are
   * stored because `/get` returns all three and dropping two would make a later
   * "what does the phone have?" a second round trip for data already in hand.
   *
   * EMPTY IS NOT "DEFAULTS". Before the first `ClientPreferences/get` this map
   * is `{}`, and the device runs on its own localStorage prefs — which is the
   * correct behaviour offline. Applying a synced appearance is gated on the row
   * being HERE, never on a default invented in its place. */
  clientPreferences: Record<string, ClientPreferences>;
  /** The last Area/getSummary answer for the desk's current scope, or null.
   *
   * It carries the `scope` and `threshold` it was taken at ON PURPOSE: these
   * counts answer one specific question, and rendering them beside a desk that
   * has since switched account — or beside a preview line — would be reporting
   * numbers about something else.
   *
   * `allMail` rides along because it is measured in the SAME BATCH and at the
   * same scope (sync.ts loadDesk): it is not part of Area/getSummary — nothing
   * about it is per-area — but a count taken at another moment, or under
   * another account, would be exactly the kind of number this record exists to
   * prevent. It is `null` when that one call failed, which is not zero: "all
   * mail is empty" and "we could not count it" are different facts and only one
   * of them belongs on a badge. */
  deskSummary: {
    scope: Id | null;
    threshold: number;
    rows: AreaSummaryRow[];
    state: string;
    allMail: number | null;
  } | null;
  /** Named lists the desk renders side by side — the tray, one per area on the
   * surface, one per open drawer — keyed by desk.sectionKey(). Independent of
   * `list`, which stays the single list every other view uses. */
  sections: Record<string, SectionState>;
  /** Wall-clock ms of the last successful server round trip (load or poll); the
   * header sentence's "synced 40 s ago". Null until the first.
   *
   * It is about CONTACT WITH THE SERVER, not about mail moving: a poll that
   * found nothing still advances it, because "nothing has changed" is only
   * reassuring if you know when it was last checked. */
  lastSyncAt: number | null;

  overlay: Record<Id, EmailOverlay>;
  /**
   * MOVES THE SERVER HAS AGREED WITH, STILL APPLIED TO EVERY LIST OLDER THAN
   * THE AGREEMENT (#685).
   *
   * A move's overlay retires in `putEmails` the moment the server's Email shows
   * the message where it was sent — and that is the moment it stopped hiding
   * the row. The ids of every list on screen came from a query asked BEFORE
   * that, so without the overlay nothing filtered them: an archived message
   * came back until the next query landed, and a query that had left before
   * the archive was committed and answered after it put the row back too.
   *
   * So a retired move is recorded here with the `moveEpoch` it retired at, and
   * the list selectors keep applying it to any list whose ids are OLDER than
   * that (`ListState.epoch`, `SectionState.epoch`, stamped when the query is
   * sent). A query sent after the retirement was evaluated after the server had
   * the move — it had already said so — and is believed over this.
   *
   * In memory only: after a reload every list is queried afresh.
   *
   * ⚠ NEVER PRUNED AGAINST THE LISTS ON RECORD. "Every list has been queried
   * since" is not "no older answer is coming": ordinary list loads are not
   * single-flight, so a query sent before the move can land AFTER a fresher
   * one — and pruning on the fresher one's arrival handed the row straight
   * back to it (sync.moveRace.test.ts). An entry is one id and two numbers,
   * one per message moved in this tab's lifetime, and an entry older than
   * every list it could be compared with filters nothing.
   */
  settledMoves: Record<Id, { move: LocalMove; epoch: number }>;
  /** Advanced every time a move settles; read by a load before it sends. */
  moveEpoch: number;
  list: ListState;
  /** Pending offline-queue actions (depth is surfaced in the UI). */
  queue: QueuedAction[];
  /** Set when the last sync attempt failed; cleared on the next success. */
  syncError: string | null;
  /** Which KIND of failure that was — null exactly when `syncError` is. */
  syncErrorKind: SyncErrorKind | null;
  /** Draft revisions THIS client replaced by autosaving, old id → newest id
   * (#610, #612). In memory only: it is what lets things holding a draft's id
   * follow it across a save, and a reload re-reads the newest id from the URL. */
  draftSuccessors: Successors;
}

const OVERLAY_KEY = "mailroom.overlay.v1";

function emptyList(): ListState {
  return {
    key: "",
    ids: [],
    total: 0,
    limit: 0,
    loading: false,
    error: null,
    searchUnavailable: false,
    searchCapped: null,
    epoch: 0,
  };
}

function emptySection(key: string): SectionState {
  return { key, ids: [], total: 0, limit: 0, loading: false, error: null, loadedAt: null, epoch: 0 };
}

export function initialState(): MailState {
  return {
    conn: "connecting",
    fatal: null,
    session: null,
    accountId: "",
    cursor: emptySyncCursor(""),
    mailboxes: {},
    labels: {},
    remoteAllows: {},
    composeSnippets: {},
    savedSearches: {},
    categoryTabs: {},
    identities: {},
    emails: {},
    threads: {},
    submissions: {},
    snippets: {},
    areas: {},
    deskPreferences: null,
    clientPreferences: {},
    deskSummary: null,
    sections: {},
    lastSyncAt: null,
    overlay: {},
    settledMoves: {},
    moveEpoch: 0,
    list: emptyList(),
    queue: [],
    syncError: null,
    syncErrorKind: null,
    draftSuccessors: {},
  };
}

type Listener = () => void;

export class MailStore {
  private state: MailState = initialState();
  private readonly listeners = new Set<Listener>();
  private readonly storage: KeyValueStorage | null;
  /** This device's appearance writes a server read may not reflect yet
   * (#562). Not state: nothing renders it, it only decides what a read puts. */
  private readonly prefsLedger = new PrefsLedger();

  constructor(storage: KeyValueStorage | null = null) {
    this.storage = storage;
    const raw = storage?.getItem(OVERLAY_KEY);
    if (raw) {
      try {
        this.state = { ...this.state, overlay: JSON.parse(raw) as Record<Id, EmailOverlay> };
      } catch {
        storage?.removeItem(OVERLAY_KEY);
      }
    }
  }

  getState = (): MailState => this.state;

  subscribe = (fn: Listener): (() => void) => {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  };

  private set(patch: Partial<MailState>): void {
    this.state = { ...this.state, ...patch };
    for (const l of this.listeners) l();
  }

  private persistOverlay(): void {
    if (!this.storage) return;
    try {
      this.storage.setItem(OVERLAY_KEY, JSON.stringify(this.state.overlay));
    } catch {
      // A full/blocked storage must never break the UI: the overlay simply
      // stops surviving reloads.
    }
  }

  // -- connection / session --------------------------------------------------

  setConn(conn: ConnState, fatal: string | null = null): void {
    this.set({ conn, fatal });
  }

  setSession(session: JmapSession, accountId: string): void {
    this.set({ session, accountId, cursor: { ...emptySyncCursor(accountId), ...this.state.cursor, accountId } });
  }

  setCursor(cursor: SyncCursor): void {
    this.set({ cursor });
  }

  /**
   * Records (or clears) the last sync failure.
   *
   * The KIND is required whenever there is an error: a caller that knew a
   * request failed also knew whether it was the network or a refusal, and
   * defaulting it would silently file every server refusal as a dropped
   * connection (or the reverse) at exactly the call site that had the answer.
   */
  setSyncError(err: string | null, kind: SyncErrorKind | null = null): void {
    const nextKind = err === null ? null : kind;
    if (this.state.syncError === err && this.state.syncErrorKind === nextKind) return;
    this.set({ syncError: err, syncErrorKind: nextKind });
  }

  setQueue(queue: QueuedAction[]): void {
    this.set({ queue });
  }

  // -- server data -----------------------------------------------------------

  putMailboxes(list: Mailbox[], replace = false): void {
    const mailboxes = replace ? {} : { ...this.state.mailboxes };
    for (const m of list) mailboxes[m.id] = m;
    this.set({ mailboxes });
  }

  removeMailboxes(ids: Id[]): void {
    const mailboxes = { ...this.state.mailboxes };
    for (const id of ids) delete mailboxes[id];
    this.set({ mailboxes });
  }

  /**
   * Records a folder's new sync scope after the SERVER accepted it (#62).
   *
   * Deliberately not an optimistic overlay like the flags: the scope decides
   * whether the server keeps the folder current, and drawing a folder as "not
   * being updated" before that is true would be a claim about mail we have no
   * basis for. A no-op when the mailbox is unknown — a folder that is not loaded
   * has nothing to redraw.
   */
  applyMailboxSyncScope(id: Id, scope: "full" | "off"): void {
    const existing = this.state.mailboxes[id];
    if (!existing) return;
    this.set({ mailboxes: { ...this.state.mailboxes, [id]: { ...existing, syncScope: scope } } });
  }

  putLabels(list: Label[], replace = false): void {
    const labels = replace ? {} : { ...this.state.labels };
    for (const l of list) labels[l.id] = l;
    this.set({ labels });
  }

  /** Replaces the remote-content grants (#202).
   *
   * Always a full replace, unlike putLabels: the list is a handful of rows the
   * client reads whole, and a merge would leave a REVOKED grant in place — which
   * for this object means the client keeps loading a sender's images after the user
   * said stop. Whole-list is the safe shape here.
   */
  /** Replaces the saved signatures and templates (#56).
   *
   * A full replace, like putRemoteAllows and for the same reason: a merge would
   * leave a DELETED snippet in the picker, so the user would keep being offered
   * text they removed. */
  putComposeSnippets(list: ComposeSnippet[]): void {
    const composeSnippets: Record<Id, ComposeSnippet> = {};
    for (const s of list) composeSnippets[s.id] = s;
    this.set({ composeSnippets });
  }

  /** Replaces the whole set (#57). A saved search list is a handful of rows the
   * sidebar renders in full, so a wholesale replace is both simpler and correct:
   * a merge would leave a deleted folder on screen until a reload. */
  /** Replace the category-tab strip (#228). Keyed by TAB NAME rather than id:
   * the vocabulary is closed and a tab is identified by which tab it is, so a
   * caller asking "is promotions enabled" should not have to know its ULID. */
  putCategoryTabs(list: CategoryTabState[]): void {
    const categoryTabs: Record<string, CategoryTabState> = {};
    for (const t of list) categoryTabs[t.tab] = t;
    this.set({ categoryTabs });
  }

  // -- the desk (#375/#442) ---------------------------------------------------

  /** Absorbs Area objects. `replace` is for a full re-read (the reference-data
   * load, and the `/changes` refetch): a merge would leave a DESTROYED area on
   * the desk, still drawing a section for mail that now belongs to another. */
  putAreas(list: Area[], replace = false): void {
    const areas = replace ? {} : { ...this.state.areas };
    for (const a of list) areas[a.id] = a;
    this.set({ areas });
  }

  removeAreas(ids: Id[]): void {
    if (ids.length === 0) return;
    const areas = { ...this.state.areas };
    for (const id of ids) delete areas[id];
    this.set({ areas });
  }

  /** Records the desk record the SERVER returned. Never written optimistically:
   * `lineThreshold` decides which mail is on the surface, and drawing a
   * different desk from the one the server counts is a claim about mail. */
  setDeskPreferences(prefs: DeskPreferences): void {
    this.set({ deskPreferences: prefs });
  }

  /**
   * Absorbs ClientPreferences rows (#375 increment 5), keyed by family.
   *
   * Written ONLY from a server answer — a `/get`, or the `/get` that rides
   * beside a `/set` — never optimistically. The device applies a local change
   * to itself immediately (that is what makes the setting feel instant, and
   * what keeps it working offline); this map is the record of what the SERVER
   * holds, and the two are deliberately allowed to disagree until the write
   * lands, because a refused write must leave the device on the value the user
   * chose rather than snapping back to the server's.
   *
   * ⚠ EXCEPT WHERE THE READ CANNOT SPEAK FOR A FIELD (#562). `readAt` is the
   * tick `beginClientPreferencesRead` gave the read when it was SENT; a field
   * this device wrote whose answer had not arrived by then is laid over with
   * the value the user chose, because the read may have been served before that
   * write committed and the shell puts whatever is here on screen. Without it,
   * the echo of an earlier choice undid a later one (core/prefsLedger.ts has
   * the whole argument). `confirms` names the writes this read was served
   * after — the `/get` beside their own `/set`.
   *
   * `readAt` is REQUIRED so a new read path cannot forget the question: a read
   * put here without a tick would be treated as fresh, which is the bug.
   */
  putClientPreferences(
    list: ClientPreferences[],
    readAt: number,
    confirms: readonly number[] = [],
  ): void {
    if (list.length === 0) return;
    const clientPreferences = { ...this.state.clientPreferences };
    for (const p of list) {
      clientPreferences[p.family] = this.prefsLedger.reconcile(p, readAt, confirms);
    }
    this.set({ clientPreferences });
  }

  /** A tick for a ClientPreferences read, taken just before it is SENT. */
  beginClientPreferencesRead(): number {
    return this.prefsLedger.beginRead();
  }

  /** Records a local appearance write the moment the user makes it (before
   * any debounce). Returns its id for `settleClientPreferencesWrites`. */
  beginClientPreferencesWrite(family: string, patch: PrefsPatch): number {
    return this.prefsLedger.beginWrite(family, patch);
  }

  /** The server answered these writes — whether it accepted them, refused
   * them, or could not be reached. Every write that was begun MUST be settled,
   * or its field keeps overriding reads until the user writes it again. */
  settleClientPreferencesWrites(family: string, seqs: readonly number[]): void {
    this.prefsLedger.settle(family, seqs);
  }

  /** Records (or clears) the Area/getSummary counts. Cleared with `null` when
   * the desk's scope changes, so nothing renders last account's numbers beside
   * this account's mail. */
  setDeskSummary(summary: MailState["deskSummary"]): void {
    this.set({ deskSummary: summary });
  }

  /**
   * Patches one desk section, creating it if this key is new.
   *
   * Same rule as setList: a patch for the key that is already there MERGES into
   * it (so `{loading: true}` does not blank the rows still on screen), and a
   * patch for a fresh key starts from an empty section rather than inheriting
   * another section's ids — which is what keeps two areas' lists from bleeding
   * into each other.
   */
  setSection(patch: Partial<SectionState> & { key: string }): void {
    const cur = this.state.sections[patch.key];
    const base = cur ?? emptySection(patch.key);
    this.set({ sections: { ...this.state.sections, [patch.key]: { ...base, ...patch } } });
  }

  /** The store's move epoch — read by a load just BEFORE it sends its query,
   * and handed back with the ids it gets (see `settledMoves`). */
  moveEpoch(): number {
    return this.state.moveEpoch;
  }

  /** Forgets every section whose key the predicate accepts — how the desk drops
   * another account's sections when the scope switches. Keyed rather than
   * wholesale so an open drawer survives a refresh of the same scope. */
  dropSections(predicate: (key: string) => boolean): void {
    const sections: Record<string, SectionState> = {};
    let dropped = false;
    for (const [key, s] of Object.entries(this.state.sections)) {
      if (predicate(key)) {
        dropped = true;
        continue;
      }
      sections[key] = s;
    }
    if (!dropped) return;
    this.set({ sections });
  }

  /** Stamps a successful round trip (the header's "synced 40 s ago"). */
  setLastSyncAt(ms: number): void {
    this.set({ lastSyncAt: ms });
  }

  putSavedSearches(list: SavedSearch[]): void {
    const savedSearches: Record<Id, SavedSearch> = {};
    for (const s of list) savedSearches[s.id] = s;
    this.set({ savedSearches });
  }

  putRemoteAllows(list: RemoteAllow[]): void {
    const remoteAllows: Record<Id, RemoteAllow> = {};
    for (const a of list) remoteAllows[a.id] = a;
    this.set({ remoteAllows });
  }

  putIdentities(list: Identity[], replace = false): void {
    const identities = replace ? {} : { ...this.state.identities };
    for (const i of list) identities[i.id] = i;
    this.set({ identities });
  }

  putThreads(list: Thread[]): void {
    const threads = { ...this.state.threads };
    for (const t of list) threads[t.id] = t;
    this.set({ threads });
  }

  /** Records a mute the server has already ACCEPTED (#51). Unlike the Email
   * overlay this is not optimistic — Thread/set is sent directly and awaited, so
   * by the time this runs the change is real and simply needs reflecting until
   * the next Thread/get. */
  applyThreadMuted(id: Id, muted: boolean): void {
    const cur = this.state.threads[id];
    if (!cur) return;
    this.set({ threads: { ...this.state.threads, [id]: { ...cur, isMuted: muted } } });
  }

  putSubmissions(list: EmailSubmission[]): void {
    const submissions = { ...this.state.submissions };
    for (const s of list) submissions[s.id] = s;
    this.set({ submissions });
  }

  putSnippets(list: { emailId: Id; subject: string | null; preview: string | null }[]): void {
    const snippets = { ...this.state.snippets };
    for (const s of list) snippets[s.emailId] = { subject: s.subject, preview: s.preview };
    this.set({ snippets });
  }

  /**
   * Absorbs authoritative Email objects and RECONCILES the overlay against
   * them: every local intent the server now agrees with is retired.
   */
  putEmails(list: Email[]): void {
    if (list.length === 0) return;
    const emails = { ...this.state.emails };
    const overlay = { ...this.state.overlay };
    let overlayChanged = false;
    // A move retiring here stops hiding its row; `settledMoves` takes over for
    // the lists whose ids predate this moment (#685).
    let settledMoves: MailState["settledMoves"] | null = null;
    const epoch = this.state.moveEpoch + 1;

    for (const e of list) {
      // MERGE, don't replace: an Email object is a PROJECTION of the requested
      // properties, not the whole object. The reading pane fetches the body; the
      // change poll re-fetches the same message with the cheap property set. A
      // wholesale overwrite would blank the body of the message the user is
      // reading. (Emails are immutable apart from keywords/mailboxIds — RFC 8621
      // §4 — so a body kept from an earlier fetch cannot go stale.)
      const prev = emails[e.id];
      emails[e.id] = prev ? { ...prev, ...e } : e;
      const ov = overlay[e.id];
      if (!ov) continue;

      const keywords: Record<string, boolean> = {};
      for (const [kw, want] of Object.entries(ov.keywords)) {
        const has = e.keywords[kw] === true;
        if (has !== want) keywords[kw] = want; // still pending
      }

      // A local move is honored until the server's own mailboxIds show the
      // message in the moved-to folder — which the server reports as soon as
      // Email/set's own commit lands (moved_mailbox_id is canonical there;
      // see the header comment), not only after the IMAP writeback completes.
      let move = ov.move;
      if (move && this.serverAgreesWithMove(e, move)) {
        settledMoves ??= { ...this.state.settledMoves };
        settledMoves[e.id] = { move, epoch };
        move = null;
      }

      // The pin clears as soon as the server's own value matches what we asked
      // for — the same "still pending until the server agrees" rule the keywords
      // above use. `?? false` treats an absent property as unpinned rather than
      // as disagreement, which would pin the overlay open forever.
      let pinned = ov.pinned;
      if (pinned !== undefined && (e.isPinned ?? false) === pinned) pinned = undefined;

      // The snooze retires on the same rule, comparing against the server's own
      // wake time. `?? null` maps an absent property onto "not snoozed", which is
      // what makes an un-snooze (intent `null`) resolve instead of pending forever.
      let snoozedUntil = ov.snoozedUntil;
      if (snoozedUntil !== undefined && (e.snoozedUntil ?? null) === snoozedUntil) {
        snoozedUntil = undefined;
      }

      // An importance correction retires when the server's own verdict matches
      // it. If the model keeps disagreeing the override stays, and that is the
      // honest outcome: the user said otherwise, and they should keep seeing
      // what they said rather than have it silently reverted.
      let important = ov.important;
      if (important !== undefined && (e.isImportant ?? false) === important) {
        important = undefined;
      }

      // `answered` is NOT retired here. Every other intent in this overlay can
      // be checked against a property on the Email — a pin, a wake time, a
      // keyword — and this one cannot: whether a message is still in the
      // needs-a-category queue depends on a dismissal tombstone the server does
      // not expose, and on the absence of a label, which is already absent.
      //
      // Retiring it on `suggestedLabels` being empty was the obvious thing and
      // it was wrong: for the majority of the queue — unlabelled mail nothing
      // has ever asked about — that condition is true BEFORE the user acts, so
      // the flag would retire on the very next poll and the row would come back.
      // It is retired against a fresh QUERY instead (setList), which is the only
      // thing that actually knows what is still queued.
      const answered = ov.answered;

      // Labels applied by id (#704) retire on the keywords' rule, against the
      // server's own labelIds — read off the MERGED email, so a projection that
      // did not carry labelIds (the reading pane's, before #704) compares
      // against the last value we had rather than against nothing. `?? {}`
      // reads a server that never sends it as "no labels", which retires an
      // add rather than pinning it open forever.
      let labels: Record<Id, boolean> | undefined;
      if (ov.labels) {
        const have = (emails[e.id] as Email).labelIds ?? {};
        for (const [lid, want] of Object.entries(ov.labels)) {
          if ((have[lid] === true) !== want) (labels ??= {})[lid] = want; // still pending
        }
      }

      const next: EmailOverlay = {
        keywords,
        move,
        pinned,
        snoozedUntil,
        important,
        answered,
        labels,
        updatedAt: ov.updatedAt,
      };
      if (
        Object.keys(keywords).length === 0 &&
        move === null &&
        pinned === undefined &&
        snoozedUntil === undefined &&
        important === undefined &&
        answered === undefined &&
        labels === undefined
      ) {
        delete overlay[e.id];
        overlayChanged = true;
      } else if (
        move !== ov.move ||
        pinned !== ov.pinned ||
        snoozedUntil !== ov.snoozedUntil ||
        important !== ov.important ||
        Object.keys(keywords).length !== Object.keys(ov.keywords).length ||
        Object.keys(labels ?? {}).length !== Object.keys(ov.labels ?? {}).length
      ) {
        overlay[e.id] = next;
        overlayChanged = true;
      }
    }

    this.set({
      emails,
      ...(overlayChanged ? { overlay } : {}),
      ...(settledMoves ? { settledMoves, moveEpoch: epoch } : {}),
    });
    if (overlayChanged) this.persistOverlay();
  }

  /** True when the server's Email already reflects the local move: the message
   * now lives (only) in a mailbox with the target role. */
  private serverAgreesWithMove(e: Email, move: LocalMove): boolean {
    const roles = Object.keys(e.mailboxIds)
      .map((id) => this.state.mailboxes[id]?.role)
      .filter((r): r is Exclude<Mailbox["role"], null> => !!r);
    if (roles.length === 0) return false; // mailboxes not loaded yet: keep the overlay
    return roles.every((r) => r === move);
  }

  /** Records that an autosave of THIS client replaced draft `from` with `to`
   * (MailActions.saveDraft; see core/draftLineage.ts for who may call it). */
  recordDraftSuccessor(from: Id, to: Id): void {
    this.set({ draftSuccessors: recordSuccessor(this.state.draftSuccessors, from, to) });
  }

  removeEmails(ids: Id[]): void {
    if (ids.length === 0) return;
    const emails = { ...this.state.emails };
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      delete emails[id];
      delete overlay[id];
    }
    const list = { ...this.state.list, ids: this.state.list.ids.filter((id) => !ids.includes(id)) };
    this.set({ emails, overlay, list });
    this.persistOverlay();
  }

  // -- lists -----------------------------------------------------------------

  /**
   * Replaces (or patches) the current list.
   *
   * `queriedAt` is when the query behind `patch.ids` was ISSUED, and it is what
   * retires the `answered` flags (#176). The rule, and it has to be the issue
   * time rather than the arrival time:
   *
   *   - an answer given BEFORE the query went out is one the query should
   *     already reflect. If the message came back anyway, the write did not take
   *     — a failed dismiss, a rolled-back label — and the honest thing is to show
   *     the row again rather than keep hiding work that is not done.
   *   - an answer given AFTER the query went out cannot possibly be in the
   *     result, so the flag stands. Without this half, a poll that happened to
   *     overlap a batch dismissal would flash all two dozen rows back.
   *
   * There is no equivalent for the other overlay fields because there does not
   * need to be: each of those is checked against a property on the Email itself
   * (putEmails). "Is this still in the queue?" is not readable from any property
   * — the dismissal tombstone is deliberately not exposed — so a query result is
   * the only evidence there is.
   *
   * ONLY THE QUEUE'S OWN QUERY COUNTS. An Inbox or Starred query has no opinion
   * about queue membership, and retiring against one would be retiring on
   * evidence that does not exist: navigate away and back mid-write, or catch an
   * unrelated poll, and every answered row flashes back.
   */
  setList(patch: Partial<ListState> & { key: string }, queriedAt?: number): void {
    const cur = this.state.list;
    const base = cur.key === patch.key ? cur : { ...emptyList(), key: patch.key };
    if (patch.ids && queriedAt !== undefined && patch.key === "needsCategory") {
      // Whether the id is in the fresh result or not, the flag goes: if it is
      // absent the flag has done its job, and if it is present the write did not
      // take and the row must come back. So this needs no id set — only the
      // timestamp that says the query is newer than the answer.
      let overlay: Record<Id, EmailOverlay> | null = null;
      for (const [id, ov] of Object.entries(this.state.overlay)) {
        if (!ov.answered || ov.updatedAt >= queriedAt) continue;
        if (!overlay) overlay = { ...this.state.overlay };
        const next: EmailOverlay = { ...ov, answered: undefined };
        if (
          Object.keys(next.keywords).length === 0 &&
          next.move === null &&
          next.pinned === undefined &&
          next.snoozedUntil === undefined &&
          next.important === undefined &&
          !hasLabelIntent(next)
        ) {
          delete overlay[id];
        } else {
          overlay[id] = next;
        }
      }
      if (overlay) {
        this.set({ overlay });
        this.persistOverlay();
      }
    }
    this.set({ list: { ...base, ...patch } });
  }

  // -- optimistic mutations ---------------------------------------------------

  /** Applies a keyword intent locally (instant), returning the previous overlay
   * entry so the action queue can roll it back on a permanent failure. */
  applyKeyword(ids: Id[], keyword: string, present: boolean): Record<Id, EmailOverlay | undefined> {
    const before: Record<Id, EmailOverlay | undefined> = {};
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      before[id] = overlay[id];
      const cur = overlay[id] ?? { keywords: {}, move: null, updatedAt: 0 };
      overlay[id] = {
        ...cur,
        keywords: { ...cur.keywords, [keyword]: present },
        updatedAt: Date.now(),
      };
    }
    this.set({ overlay });
    this.persistOverlay();
    return before;
  }

  /** Applies a label-by-id intent locally (#704) — the `labelIds/<id>`
   * counterpart of applyKeyword, for a label with no keyword atom. Returns the
   * previous overlay entries for a rollback, like applyKeyword. */
  applyLabelId(ids: Id[], labelId: Id, present: boolean): Record<Id, EmailOverlay | undefined> {
    const before: Record<Id, EmailOverlay | undefined> = {};
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      before[id] = overlay[id];
      const cur = overlay[id] ?? { keywords: {}, move: null, updatedAt: 0 };
      overlay[id] = {
        ...cur,
        labels: { ...(cur.labels ?? {}), [labelId]: present },
        updatedAt: Date.now(),
      };
    }
    this.set({ overlay });
    this.persistOverlay();
    return before;
  }

  /** Drops one label-by-id intent (the action that set it was rejected). The
   * delta, not a snapshot — the same concurrency argument as revertKeyword. */
  revertLabelId(ids: Id[], labelId: Id): void {
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      const cur = overlay[id];
      if (!cur?.labels || !(labelId in cur.labels)) continue;
      const labels = { ...cur.labels };
      delete labels[labelId];
      const next: EmailOverlay = {
        ...cur,
        labels: Object.keys(labels).length > 0 ? labels : undefined,
      };
      if (
        Object.keys(next.keywords).length === 0 &&
        next.move === null &&
        next.pinned === undefined &&
        next.snoozedUntil === undefined &&
        next.important === undefined &&
        next.answered === undefined &&
        !hasLabelIntent(next)
      ) {
        delete overlay[id];
      } else {
        overlay[id] = next;
      }
    }
    this.set({ overlay });
    this.persistOverlay();
  }

  /** Applies a local move (archive/trash/restore-to-inbox). */
  applyMove(ids: Id[], move: LocalMove): Record<Id, EmailOverlay | undefined> {
    const before: Record<Id, EmailOverlay | undefined> = {};
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      before[id] = overlay[id];
      const cur = overlay[id] ?? { keywords: {}, move: null, updatedAt: 0 };
      overlay[id] = { ...cur, move, updatedAt: Date.now() };
    }
    // A settled move for these ids is left alone: the pending overlay outranks
    // it (store moveFor), and if this move is refused the settled one is still
    // where the message is.
    this.set({ overlay });
    this.persistOverlay();
    return before;
  }

  /** Applies an optimistic pin/unpin (#51). */
  applyPin(ids: Id[], pinned: boolean): Record<Id, EmailOverlay | undefined> {
    const before: Record<Id, EmailOverlay | undefined> = {};
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      before[id] = overlay[id];
      const cur = overlay[id] ?? { keywords: {}, move: null, updatedAt: 0 };
      overlay[id] = { ...cur, pinned, updatedAt: Date.now() };
    }
    this.set({ overlay });
    this.persistOverlay();
    return before;
  }

  /** Drops a pin intent the server refused. */
  revertPin(ids: Id[]): void {
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      const cur = overlay[id];
      if (!cur) continue;
      if (
        Object.keys(cur.keywords).length === 0 &&
        cur.move === null &&
        cur.snoozedUntil === undefined &&
        !hasLabelIntent(cur)
      ) {
        delete overlay[id];
      } else {
        overlay[id] = { ...cur, pinned: undefined };
      }
    }
    this.set({ overlay });
    this.persistOverlay();
  }

  /** Applies an optimistic snooze (#51): an ISO wake time, or null to un-snooze.
   * Snoozing from a mailbox view makes the row leave the list at once, which is
   * the whole point — "not now" should look like it took effect immediately. */
  applySnooze(ids: Id[], snoozedUntil: string | null): Record<Id, EmailOverlay | undefined> {
    const before: Record<Id, EmailOverlay | undefined> = {};
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      before[id] = overlay[id];
      const cur = overlay[id] ?? { keywords: {}, move: null, updatedAt: 0 };
      overlay[id] = { ...cur, snoozedUntil, updatedAt: Date.now() };
    }
    this.set({ overlay });
    this.persistOverlay();
    return before;
  }

  /**
   * Marks a message's open questions as ANSWERED locally (#176), so it leaves
   * the needs-a-category queue the instant the user acts rather than at the next
   * poll.
   *
   * Applied for a dismissal AND for a label, because the server clears the queue
   * for both — and a message that stays on screen after being filed is the
   * fastest way to teach someone the queue is lying to them.
   *
   * Reversed by clearAnswered, which exists for the one case that is a real
   * change of mind rather than a failed write: the user takes back a batch
   * answer (#182). Failures still need no revert here — a failed dismiss leaves
   * the questions in place and the next queue query puts the row back, and a
   * failed label is rolled back through the action queue's own keyword rollback.
   */
  applyAnswered(ids: Id[]): void {
    if (ids.length === 0) return;
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      const cur = overlay[id] ?? { keywords: {}, move: null, updatedAt: 0 };
      overlay[id] = { ...cur, answered: true, updatedAt: Date.now() };
    }
    this.set({ overlay });
    this.persistOverlay();
  }

  /**
   * Takes back an "answered" flag (#182) — the user changed their mind about a
   * batch, not a write that failed.
   *
   * ON ITS OWN THIS DOES NOT BRING THE ROWS BACK, and it is worth being clear
   * about why, because the flag looks like the thing hiding them. By the time an
   * undo is possible the queue has already been re-queried without those
   * messages — they carry a label, so the server does not return them — and
   * `visibleIds` can only order and filter `list.ids`. Clearing the flag removes
   * the LOCAL half; the rows return when the label is actually gone and a fresh
   * query says so. See MailActions.undoBatchLabel, which does both in order.
   */
  clearAnswered(ids: Id[]): void {
    if (ids.length === 0) return;
    const overlay = { ...this.state.overlay };
    let touched = false;
    for (const id of ids) {
      const cur = overlay[id];
      if (!cur?.answered) continue;
      touched = true;
      const next: EmailOverlay = { ...cur, answered: undefined };
      // Drop an overlay entry that no longer says anything, so it does not
      // accumulate for every message the user ever answered and un-answered.
      if (
        Object.keys(next.keywords).length === 0 &&
        next.move === null &&
        next.pinned === undefined &&
        next.snoozedUntil === undefined &&
        next.important === undefined
      ) {
        delete overlay[id];
      } else {
        overlay[id] = next;
      }
    }
    if (!touched) return;
    this.set({ overlay });
    this.persistOverlay();
  }

  /** Records an importance correction locally so the row moves at once (#154).
   * The authoritative answer is a re-score, which arrives on the next poll. */
  applyImportance(ids: Id[], important: boolean): Record<Id, EmailOverlay | undefined> {
    const before: Record<Id, EmailOverlay | undefined> = {};
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      before[id] = overlay[id];
      const cur = overlay[id] ?? { keywords: {}, move: null, updatedAt: 0 };
      overlay[id] = { ...cur, important, updatedAt: Date.now() };
    }
    this.set({ overlay });
    this.persistOverlay();
    return before;
  }

  /** Drops an importance correction the server refused. */
  revertImportance(ids: Id[]): void {
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      const cur = overlay[id];
      if (!cur) continue;
      if (
        Object.keys(cur.keywords).length === 0 &&
        cur.move === null &&
        cur.pinned === undefined &&
        cur.snoozedUntil === undefined &&
        !hasLabelIntent(cur)
      ) {
        delete overlay[id];
      } else {
        overlay[id] = { ...cur, important: undefined };
      }
    }
    this.set({ overlay });
    this.persistOverlay();
  }

  /** Drops a snooze intent the server refused. */
  revertSnooze(ids: Id[]): void {
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      const cur = overlay[id];
      if (!cur) continue;
      if (
        Object.keys(cur.keywords).length === 0 &&
        cur.move === null &&
        cur.pinned === undefined &&
        !hasLabelIntent(cur)
      ) {
        delete overlay[id];
      } else {
        overlay[id] = { ...cur, snoozedUntil: undefined };
      }
    }
    this.set({ overlay });
    this.persistOverlay();
  }

  /** Restores overlay entries captured before an optimistic apply (rollback). */
  rollback(before: Record<Id, EmailOverlay | undefined>): void {
    const overlay = { ...this.state.overlay };
    for (const [id, prev] of Object.entries(before)) {
      if (prev) overlay[id] = prev;
      else delete overlay[id];
    }
    this.set({ overlay });
    this.persistOverlay();
  }

  /** Drops one keyword intent (the action that set it was rejected). Dropping
   * the delta — rather than restoring a snapshot — is what makes rollback
   * correct under concurrency: whatever is left in the overlay is still-pending
   * intent, and the server's own value shows through for everything else. */
  revertKeyword(ids: Id[], keyword: string): void {
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      const cur = overlay[id];
      if (!cur) continue;
      const keywords = { ...cur.keywords };
      delete keywords[keyword];
      // The pin and the snooze are further kinds of pending intent: dropping a
      // keyword must not discard an in-flight one that shares this overlay entry.
      if (
        Object.keys(keywords).length === 0 &&
        cur.move === null &&
        cur.pinned === undefined &&
        cur.snoozedUntil === undefined &&
        !hasLabelIntent(cur)
      ) {
        delete overlay[id];
      } else {
        overlay[id] = { ...cur, keywords };
      }
    }
    this.set({ overlay });
    this.persistOverlay();
  }

  /** Drops a local move intent (the archive/trash the server refused). A
   * refused move never settled, so it never reached `settledMoves`. */
  revertMove(ids: Id[]): void {
    const overlay = { ...this.state.overlay };
    for (const id of ids) {
      const cur = overlay[id];
      if (!cur) continue;
      if (
        Object.keys(cur.keywords).length === 0 &&
        cur.pinned === undefined &&
        cur.snoozedUntil === undefined &&
        !hasLabelIntent(cur)
      ) {
        delete overlay[id];
      } else {
        overlay[id] = { ...cur, move: null };
      }
    }
    this.set({ overlay });
    this.persistOverlay();
  }
}

/**
 * The move that decides whether `id` belongs in a list whose ids were queried
 * at `listEpoch`: the pending overlay move if there is one, else a SETTLED move
 * the list is older than (#685, see MailState.settledMoves), else none.
 */
function moveFor(state: MailState, id: Id, listEpoch: number): LocalMove | null {
  const pending = state.overlay[id]?.move;
  if (pending) return pending;
  const settled = state.settledMoves[id];
  return settled && settled.epoch > listEpoch ? settled.move : null;
}

// ---------------------------------------------------------------------------
// Selectors (pure; used by the UI and by the list assembler)
// ---------------------------------------------------------------------------

/** An Email with the local overlay applied — what the UI must always render. */
export interface EffectiveEmail extends Email {
  isSeen: boolean;
  isFlagged: boolean;
  isDraft: boolean;
  /** Pinned after the overlay (#51). Narrowed from Email's optional property to
   * a definite boolean, so callers never have to think about "absent". */
  isPinned: boolean;
  /** The wake time after the overlay (#51), or null when the message is not
   * snoozed. `isSnoozed` is that compared with NOW — recomputed here rather than
   * taken from the server's boolean, because a list left open across the wake
   * moment would otherwise keep claiming a returned message is still away. */
  snoozedUntil: string | null;
  isSnoozed: boolean;
  /** When a snooze last brought this message back (#152), or null. Non-null is
   * why an old message can be at the top of the list. */
  returnedAt: string | null;
  /** The model's verdict after any correction still in flight (#154). */
  isImportant: boolean;
  /** The classifier's still-open questions about this message (#176), after the
   * overlay. Narrowed from Email's optional property to a definite array, and
   * EMPTY the moment the user answers — so "is this still asking?" is one check
   * (`suggestions.length > 0`) that already accounts for a label or a dismissal
   * the server has not confirmed yet. */
  suggestions: SuggestedLabel[];
  /** The highest confidence among the open questions, or null when there are
   * none. The needs-a-category queue's sort key, mirroring the server's
   * correlated max(): a message is ranked by its BEST question, never by an
   * arbitrary one. */
  topConfidence: number | null;
  /** Label keyword atoms present on this message (after the overlay). */
  labelKeywords: string[];
  /** Every label on this message BY ID (#704), after the overlay — mirrored
   * and local-only alike. Narrowed from Email's optional property to a
   * definite set. A pending keyword change to a MIRRORED label is reflected
   * here too (through its atom), so "does it carry label X?" is one lookup
   * whichever path applied X. */
  labelIds: Record<Id, true>;
  /** Non-null when the message has an un-reconciled local move. */
  localMove: LocalMove | null;
  /** True while any local intent for this message is still un-reconciled. */
  pending: boolean;
}

export function effectiveEmail(state: MailState, id: Id): EffectiveEmail | null {
  const e = state.emails[id];
  if (!e) return null;
  const ov = state.overlay[id];
  const keywords: Record<string, true> = { ...e.keywords };
  if (ov) {
    for (const [kw, want] of Object.entries(ov.keywords)) {
      if (want) keywords[kw] = true;
      else delete keywords[kw];
    }
  }
  const labelAtoms = Object.keys(keywords).filter((k) => !k.startsWith("$"));
  const labelIds = effectiveLabelIds(state, e, ov, keywords);
  const snoozedUntil = ov?.snoozedUntil !== undefined ? ov.snoozedUntil : e.snoozedUntil ?? null;
  // An answered message has no open questions, whatever the server last said —
  // it is the local half of ClearLabelSuggestions, and what makes the row leave
  // the queue on the keystroke rather than on the next poll. Ordered
  // most-confident first to match SuggestionsForMessages, so the leading
  // suggestion is the same one the server would name.
  const suggestions = ov?.answered
    ? []
    : [...(e.suggestedLabels ?? [])].sort((x, y) => y.confidence - x.confidence);
  return {
    ...e,
    keywords,
    isSeen: keywords[KEYWORD_SEEN] === true,
    isFlagged: keywords[KEYWORD_FLAGGED] === true,
    isDraft: keywords["$draft"] === true,
    // The overlay wins while the write is in flight; `?? false` covers a server
    // that did not send the property at all.
    isPinned: ov?.pinned ?? e.isPinned ?? false,
    snoozedUntil,
    isSnoozed: isSnoozedAt(snoozedUntil),
    // Cleared the moment it is snoozed again — it is away, not back.
    returnedAt: ov?.snoozedUntil ? null : e.returnedAt ?? null,
    isImportant: ov?.important ?? e.isImportant ?? false,
    suggestions,
    topConfidence: suggestions.length > 0 ? (suggestions[0] as SuggestedLabel).confidence : null,
    labelKeywords: labelAtoms,
    labelIds,
    localMove: ov?.move ?? null,
    // "Change not yet confirmed by the server" (the list's dot, the desk's ◌):
    // only intents the server can confirm. `answered` is NOT one of them. It is
    // a local "this left the needs-a-category queue" note, retired only by a
    // fresh query of that queue (setList), so counting it here left every
    // labelled message marked unconfirmed in every other list until a reload,
    // long after the server had applied the change. Seen in practice: messages
    // relabelled and archived, every write applied, every row dotted.
    pending:
      !!ov &&
      (ov.move !== null ||
        ov.pinned !== undefined ||
        ov.snoozedUntil !== undefined ||
        ov.important !== undefined ||
        hasLabelIntent(ov) ||
        Object.keys(ov.keywords).length > 0),
  };
}

/** atom → label id, cached per `state.labels` object (it is replaced, never
 * mutated, so the cache can never answer for a stale label set). */
const atomIndexCache = new WeakMap<Record<Id, Label>, Map<string, Id>>();
function labelIdByAtom(labels: Record<Id, Label>): Map<string, Id> {
  let idx = atomIndexCache.get(labels);
  if (!idx) {
    idx = new Map();
    for (const l of Object.values(labels)) if (l.keyword) idx.set(l.keyword, l.id);
    atomIndexCache.set(labels, idx);
  }
  return idx;
}

/**
 * The label ids on a message after the overlay (#704).
 *
 * Starts from the server's own `labelIds`. A server (or a projection) that did
 * not send it falls back to the mirrored labels readable from `keywords`,
 * which is honest about what is knowable: local labels are then simply unknown.
 * Pending KEYWORD changes to mirrored labels are folded in through their atoms,
 * and pending label-by-id changes on top — so a toggle shows on the keystroke
 * whichever path carried it.
 */
function effectiveLabelIds(
  state: MailState,
  e: Email,
  ov: EmailOverlay | undefined,
  keywords: Record<string, true>,
): Record<Id, true> {
  const pendingAtoms = ov ? Object.keys(ov.keywords).some((k) => !k.startsWith("$")) : false;
  if (e.labelIds && !pendingAtoms && !(ov && hasLabelIntent(ov))) return e.labelIds;
  const out: Record<Id, true> = {};
  const byAtom = labelIdByAtom(state.labels);
  if (e.labelIds) {
    Object.assign(out, e.labelIds);
    if (ov) {
      for (const [kw, want] of Object.entries(ov.keywords)) {
        const lid = byAtom.get(kw);
        if (!lid) continue;
        if (want) out[lid] = true;
        else delete out[lid];
      }
    }
  } else {
    for (const kw of Object.keys(keywords)) {
      const lid = byAtom.get(kw);
      if (lid) out[lid] = true;
    }
  }
  if (ov?.labels) {
    for (const [lid, want] of Object.entries(ov.labels)) {
      if (want) out[lid] = true;
      else delete out[lid];
    }
  }
  return out;
}

/** Whether a wake time means "still away" right now. One definition, matching
 * the server's (internal/api/email.go isSnoozed): a wake time that has passed is
 * NOT snoozed, whether or not the server's wake job has run yet. */
export function isSnoozedAt(snoozedUntil: string | null, now: number = Date.now()): boolean {
  if (!snoozedUntil) return false;
  const at = Date.parse(snoozedUntil);
  return !Number.isNaN(at) && at > now;
}

/**
 * How a list is ordered — one of the three orders the server can page a window
 * in, reproduced locally so merging and the optimistic overlay do not scramble
 * what the server chose.
 *
 * A discriminator rather than a stack of booleans: the orders are mutually
 * exclusive (a list is ordered one way), and the previous `bucketImportant` flag
 * had already reached the point where adding a second one would have made
 * "priority AND confidence" expressible and meaningless.
 */
/** "server" keeps the order the server returned (#724's best match), which has
 * no client-side key to re-sort by. */
export type ListOrder = "arrival" | "priority" | "confidence" | "server";

// ---------------------------------------------------------------------------
// The overlay-aware primitives every ordered list shares.
//
// These were local closures inside visibleIds until the desk needed the same
// four answers for its sections (#375). They are module-level functions rather
// than a second copy because the two lists MUST agree: a row that is away in
// one and present in the other, or pinned-first here and arrival-first there,
// is the same message being described two ways on one screen.
// ---------------------------------------------------------------------------

/** Whether this message is currently snoozed AWAY, overlay included — so a
 * just-snoozed row leaves the list now rather than at the next refetch. */
function isAway(state: MailState, id: Id): boolean {
  const ov = state.overlay[id];
  return isSnoozedAt(
    ov?.snoozedUntil !== undefined ? (ov.snoozedUntil ?? null) : state.emails[id]?.snoozedUntil ?? null,
  );
}

/**
 * When this message was pinned, after the overlay, or null when it is not.
 *
 * Read through the overlay so a just-pinned message jumps to the top
 * immediately. Pinned messages order by WHEN THEY WERE PINNED (most recent
 * first) — the whole reason pinnedAt is a timestamp rather than a boolean:
 * ordering them by receivedAt would scatter a deliberate set of pins by the
 * unrelated accident of when each message happened to arrive.
 */
function pinnedAtOf(state: MailState, id: Id): string | null {
  const ov = state.overlay[id];
  const e = state.emails[id];
  if (ov?.pinned === false) return null;
  if (ov?.pinned === true) return e?.pinnedAt ?? new Date(ov.updatedAt).toISOString();
  return (e?.isPinned ?? false) ? (e?.pinnedAt ?? "") : null;
}

/**
 * EFFECTIVE ARRIVAL (#152), matching the order the server paged this window in:
 * a message that came back from a snooze sorts by WHEN IT RETURNED, so it sits
 * at the top rather than three days down where its age would put it. For
 * everything never snoozed this is receivedAt.
 */
function arrivedAtOf(state: MailState, id: Id): string {
  const e = state.emails[id];
  return e?.returnedAt ?? e?.receivedAt ?? "";
}

/** The default order for any list of ids: PINNED FIRST (#51), then newest
 * effective arrival. A message the store has not loaded yet compares equal
 * rather than sorting to an end it has not earned. */
function comparePinnedThenArrival(state: MailState, a: Id, b: Id): number {
  const ea = state.emails[a];
  const eb = state.emails[b];
  if (!ea || !eb) return 0;
  const pa = pinnedAtOf(state, a);
  const pb = pinnedAtOf(state, b);
  if ((pa !== null) !== (pb !== null)) return pa !== null ? -1 : 1;
  if (pa !== null && pb !== null && pa !== pb) return pb.localeCompare(pa);
  return arrivedAtOf(state, b).localeCompare(arrivedAtOf(state, a));
}

/**
 * The ids the current list should actually show: the server's ids, minus the
 * ones locally moved OUT of this view, plus the ones locally moved INTO it
 * (so the Trash view shows what you just trashed even though the IMAP move has
 * not happened yet). Sorted by receivedAt desc, which is the server's order.
 */
export function visibleIds(
  state: MailState,
  role: LocalMove | string | null,
  snoozed: boolean | null = null,
  order: ListOrder = "arrival",
): Id[] {
  const out: Id[] = [];
  const seen = new Set<Id>();
  // Whether THIS message is currently away, overlay included (isAway, above).
  const away = (id: Id): boolean => isAway(state, id);
  const listEpoch = state.list.epoch;
  for (const id of state.list.ids) {
    const ov = state.overlay[id];
    // Moved out of this view — pending, or settled after this list was queried
    // (#685: the retired overlay must not hand the row back to a stale list).
    const move = moveFor(state, id, listEpoch);
    if (move && move !== role) continue;
    // A view that filters on the snooze applies it locally too, so snoozing
    // from the inbox removes the row NOW rather than at the next refetch — the
    // same optimism archive and trash already get (#51).
    if (snoozed !== null && away(id) !== snoozed) continue;
    // The local half of the server's `needsCategory: true` (#176): a message
    // whose questions the user just answered leaves the queue on the keystroke.
    // Waiting for the round trip would leave answered work sitting on screen,
    // which is exactly how a review surface teaches people to stop trusting it.
    if (order === "confidence" && ov?.answered) continue;
    out.push(id);
    seen.add(id);
  }
  if (role === "trash" || role === "archive" || role === "inbox") {
    for (const [id, ov] of Object.entries(state.overlay)) {
      if (ov.move === role && !seen.has(id) && state.emails[id]) {
        out.push(id);
        seen.add(id);
      }
    }
    // ...and moved INTO it, settled since this list was queried: the same gap
    // the other way round (#685).
    for (const [id, m] of Object.entries(state.settledMoves)) {
      if (m.move === role && m.epoch > listEpoch && !seen.has(id) && !state.overlay[id]?.move && state.emails[id]) {
        out.push(id);
        seen.add(id);
      }
    }
  }
  // PINNED FIRST (#51), then newest effective arrival (#152) — see
  // pinnedAtOf/arrivedAtOf above, which the desk's sections share so the two
  // lists cannot order the same mail differently.
  const arrivedAt = (id: Id): string => arrivedAtOf(state, id);
  // The SNOOZED view orders by when each message comes BACK, soonest first —
  // it is a queue of things about to happen, and "newest first" would be an
  // answer to a question nobody asked of it. Pinning does not reorder it either:
  // a pin says "keep this at the top of my mail", and this list is not that.
  if (snoozed === true) {
    out.sort((a, b) => {
      const wa = state.overlay[a]?.snoozedUntil ?? state.emails[a]?.snoozedUntil ?? "";
      const wb = state.overlay[b]?.snoozedUntil ?? state.emails[b]?.snoozedUntil ?? "";
      if (wa !== wb) return wa.localeCompare(wb);
      return (state.emails[b]?.receivedAt ?? "").localeCompare(state.emails[a]?.receivedAt ?? "");
    });
    return out;
  }
  // THE NEEDS-A-CATEGORY QUEUE (#176): most-confident first, reproducing the
  // server's `suggestionConfidence` order over the window it returned.
  //
  // Doing it again here is not redundant. The server decides WHICH messages are
  // on the page — the part a client cannot do — and this decides what order they
  // sit in after the overlay has had its say, which the server cannot know.
  //
  // Pinning does NOT reorder this list, for the same reason it does not reorder
  // the Snoozed view: a pin means "keep this at the top of my mail", and a queue
  // of questions is not that. Ties fall back to arrival, newest first, so the
  // order is total and stable rather than dependent on the merge.
  // BEST MATCH (#724): the server's relevance order is the order. There is no
  // score on the client to re-sort by, and pinning does not reorder it either —
  // a search ranked by relevance has asked a question a pin does not answer.
  if (order === "server") return out;
  if (order === "confidence") {
    out.sort((a, b) => {
      const ca = topConfidence(state, a);
      const cb = topConfidence(state, b);
      if (ca !== cb) return cb - ca;
      return arrivedAt(b).localeCompare(arrivedAt(a));
    });
    return out;
  }
  out.sort((a, b) => {
    // The importance bucket leads in the Priority view (#154). The SERVER
    // already grouped each query, but a role view is a UNION over one query per
    // account, and merging them here would interleave the buckets again.
    if (order === "priority") {
      const ea = state.emails[a];
      const eb = state.emails[b];
      if (!ea || !eb) return 0;
      const ia = isImportant(state, a);
      const ib = isImportant(state, b);
      if (ia !== ib) return ia ? -1 : 1;
    }
    return comparePinnedThenArrival(state, a, b);
  });
  return out;
}

/**
 * The ids one DESK SECTION should show (#375) — visibleIds' twin for the lists
 * the desk renders side by side.
 *
 * It walks the section's server-given ids through the same overlay, so a row
 * the user just acted on leaves at the keystroke rather than at the next poll:
 *
 *   - an optimistic ARCHIVE or TRASH (`overlay.move` set and not what this
 *     section is a list of) leaves now;
 *   - a row whose overlay contradicts the section's own KEYWORD filter leaves
 *     now — the tray asks for `{atom: $flagged, present: true}`, so unflagging
 *     from the strip empties that row immediately;
 *   - a row snoozed AWAY leaves any section whose query asked for
 *     `isSnoozed: false`, which is every surface and every drawer.
 *
 * Deliberately NOT the mirror image of visibleIds' second half: this never adds
 * a message the section's query did not return. A section is a server-defined
 * set (`inArea` + which side of the line), and a client cannot know that a
 * message it moved locally now belongs to some other area's list — inventing
 * membership would put mail in a section the server would not put it in.
 *
 * `opts.snoozed` mirrors QueryPlan.snoozed and defaults to `false`, because
 * every surface and drawer filter asks for `isSnoozed: false`. The TRAY passes
 * `null`: its query is `hasKeyword: $flagged` with no snooze condition — the
 * same deliberate choice the Starred view makes (views.ts hidesSnoozed) — and
 * hiding rows the query returned would make `ids.length` disagree with the
 * `total` the section reports beside it.
 */
export function sectionIds(
  state: MailState,
  key: string,
  opts: {
    move: "inbox" | null;
    keyword?: { atom: string; present: boolean };
    snoozed?: boolean | null;
  },
): Id[] {
  const section = state.sections[key];
  if (!section) return [];
  const snoozed = opts.snoozed === undefined ? false : opts.snoozed;
  const out: Id[] = [];
  for (const id of section.ids) {
    const ov = state.overlay[id];
    // Pending, or settled after this section was queried (#685).
    const move = moveFor(state, id, section.epoch);
    if (move && move !== opts.move) continue;
    if (opts.keyword) {
      const intent = ov?.keywords[opts.keyword.atom];
      if (intent !== undefined && intent !== opts.keyword.present) continue;
    }
    if (snoozed !== null && isAway(state, id) !== snoozed) continue;
    out.push(id);
  }
  out.sort((a, b) => comparePinnedThenArrival(state, a, b));
  return out;
}

/** Whether the model called this message important, after any correction the
 * user has made but the server has not confirmed yet (#154). The correction
 * wins while it is in flight, so marking something not-important moves it out of
 * the bucket immediately rather than after a round trip. */
export function isImportant(state: MailState, id: Id): boolean {
  const override = state.overlay[id]?.important;
  if (override !== undefined) return override;
  return state.emails[id]?.isImportant ?? false;
}

/**
 * The highest confidence among a message's still-open questions (#176), or -1
 * when it has none.
 *
 * -1 rather than null or 0 so it is a total order for the sort with no
 * special-casing, and so an unsuggested message sorts LAST — mirroring the
 * server's `DESC NULLS LAST`. A real confidence is in [0,1], so nothing can
 * collide with it.
 *
 * An ANSWERED message reads as having none, which is what makes the queue's
 * order settle immediately when a row is labelled rather than shuffling once on
 * the keystroke and again on the next poll.
 */
export function topConfidence(state: MailState, id: Id): number {
  if (state.overlay[id]?.answered) return -1;
  const suggestions = state.emails[id]?.suggestedLabels;
  if (!suggestions || suggestions.length === 0) return -1;
  return suggestions.reduce((best, s) => (s.confidence > best ? s.confidence : best), -1);
}
