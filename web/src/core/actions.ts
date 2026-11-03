// SPDX-License-Identifier: AGPL-3.0-only

// MailActions — every mutation the UI can perform, with INSTANT LOCAL APPLY.
// DOM-free (spec §12).
//
// The rule (spec §4a, restated client-side): a user action is an INTENT DELTA,
// applied to the overlay immediately, appended to the durable action queue, and
// replayed against `Email/set` by the runner. The UI never waits on a round trip
// for a flag change; the round trip only decides whether the intent survives.
//
//   optimistic apply → enqueue → drain
//        │                          ├─ 2xx + updated  → dequeue (overlay retires
//        │                          │                   when the server's own
//        │                          │                   Email agrees — store.ts)
//        │                          ├─ transport error → keep queued, retry with
//        │                          │                   backoff (deltas replay
//        │                          │                   safely; `Email/set` with
//        │                          │                   the same patch twice is a
//        │                          │                   no-op server-side)
//        └──────────────────────────┴─ SetError       → ROLL BACK the overlay and
//                                                       surface the server's own
//                                                       description (this is how
//                                                       `forbiddenFrom` reaches
//                                                       the user instead of being
//                                                       swallowed).

import type { JmapClient } from "./jmap/client";
import { JmapAuthError, JmapMethodError, JmapTransportError } from "./jmap/client";
import type { Call } from "./jmap/client";
import type {
  Area,
  BlobId,
  ClientPreferences,
  ComposeSnippet,
  DeskPreferences,
  GetResult,
  SavedSearch,
  Email,
  EmailAddress,
  EmailSubmission,
  Id,
  Label,
  PatchObject,
  Rule,
  RemovedAreaLabel,
  RuleApplyResult,
  RuleCondition,
  RuleInduceResult,
  AreaRuleSuggestion,
  LearningStatus,
  RulePreview,
  RuleTightenResult,
  SetError,
  SetResult,
  SetupCensusResult,
  SetupCounts,
  SetupPlan,
  SetupRun,
} from "./jmap/types";
import { ERR_SETUP_RUN_BUSY, KEYWORD_FLAGGED, KEYWORD_SEEN } from "./jmap/types";
import { maxCallsInRequest } from "./session";
import { importKeyErrorOf } from "./keyImport";
import type { ImportKeyOutcome } from "./keyImport";
import { sectionFilter } from "./desk";
import { MAX_NAME_ATTEMPTS, bumpRuleName, nameTakenBy } from "./ruleIdentity";
import type { OfflineActionQueue, QueuedAction } from "./queue";
import type { QueuedSend } from "./sendQueue";
import { asQueuedSend, newIdempotencyKey } from "./sendQueue";
import { notJunkPatch } from "./junk";
import type { MailStore } from "./store";
import type { SyncEngine } from "./sync";

/** One message's label delta for `MailActions.relabel` (#704). Labels, not ids,
 * so each one can ride the path its own kind needs (atom or id). */
export interface LabelChange {
  id: Id;
  add: Label[];
  remove: Label[];
}

/**
 * The outcome of an `Area/set` (#374).
 *
 * ⚠ A PER-OBJECT REFUSAL DOES NOT BECOME A TOAST, unlike every other write in
 * this file, and that is a deliberate exception rather than an oversight. The two
 * refusals this surface actually produces are `ErrFeedTaken` ("the promotions tab
 * already feeds Shopping") and `ErrAreaNameTaken` — both of them are answers about
 * the CARD the user is editing, and both are things they are about to fix in the
 * control that is still on screen. A strip at the bottom of the window that
 * outlives the form is the wrong place for either.
 *
 * `refusal` is null when the failure was NOT a per-object refusal — a transport
 * error, a method error, being logged out. Those are not about the card, and they
 * DO toast, through the same path everything else uses.
 */
export interface AreaWrite {
  ok: boolean;
  refusal: string | null;
  /** The created area — `createArea` only. */
  area?: Area;
}

/**
 * What became of `Setup/apply` (#446).
 *
 * ⚠ `busy` IS NOT A FAILURE, and folding it into one would be the worst possible
 * answer: a run is already draining, and telling the user the import failed would
 * invite them to start it again. The server refuses that with `setupRunBusy`
 * precisely so a client can show the run that IS going instead — which is why
 * this is a value the caller renders rather than a toast (core/actions.ts's
 * day-one section says more).
 */
export type SetupApplyOutcome =
  | { kind: "started"; runId: Id; counts: SetupCounts }
  | { kind: "busy" }
  | { kind: "failed"; message: string };

/** How the app is told about a mutation that the server refused, or that
 * finally landed. The web shell renders these as toasts. */
export interface ActionEvents {
  onError?: (message: string) => void;
  onInfo?: (message: string) => void;
}

/**
 * What became of a send.
 *
 * `queued` is the outcome #62 adds, and it is a THIRD state that callers must not
 * fold into either of the other two: the message has not been sent, and it has not
 * been lost. Reporting it as sent would be a lie about mail; reporting it as a
 * failure would invite the user to write it again, and then it goes twice.
 */
export interface SendOutcome {
  /** The draft's server-assigned id — null for a queued send, which has none yet. */
  emailId: Id | null;
  submissionId: Id | null;
  /** The server could not be reached; the send is held for replay. */
  queued?: boolean;
  /**
   * The server REFUSED the submission (or it could not be scheduled): the message
   * has not gone and is not held, only the send-created draft `emailId` exists.
   * A FOURTH state, not "sent" (#692) — callers keep the composer open.
   */
  refused?: boolean;
  /**
   * WHEN THE MESSAGE ACTUALLY GOES (#375 increment 6): the submission's own
   * `sendAt`, as the server set it.
   *
   * For an ordinary send that is now + the server's Undo-Send hold window, and
   * it is therefore the DEADLINE of the undo offer — the one number that makes
   * the countdown on the send toast a fact rather than a client-side guess at a
   * window this client does not configure. Null when there is no submission to
   * speak of (a draft-only save, a send the queue is holding).
   */
  sendAt?: string | null;
  /**
   * SEND & ARCHIVE (#648): true when the server echoed `archiveOnSuccess` on the
   * submission, i.e. it WILL archive the conversation once the message is
   * actually sent. Until then the conversation stays where it is, so the toast
   * says "will be archived", never "archived".
   */
  archivePending?: boolean;
}

/**
 * What became of an autosave (#448).
 *
 * `reason` is a SHORT phrase, not a sentence: the footer prints it after
 * `Not saved · `, so a capitalised sentence with a full stop would read as two
 * statements glued together.
 *
 * `retiredPrevious` is the honest half. `Email/set` can refuse the create and
 * still carry out the destroy in the same transaction (internal/api/email.go runs
 * the two loops independently), so a failed save may nonetheless have retired the
 * previous version. The caller must forget that id — a later destroy naming it
 * would be aimed at a draft the server no longer has.
 */
export type DraftSaveOutcome =
  | { ok: true; draftId: Id; retiredPrevious: boolean }
  | { ok: false; reason: string; retiredPrevious: boolean };

/**
 * True for the failures that mean "we do not know what the server did, or it never
 * heard us" — as opposed to a server that answered and said no.
 *
 * Only these are queueable. A `forbiddenFrom` must reach the user now, while the
 * compose window is still open and the From line can be changed; holding it for a
 * replay that will be refused identically would be a queue that pretends.
 */
function isUnreachable(err: unknown): boolean {
  return err instanceof JmapTransportError || err instanceof JmapAuthError;
}

/** Email/set caps objects per call (session `maxObjectsInSet`, 500). Bulk
 * actions over a big selection are chunked well under it. */
const SET_CHUNK = 200;

/**
 * The most messages ONE archive-all will move (#375).
 *
 * A cap rather than "everything", because a drawer is unbounded by design — the
 * sweep is what keeps it from being — and an action that walks an unbounded set
 * is a foot-gun. What makes the cap honest is that `archiveDrawer` REPORTS it:
 * `bound: true` says the drawer still holds mail, so a partial sweep is never
 * presented as a finished one.
 */
const ARCHIVE_ALL_CAP = 5_000;

/** How many ids one Email/query collects while gathering a drawer. Well under
 * the server's own query cap, and it means a 5,000-message drawer is ten reads
 * rather than a hundred. */
const ARCHIVE_ALL_PAGE = 500;

/** Retry backoff for a queued action whose replay hit the network, not a "no". */
const BACKOFF_MS = [1_000, 4_000, 15_000, 60_000];

/** Which ClientPreferences row this client owns. The web app is one family;
 * the phone and desktop rows exist and are none of its business. */
export const WEB_CLIENT_FAMILY = "web";

/** What a client may change about its own appearance — every property
 * `updateClientPreferences` accepts, and no other. `id` and `family` are
 * immutable server-side and are absent here so a patch cannot name them. */
export type ClientPreferencesPatch = Partial<
  Pick<
    ClientPreferences,
    "theme" | "density" | "messageOpens" | "paneSplitPct" | "textSize" | "rowLayout"
  >
>;

export class MailActions {
  private readonly client: JmapClient;
  private readonly store: MailStore;
  private readonly queue: OfflineActionQueue;
  private readonly sync: SyncEngine;
  private readonly events: ActionEvents;
  /** The drain in flight, or null. A PROMISE rather than a boolean so a
   * concurrent caller can await the one already running (see drain). */
  private draining: Promise<void> | null = null;
  private retryTimer: ReturnType<typeof setTimeout> | null = null;
  /** The coalescing state behind saveClientPreferences' debounce — see there. */
  private prefsTimer: ReturnType<typeof setTimeout> | null = null;
  private prefsPending: ClientPreferencesPatch | null = null;
  /** The store's ids for the writes folded into `prefsPending` (#562). */
  private prefsPendingSeqs: number[] = [];
  private prefsWaiters: ((ok: boolean) => void)[] = [];

  constructor(
    client: JmapClient,
    store: MailStore,
    queue: OfflineActionQueue,
    sync: SyncEngine,
    events: ActionEvents = {},
  ) {
    this.client = client;
    this.store = store;
    this.queue = queue;
    this.sync = sync;
    this.events = events;
  }

  /** Re-publishes the queue depth into the store (the UI shows it). */
  private async publishQueue(): Promise<void> {
    this.store.setQueue(await this.queue.list());
  }

  /** Loads any actions left over from a previous session and drains them. */
  async resume(): Promise<void> {
    await this.publishQueue();
    void this.drain();
  }

  // -- the user-facing verbs -------------------------------------------------

  async setSeen(ids: Id[], seen: boolean): Promise<void> {
    await this.keyword(ids, KEYWORD_SEEN, seen, seen ? "Marked read" : "Marked unread");
  }

  async setFlagged(ids: Id[], flagged: boolean): Promise<void> {
    await this.keyword(ids, KEYWORD_FLAGGED, flagged, flagged ? "Starred" : "Unstarred");
  }

  /**
   * Pins/unpins messages (spec §6, #51).
   *
   * Goes through the offline QUEUE, like the flags: pinning is a frequent list
   * action and should survive being offline, and it is an Email/set patch, so it
   * merges into the same batch as a star or a mark-read on the same message.
   */
  async setPinned(ids: Id[], pinned: boolean): Promise<void> {
    if (ids.length === 0) return;
    this.store.applyPin(ids, pinned);
    const patch: PatchObject = { isPinned: pinned };
    for (const id of ids) {
      await this.queue.enqueue({
        accountId: this.store.getState().accountId,
        entity: "Email",
        op: "update",
        targetId: id,
        payload: patch,
        label: pinned ? "Pinned" : "Unpinned",
      });
    }
    await this.publishQueue();
    void this.drain();
  }

  /**
   * Snoozes messages until `wake` (an ISO timestamp), or un-snoozes them when
   * `wake` is null (spec §6, #51).
   *
   * Through the offline QUEUE, like the pin: it is an Email/set patch, so it
   * merges into the same batch as a star or a mark-read on the same message.
   * The wake time is resolved to an ABSOLUTE instant here, at the moment the
   * user acts — a queued "tomorrow morning" that was re-interpreted whenever the
   * queue happened to drain would mean something different from what was asked.
   */
  async setSnoozed(ids: Id[], wake: string | null): Promise<void> {
    if (ids.length === 0) return;
    this.store.applySnooze(ids, wake);
    const patch: PatchObject = { snoozedUntil: wake };
    for (const id of ids) {
      await this.queue.enqueue({
        accountId: this.store.getState().accountId,
        entity: "Email",
        op: "update",
        targetId: id,
        payload: patch,
        label: wake ? "Snoozed" : "Un-snoozed",
      });
    }
    await this.publishQueue();
    void this.drain();
  }

  /**
   * Corrects the model's importance verdict (spec §8, #154).
   *
   * Sent DIRECTLY rather than through the offline queue, for the same reason
   * muting is: that queue speaks Email/set only. It is also not a property
   * patch — Triage/correct records a feedback event, and what comes back is a
   * RE-SCORED model rather than the value we sent, so there is nothing for the
   * queue's "did the server agree" reconciliation to compare against.
   *
   * The local override applies immediately so a row leaves the important bucket
   * the moment you say it does not belong there; the authoritative score arrives
   * on the next poll.
   */
  async setImportant(ids: Id[], important: boolean): Promise<boolean> {
    if (ids.length === 0) return false;
    const before = this.store.applyImportance(ids, important);
    const corrections: Record<string, "important" | "notImportant"> = {};
    for (const id of ids) corrections[id] = important ? "important" : "notImportant";
    try {
      const res = await this.client.triageCorrect({
        accountId: this.store.getState().accountId,
        corrections,
      });
      const refused = Object.keys(res.notCorrected ?? {});
      if (refused.length > 0) {
        this.store.revertImportance(refused);
        this.events.onError?.(
          res.notCorrected?.[refused[0] as string] ?? "That correction was not recorded.",
        );
      }
      void this.sync.poll();
      return refused.length === 0;
    } catch (err) {
      this.store.rollback(before);
      this.events.onError?.(describeError(err, "Could not record that correction"));
      return false;
    }
  }

  /**
   * Records that the user READ a message (#842): it stayed in the reading pane
   * for the open dwell. Best-effort and silent — the signal teaches the priority
   * model what matters, and a lost one costs nothing the user can see, so a
   * failure is never surfaced as an error and is not queued for retry.
   */
  async recordOpen(id: Id): Promise<void> {
    try {
      await this.client.triageOpen({ accountId: this.store.getState().accountId, emailIds: [id] });
    } catch {
      // Deliberately ignored: see above.
    }
  }

  /**
   * Mutes/unmutes a THREAD (spec §6, #51).
   *
   * Sent DIRECTLY rather than through the offline queue, which is deliberate and
   * worth stating: that queue speaks Email/set only (flushChunk builds exactly
   * one Email/set call), so routing a Thread/set through it would mean
   * generalizing the retry path — the machinery that keeps the app usable
   * offline — for one infrequent, low-stakes action. Label and submission
   * mutations already take this direct route for the same reason.
   *
   * The cost, honestly: muting while offline fails instead of queueing. It
   * surfaces as an error rather than being silently dropped.
   */
  async setThreadMuted(threadId: Id, muted: boolean): Promise<boolean> {
    try {
      const res = await this.client.threadSet({
        accountId: this.store.getState().accountId,
        update: { [threadId]: { isMuted: muted } },
      });
      if (res.notUpdated?.[threadId]) {
        this.events.onError?.(
          res.notUpdated[threadId]?.description ?? "That conversation could not be muted.",
        );
        return false;
      }
      this.store.applyThreadMuted(threadId, muted);
      return true;
    } catch (err) {
      this.events.onError?.(err instanceof Error ? err.message : String(err));
      return false;
    }
  }

  /** Adds/removes a LABEL. Labels are JMAP keywords on the Email (spec §13); the
   * atom is the server-assigned `Label.keyword`. */
  async setLabel(ids: Id[], keywordAtom: string, present: boolean): Promise<void> {
    // Filing a message ANSWERS every open question about it — the server does
    // exactly this (ApplyEmailUpdate → ClearLabelSuggestions), and mirroring it
    // locally is what takes the row out of the needs-a-category queue on the
    // keystroke instead of at the next poll (#176).
    //
    // Only on ADD. Removing a label is not an answer; it is the withdrawal of
    // one, and it must not close a question the user never addressed.
    if (present) this.store.applyAnswered(ids);
    await this.keyword(ids, keywordAtom, present, present ? "Label added" : "Label removed");
  }

  /**
   * Adds/removes a label BY ID, over `labelIds/<id>` (#704) — the only way to
   * reach a LOCAL-ONLY label (every area's own label, ADR-031), which has no
   * keyword atom for `setLabel` to send.
   *
   * The same queued/optimistic/rollback shape as `setLabel`: the overlay shows
   * the change on the keystroke (store.applyLabelId), the queue carries it
   * offline, and a refusal drops exactly this delta (`revert` → revertLabelId).
   *
   * WHY MIRRORED LABELS STILL GO THROUGH `setLabel` (see `applyLabel`). The
   * server would accept a mirrored label here and write it back identically
   * (internal/api email_label_ids_integration_test.go proves the intents
   * match), but the CLIENT would not behave identically: everything that reads
   * a mirrored label locally — the chips, `labelKeywords`, keyword-filtered
   * lists — reads the KEYWORD overlay, and an id-path toggle would leave those
   * showing the old state until the next poll. So the atom path stays for the
   * labels that have an atom, unchanged, and this one exists for the labels
   * that do not.
   */
  async setLabelById(ids: Id[], labelId: Id, present: boolean): Promise<void> {
    if (ids.length === 0) return;
    // Filing answers the open questions, exactly as setLabel says.
    if (present) this.store.applyAnswered(ids);
    this.store.applyLabelId(ids, labelId, present);
    const patch: PatchObject = { [`labelIds/${labelId}`]: present ? true : null };
    for (const id of ids) {
      await this.queue.enqueue({
        accountId: this.store.getState().accountId,
        entity: "Email",
        op: "update",
        targetId: id,
        payload: patch,
        label: present ? "Label added" : "Label removed",
      });
    }
    await this.publishQueue();
    void this.drain();
  }

  /** Adds/removes a label whichever way it is reachable: its keyword atom when
   * it has one (`setLabel`, unchanged), its id when it is local-only
   * (`setLabelById`). The one entry point a menu should call. */
  async applyLabel(ids: Id[], label: Label, present: boolean): Promise<void> {
    if (label.keyword) await this.setLabel(ids, label.keyword, present);
    else await this.setLabelById(ids, label.id, present);
  }

  /**
   * Applies a PER-MESSAGE set of label changes (#704) — one Email/set patch per
   * message carrying every add and remove for it, so a re-file is one update
   * per message rather than a remove and an add that could land apart.
   *
   * Each label rides the path `applyLabel` would pick (atom when it has one,
   * id when it is local), with the matching optimistic overlay, so a refusal
   * rolls back key by key through `revert`. Returns nothing: like the other
   * verbs it resolves once the intents are QUEUED.
   */
  async relabel(changes: LabelChange[], summary: string): Promise<void> {
    const real = changes.filter((c) => c.add.length + c.remove.length > 0);
    if (real.length === 0) return;
    const answered = real.filter((c) => c.add.length > 0).map((c) => c.id);
    if (answered.length > 0) this.store.applyAnswered(answered);
    for (const c of real) {
      const patch: PatchObject = {};
      for (const [labels, present] of [
        [c.remove, false],
        [c.add, true],
      ] as const) {
        for (const l of labels) {
          if (l.keyword) {
            this.store.applyKeyword([c.id], l.keyword, present);
            patch[`keywords/${l.keyword}`] = present ? true : null;
          } else {
            this.store.applyLabelId([c.id], l.id, present);
            patch[`labelIds/${l.id}`] = present ? true : null;
          }
        }
      }
      await this.queue.enqueue({
        accountId: this.store.getState().accountId,
        entity: "Email",
        op: "update",
        targetId: c.id,
        payload: patch,
        label: summary,
      });
    }
    await this.publishQueue();
    void this.drain();
  }

  /**
   * Takes back a batch answer (#182): strips the label the batch applied and
   * returns those messages to the needs-a-category queue.
   *
   * THE ORDER IS THE WHOLE FUNCTION. Clearing the overlay flag alone looks like
   * it should work and does nothing visible: by the time an undo is possible the
   * queue has been re-queried WITHOUT these messages — they carry a label — so
   * there is no row for the flag to reveal. And refreshing before the removal has
   * landed just re-fetches the same list, because `Rule/induce`'s cousin problem
   * applies here too: the server answers about what it has, not about what is
   * still sitting in a client queue. So: clear the local flag, remove the label,
   * WAIT for it to settle, then ask for a fresh list.
   *
   * Returns false when the removal did not reach the server (offline, or a
   * transport failure being retried). The caller must not then report the batch
   * as undone — the label is still on the mail, and the rows are still gone.
   *
   * It cannot restore suggestions the original labelling cleared: a user-applied
   * label calls ClearLabelSuggestions server-side and those rows are deleted.
   * The message comes back as merely uncategorised, which is enough for the
   * queue to ask about it again.
   */
  async undoBatchLabel(ids: Id[], keywordAtom: string): Promise<boolean> {
    if (ids.length === 0) return true;
    this.store.clearAnswered(ids);
    await this.setLabel(ids, keywordAtom, false);
    const settled = await this.settle();
    if (settled) await this.sync.refreshView();
    return settled;
  }

  /**
   * Waits until every queued mutation has reached the server, and reports
   * whether it got there (#176).
   *
   * The verbs return as soon as an intent is QUEUED, which is the right shape
   * for a keystroke — nothing should block on a round trip. It is the wrong
   * shape for the batch-triage flow, which immediately asks `Rule/induce` what a
   * labelling implies, and induce learns from the PERSISTED hand-applied labels
   * (internal/api/rule_induce.go). Asked against the queued-but-undrained state
   * it sees no new examples, falls under minInduceExamples and returns no
   * candidates — which reads as "these have too little in common". A false
   * diagnosis of the very number the flow exists to produce.
   *
   * FALSE means the writes are still queued (offline, or a transport failure
   * being retried). A caller must not go on to state what the labelling implies:
   * the server has not seen it, and anything it says would be about a different
   * set of examples.
   */
  async settle(): Promise<boolean> {
    await this.drain();
    // The queue is the authority, not the promise: drain swallows transport
    // errors and leaves failed actions queued for retry, so "settled" is an
    // empty queue rather than a resolved drain.
    const pending = await this.queue.list();
    return pending.length === 0;
  }

  /**
   * "None of these" (#176): closes a message's open questions without filing it.
   *
   * NOT queued through the offline action queue, unlike a label. The queue
   * replays Email/set patches, and a dismissal is not one — but the deeper
   * reason is that a dismissal is the cheapest thing in this surface to redo and
   * the most confusing thing to have replayed later. If it fails, the questions
   * are still there and the next poll simply puts the row back, which is a
   * truthful outcome; a dismissal that lands hours later, against a message the
   * user has since labelled, is not.
   *
   * Optimistic regardless: the row leaves now, and the retirement rule in
   * putEmails restores it if the server disagrees.
   */
  async dismissCategory(ids: Id[]): Promise<Id[]> {
    if (ids.length === 0) return [];
    this.store.applyAnswered(ids);
    try {
      const res = await this.client.dismissCategory({
        accountId: this.store.getState().accountId,
        ids,
      });
      // The dismissal appends an Email/updated row per changed message, so the
      // next poll re-runs the query and the rows are gone for real. Asking for
      // that poll now rather than waiting for the timer is what makes the
      // optimistic flag short-lived instead of load-bearing.
      void this.sync.poll();
      // What actually CHANGED, so an undo offer covers exactly that. A message
      // already dismissed is reported by neither list, and offering to undo it
      // would restore something this call did not remove.
      return res.dismissed ?? [];
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not dismiss those messages"));
      return [];
    }
  }

  /**
   * Puts dismissed messages back in the needs-a-category queue (#176).
   *
   * It does NOT restore suggestions the dismissal deleted — the server cannot,
   * and inventing confidences to fill the gap is exactly the kind of unmeasured
   * assertion this surface exists to avoid. The message returns as merely
   * uncategorised, which is enough for it to be asked about again.
   */
  async undoDismissCategory(ids: Id[]): Promise<void> {
    if (ids.length === 0) return;
    try {
      await this.client.dismissCategory({
        accountId: this.store.getState().accountId,
        ids,
        undo: true,
      });
      void this.sync.poll();
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not undo that"));
    }
  }

  /**
   * Lifts — or restores — the malware scanner's quarantine on ONE projected
   * attachment part (#396/#412, ADR-027 §8/§14).
   *
   * NOT queued and NOT optimistic, for the same reason `setThreadMuted` and
   * `setMailboxSyncScope` are direct: there is no local overlay this chip
   * renders ahead of the server to be right about, and replaying a stale
   * release (or revoke) after coming back online would silently reopen — or
   * reclose — a door on content the user is no longer looking at. It fails
   * loudly instead, via the ordinary error toast.
   *
   * Refreshes the whole THREAD rather than patching one Email in the store:
   * the server's decision is keyed on the part's content digest and can affect
   * every copy of those exact bytes across the mailbox, not just `emailId`
   * (emailReleaseAttachment's own doc comment) — and `sync.loadThread` is the
   * one call that re-asks `Email/get` with `attachmentScan` among its body
   * properties, which is what the chip actually re-renders from.
   */
  async releaseAttachment(
    threadId: Id,
    emailId: Id,
    partId: string,
    opts: { revoke?: boolean } = {},
  ): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    try {
      await this.client.emailReleaseAttachment({
        accountId,
        id: emailId,
        partId,
        ...(opts.revoke ? { revoke: true } : {}),
      });
      await this.sync.loadThread(threadId);
      return true;
    } catch (err) {
      this.events.onError?.(
        describeError(
          err,
          opts.revoke ? "Could not re-quarantine that file" : "Could not release that file",
        ),
      );
      return false;
    }
  }

  /**
   * THE KEY-CHANGED DECISION (#47, ADR-039): trust the key a sender has signed
   * with from now on, or keep the old one. One server call; then the open thread
   * is reloaded when `threadId` is given, because the signature line re-renders
   * from `pgpSignature` and must not go on showing the question it just answered.
   * True when the server took the decision; on failure the error toast is raised
   * and the line is left exactly as it was.
   */
  async decideContactKey(
    address: string,
    fingerprint: string,
    decision: "trust" | "keep",
    threadId?: Id,
  ): Promise<boolean> {
    try {
      await this.client.contactKeyDecide(address, fingerprint, decision);
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not save that decision"));
      return false;
    }
    if (threadId) {
      try {
        await this.sync.loadThread(threadId);
      } catch {
        // The decision is saved; a stale line is corrected by the next open.
      }
    }
    return true;
  }

  /**
   * AFTER AN OWN-KEY CHANGE (#796): binding, retiring, revoking or extending a
   * key moves `Identity.signingKeyId` server-side, with Identity change rows.
   * One poll picks them up now rather than at the next tick, so "Will sign" in
   * the composer and "Signs as" on the Keys page agree with what just happened.
   */
  refreshAfterKeyChange(): void {
    void this.sync.poll();
  }

  /**
   * IMPORT A CORRESPONDENT'S PUBLIC KEY (#47, ADR-038). With `dryRun` it only
   * checks the key and returns what the dialog must show BEFORE the user confirms
   * (the fingerprint and the user ID that names the address); nothing is stored.
   * Without it the key is stored, becomes the pin for the address, and - when
   * `threadId` is given - that thread is reloaded so the signature line is judged
   * again against the new key.
   *
   * Never raises a toast: the dialog is open and shows the refusal where the user
   * is looking, in plain words (core/keyImport.ts).
   */
  async importContactKey(
    address: string,
    publicKey: string,
    opts: { dryRun?: boolean; threadId?: Id } = {},
  ): Promise<ImportKeyOutcome> {
    if (publicKey.trim() === "") return { ok: false, error: "empty" };
    let res: Awaited<ReturnType<JmapClient["contactKeyImport"]>>;
    try {
      res = await this.client.contactKeyImport(address, publicKey, opts.dryRun ? { dryRun: true } : {});
    } catch (err) {
      return { ok: false, error: importKeyErrorOf(err) };
    }
    if (!opts.dryRun && opts.threadId) {
      try {
        await this.sync.loadThread(opts.threadId);
      } catch {
        // The key is saved; a stale line is corrected by the next open.
      }
    }
    return { ok: true, fingerprint: res.fingerprint, userId: res.userId };
  }

  /**
   * ANSWER A CALENDAR INVITATION (#725): Accept / Maybe / Decline.
   *
   * One call. The server builds the iTIP REPLY from the bytes it holds (this
   * client never sees a raw .ics), sends it FROM the address the invitation was
   * delivered to, and submits it through the ordinary submission path — so the
   * result carries a real submission whose `sendAt` is the Undo Send deadline,
   * exactly like a send from the composer. The thread is reloaded so the card
   * reads "You accepted" from the server's record rather than from a guess.
   *
   * ⚠ NOT QUEUED OFFLINE, deliberately unlike sendMessage: an answer replayed
   * later, after the event changed or was cancelled, would be an answer to a
   * question nobody is asking any more. A failure says so and the user can
   * press the button again.
   *
   * Returns null on any failure, having said why (the server's sentence for a
   * refusal — "this invitation does not name a valid organiser…" — is shown
   * verbatim, because it is written for the person who clicked).
   */
  async replyToInvite(
    threadId: Id,
    emailId: Id,
    partstat: "accepted" | "tentative" | "declined",
  ): Promise<{ submissionId: Id | null; sendAt: string | null } | null> {
    const accountId = this.store.getState().accountId;
    let res: Awaited<ReturnType<JmapClient["emailReplyToInvite"]>>;
    try {
      res = await this.client.emailReplyToInvite({ accountId, emailId, partstat });
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not answer the invitation"));
      return null;
    }
    const submission = res.submission;
    // Into the store so the Outbox — and a failure, should the provider refuse
    // it — shows the reply like any other send.
    if (submission) this.store.putSubmissions([submission]);
    await this.sync.loadThread(threadId, emailId);
    void this.sync.poll();
    return { submissionId: submission?.id ?? null, sendAt: submission?.sendAt ?? null };
  }

  /** Undo an answer inside its Undo Send window (#725): cancel the submission
   * that carries the REPLY — the server then stops counting the answer — and
   * reload the thread so the card stops saying "You accepted". */
  async withdrawInviteReply(threadId: Id, emailId: Id, submissionId: Id): Promise<boolean> {
    const ok = await this.cancelSubmission(submissionId);
    if (ok) await this.sync.loadThread(threadId, emailId);
    return ok;
  }

  private async keyword(ids: Id[], kw: string, present: boolean, label: string): Promise<void> {
    if (ids.length === 0) return;
    this.store.applyKeyword(ids, kw, present);
    const patch: PatchObject = { [`keywords/${kw}`]: present ? true : null };
    for (const id of ids) {
      await this.queue.enqueue({
        accountId: this.store.getState().accountId,
        entity: "Email",
        op: "update",
        targetId: id,
        payload: patch,
        label,
      });
    }
    await this.publishQueue();
    void this.drain();
  }

  /** Archive = "done" (spec §6). Expressed as a move into the Archive folder;
   * the server reads only the ROLE of the mailbox id in the patch and moves the
   * message in every account it lives in (internal/api/email.go setMailbox). */
  async archive(ids: Id[]): Promise<boolean> {
    const archiveBox = Object.values(this.store.getState().mailboxes).find(
      (m) => m.role === "archive",
    );
    if (!archiveBox) {
      this.events.onError?.(
        "This account has no Archive folder, so there is nothing to archive into.",
      );
      return false;
    }
    if (ids.length === 0) return false;
    this.store.applyMove(ids, "archive");
    const patch: PatchObject = { [`mailboxIds/${archiveBox.id}`]: true };
    for (const id of ids) {
      await this.queue.enqueue({
        accountId: this.store.getState().accountId,
        entity: "Email",
        op: "update",
        targetId: id,
        payload: patch,
        label: "Archived",
      });
    }
    await this.publishQueue();
    void this.drain();
    return true;
  }

  /** Delete = move to Trash (soft, ADR-009). `Email/set destroy` IS the trash
   * move on this server — a draft is retired, anything else is trashed. */
  async trash(ids: Id[]): Promise<void> {
    if (ids.length === 0) return;
    this.store.applyMove(ids, "trash");
    for (const id of ids) {
      await this.queue.enqueue({
        accountId: this.store.getState().accountId,
        entity: "Email",
        op: "destroy",
        targetId: id,
        payload: null,
        label: "Moved to Trash",
      });
    }
    await this.publishQueue();
    void this.drain();
  }

  /** Restore from Trash. The server puts a restored message in Archive
   * (internal/store: Restore ⇒ MoveToRole "archive"), so the overlay says so
   * too — the UI must not claim it went back to the Inbox. */
  async untrash(ids: Id[]): Promise<boolean> {
    const trashBox = Object.values(this.store.getState().mailboxes).find((m) => m.role === "trash");
    if (!trashBox) {
      this.events.onError?.("This account has no Trash folder.");
      return false;
    }
    if (ids.length === 0) return false;
    this.store.applyMove(ids, "archive");
    const patch: PatchObject = { [`mailboxIds/${trashBox.id}`]: false };
    for (const id of ids) {
      await this.queue.enqueue({
        accountId: this.store.getState().accountId,
        entity: "Email",
        op: "update",
        targetId: id,
        payload: patch,
        label: "Restored from Trash",
      });
    }
    await this.publishQueue();
    void this.drain();
    return true;
  }

  /**
   * PUTS ARCHIVED MAIL BACK IN THE INBOX (#375 increment 6) — the inverse of
   * `archive`, and the reason an archive can be undone at all.
   *
   * Exactly the mechanism `archive` uses, pointed the other way: the patch
   * names the INBOX mailbox's id and the server reads only its ROLE, moving the
   * message back in every account it lives in (internal/api/email.go
   * setMailbox). So a multi-account conversation comes back whole rather than
   * in the one account whose id we happened to send.
   *
   * ⚠ IT RESTORES TO THE INBOX, NOT TO WHERE THE MESSAGE WAS. Nothing here
   * remembers a previous mailbox, and inventing one would be worse than not
   * having it: the only honest inverse of "this is done" is "this is not done",
   * which is the inbox. A message archived from a folder therefore comes back
   * to the inbox — where an unactioned message belongs, and where the desk will
   * show it again.
   *
   * TWO WAYS OUT, and the difference is the size of the set:
   *
   *   * the default is the offline QUEUE, one action per message, exactly like
   *     `archive` — so undoing a mis-keyed archive works with no connection,
   *     which is when a mis-key is most annoying;
   *   * `direct` sends the sets now in bounded chunks and needs a connection.
   *     That is the inverse of `archiveDrawer`, which is thousands of messages:
   *     queueing those would be thousands of durable rows and thousands of
   *     replays, which is the client loop `archiveDrawer` exists to avoid.
   */
  async restoreToInbox(ids: Id[], opts: { direct?: boolean } = {}): Promise<boolean> {
    const st = this.store.getState();
    const inbox = Object.values(st.mailboxes).find((m) => m.role === "inbox");
    if (!inbox) {
      this.events.onError?.("This account has no Inbox, so there is nowhere to put it back.");
      return false;
    }
    if (ids.length === 0) return false;
    const patch: PatchObject = { [`mailboxIds/${inbox.id}`]: true };

    if (opts.direct) {
      if (st.conn === "offline") {
        this.events.onError?.(
          "Putting a whole drawer back needs a connection — it is too much to hold until you are back online.",
        );
        return false;
      }
      this.store.applyMove(ids, "inbox");
      const { changed, failed } = await this.moveInChunks(ids, patch, {
        rest: "Could not put the rest of that drawer back",
        part: "Could not put part of that drawer back",
      });
      // Same reason `archiveDrawer` re-asks: the header counts and the drawer's
      // own row are the server's numbers, and a few thousand messages just moved.
      if (changed.length > 0) void this.sync.refreshDesk();
      // A PER-ID REFUSAL IS SILENT INSIDE moveInChunks — the sweep's caller
      // reports those in its own toast ("188 could not be archived"), and an
      // undo has no such toast to put them in. So it says so here, or the user
      // presses Undo, watches most of the mail come back, and is never told
      // that some of it did not.
      if (failed > 0) {
        this.events.onError?.(
          failed === 1
            ? "1 message could not be put back."
            : `${failed} messages could not be put back.`,
        );
      }
      return failed === 0 && changed.length === ids.length;
    }

    this.store.applyMove(ids, "inbox");
    for (const id of ids) {
      await this.queue.enqueue({
        accountId: st.accountId,
        entity: "Email",
        op: "update",
        targetId: id,
        payload: patch,
        label: "Restored to Inbox",
      });
    }
    await this.publishQueue();
    void this.drain();
    return true;
  }

  /**
   * "NOT JUNK" (#528, spec §6): rescue a false positive from Junk into the Inbox.
   *
   * The one write spec §6 makes MANDATORY on a folder that is otherwise
   * read-only, and the move is itself the ham signal to the provider — so this
   * is a real IMAP move, through the same queue as `archive`, and it works
   * offline the same way.
   *
   * Shown as an optimistic move to `inbox`, so the row leaves the Junk list at
   * once and appears in the Inbox list (store.visibleIds). The patch is
   * core/junk.ts `notJunkPatch`, whose comment says why it names the inbox
   * explicitly: that key is what lets a refusal roll the move back.
   *
   * ⚠ THERE IS NO INVERSE HERE, DELIBERATELY. Undoing a rescue would be marking
   * the message as junk, which the server refuses (`keywords/$junk: true` is the
   * provider's verdict, not ours) and which as a MOVE into Junk is a separate
   * decision #528 leaves open. An undo that could not do what it says is worse
   * than none; a mis-click is one Delete away.
   */
  async notJunk(ids: Id[]): Promise<boolean> {
    const st = this.store.getState();
    const inbox = Object.values(st.mailboxes).find((m) => m.role === "inbox");
    if (!inbox) {
      this.events.onError?.("This account has no Inbox, so there is nowhere to move it to.");
      return false;
    }
    if (ids.length === 0) return false;
    this.store.applyMove(ids, "inbox");
    const patch = notJunkPatch(inbox.id);
    for (const id of ids) {
      await this.queue.enqueue({
        accountId: st.accountId,
        entity: "Email",
        op: "update",
        targetId: id,
        payload: patch,
        label: "Not junk",
      });
    }
    await this.publishQueue();
    void this.drain();
    return true;
  }

  /**
   * MOVES MAIL INTO JUNK (#59): the "move their existing mail too" half of
   * blocking a sender, and the clean-up view's block.
   *
   * An ORDINARY move to the junk-role folder — the patch names this login's
   * Junk mailbox and the server reads its ROLE, moving the message in every
   * account it lives in and writing the move back to IMAP (internal/api/email.go
   * setMailbox), exactly as `archive` does for Archive. It does NOT set
   * `$junk`: that keyword is the provider's verdict, which Mailroom never
   * claims (spec §6), and the server refuses it.
   *
   * The inverse is `restoreToInbox` — a message in Junk is "not where it
   * belongs", and the inbox is where an unactioned message belongs.
   */
  async moveToJunk(ids: Id[]): Promise<boolean> {
    const st = this.store.getState();
    const junk = Object.values(st.mailboxes).find((m) => m.role === "junk");
    if (!junk) {
      this.events.onError?.("This account has no Junk folder, so there is nowhere to move it to.");
      return false;
    }
    if (ids.length === 0) return false;
    this.store.applyMove(ids, "junk");
    const patch: PatchObject = { [`mailboxIds/${junk.id}`]: true };
    for (const id of ids) {
      await this.queue.enqueue({
        accountId: st.accountId,
        entity: "Email",
        op: "update",
        targetId: id,
        payload: patch,
        label: "Moved to Junk",
      });
    }
    await this.publishQueue();
    void this.drain();
    return true;
  }

  /**
   * The user's markers on a contact (#59): VIP, blocked, and where a block
   * sends new mail. NOT queued, for `updateDeskPreferences`' reason: a block
   * decides where mail GOES, and one replayed hours later would junk mail the
   * user has since decided to read. It fails loudly instead, and resolves true
   * only when the server said the contact changed.
   */
  async setContactMarkers(
    contactId: Id,
    patch: { isVip?: boolean; isBlocked?: boolean; blockDestination?: "junk" | "trash"; isSaved?: boolean },
    label = "Could not change that sender",
  ): Promise<boolean> {
    return (await this.setContactsMarkers([contactId], patch, label)).length === 1;
  }

  /**
   * The same patch on several contacts in ONE Contact/set (#53: blocking every
   * address a domain or a list has used). Resolves to the ids the server said
   * it changed; any refusal is reported, and the caller acts on — and offers
   * an Undo for — exactly the ones that changed.
   */
  async setContactsMarkers(
    contactIds: Id[],
    patch: { isVip?: boolean; isBlocked?: boolean; blockDestination?: "junk" | "trash"; isSaved?: boolean },
    label = "Could not change that sender",
  ): Promise<Id[]> {
    if (contactIds.length === 0) return [];
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.contactSet({
        accountId,
        update: Object.fromEntries(contactIds.map((id) => [id, patch])),
      });
      const changed = contactIds.filter((id) => res.updated && id in res.updated);
      const refused = contactIds.map((id) => res.notUpdated?.[id]).find((r) => r !== undefined);
      if (refused) this.events.onError?.(describeSetError(refused, label));
      else if (changed.length < contactIds.length) {
        this.events.onError?.(`${label}: the server did not say it changed.`);
      }
      return changed;
    } catch (err) {
      this.events.onError?.(describeError(err, label));
      return [];
    }
  }

  /**
   * Applies ONE Email/set patch to many messages, in chunks the server will
   * take, rolling back locally whatever it refuses (#375).
   *
   * Factored out of `archiveDrawer` when the undo landed (#375 increment 6),
   * because the inverse of an archive-all is the identical walk in the opposite
   * direction — and a second copy of it would be a second place for the
   * roll-back rules to be subtly different. The three failures it has to get
   * right are the ones a copy gets wrong:
   *
   *   * the connection drops PART WAY: everything from the current batch on is
   *     un-moved, so the local overlay has to be taken back for all of it, not
   *     just for the request that threw;
   *   * a whole call comes back with a method error: that chunk did not happen;
   *   * individual ids come back in `notUpdated`: only those did not happen.
   *
   * Returns the ids that actually MOVED. A count would be enough for the toast
   * and is not enough for the caller that has to be able to undo it.
   */
  private async moveInChunks(
    targets: Id[],
    patch: PatchObject,
    words: { rest: string; part: string },
  ): Promise<{ changed: Id[]; failed: number }> {
    const st = this.store.getState();
    const chunks: Id[][] = [];
    for (let i = 0; i < targets.length; i += SET_CHUNK) chunks.push(targets.slice(i, i + SET_CHUNK));

    const perRequest = maxCallsInRequest(st.session);
    const changed: Id[] = [];
    let failed = 0;
    for (let first = 0; first < chunks.length; first += perRequest) {
      const batch = chunks.slice(first, first + perRequest);
      const calls: Call[] = batch.map((chunk, j) => ({
        name: "Email/set",
        args: {
          accountId: st.accountId,
          update: Object.fromEntries(chunk.map((id) => [id, patch])),
        },
        // The call id carries the ABSOLUTE chunk index, which is what lets the
        // results below be matched back to the ids they were about.
        id: `s${first + j}`,
      }));
      let results;
      try {
        results = await this.client.request(calls);
      } catch (err) {
        const unsent = chunks.slice(first).flat();
        this.store.revertMove(unsent);
        failed += unsent.length;
        this.events.onError?.(describeError(err, words.rest));
        break;
      }
      for (const r of results) {
        const chunk = chunks[Number(r.id.slice(1))];
        if (!chunk) continue;
        if (r.error) {
          this.store.revertMove(chunk);
          failed += chunk.length;
          this.events.onError?.(describeError(r.error, words.part));
          continue;
        }
        const res = r.value as unknown as SetResult;
        const refused = new Set(
          Object.keys(res?.notUpdated ?? {}).filter((id) => chunk.includes(id)),
        );
        if (refused.size > 0) this.store.revertMove([...refused]);
        failed += refused.size;
        for (const id of chunk) if (!refused.has(id)) changed.push(id);
      }
    }
    return { changed, failed };
  }

  // -- the desk (#375) --------------------------------------------------------

  /**
   * Writes the login's DESK PREFERENCES — the line, the default drawer period,
   * the tray nag (#375, generalised for #374's settings page).
   *
   * ⚠ THIS USED TO BE `setLineThreshold` AND ONLY THAT, and every word of the
   * reasoning below was written for the line. It applies unchanged to the other
   * two, which is why this is one method rather than three: they are one
   * singleton record, one `set`, and one set of failure modes.
   *
   * NOT queued, and for the same reason `setMailboxSyncScope` is not: these
   * settings decide what the user SEES. A threshold replayed hours after it was
   * set would silently move a screenful of mail into drawers the user never
   * asked to fill, long after they changed their mind. It fails loudly instead.
   *
   * The store is written from the SERVER's answer — a `DeskPreferences/get` in
   * the same batch, which runs after the set (RFC 8620 §3.2: calls execute in
   * order) — rather than optimistically. An optimistic line would redraw the
   * desk at a threshold the server may have rounded (`lineThreshold` is a
   * float32 server-side) or refused outright, and the whole point of the desk is
   * that what it shows is what the server counts.
   *
   * ⚠ IT ALWAYS REFRESHES THE DESK, even for a patch that cannot move mail (the
   * tray nag). The alternative is a per-property table of what invalidates what,
   * which is a second copy of the server's semantics and the sort of thing that
   * is right until a property is added. One extra desk read on a settings write
   * nobody makes twice a minute is the cheaper mistake.
   *
   * `label` is what a refusal is reported as, so the toast says which control the
   * user was using rather than naming a JMAP type.
   */
  async updateDeskPreferences(
    patch: PatchObject,
    label = "Could not change that setting",
  ): Promise<boolean> {
    const st = this.store.getState();
    // The record's id IS the user id, which is this account's id
    // (internal/api/desk_preferences.go) — so a preference can be written before
    // the record has ever been read.
    const id = st.deskPreferences?.id ?? st.accountId;
    if (!id) {
      this.events.onError?.(`${label}: not connected yet.`);
      return false;
    }
    try {
      const results = await this.client.request([
        {
          name: "DeskPreferences/set",
          args: { accountId: st.accountId, update: { [id]: patch } },
          id: "set",
        },
        { name: "DeskPreferences/get", args: { accountId: st.accountId, ids: null }, id: "get" },
      ]);
      const setResult = results.find((r) => r.id === "set");
      if (setResult?.error) {
        this.events.onError?.(describeError(setResult.error, label));
        return false;
      }
      const res = setResult?.value as unknown as SetResult | undefined;
      const refusal = res?.notUpdated?.[id];
      if (refusal) {
        this.events.onError?.(describeSetError(refusal, label));
        return false;
      }
      if (!res?.updated || !(id in res.updated)) {
        // Neither accepted nor refused by name. Nothing is written: a set whose
        // outcome the server did not state is not evidence that it happened.
        this.events.onError?.(`${label}: the server did not say it changed.`);
        return false;
      }
      const prefs = (
        (results.find((r) => r.id === "get")?.value as unknown as GetResult<DeskPreferences> | undefined)
          ?.list ?? []
      )[0];
      if (prefs) this.store.setDeskPreferences(prefs);
      // The line decides which side of it every message is on, so every section
      // has to be re-asked — the counts and the lists both.
      void this.sync.refreshDesk();
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, label));
      return false;
    }
  }

  /**
   * Moves the LINE — the score at which mail stops being shown and starts being
   * kept (#375).
   *
   * A thin wrapper over `updateDeskPreferences`, kept because the desk's own
   * control calls it and because the RANGE CHECK belongs to this property and not
   * to the record. Validated here before anything is sent: the server refuses a
   * value outside [0,1] anyway, and a slider that fires a request per rejected
   * keystroke is just a slower way of being told no.
   */
  async setLineThreshold(value: number): Promise<boolean> {
    if (!Number.isFinite(value) || value < 0 || value > 1) {
      this.events.onError?.("The line has to be a number between 0 and 1.");
      return false;
    }
    return this.updateDeskPreferences({ lineThreshold: value }, "Could not move the line");
  }

  // -- day-one setup (#446) ---------------------------------------------------
  //
  // THE READS ARE SEPARATE FROM THE WRITE, and they fail differently on purpose.
  //
  // `setupCensus` and `setupPreview` write nothing, so a failure yields `null`
  // and the wizard says the number could not be measured rather than printing a
  // zero — "0 messages would be labelled" and "we could not ask" are different
  // facts and only one of them is a reason to press the button (#159/#176).
  //
  // `setupApply` is the only write, and its refusal is not a toast: `setupRunBusy`
  // means "a run is already going", whose remedy is to LOOK AT THAT RUN, which is
  // an answer about the panel still on screen. So the outcome is a value the
  // caller renders, in the same spirit as AreaWrite above.

  /** Every folder day-one can act on, at this window. Null when the census could
   * not be taken — never an empty folder list, which would read as "you have no
   * folders" and hide the whole first step. */
  async setupCensus(opts: { mailAccountId?: Id; windowDays?: number } = {}): Promise<
    SetupCensusResult | null
  > {
    const accountId = this.store.getState().accountId;
    try {
      return await this.client.setupCensus({
        accountId,
        ...(opts.mailAccountId ? { mailAccountId: opts.mailAccountId } : {}),
        ...(opts.windowDays ? { windowDays: opts.windowDays } : {}),
      });
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not look at your folders"));
      return null;
    }
  }

  /** What this plan would write, per folder and in total. Null on failure.
   *
   * NOT surfaced as a toast: this is called on every change to the plan, debounced,
   * and a server that is unwell would otherwise stack a toast per keystroke. The
   * step renders "the preview could not be measured" in place of the sentence,
   * which is where the user is looking. */
  async setupPreview(plan: SetupPlan): Promise<SetupCounts | null> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.setupPreview({ accountId, plan });
      return res.counts ?? null;
    } catch {
      return null;
    }
  }

  /** Creates the run and enqueues the import. See the section header for why the
   * refusal is a value rather than a toast. */
  async setupApply(plan: SetupPlan): Promise<SetupApplyOutcome> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.setupApply({ accountId, plan });
      return { kind: "started", runId: res.runId, counts: res.counts };
    } catch (err) {
      if (err instanceof JmapMethodError && err.type === ERR_SETUP_RUN_BUSY) {
        return { kind: "busy" };
      }
      return { kind: "failed", message: describeError(err, "Could not start the import") };
    }
  }

  /** The runs, for the progress panel. Null on failure — a panel with no run is
   * not the same as a panel that could not ask, and only the second is worth
   * saying out loud. */
  async setupRuns(ids?: Id[]): Promise<SetupRun[] | null> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.setupGet({ accountId, ids: ids ?? null });
      return res.list ?? [];
    } catch {
      return null;
    }
  }

  /**
   * Reverses a whole run.
   *
   * The refusal IS surfaced here, unlike the preview: this is an explicit,
   * consequential press, and a silent null would be indistinguishable from an
   * undo that quietly did nothing — which is the exact failure shape this
   * project keeps paying for.
   */
  async setupUndo(runId: Id): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    try {
      await this.client.setupUndo({ accountId, runId });
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not undo that import"));
      return false;
    }
  }

  /**
   * "Folders feed the desk" for ONE folder (#442/#446) — `Mailbox/set feedsDesk`.
   *
   * Not queued, for `setMailboxSyncScope`'s reason: it decides what the user
   * SEES, and a replay hours later would fold a folder into the desk long after
   * they changed their mind. No optimistic store write either — nothing renders
   * this ahead of the server, and the poll below is what re-reads the Mailbox so
   * every other view of the folder agrees.
   */
  async setMailboxFeedsDesk(id: Id, feedsDesk: boolean): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.mailboxSet({ accountId, update: { [id]: { feedsDesk } } });
      const err = res.notUpdated?.[id];
      if (err) {
        this.events.onError?.(describeSetError(err, "Could not point that folder at the desk"));
        return false;
      }
      void this.sync.poll();
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not point that folder at the desk"));
      return false;
    }
  }

  /**
   * Ask the server what rule this FOLDER's mail implies (#446 step 3).
   *
   * The sibling of `induceRule` one argument over, and a separate method rather
   * than an optional parameter because the server refuses both at once: exactly
   * one of `labelId` and `mailboxId` is the contract (jmap/client.ts ruleInduce).
   *
   * Null on failure, never a fabricated empty result: "this folder implies no
   * clean rule" is an answer the step prints, and it must not be what a broken
   * request looks like.
   */
  async induceRuleForMailbox(mailboxId: Id, limit?: number): Promise<RuleInduceResult | null> {
    const accountId = this.store.getState().accountId;
    try {
      return await this.client.ruleInduce({
        accountId,
        mailboxId,
        ...(limit ? { limit } : {}),
      });
    } catch {
      return null;
    }
  }

  /**
   * Saves this device's APPEARANCE to the server, for the `web` client family
   * (#375 increment 5).
   *
   * WARNING: `update`, NEVER `create`, AND THAT IS THE SERVER'S RULE RATHER
   * THAN A PREFERENCE. `clientPreferencesSet` refuses every `create` outright
   * — "the client families are a fixed set; update one instead of creating a
   * row" — and it does not need one: `ClientPreferences/get` MATERIALISES all
   * three families before it answers (GetClientPreferences inserts on first
   * contact), so by the time this app has an id to name, the row exists. When
   * the id is not known yet — a save before the reference-data load finished —
   * this asks for it rather than inventing a create the server would refuse.
   *
   * NOT QUEUED, for `setLineThreshold`'s reason: this decides what the user
   * SEES. A theme replayed hours later would repaint the app long after they
   * changed their mind. It fails loudly and the DEVICE KEEPS THE VALUE: the
   * local apply has already happened, and a refusal leaves it alone rather than
   * snapping the UI back to the server's copy. Working offline is the point of
   * a device preference.
   *
   * `debounceMs` is for the SPLITTER, which fires on every pointermove. The
   * patches coalesce — the last value of each property wins — so a drag across
   * the window is one `set`, not two hundred.
   */
  async saveClientPreferences(
    patch: ClientPreferencesPatch,
    opts: { debounceMs?: number } = {},
  ): Promise<boolean> {
    // RECORDED NOW, NOT WHEN IT IS SENT (#562). The device has already changed
    // (the caller applied it locally), and a server read that lands before
    // this write is answered — including during the debounce — cannot be
    // shown to have seen it. The store holds that read back for these fields
    // rather than letting it undo the user's choice (core/prefsLedger.ts).
    const seq = this.store.beginClientPreferencesWrite(WEB_CLIENT_FAMILY, patch);
    if (!opts.debounceMs) return this.flushClientPreferences(patch, [seq]);
    this.prefsPending = { ...(this.prefsPending ?? {}), ...patch };
    this.prefsPendingSeqs.push(seq);
    if (this.prefsTimer !== null) clearTimeout(this.prefsTimer);
    return new Promise<boolean>((resolve) => {
      this.prefsWaiters.push(resolve);
      this.prefsTimer = setTimeout(() => {
        const pending = this.prefsPending ?? {};
        const waiters = this.prefsWaiters;
        const seqs = this.prefsPendingSeqs;
        this.prefsPending = null;
        this.prefsTimer = null;
        this.prefsWaiters = [];
        this.prefsPendingSeqs = [];
        // Every caller folded into this one write is told what happened to it:
        // a superseded promise that never settles is a leak and, worse, an
        // await that hangs.
        void this.flushClientPreferences(pending, seqs).then((ok) => {
          for (const w of waiters) w(ok);
        });
      }, opts.debounceMs);
    });
  }

  /** The id of the `web` ClientPreferences row, asking the server for it if
   * this client has not read it yet. Null only when the server could not be
   * reached, or answered with no web row at all. */
  private async webPreferencesId(): Promise<Id | null> {
    const st = this.store.getState();
    const known = st.clientPreferences[WEB_CLIENT_FAMILY]?.id;
    if (known) return known;
    if (!st.accountId) return null;
    const readAt = this.store.beginClientPreferencesRead();
    const [r] = await this.client.request([
      { name: "ClientPreferences/get", args: { accountId: st.accountId, ids: null }, id: "get" },
    ]);
    if (!r || r.error || !r.value) return null;
    const list = (r.value as unknown as GetResult<ClientPreferences>).list ?? [];
    this.store.putClientPreferences(list, readAt);
    return list.find((p) => p.family === WEB_CLIENT_FAMILY)?.id ?? null;
  }

  /** Writes `patch` and then SETTLES the writes it carries, whatever happened —
   * an accepted, refused or unsent write is answered all the same, and one left
   * unsettled would hold its field against every read until it was written
   * again (store.settleClientPreferencesWrites). */
  private async flushClientPreferences(
    patch: ClientPreferencesPatch,
    seqs: readonly number[],
  ): Promise<boolean> {
    try {
      return await this.writeClientPreferences(patch, seqs);
    } finally {
      this.store.settleClientPreferencesWrites(WEB_CLIENT_FAMILY, seqs);
    }
  }

  private async writeClientPreferences(
    patch: ClientPreferencesPatch,
    seqs: readonly number[],
  ): Promise<boolean> {
    if (Object.keys(patch).length === 0) return true;
    const st = this.store.getState();
    if (!st.accountId) {
      this.events.onError?.("Could not save your appearance: not connected yet.");
      return false;
    }
    try {
      const id = await this.webPreferencesId();
      if (!id) {
        this.events.onError?.(
          "Could not save your appearance: the server did not name this client.",
        );
        return false;
      }
      const readAt = this.store.beginClientPreferencesRead();
      const results = await this.client.request([
        {
          name: "ClientPreferences/set",
          args: { accountId: st.accountId, update: { [id]: patch } },
          id: "set",
        },
        // The server's own copy, read AFTER the set (RFC 8620 section 3.2:
        // calls run in order), because `updated` carries a bare null here — the
        // answer to "what is stored now" is this call, not the set echoing back
        // what we sent.
        { name: "ClientPreferences/get", args: { accountId: st.accountId, ids: [id] }, id: "get" },
      ]);
      const setResult = results.find((r) => r.id === "set");
      if (setResult?.error) {
        this.events.onError?.(describeError(setResult.error, "Could not save your appearance"));
        return false;
      }
      const res = setResult?.value as unknown as SetResult | undefined;
      const refusal = res?.notUpdated?.[id];
      if (refusal) {
        this.events.onError?.(describeSetError(refusal, "Could not save your appearance"));
        return false;
      }
      if (!res?.updated || !(id in res.updated)) {
        // Neither accepted nor refused by name: a set whose outcome the server
        // did not state is not evidence that it happened (setLineThreshold's
        // rule, for the same reason).
        this.events.onError?.(
          "Could not save your appearance: the server did not say it changed.",
        );
        return false;
      }
      const list =
        (
          results.find((r) => r.id === "get")?.value as unknown as
            | GetResult<ClientPreferences>
            | undefined
        )?.list ?? [];
      // `confirms`: this `/get` ran after THIS `/set` in the same request, so
      // it is the server's word for these writes — but not for a newer write
      // of the same field still in flight, which the store holds it to.
      this.store.putClientPreferences(list, readAt, seqs);
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not save your appearance"));
      return false;
    }
  }

  /**
   * Archives everything in one drawer (#375).
   *
   * NOT through the offline queue, which is one action per message: a client
   * loop of single `Email/set` calls is exactly what this feature exists to
   * avoid, and 3,000 queued rows would also mean 3,000 replays. So it needs a
   * connection, and says so up front rather than pretending to have swept a
   * drawer it has only written down.
   *
   * BOUNDED, AND IT REPORTS ITS BOUND. A drawer can legitimately hold months of
   * mail; this archives at most ARCHIVE_ALL_CAP messages and returns
   * `bound: true` when there was more, so the caller can say "5,000 archived,
   * more remain" instead of presenting a partial sweep as a finished one.
   *
   * Optimistic, like `archive`: the rows leave the drawer at once, and anything
   * the server refuses is rolled back and counted in `failed` rather than
   * quietly left looking archived.
   *
   * RETURNS THE IDS IT ARCHIVED (#375 increment 6), which is what makes the
   * sweep undoable: the inverse is `restoreToInbox` over exactly this list, in
   * the same chunks, and a count would not be enough to build it. It is the
   * ids that MOVED — not the ones it tried — so an undo never asks the server
   * to put back a message that never left.
   */
  async archiveDrawer(
    areaId: Id,
  ): Promise<{ archived: number; failed: number; bound: boolean; ids: Id[] }> {
    const nothing = { archived: 0, failed: 0, bound: false, ids: [] as Id[] };
    const desk = this.sync.currentDesk;
    if (!desk) {
      // Without a desk there is no scope, and a drawer archived across the
      // wrong set of accounts is mail moved that nobody asked about.
      this.events.onError?.("Open the desk before archiving a drawer.");
      return nothing;
    }
    const st = this.store.getState();
    const archiveBox = Object.values(st.mailboxes).find((m) => m.role === "archive");
    if (!archiveBox) {
      this.events.onError?.(
        "This account has no Archive folder, so there is nothing to archive into.",
      );
      return nothing;
    }
    if (st.conn === "offline") {
      this.events.onError?.(
        "Archiving a whole drawer needs a connection — it is too much to hold until you are back online.",
      );
      return nothing;
    }

    const filter = sectionFilter({ kind: "drawer", areaId }, desk.scope);
    const ids: Id[] = [];
    let total = 0;
    try {
      for (let position = 0; ids.length < ARCHIVE_ALL_CAP; position += ARCHIVE_ALL_PAGE) {
        const q = await this.client.emailQuery({
          accountId: st.accountId,
          filter,
          // Any total order pages correctly; arrival is the one the drawer is
          // already drawn in.
          sort: [{ property: "receivedAt", isAscending: false }],
          position,
          limit: ARCHIVE_ALL_PAGE,
          calculateTotal: true,
        });
        total = Math.max(total, q.total ?? q.ids.length);
        ids.push(...q.ids);
        if (q.ids.length < ARCHIVE_ALL_PAGE) break;
      }
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not read that drawer"));
      return nothing;
    }

    const bound = total > ARCHIVE_ALL_CAP || ids.length > ARCHIVE_ALL_CAP;
    const targets = [...new Set(ids)].slice(0, ARCHIVE_ALL_CAP);
    if (targets.length === 0) return { archived: 0, failed: 0, bound, ids: [] };

    this.store.applyMove(targets, "archive");
    const patch: PatchObject = { [`mailboxIds/${archiveBox.id}`]: true };
    // The chunking, the roll-back on a dropped connection, and the per-id
    // refusals all live in moveInChunks — the undo of this sweep is the same
    // walk backwards, and one copy is the only way the two cannot disagree.
    const { changed, failed } = await this.moveInChunks(targets, patch, {
      rest: "Could not archive the rest of that drawer",
      part: "Could not archive part of that drawer",
    });

    // The counts in the header and the drawer's own row are server-side
    // numbers, and a few thousand messages just moved: re-ask rather than
    // leaving the desk describing the drawer it had before.
    if (changed.length > 0) void this.sync.refreshDesk();
    return { archived: changed.length, failed, bound, ids: changed };
  }

  /**
   * Turns a folder's syncing on or off (#62).
   *
   * NOT queued, and for a sharper reason than the other direct writes: this
   * setting decides whether the server keeps a folder up to date, so a stale
   * replay is not a harmless late write — coming back online and having
   * "stop syncing Archive" land hours after the user changed their mind would
   * freeze a folder they are actively reading. It fails loudly instead.
   *
   * The store is updated from the SERVER's answer rather than optimistically. An
   * optimistic scope would draw the folder as frozen before the server agreed,
   * and "this folder is not being updated" is precisely the claim that must not be
   * made until it is true.
   */
  async setMailboxSyncScope(id: Id, scope: "full" | "off"): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.mailboxSet({
        accountId,
        update: { [id]: { syncScope: scope } },
      });
      const err = res.notUpdated?.[id];
      if (err) {
        this.events.onError?.(describeSetError(err, "Could not change that folder's syncing"));
        return false;
      }
      this.store.applyMailboxSyncScope(id, scope);
      // The change moved the Mailbox state; pick it up so every other view of this
      // folder agrees rather than waiting for the next poll.
      void this.sync.poll();
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not change that folder's syncing"));
      return false;
    }
  }

  /** Creates a label (Label/set create). Not queued: label creation has no
   * optimistic story worth telling — the UI wants the server-assigned keyword
   * atom before it can apply the label to anything. */
  async createLabel(name: string, color?: string): Promise<Label | null> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.labelSet({
        accountId,
        create: { new: { name, ...(color ? { color } : {}) } },
      });
      const err = res.notCreated?.["new"];
      if (err) {
        this.events.onError?.(describeSetError(err, `Could not create the label “${name}”`));
        return null;
      }
      const created = res.created?.["new"] ?? null;
      if (created) this.store.putLabels([created]);
      return created;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not create the label"));
      return null;
    }
  }

  // -- mail accounts and desk areas (#374) -----------------------------------
  //
  // None of these is queued, for the reason the desk's own writes are not: they
  // decide what the user sees and what the server SYNCS. "Stop syncing this
  // account" replayed after a reconnect, hours after the user changed their mind,
  // would silently stop their mail; an area rename replayed over a later rename
  // would undo it. They fail loudly instead.
  //
  // NOTHING IS APPLIED OPTIMISTICALLY. Every one of them either reads the object
  // back from the server in the same batch or leaves the caller to re-read, and
  // that is the same rule `setMailboxSyncScope` states: "this account is paused"
  // is precisely the claim that must not be made until it is true.

  /**
   * Changes one mail account — its name, its colour, whether it syncs, or its
   * connection settings (#374, spec §12).
   *
   * ⚠ A CONNECTION CHANGE IS NOT A PARTIAL PATCH. internal/api/account.go treats
   * any of `imapHost`/`imapPort`/`smtpHost`/`smtpPort`/`username`/`password`/
   * `providerProfile` as a CONNECTION CHANGE, which must re-supply the FULL
   * endpoint block, the username, the password AND the name — the server never
   * keeps the old plaintext secret, so there is no "leave the password alone"
   * form of this write. It then re-seals the credentials and DIALS IMAP before
   * accepting, so this call can take seconds and can fail with
   * `validationFailed`. Metadata-only patches (`name`/`syncEnabled`/`color`) dial
   * nothing and return immediately.
   *
   * NOTHING LOCAL IS WRITTEN. The account list and its telemetry live in
   * ui/useAccounts.ts's own cache, not in the mail store, so the caller re-reads
   * that after a `true` — which is what makes the switcher, the desk dots and this
   * control incapable of disagreeing about whether an account is paused.
   *
   * The per-object refusal is surfaced verbatim where the server gave one: the
   * descriptions here name the property that was wrong (`color must be null or a
   * #RRGGBB hex string`, `an account with that name already exists`), which is
   * more use than any sentence this layer could invent.
   */
  async updateAccount(
    id: Id,
    patch: PatchObject,
    label = "Could not change that account",
  ): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.accountSet({ accountId, update: { [id]: patch } });
      const refusal = res.notUpdated?.[id];
      if (refusal) {
        this.events.onError?.(describeSetError(refusal, label));
        return false;
      }
      if (!res.updated || !(id in res.updated)) {
        // A set the server neither accepted nor refused BY NAME is not evidence
        // that it happened (updateDeskPreferences says the same).
        this.events.onError?.(`${label}: the server did not say it changed.`);
        return false;
      }
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, label));
      return false;
    }
  }

  /**
   * Removes a mail account and everything that lived only in it (#374).
   *
   * ⚠ WHAT THIS DESTROYS, from internal/store/accounts.go DeleteAccount — the
   * caller MUST have said all of this before calling, because none of it is
   * recoverable and none of it is guessable from the word "remove":
   *
   *   * the account, its folders, its identities, the signatures bound to those
   *     identities, and its outbound history (all FK ON DELETE CASCADE);
   *   * every MESSAGE that lived only in this account — hard-deleted, not
   *     trashed. A message that also exists in another account (content-hash
   *     dedup, ADR-001) is KEPT;
   *   * a PGP key bound to one of those identities SURVIVES; only its binding is
   *     cleared.
   *
   * The dialog in ui/settings/rows/AccountsRow.tsx states exactly that list, and
   * it is the server's comment rather than a paraphrase of it.
   *
   * A refusal is reported per object, like every other `/set`. `notFound` is the
   * ordinary one here: two tabs, one account, one of them a moment behind.
   */
  async destroyAccount(id: Id): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.accountSet({ accountId, destroy: [id] });
      const refusal = res.notDestroyed?.[id];
      if (refusal) {
        this.events.onError?.(describeSetError(refusal, "Could not remove that account"));
        return false;
      }
      if (!res.destroyed?.includes(id)) {
        this.events.onError?.("Could not remove that account: the server did not say it was gone.");
        return false;
      }
      // Everything that account held is gone server-side; the client's caches are
      // now describing mail that does not exist. The mailboxes, identities and
      // messages come back through the poll (Account `destroyed` is the trigger —
      // per-Email rows are deliberately not fanned out, see DeleteAccount), and
      // the caller drops ui/useAccounts.ts's cache.
      void this.sync.poll();
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not remove that account"));
      return false;
    }
  }

  /**
   * Makes a new area (#374). The SERVER mints its own label — that is what makes
   * an area an area (types.ts "areas are labels"), and there is no client-side
   * form of it.
   *
   * Position is left to the server, which puts a create with no `position` one
   * past the current maximum (internal/api/area.go createArea).
   */
  async createArea(name: string): Promise<AreaWrite> {
    const accountId = this.store.getState().accountId;
    const trimmed = name.trim();
    if (trimmed === "") return { ok: false, refusal: "An area needs a name." };
    try {
      const res = await this.client.areaSet({ accountId, create: { new: { name: trimmed } } });
      const refusal = res.notCreated?.["new"];
      if (refusal) return { ok: false, refusal: describeSetError(refusal, "Refused") };
      const created = (res.created?.["new"] ?? null) as Area | null;
      if (!created) {
        return { ok: false, refusal: "The server did not say the area was created." };
      }
      // A new area mints a LABEL as well as an area, and it adds a section to the
      // desk. Both halves are re-read rather than patched in: putAreas alone would
      // leave the new area's own label missing from every label picker.
      await this.refreshAreas();
      return { ok: true, refusal: null, area: created };
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not create the area"));
      return { ok: false, refusal: null };
    }
  }

  /**
   * Changes one area — its name, its position, its feeds, its drawer period, or
   * whether it shows in the rail (#374).
   *
   * ⚠ `tabs` AND `labelIds` ARE REPLACE-ALL when present. Build them with
   * ui/settings/areaFeeds.ts `feedPatch`, which is the only thing that produces
   * complete arrays; a patch naming just the feed that changed SUCCEEDS and
   * unsubscribes the area from all the others.
   *
   * The refusal comes back VERBATIM for the caller to render: `ErrFeedTaken`'s
   * message already names the feed AND the area holding it (internal/api/area.go
   * passes it through for exactly that reason), and `ErrAreaNameTaken` names the
   * collision. No sentence written here could improve on either.
   */
  async updateArea(id: Id, patch: PatchObject): Promise<AreaWrite> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.areaSet({ accountId, update: { [id]: patch } });
      const refusal = res.notUpdated?.[id];
      if (refusal) return { ok: false, refusal: describeSetError(refusal, "Refused") };
      if (!res.updated || !(id in res.updated)) {
        return { ok: false, refusal: "The server did not say the area changed." };
      }
      await this.refreshAreas();
      return { ok: true, refusal: null };
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not change that area"));
      return { ok: false, refusal: null };
    }
  }

  /**
   * Puts the areas in a given order (#374) — `↑` and `↓` on the Areas row.
   *
   * ⚠ IT RENUMBERS EVERY AREA THAT NEEDS IT, IN ONE `Area/set`, AND A ONE-SIDED
   * WRITE WOULD NOT WORK. `app.areas.position` has no uniqueness constraint
   * (db/schema.sql) and the store orders by `position, id` — so decrementing one
   * area's position to match its neighbour's leaves them TIED, and the tie breaks
   * on the id, which is creation order and has nothing to do with what the user
   * clicked. The arrow would then move the area sometimes and not others.
   * Assigning `0…n-1` to the whole list makes every position distinct, which also
   * repairs a login whose positions had already collided.
   *
   * Only the areas whose position actually changes are sent: each update is a
   * `change_log` row, and renumbering three areas because one moved would tell
   * every other client that three areas changed.
   */
  async reorderAreas(orderedIds: Id[]): Promise<AreaWrite> {
    const accountId = this.store.getState().accountId;
    const areas = this.store.getState().areas;
    const update: Record<Id, PatchObject> = {};
    orderedIds.forEach((id, i) => {
      if (areas[id]?.position !== i) update[id] = { position: i };
    });
    if (Object.keys(update).length === 0) return { ok: true, refusal: null };
    try {
      const res = await this.client.areaSet({ accountId, update });
      // Reported per object; with a handful in flight the description of the ONE
      // that was refused is more use than "2 of 3 failed" (updateCategoryTabs makes
      // the same call).
      const failed = Object.entries(res.notUpdated ?? {})[0];
      if (failed) {
        await this.refreshAreas();
        return { ok: false, refusal: describeSetError(failed[1], "Refused") };
      }
      await this.refreshAreas();
      return { ok: true, refusal: null };
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not reorder the areas"));
      return { ok: false, refusal: null };
    }
  }

  /**
   * Destroys an area (#374).
   *
   * ⚠ THE AREA'S OWN LABEL SURVIVES, on every message that carries it — the
   * server is explicit about it (types.ts: "immutable, and it survives the area's
   * destruction"). So this is not a deletion of mail or of a label; it is the
   * removal of one region of the desk. The confirm dialog says exactly that.
   */
  async destroyArea(id: Id): Promise<AreaWrite> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.areaSet({ accountId, destroy: [id] });
      const refusal = res.notDestroyed?.[id];
      if (refusal) return { ok: false, refusal: describeSetError(refusal, "Refused") };
      if (!res.destroyed?.includes(id)) {
        return { ok: false, refusal: "The server did not say the area was gone." };
      }
      await this.refreshAreas();
      return { ok: true, refusal: null };
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not delete that area"));
      return { ok: false, refusal: null };
    }
  }

  /**
   * Re-reads the areas and the labels from the server and redraws the desk.
   *
   * ⚠ A FULL RE-READ, NOT A PATCH, and `replace: true` is the load-bearing part:
   * a destroy has to remove the row and a create has to bring in the label the
   * server minted, and a merge would leave the first behind and miss the second.
   * The LABELS come too, because an area create mints one and an area destroy
   * leaves one behind — a label picker built from a stale map would be missing the
   * new area's own label, which is the one the user is about to want.
   *
   * The desk is redrawn last: the areas ARE the desk's sections, so a section list
   * from before this write would render a drawer for an area that is gone.
   */
  private async refreshAreas(): Promise<void> {
    const accountId = this.store.getState().accountId;
    try {
      const results = await this.client.request([
        { name: "Area/get", args: { accountId, ids: null }, id: "ar" },
        { name: "Label/get", args: { accountId, ids: null }, id: "lb" },
      ]);
      const areas = results.find((r) => r.id === "ar");
      if (!areas?.error && areas?.value) {
        this.store.putAreas(((areas.value as unknown as GetResult<Area>).list ?? []), true);
      }
      const labels = results.find((r) => r.id === "lb");
      if (!labels?.error && labels?.value) {
        this.store.putLabels((labels.value as unknown as GetResult<Label>).list ?? [], true);
      }
    } catch {
      // The write already succeeded; a failed re-read leaves the client one poll
      // behind rather than reporting a failure that did not happen.
    }
    void this.sync.refreshDesk();
  }

  // -- category tabs (#228) --------------------------------------------------
  //
  // Neither of these is queued, for the reason the rules below give: there is no
  // optimistic rendering to be right about, and replaying a stale arrangement
  // after a reconnect would silently undo a later change of mind. They fail
  // loudly instead.

  /**
   * Enables, disables or reorders tabs — one `CategoryTab/set` for the whole
   * gesture, since moving one tab renumbers its neighbour too.
   *
   * THE STORE IS UPDATED FROM THE SERVER'S ANSWER, not optimistically, and that
   * is not caution but correctness: the strip decides which `inTab` queries the
   * client is allowed to make, and the server REFUSES a query naming a disabled
   * tab. Drawing a tab the server would refuse produces a dead button, so the
   * arrangement the strip renders has to be one the server has already agreed to.
   */
  async updateCategoryTabs(update: Record<Id, PatchObject>): Promise<boolean> {
    if (Object.keys(update).length === 0) return true;
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.categoryTabSet({ accountId, update });
      // Reported per object by the server, so surface the first refusal rather
      // than a count: with at most five rows in flight the description of one is
      // more use than "2 of 3 failed".
      const failed = Object.entries(res.notUpdated ?? {})[0];
      if (failed) {
        this.events.onError?.(describeSetError(failed[1], "Could not change your categories"));
      }
      const fresh = await this.client.categoryTabGet({ accountId, ids: null });
      this.store.putCategoryTabs(fresh.list);
      // Switching a tab off moves its mail into Primary, so the list on screen
      // is now answering the previous arrangement. NOTHING IN THE POLL WOULD
      // NOTICE: a tab moves with no Email change_log row at all, which is the
      // same fact that makes `inTab` refuse `queryChanges`. Re-run it.
      await this.sync.refreshView();
      return !failed;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not change your categories"));
      return false;
    }
  }

  /**
   * Points a label at a category tab, or clears it (`null`).
   *
   * This is the MAPPING half of a message's tab (ADR-021/022): a label's tab
   * wins over the classifier, and because it stays a live join, this one write
   * moves every message carrying that label on the next read — no backfill, no
   * job. Which is why the local apply is a single label patch rather than a
   * reload: nothing about the MESSAGES is cached that this invalidates.
   */
  async setLabelTab(id: Id, tab: string | null): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.labelSet({ accountId, update: { [id]: { tab } } });
      const err = res.notUpdated?.[id];
      if (err) {
        this.events.onError?.(describeSetError(err, "Could not change that label's category"));
        return false;
      }
      const label = this.store.getState().labels[id];
      // Applied from the value the server ACCEPTED, not from a guess about what
      // it stored: the patch went through unchanged or we would be in the branch
      // above.
      if (label) this.store.putLabels([{ ...label, tab: tab as Label["tab"] }]);
      // Every message carrying this label just changed tab — with no Email
      // change_log row, because the mapping is a live join rather than a stored
      // value (ADR-022). The poll re-reads Labels on a Label change and stops
      // there, so without this the list keeps showing the old mapping's answer
      // and the edit looks like it did nothing.
      await this.sync.refreshView();
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not change that label's category"));
      return false;
    }
  }

  // -- T0 rules (#155) -------------------------------------------------------
  //
  // None of these are queued. The offline queue exists for MAIL actions, whose
  // optimistic result the user can see and whose replay order matters; a rule
  // edit has no local rendering to be optimistic about, and replaying a stale
  // rule write after coming back online would silently resurrect an automation
  // the user may have changed their mind about. They fail loudly instead.

  /**
   * Every rule, in EVALUATION order (priority, lower first). That order is the
   * semantics — `stop` halts everything after it — so the list must never be
   * re-sorted for display.
   *
   * Null means the load FAILED. An empty array means "you have no rules", and
   * the two must not collapse: one is an error to report, the other is the
   * empty state that invites writing a first rule.
   */
  async listRules(): Promise<Rule[] | null> {
    const accountId = this.store.getState().accountId;
    try {
      const q = await this.client.ruleQuery({ accountId });
      if (q.ids.length === 0) return [];
      const res = await this.client.ruleGet({ accountId, ids: q.ids });
      // Rule/get does not promise the id order it was asked for; the query does.
      const byId = new Map(res.list.map((r) => [r.id, r]));
      return q.ids.map((id) => byId.get(id)).filter((r): r is Rule => r !== undefined);
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not load your rules"));
      return null;
    }
  }

  /**
   * Creates a rule, RETRYING UNDER A BUMPED NAME when the server says the name
   * is taken (#292).
   *
   * This retry is required whatever else the client does about naming, and the
   * reason is that every check the client could make is ADVISORY. Rule names are
   * unique per user in the database (`UNIQUE (user_id, name)`), and between the
   * moment this client reads the rule list and the moment it writes, a second
   * tab, another device, or an `mailroom-admin accept-suggestion` run can take
   * the name. The constraint is the only thing that knows; asking it and
   * handling its answer is the only way to be right. A pre-flight uniqueness
   * check narrows the window, it does not close it.
   *
   * Bounded at MAX_NAME_ATTEMPTS. Past that the SetError is surfaced exactly as
   * before — a client that grinds through suffixes forever turns one refused
   * write into a burst of them, and the user learns nothing.
   *
   * The rename is REPORTED, not silent. Saving "Venmo → Promotions" and getting
   * "Venmo → Promotions (2)" without being told is the small, deniable version
   * of the failure this project keeps meeting: the write succeeded, the outcome
   * differs from what was asked, and nothing says so.
   */
  async createRule(
    input: Omit<Rule, "id" | "systemSuggested" | "corrections">,
  ): Promise<Rule | null> {
    const accountId = this.store.getState().accountId;
    const wanted = input.name;
    try {
      for (let attempt = 1; attempt <= MAX_NAME_ATTEMPTS; attempt++) {
        const name = attempt === 1 ? wanted : bumpRuleName(wanted, attempt);
        const res = await this.client.ruleSet({ accountId, create: { new: { ...input, name } } });
        const err = res.notCreated?.["new"];
        if (!err) {
          if (name !== wanted) {
            this.events.onInfo?.(`You already had a rule called “${wanted}”, so this one is “${name}”.`);
          }
          return res.created?.["new"] ?? null;
        }
        // Only a NAME COLLISION is retryable, and it is recognised by the
        // structured SetError fields rather than by its English description —
        // see nameTakenBy, which also explains why the name we sent has to be
        // part of that judgement.
        if (attempt < MAX_NAME_ATTEMPTS && nameTakenBy(err, name)) continue;
        this.events.onError?.(describeSetError(err, `Could not create the rule “${name}”`));
        return null;
      }
      // Unreachable: the loop either returns or continues, and the last
      // iteration cannot continue. Present because the type says so.
      return null;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not create the rule"));
      return null;
    }
  }

  async updateRule(id: Id, patch: Partial<Rule>): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.ruleSet({
        accountId,
        update: { [id]: patch as unknown as PatchObject },
      });
      const err = res.notUpdated?.[id];
      if (err) {
        this.events.onError?.(describeSetError(err, "Could not save the rule"));
        return false;
      }
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not save the rule"));
      return false;
    }
  }

  // -- saved signatures and templates (#56) -----------------------------------

  // -- saved searches / smart folders (#57) -----------------------------------
  //
  // A saved search is a NAMED QUERY. Running one is not a method here: the caller
  // puts its `query` into the ordinary search view, which is the same path typed
  // text takes. That is deliberate — a second way to run a search is a second
  // thing that could disagree with the first.

  /** Reloads the whole list. Called after any change, because the sidebar renders
   * all of the pinned ones and a stale one is a folder that lies. */
  async loadSavedSearches(): Promise<void> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.savedSearchGet({ accountId, ids: null });
      this.store.putSavedSearches(res.list ?? []);
    } catch {
      // Keep whatever we had. An empty list means no smart folders are shown,
      // which is the same as not having made any — nothing is lost.
    }
  }

  /** Creates or updates one. A FULL object either way: name, query and inSidebar
   * together decide what it is and where it appears, so a partial write could move
   * a folder out of the sidebar while renaming it (the #170 class of bug). */
  async saveSavedSearch(
    search: Omit<SavedSearch, "id"> & { id?: Id },
  ): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    const { id, ...fields } = search;
    try {
      const res = id
        ? await this.client.savedSearchSet({ accountId, update: { [id]: fields } })
        : await this.client.savedSearchSet({ accountId, create: { new1: fields } });
      const err = id ? res.notUpdated?.[id] : res.notCreated?.new1;
      if (err) {
        this.events.onError?.(describeSetError(err, "Could not save that search"));
        return false;
      }
      await this.loadSavedSearches();
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not save that search"));
      return false;
    }
  }

  async deleteSavedSearch(id: Id): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.savedSearchSet({ accountId, destroy: [id] });
      const err = res.notDestroyed?.[id];
      if (err) {
        this.events.onError?.(describeSetError(err, "Could not delete that search"));
        return false;
      }
      await this.loadSavedSearches();
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not delete that search"));
      return false;
    }
  }

  /** Reloads the whole snippet list into the store. */
  async loadComposeSnippets(): Promise<void> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.composeSnippetGet({ accountId, ids: null });
      this.store.putComposeSnippets(res.list ?? []);
    } catch {
      // Keep whatever we had. An empty list means no pickers are offered, which is
      // the same as not having made any — nothing is lost and nothing is wrong.
    }
  }

  /** Creates or updates one snippet. A FULL object either way: kind and identityId
   * together decide which picker offers it, so a partial write could move a
   * signature to the wrong address (the #170 class of bug). */
  async saveComposeSnippet(
    snippet: Omit<ComposeSnippet, "id"> & { id?: Id },
  ): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    const { id, ...fields } = snippet;
    try {
      const res = id
        ? await this.client.composeSnippetSet({ accountId, update: { [id]: fields } })
        : await this.client.composeSnippetSet({ accountId, create: { new1: fields } });
      const err = id ? res.notUpdated?.[id] : res.notCreated?.new1;
      if (err) {
        this.events.onError?.(describeSetError(err, "Could not save that"));
        return false;
      }
      await this.loadComposeSnippets();
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not save that"));
      return false;
    }
  }

  async deleteComposeSnippet(id: Id): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.composeSnippetSet({ accountId, destroy: [id] });
      const err = res.notDestroyed?.[id];
      if (err) {
        this.events.onError?.(describeSetError(err, "Could not delete that"));
        return false;
      }
      await this.loadComposeSnippets();
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not delete that"));
      return false;
    }
  }

  // -- remembered remote-content grants (#202) --------------------------------
  //
  // These do NOT unblock anything by themselves. The server still parks every
  // remote URL unconditionally; this list only decides what the reading pane
  // un-parks without asking (docs/hostile-content.md §2.3).

  /** Loads the whole grant list into the store. Called on connect and after any
   * change, because the reading pane consults it per message — fetching per render
   * would be one request per message to answer a question about a handful of rows.
   *
   * A FAILURE IS NOT SURFACED AS AN ERROR, and that is deliberate: if this cannot
   * be read the store keeps an empty list, which means "block everything and ask" —
   * the safe default. A toast saying image preferences failed to load would be
   * noise about a state the user cannot act on and is not harmed by. */
  async loadRemoteAllows(): Promise<void> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.remoteAllowGet({ accountId, ids: null });
      this.store.putRemoteAllows(res.list ?? []);
    } catch {
      // Keep whatever we had; an empty list is the blocking default.
    }
  }

  /** Tells the server the user clicked "Show remote content" on ONE message.
   *
   * ⚠ WITHOUT THIS THE ONE-OFF CLICK LOADS NOTHING when the image proxy is on.
   * A one-off allow is deliberately not remembered (#202) — it lives as component
   * state and writes no grant — which was fine while the BROWSER did the
   * fetching. With the proxy the browser asks the server, and the server has no
   * other way to know the user asked. The record is short-lived and in-memory
   * server-side: still not a grant, still nothing durable.
   *
   * Failure is deliberately not surfaced: the images simply do not load, which
   * the reader can see, and inventing an error toast for it would be noisier
   * than the thing it describes. Returns whether the server recorded it. */
  async requestRemoteContent(messageId: string): Promise<boolean> {
    if (!this.client.imageProxyEnabled()) return true;
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.remoteContentRequest({ accountId, messageId });
      return res.requested === true;
    } catch {
      return false;
    }
  }

  /** Remembers "always load this sender's images", as an exact address or a whole
   * domain. Re-granting an existing one is a no-op server-side. */
  async allowRemoteContent(kind: "address" | "domain", value: string): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.remoteAllowSet({
        accountId,
        create: { new1: { kind, value } },
      });
      const err = res.notCreated?.new1;
      if (err) {
        this.events.onError?.(describeSetError(err, "Could not remember that sender"));
        return false;
      }
      await this.loadRemoteAllows();
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not remember that sender"));
      return false;
    }
  }

  /** Forgets one grant. The list is reloaded before returning, so the reading pane
   * goes back to blocking on the NEXT RENDER rather than at the next restart —
   * revocation that does not take effect promptly is not revocation. */
  async revokeRemoteContent(id: Id): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.remoteAllowSet({ accountId, destroy: [id] });
      const err = res.notDestroyed?.[id];
      if (err) {
        this.events.onError?.(describeSetError(err, "Could not forget that sender"));
        return false;
      }
      await this.loadRemoteAllows();
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not forget that sender"));
      return false;
    }
  }

  async deleteRule(id: Id): Promise<boolean> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.ruleSet({ accountId, destroy: [id] });
      const err = res.notDestroyed?.[id];
      if (err) {
        this.events.onError?.(describeSetError(err, "Could not delete the rule"));
        return false;
      }
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not delete the rule"));
      return false;
    }
  }

  /**
   * The blast radius of a condition tree, evaluated server-side against real
   * mail (#155).
   *
   * Returns null on failure rather than a zero: "0 messages match" and "we could
   * not check" must never look the same, because the first invites saving the
   * rule and the second says nothing at all.
   */
  async previewRule(
    conditions: RuleCondition,
    opts: { targetLabelId?: string; limit?: number } = {},
  ): Promise<RulePreview | null> {
    const accountId = this.store.getState().accountId;
    try {
      return await this.client.rulePreview({
        accountId,
        conditions,
        ...(opts.targetLabelId ? { targetLabelId: opts.targetLabelId } : {}),
        ...(opts.limit ? { limit: opts.limit } : {}),
      });
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not preview the rule"));
      return null;
    }
  }

  /**
   * Ask the server what rule this label's examples imply (#168).
   *
   * Returns null on failure — never a fabricated empty result. "No suggestion"
   * and "we could not look" must not read the same, because the first is an
   * answer and the second is a broken feature that looks like one.
   *
   * This is called speculatively after labelling, so a failure is deliberately
   * NOT surfaced as an error toast: the user asked to label a message, they did
   * not ask for a suggestion, and interrupting them with the failure of
   * something they never requested is noise.
   */
  async induceRule(labelId: Id): Promise<RuleInduceResult | null> {
    const accountId = this.store.getState().accountId;
    try {
      return await this.client.ruleInduce({ accountId, labelId });
    } catch {
      return null;
    }
  }

  /**
   * Should "Always put mail from {address} in {area}?" be offered (Area/ruleSuggestion)?
   *
   * Asked after a manual move, so the server must have SEEN that move: it counts
   * the persisted filings, and the verbs only queue. Hence `settle()` first; if the
   * queue did not empty (offline, a retry pending) the answer would be about an
   * older state, so there is none.
   *
   * Returns null on any failure, never a fabricated "no": this is asked
   * speculatively, so a failure is deliberately NOT surfaced as an error toast.
   */
  async ruleSuggestion(areaId: Id, address: string): Promise<AreaRuleSuggestion | null> {
    const accountId = this.store.getState().accountId;
    try {
      if (!(await this.settle())) return null;
      return await this.client.areaRuleSuggestion({ accountId, areaId, address });
    } catch {
      return null;
    }
  }

  /**
   * Learned placement's status for Settings (LearnedPlacement/getStatus).
   * Null on any failure: it is a status line, never an error toast.
   */
  async learningStatus(): Promise<LearningStatus | null> {
    const accountId = this.store.getState().accountId;
    try {
      return await this.client.learnedPlacementGetStatus({ accountId });
    } catch {
      return null;
    }
  }

  /** "Not now" on the offer above; best effort and silent. */
  async dismissRuleSuggestion(areaId: Id, address: string): Promise<void> {
    const accountId = this.store.getState().accountId;
    try {
      await this.client.areaDismissRuleSuggestion({ accountId, areaId, address });
    } catch {
      // The offer simply comes back after the next qualifying move.
    }
  }

  /**
   * Run a saved rule over the mail already in the mailbox (#191).
   *
   * THE ONE Rule/* ACTION THAT MUTATES — every other one proposes — so it
   * refreshes the view afterwards. Without that the labels are on the server and
   * invisible in the list the user is looking at, which reads as "nothing
   * happened" and invites them to run it again.
   *
   * Failure is surfaced: this is an explicit, consequential action, and a silent
   * null would be indistinguishable from "it matched nothing".
   */
  async applyRule(ruleId: Id): Promise<RuleApplyResult | null> {
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.ruleApply({ accountId, ruleId });
      if (res.changed > 0) await this.sync.refreshView();
      return res;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not apply the rule"));
      return null;
    }
  }

  /**
   * Reverse a backfill (#191): remove the labels that rule put on those
   * messages.
   *
   * It records NO corrections against the rule — undoing an operation is not
   * disagreeing with it, and the server draws that distinction
   * (EmailUpdate.RemovalIsUndo). Passing the ids the apply returned is what
   * keeps it exact — and, for a rule that files into an area (#708), so is
   * passing back the desk/setup labels it removed (`removedAreaLabels`):
   * without them the messages lose the rule's label but not their old place.
   */
  async undoRuleApply(
    ruleId: Id,
    messageIds: Id[],
    restoreAreaLabels?: Record<Id, RemovedAreaLabel[]>,
  ): Promise<RuleApplyResult | null> {
    if (messageIds.length === 0) return null;
    const accountId = this.store.getState().accountId;
    try {
      const res = await this.client.ruleApply({
        accountId,
        ruleId,
        undo: true,
        messageIds,
        ...(restoreAreaLabels && Object.keys(restoreAreaLabels).length > 0 ? { restoreAreaLabels } : {}),
      });
      if (res.undone > 0) await this.sync.refreshView();
      return res;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not undo it"));
      return null;
    }
  }

  /**
   * Ask the server for a narrower form of a rule the user keeps correcting
   * (#170).
   *
   * Failure IS surfaced, unlike induceRule's: this one is asked for explicitly —
   * somebody clicked "narrow it" — and silently returning nothing would look
   * exactly like "there is no narrower rule", which is the one answer that must
   * stay distinguishable from a broken request.
   */
  async tightenRule(ruleId: Id): Promise<RuleTightenResult | null> {
    const accountId = this.store.getState().accountId;
    try {
      return await this.client.ruleTighten({ accountId, ruleId });
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not work out a narrower rule"));
      return null;
    }
  }

  // -- compose / send --------------------------------------------------------

  /**
   * Saves a draft and (optionally) submits it.
   *
   * TWO round trips, on purpose: RFC 8620 result references are resolved only
   * for TOP-LEVEL arguments (internal/api/request.go resolveReferences), so the
   * submission's `emailId` — which lives inside `create` — cannot back-reference
   * the Email/set that created the draft. The draft id has to come back to the
   * client first.
   *
   * The From address is validated SERVER-SIDE against the user's verified
   * identities (spec §5) and the failure comes back as a `forbiddenFrom`
   * SetError. It is surfaced verbatim, because "your From is not verified" is
   * information the user needs, not noise to swallow.
   */
  async sendMessage(input: {
    from: EmailAddress;
    to: EmailAddress[];
    cc?: EmailAddress[];
    bcc?: EmailAddress[];
    subject: string;
    text: string;
    /**
     * The rich (HTML) body, when the user composed in rich-text mode. When
     * present the draft becomes multipart/alternative: `text` is the plain-text
     * fallback (RFC 2046 §5.1.4, least-rich first) and this is the text/html
     * part. Absent = a plain-text-only send, exactly as before. The server
     * SANITIZES this HTML before it is transmitted (internal/api/draft.go →
     * SanitizeOutboundHTML) — the editor is a convenience, not a security
     * boundary.
     */
    html?: string;
    inReplyTo?: string[];
    references?: string[];
    /**
     * The identity to submit through — sent ONLY when it is the exact,
     * non-wildcard identity for `from`. The server re-derives the identity from
     * the From address in the bytes it will transmit and REJECTS a submission
     * whose `identityId` disagrees with that derivation (compose.Submit:
     * `req.IdentityID != si.IdentityID` ⇒ ErrFromNotVerified). Sending a
     * catchall's wildcard identity id alongside a concrete derived address is
     * exactly that disagreement, so in the catchall case we let the server
     * derive it — which is the whole point of spec §5's derived-alias model.
     */
    identityId?: Id;
    /** Attachments, already uploaded (`JmapClient.upload` → blobId). The server
     * checks that each blob belongs to this user before it reads a byte of it.
     *
     * `cid` + `disposition:"inline"` mark an INLINE image the user embedded in
     * the HTML body: it is referenced from the body as `<img src="cid:…">` and
     * disposed inline, so a recipient's client renders it in place and does NOT
     * list it among the downloadable attachments (compose.Attachment.ContentID
     * drives that server-side). A normal file attachment leaves both unset. */
    attachments?: {
      blobId: BlobId;
      type: string;
      name: string;
      cid?: string;
      disposition?: "inline" | "attachment";
    }[];
    /** Save only; do not submit. */
    draftOnly?: boolean;
    /** SEND LATER (#56): an RFC 3339 instant to hold the message until.
     *
     * The engine has always taken this — a submission IS a durable job with a
     * fire-at time (spec §5/§16), and the default is "now + the undo window". So
     * scheduling adds no new machinery; it only stops defaulting. Omitted means
     * send now, exactly as before.
     *
     * Bounded by the session's `maxDelayedSend`, which the server now enforces
     * rather than merely advertising. */
    sendAt?: string;
    /** SEND & ARCHIVE (#648): Emails the SERVER archives once this message has
     * actually been sent — never on a failure, an Undo Send or exhausted
     * retries. The client performs no archive of its own: v1.80.0 did, the
     * moment the send was accepted, and a reply the provider then refused left
     * its conversation archived. */
    archiveOnSuccess?: Id[];
    /** ENCRYPT (#47, ADR-038): ask the server to sign and encrypt to every To/Cc
     * recipient and to the sender. Refused by the server (never downgraded) when it
     * cannot; carried through the offline queue so a held send stays encrypted. */
    encrypt?: boolean;
  }): Promise<SendOutcome | null> {
    const accountId = this.store.getState().accountId;

    // A non-empty HTML body promotes the draft to multipart/alternative: the
    // plain `text` stays as the text/plain fallback and the HTML rides alongside
    // it. An absent/blank html leaves the message text/plain, unchanged.
    const html = input.html && input.html.trim() !== "" ? input.html : null;

    // The Email/set create arguments, built ONCE and kept, because if the server
    // cannot be reached they are what gets queued (#62). They are the whole message
    // — attachments excepted, which are blob ids for bytes already uploaded.
    const draftCreate: Record<string, unknown> = {
      from: [input.from],
      to: input.to,
      ...(input.cc?.length ? { cc: input.cc } : {}),
      ...(input.bcc?.length ? { bcc: input.bcc } : {}),
      subject: input.subject,
      ...(input.inReplyTo?.length ? { inReplyTo: input.inReplyTo } : {}),
      ...(input.references?.length ? { references: input.references } : {}),
      bodyValues: {
        body: { value: input.text },
        ...(html ? { html: { value: html } } : {}),
      },
      textBody: [{ partId: "body", type: "text/plain" }],
      ...(html ? { htmlBody: [{ partId: "html", type: "text/html" }] } : {}),
      ...(input.attachments?.length ? { attachments: input.attachments } : {}),
    };

    // Generated HERE, before the first attempt — not at the moment of queueing.
    // The submission below is sent WITH it, so if that request reaches the server
    // and only its response is lost, the retry the queue performs carries the same
    // key and the server answers with the submission it already made instead of
    // sending the message a second time (core/sendQueue.ts).
    const idempotencyKey = newIdempotencyKey();
    const pending: QueuedSend = {
      draft: draftCreate,
      draftId: null,
      submit: input.draftOnly !== true,
      subject: input.subject,
      ...(input.identityId ? { identityId: input.identityId } : {}),
      ...(input.sendAt ? { sendAt: input.sendAt } : {}),
      idempotencyKey,
      ...(input.archiveOnSuccess?.length ? { archiveOnSuccess: input.archiveOnSuccess } : {}),
      ...(input.encrypt ? { encrypt: true } : {}),
    };

    let draft: SetResult<Email>;
    try {
      draft = await this.client.emailSet({ accountId, create: { d: draftCreate } });
    } catch (err) {
      if (isUnreachable(err)) return this.queueSend(pending, err);
      this.events.onError?.(describeError(err, "Could not save the draft"));
      return null;
    }

    const notCreated = draft.notCreated?.["d"];
    if (notCreated) {
      this.events.onError?.(describeSetError(notCreated, "Could not save the draft"));
      return null;
    }
    const created = draft.created?.["d"];
    if (!created?.id) {
      this.events.onError?.("The server accepted the draft but returned no id.");
      return null;
    }
    this.store.putEmails([created]);

    if (input.draftOnly) {
      this.events.onInfo?.("Draft saved.");
      void this.sync.poll();
      return { emailId: created.id, submissionId: null };
    }

    let sub: SetResult<EmailSubmission>;
    try {
      sub = await this.client.submissionSet({
        accountId,
        create: {
          s: {
            emailId: created.id,
            idempotencyKey,
            ...(input.identityId ? { identityId: input.identityId } : {}),
            ...(input.sendAt ? { sendAt: input.sendAt } : {}),
            ...(input.archiveOnSuccess?.length
              ? { archiveOnSuccess: input.archiveOnSuccess }
              : {}),
            ...(input.encrypt ? { encrypt: true } : {}),
          },
        },
      });
    } catch (err) {
      // THE FAILURE THIS FEATURE IS ABOUT. The draft exists and the submission may
      // or may not — we sent the request and never heard back. Queueing with the
      // draft id ALREADY SET means the replay skips straight to the submission, and
      // it carries the same key, so "the server already did this" resolves to the
      // existing submission rather than a second send.
      if (isUnreachable(err)) return this.queueSend({ ...pending, draftId: created.id }, err);
      this.events.onError?.(
        describeError(err, "The draft was saved, but the send could not be scheduled"),
      );
      return { emailId: created.id, submissionId: null, refused: true };
    }

    const notSent = sub.notCreated?.["s"];
    if (notSent) {
      this.events.onError?.(describeSetError(notSent, "The message was not sent"));
      return { emailId: created.id, submissionId: null, refused: true };
    }
    const submission = sub.created?.["s"];
    if (submission) this.store.putSubmissions([submission]);
    // The submission cursor starts here, so the changes loop tracks the send.
    const cursor = this.store.getState().cursor;
    if (!cursor.EmailSubmission && sub.newState) {
      this.store.setCursor({ ...cursor, EmailSubmission: sub.newState });
    }
    void this.sync.poll();
    return {
      emailId: created.id,
      submissionId: submission?.id ?? null,
      // The SERVER's instant, never a locally computed one: the hold window is
      // the server's setting, and a countdown from a number this client invented
      // would run out while the message was still cancellable — or, worse, keep
      // offering an undo after the mail had gone.
      sendAt: submission?.sendAt ?? null,
      // What the SERVER says it will archive, not what was asked: an older
      // server ignores the key, and the toast must not promise an archive
      // nothing is going to perform.
      archivePending: (submission?.archiveOnSuccess?.length ?? 0) > 0,
    };
  }

  /**
   * AUTOSAVE A DRAFT (#448).
   *
   * ⚠ THIS IS NOT `sendMessage({draftOnly: true})` AND CANNOT BE. That path
   * CREATES a draft and nothing else, so calling it every fifteen seconds leaves
   * one row in the Drafts folder per fifteen seconds of typing. A JMAP Email is
   * immutable apart from its keywords and mailboxIds (RFC 8621 §4.6), so editing
   * a draft means creating the new version and DESTROYING the old one — and
   * internal/api/draft.go says it must be **in the same `Email/set` call**, which
   * is what makes the draft-conflict rule fall out of `store.RetireDraft` (spec
   * §5: last write wins, loser preserved).
   *
   * ⚠ IT DOES NOT TOAST. A failed autosave is reported in the composer's footer
   * (`Not saved · <reason>`), because it is a fact about the panel the user is
   * looking at and not an event — a toast every fifteen seconds behind a server
   * that is refusing would be a machine gun. So this method returns the reason
   * instead of routing it through `this.events.onError`, and it is the ONE write
   * in this file that deliberately does not.
   *
   * ⚠ IT IS NOT QUEUED, unlike a send. `sendMessage` queues on a transport error
   * because losing a message the user pressed Send on is unrecoverable; an
   * autosave is a periodic backup of text that is still on screen, and replaying
   * a fifteen-second-old version of a draft over a newer one hours later would
   * make the queue an actively wrong thing to have. Offline, the footer says
   * `Not saved · …` and the next tick tries again.
   *
   * ⚠ A REFUSED CREATE DOES NOT PREVENT THE DESTROY, and callers must handle it.
   * `Email/set` runs its create loop and its destroy loop independently inside one
   * transaction (internal/api/email.go), so a create the server refuses can be
   * accompanied by a destroy that succeeds — leaving no server-side draft at all.
   * `retiredPrevious` reports exactly that, so the caller can forget the old id
   * rather than aiming the next save's destroy at a draft that no longer exists.
   * Nothing the user wrote is lost either way: the composer still holds it, and
   * the next successful save writes a fresh draft.
   */
  async saveDraft(input: {
    /** The draft id this save SUPERSEDES, destroyed in the same call. Null for the
     * first save of a composer. */
    previousDraftId: Id | null;
    from: EmailAddress;
    to: EmailAddress[];
    cc?: EmailAddress[];
    bcc?: EmailAddress[];
    subject: string;
    text: string;
    html?: string;
    inReplyTo?: string[];
    references?: string[];
    attachments?: {
      blobId: BlobId;
      type: string;
      name: string;
      cid?: string;
      disposition?: "inline" | "attachment";
    }[];
  }): Promise<DraftSaveOutcome> {
    const accountId = this.store.getState().accountId;
    const html = input.html && input.html.trim() !== "" ? input.html : null;
    const draftCreate: Record<string, unknown> = {
      from: [input.from],
      to: input.to,
      ...(input.cc?.length ? { cc: input.cc } : {}),
      ...(input.bcc?.length ? { bcc: input.bcc } : {}),
      subject: input.subject,
      ...(input.inReplyTo?.length ? { inReplyTo: input.inReplyTo } : {}),
      ...(input.references?.length ? { references: input.references } : {}),
      bodyValues: {
        body: { value: input.text },
        ...(html ? { html: { value: html } } : {}),
      },
      textBody: [{ partId: "body", type: "text/plain" }],
      ...(html ? { htmlBody: [{ partId: "html", type: "text/html" }] } : {}),
      ...(input.attachments?.length ? { attachments: input.attachments } : {}),
    };

    let res: SetResult<Email>;
    try {
      res = await this.client.emailSet({
        accountId,
        create: { d: draftCreate },
        ...(input.previousDraftId ? { destroy: [input.previousDraftId] } : {}),
      });
    } catch (err) {
      if (err instanceof JmapAuthError) this.store.setConn("unauthenticated");
      return { ok: false, reason: draftFailureReason(err), retiredPrevious: false };
    }

    const retiredPrevious =
      input.previousDraftId !== null && (res.destroyed ?? []).includes(input.previousDraftId);

    const notCreated = res.notCreated?.["d"];
    if (notCreated) {
      return {
        ok: false,
        reason: notCreated.description?.trim() || notCreated.type,
        retiredPrevious,
      };
    }
    const created = res.created?.["d"];
    if (!created?.id) {
      return {
        ok: false,
        reason: "the server accepted the draft but returned no id",
        retiredPrevious,
      };
    }
    this.store.putEmails([created]);
    // THE ONE PLACE A DRAFT'S CONTINUITY IS KNOWN (#610, #612): this call named
    // the old revision in the destroy and got the new one back, so whatever was
    // holding the old id — the list's cursor, the composer's URL — can follow
    // it. Only when the server confirms it retired the old one: if the destroy
    // was refused, the old revision still exists and nothing should leave it.
    if (retiredPrevious && input.previousDraftId) {
      this.store.recordDraftSuccessor(input.previousDraftId, created.id);
    }
    // Polled rather than assumed: the Drafts mailbox counts moved, the rail shows
    // them, and the retired revision has to leave the list it was in.
    void this.sync.poll();
    return { ok: true, draftId: created.id, retiredPrevious };
  }

  /**
   * DISCARD A DRAFT (#448): destroy the version the composer is holding.
   *
   * `Email/set destroy` on a draft RETIRES it (internal/api/email.go — a draft is
   * destroyed, not trashed), which is what Discard means. A draft with a PENDING
   * SUBMISSION is refused by the server rather than trashed, with "cancel the send
   * before deleting it" — which is correct, and is why this reports a refusal
   * instead of pretending.
   *
   * `quiet` suppresses the toast, and there is exactly one caller for it: the
   * composer retiring the draft AUTOSAVE was keeping after the message has already
   * gone (`sendMessage` submits a draft of its own). That cleanup is not something
   * the user asked for, so its failure is not something to interrupt them with —
   * the artifact is one visible, deletable draft row.
   */
  async discardDraft(id: Id, opts: { quiet?: boolean } = {}): Promise<boolean> {
    try {
      const res = await this.client.emailSet({
        accountId: this.store.getState().accountId,
        destroy: [id],
      });
      const err = res.notDestroyed?.[id];
      if (err) {
        if (!opts.quiet) {
          this.events.onError?.(describeSetError(err, "Could not discard the draft"));
        }
        return false;
      }
      void this.sync.poll();
      return true;
    } catch (err) {
      if (!opts.quiet) {
        this.events.onError?.(describeError(err, "Could not discard the draft"));
      }
      return false;
    }
  }

  /**
   * SEND A SCHEDULED MESSAGE NOW (#448, the Outbox's "Send now").
   *
   * ⚠ THERE IS NO RESCHEDULE ON THE SERVER. `EmailSubmission/set update` accepts
   * exactly one mutation — `undoStatus: "canceled"` — and refuses a patch that
   * names any other property (internal/api/submission.go cancelSubmission; RFC
   * 8621 §7.5 says the same). `sendAt` is settable only on create. So "send now"
   * is: cancel the scheduled job, and create a new submission for the same draft
   * with no `sendAt`, which the server defaults to now + its undo window.
   *
   * ⚠ ONE `EmailSubmission/set`, NOT TWO ROUND TRIPS, and that is a safety
   * property rather than an optimisation. Two calls have a window in which the
   * first succeeded and the second never happened: cancel-then-create can lose the
   * message, and create-then-cancel leaves TWO live submissions for one draft and
   * mails the recipient twice. One call is one transaction. The server runs its
   * create loop before its update loop, so the new submission is made while the
   * old one is still scheduled — which app.submissions permits on purpose ("NOT a
   * constraint on message_id: that would forbid a legitimate re-send after an Undo
   * Send cancellation", db/schema.sql) — and both land or neither does.
   *
   * ⚠ NO IDEMPOTENCY KEY, deliberately. A key is what makes a REPLAY safe, and
   * this call is never replayed: it is not queued, and a transport error leaves
   * the original schedule untouched, which is the safe failure. Reusing the
   * original submission's key would be actively wrong — the server would answer
   * with the submission it already made (compose.Submit's `created == false`
   * branch), i.e. the one this call just cancelled.
   */
  async sendNow(submissionId: Id): Promise<SendOutcome | null> {
    const state = this.store.getState();
    const sub = state.submissions[submissionId];
    if (!sub) {
      this.events.onError?.("That send is no longer in this client's view of the outbox.");
      return null;
    }
    try {
      const res = await this.client.submissionSet({
        accountId: state.accountId,
        create: {
          s: {
            emailId: sub.emailId,
            // The identity the server itself recorded on the submission, not one
            // re-derived here: it has already been checked against the From in
            // the message bytes (compose.Submit), and supplying a different one
            // is exactly the disagreement that call refuses with
            // ErrFromNotVerified.
            identityId: sub.identityId,
          },
        },
        update: { [submissionId]: { undoStatus: "canceled" } },
      });
      const notSent = res.notCreated?.["s"];
      if (notSent) {
        this.events.onError?.(describeSetError(notSent, "Could not send that message now"));
        return null;
      }
      const notCanceled = res.notUpdated?.[submissionId];
      if (notCanceled) {
        // The new submission exists and the old schedule stands — the one outcome
        // that could send the message twice, so it is reported loudly rather than
        // folded into a partial success.
        this.events.onError?.(
          describeSetError(notCanceled, "The message was resent but the old schedule stands"),
        );
      }
      const created = res.created?.["s"];
      if (created) this.store.putSubmissions([created]);
      if (res.newState) {
        const cursor = this.store.getState().cursor;
        if (!cursor.EmailSubmission) {
          this.store.setCursor({ ...cursor, EmailSubmission: res.newState });
        }
      }
      void this.sync.poll();
      return {
        emailId: sub.emailId,
        submissionId: created?.id ?? null,
        // The server's own instant, exactly as sendMessage returns it: it is the
        // deadline of the undo offer, and a client-computed one would run out
        // while the message was still cancellable.
        sendAt: created?.sendAt ?? null,
      };
    } catch (err) {
      if (err instanceof JmapAuthError) this.store.setConn("unauthenticated");
      this.events.onError?.(describeError(err, "Could not send that message now"));
      return null;
    }
  }

  /** Undo Send: cancel a submission inside its hold window (spec §5 — Undo Send
   * and Send Later are the same durable job). */
  async cancelSubmission(id: Id): Promise<boolean> {
    try {
      const res = await this.client.submissionSet({
        accountId: this.store.getState().accountId,
        update: { [id]: { undoStatus: "canceled" } },
      });
      const err = res.notUpdated?.[id];
      if (err) {
        this.events.onError?.(describeSetError(err, "Could not unsend"));
        return false;
      }
      this.events.onInfo?.("Send canceled.");
      void this.sync.poll();
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not unsend"));
      return false;
    }
  }

  /**
   * DISMISS A FAILED SEND (migration 00059): take a submission the provider
   * refused for good off the Outbox. The server accepts `dismissed: true` only
   * on a failed submission; nothing is deleted — the draft stays in Drafts, so
   * the user can still edit and re-send it.
   *
   * The row is dropped locally as soon as the server agrees, rather than at the
   * next sync, because the server has already said yes (and appended the
   * EmailSubmission change every other device will pick up).
   */
  async dismissSubmission(id: Id): Promise<boolean> {
    try {
      const res = await this.client.submissionSet({
        accountId: this.store.getState().accountId,
        update: { [id]: { dismissed: true } },
      });
      const err = res.notUpdated?.[id];
      if (err) {
        this.events.onError?.(describeSetError(err, "Could not dismiss"));
        return false;
      }
      const held = this.store.getState().submissions[id];
      if (held) this.store.putSubmissions([{ ...held, dismissedAt: new Date().toISOString() }]);
      void this.sync.poll();
      return true;
    } catch (err) {
      this.events.onError?.(describeError(err, "Could not dismiss"));
      return false;
    }
  }

  /**
   * Holds a send the server could not be told about, and reports it as held (#62).
   *
   * The alternative this replaces is losing the message: `sendMessage` used to
   * return null on a transport error, and the compose window closed on a toast
   * saying the draft could not be saved — with the text nowhere but the DOM that
   * was about to be torn down.
   *
   * Not optimistic in the store: no fake Email row is inserted for a draft the
   * server has never seen. The queue depth the UI already shows is what says
   * something is outstanding, and the toast returned from here is what says what.
   */
  private async queueSend(send: QueuedSend, cause: unknown): Promise<SendOutcome> {
    const label = send.submit ? `Send “${send.subject}”` : `Save draft “${send.subject}”`;
    const action = await this.queue.enqueue({
      accountId: this.store.getState().accountId,
      entity: "EmailSubmission",
      op: "create",
      // Informational for a send — flushSend reads the payload, not this — but it
      // is the draft id once phase 1 has landed, and the creation id until then.
      targetId: send.draftId ?? "d",
      payload: send as unknown as PatchObject,
      label,
    });
    if (cause instanceof JmapAuthError) this.store.setConn("unauthenticated");
    await this.publishQueue();
    // Scheduled rather than drained immediately: we have just this moment failed to
    // reach the server, so an instant replay would fail for the same reason.
    this.scheduleRetry([action]);
    return { emailId: send.draftId, submissionId: null, queued: true };
  }

  // -- the runner ------------------------------------------------------------

  /**
   * Drains the queue: one batched Email/set per chunk.
   *
   * A concurrent call JOINS the drain already running rather than returning
   * immediately (#176). The old early return was fine while every caller was
   * `void drain()` and only wanted "make sure one is happening" — but a caller
   * that AWAITS it wants "the queue is settled", and a bare `return` hands that
   * back a resolved promise while the writes are still in flight. That is how
   * the batch-triage flow came to ask the server what a labelling implied before
   * the labelling had left the client.
   */
  async drain(): Promise<void> {
    if (this.draining) return this.draining;
    const run = (async () => {
      try {
        const pending = await this.queue.list();
        if (pending.length === 0) return;

        // SENDS ARE NOT PATCHES, so they do not go through flushChunk — which
        // builds exactly one Email/set out of a whole chunk, and a send is a create
        // followed by a second method call whose argument only exists once the first
        // has answered. They are replayed one at a time, first, because a message
        // waiting to go out is the most consequential thing in the queue; a star
        // that lands a second later is not.
        for (const send of pending.filter((a) => a.entity === "EmailSubmission")) {
          await this.flushSend(send);
        }
        const patches = pending.filter((a) => a.entity !== "EmailSubmission");
        for (let i = 0; i < patches.length; i += SET_CHUNK) {
          const chunk = patches.slice(i, i + SET_CHUNK);
          await this.flushChunk(chunk);
        }
        await this.publishQueue();
      } finally {
        this.draining = null;
      }
    })();
    this.draining = run;
    return run;
  }

  /**
   * Replays one queued send: create the draft if it does not exist yet, then submit
   * it (#62).
   *
   * THE ORDER OF THE THREE WRITES IS THE WHOLE FUNCTION:
   *
   *   1. `Email/set create`      — the draft
   *   2. `queue.setPayload`      — the draft id, DURABLY, before anything else
   *   3. `EmailSubmission/set`   — the send, carrying the idempotency key
   *
   * Step 2 between them is what makes a crash-and-replay create at most one extra
   * draft instead of a second message, and the key in step 3 is what makes a lost
   * RESPONSE to step 3 resolve to the submission the server already made.
   */
  private async flushSend(a: QueuedAction): Promise<void> {
    const send = asQueuedSend(a.payload);
    if (!send) {
      // An entry we cannot read — a payload from an older shape, or a corrupted
      // one. Retrying it would fail identically forever, so it is dropped and said
      // so: a permanently stuck queue would also stop every send behind it.
      await this.queue.remove(a.queueId);
      this.events.onError?.(`“${a.label}” could not be replayed and was discarded.`);
      return;
    }
    const accountId = a.accountId || this.store.getState().accountId;
    let draftId = send.draftId;

    if (!draftId) {
      let res: SetResult<Email>;
      try {
        res = await this.client.emailSet({ accountId, create: { d: send.draft } });
      } catch (err) {
        await this.sendFailed(a, err);
        return;
      }
      const rejected = res.notCreated?.["d"];
      if (rejected) {
        await this.queue.remove(a.queueId);
        this.events.onError?.(describeSetError(rejected, `“${a.label}” was rejected`));
        return;
      }
      const created = res.created?.["d"];
      if (!created?.id) {
        await this.queue.remove(a.queueId);
        this.events.onError?.(`“${a.label}”: the server accepted the draft but returned no id.`);
        return;
      }
      this.store.putEmails([created]);
      draftId = created.id;
      // BEFORE the submission, and awaited. See the header comment.
      await this.queue.setPayload(a.queueId, { ...send, draftId } as unknown as PatchObject);
    }

    if (!send.submit) {
      await this.queue.remove(a.queueId);
      this.events.onInfo?.(`Draft saved: “${send.subject}”.`);
      void this.sync.poll();
      return;
    }

    let sub: SetResult<EmailSubmission>;
    try {
      sub = await this.client.submissionSet({
        accountId,
        create: {
          s: {
            emailId: draftId,
            idempotencyKey: send.idempotencyKey,
            ...(send.identityId ? { identityId: send.identityId } : {}),
            ...(send.sendAt ? { sendAt: send.sendAt } : {}),
            ...(send.archiveOnSuccess?.length ? { archiveOnSuccess: send.archiveOnSuccess } : {}),
            ...(send.encrypt ? { encrypt: true } : {}),
          },
        },
      });
    } catch (err) {
      await this.sendFailed(a, err);
      return;
    }

    const notSent = sub.notCreated?.["s"];
    if (notSent) {
      await this.queue.remove(a.queueId);
      this.events.onError?.(describeSetError(notSent, `“${send.subject}” was not sent`));
      return;
    }
    const submission = sub.created?.["s"];
    if (submission) this.store.putSubmissions([submission]);
    const cursor = this.store.getState().cursor;
    if (!cursor.EmailSubmission && sub.newState) {
      this.store.setCursor({ ...cursor, EmailSubmission: sub.newState });
    }
    await this.queue.remove(a.queueId);
    // The user hit Send some time ago and was told it was waiting. They are owed the
    // other half of that: it has now gone.
    this.events.onInfo?.(`Sent: “${send.subject}”.`);
    void this.sync.poll();
  }

  /**
   * What to do when replaying a send failed.
   *
   * Unreachable ⇒ IT STAYS QUEUED. Refused ⇒ it is dropped, with the server's own
   * words, because a "no" replayed on a timer is a loop that never ends and never
   * sends. That costs the message text, which is the honest price of the server
   * having declined it; the toast names the subject so the user knows which one.
   */
  private async sendFailed(a: QueuedAction, err: unknown): Promise<void> {
    if (err instanceof JmapAuthError) {
      // Not counted as an attempt: nothing is wrong with the send, the session
      // lapsed. Signing in drains the queue and the message goes out.
      this.store.setConn("unauthenticated");
      return;
    }
    if (err instanceof JmapTransportError) {
      await this.queue.markFailed(a.queueId, err.message);
      await this.publishQueue();
      this.scheduleRetry([a]);
      return;
    }
    if (err instanceof JmapMethodError) {
      await this.queue.remove(a.queueId);
      this.events.onError?.(describeError(err, `“${a.label}” was not sent`));
      await this.publishQueue();
      return;
    }
    throw err;
  }

  private async flushChunk(chunk: QueuedAction[]): Promise<void> {
    const accountId = this.store.getState().accountId;
    const update: Record<Id, PatchObject> = {};
    const destroy: Id[] = [];
    // A target can appear more than once (star, then read). Merging the patches
    // preserves both, and both stay in the queue until the server confirms.
    for (const a of chunk) {
      if (a.op === "update" && a.payload) {
        update[a.targetId] = { ...(update[a.targetId] ?? {}), ...a.payload };
      } else if (a.op === "destroy") {
        if (!destroy.includes(a.targetId)) destroy.push(a.targetId);
      }
    }
    const calls: Call[] = [
      {
        name: "Email/set",
        args: {
          accountId,
          ...(Object.keys(update).length > 0 ? { update } : {}),
          ...(destroy.length > 0 ? { destroy } : {}),
        },
        id: "set",
      },
    ];

    let res;
    try {
      const [result] = await this.client.request(calls);
      if (!result) return;
      if (result.error) throw result.error;
      res = result.value as unknown as SetResult<Email>;
    } catch (err) {
      if (err instanceof JmapAuthError) {
        this.store.setConn("unauthenticated");
        return;
      }
      if (err instanceof JmapTransportError) {
        // Offline / server down: KEEP the actions and retry. The overlay stays
        // applied, so the UI still shows what the user did.
        for (const a of chunk) await this.queue.markFailed(a.queueId, err.message);
        await this.publishQueue();
        this.scheduleRetry(chunk);
        return;
      }
      if (err instanceof JmapMethodError) {
        // The whole method failed (e.g. stateMismatch). Roll the chunk back:
        // it did not happen.
        for (const a of chunk) {
          this.revert(a);
          await this.queue.remove(a.queueId);
        }
        this.events.onError?.(
          err.description ?? `The server rejected the change (${err.type}).`,
        );
        await this.publishQueue();
        return;
      }
      throw err;
    }

    const notUpdated = res.notUpdated ?? {};
    const notDestroyed = res.notDestroyed ?? {};
    for (const a of chunk) {
      const rejection = a.op === "destroy" ? notDestroyed[a.targetId] : notUpdated[a.targetId];
      if (rejection) {
        this.revert(a);
        this.events.onError?.(describeSetError(rejection, `“${a.label}” was rejected`));
      }
      await this.queue.remove(a.queueId);
    }
    // The set moved the Email state; pick the change up (and refresh counts).
    void this.sync.poll();
  }

  /** Undoes exactly the overlay delta this action applied. */
  private revert(a: QueuedAction): void {
    if (a.op === "destroy") {
      this.store.revertMove([a.targetId]);
      return;
    }
    for (const key of Object.keys(a.payload ?? {})) {
      if (key.startsWith("keywords/")) {
        this.store.revertKeyword([a.targetId], key.slice("keywords/".length));
      } else if (key.startsWith("labelIds/")) {
        this.store.revertLabelId([a.targetId], key.slice("labelIds/".length));
      } else if (key.startsWith("mailboxIds/")) {
        this.store.revertMove([a.targetId]);
      } else if (key === "isPinned") {
        this.store.revertPin([a.targetId]);
      } else if (key === "snoozedUntil") {
        this.store.revertSnooze([a.targetId]);
      }
    }
  }

  private scheduleRetry(chunk: QueuedAction[]): void {
    if (this.retryTimer) return;
    const attempts = Math.max(...chunk.map((a) => a.attempts), 0);
    const delay = BACKOFF_MS[Math.min(attempts, BACKOFF_MS.length - 1)] ?? 60_000;
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null;
      void this.drain();
    }, delay);
  }
}

/** Turns a SetError into something a human can act on. */
export function describeSetError(err: SetError, prefix: string): string {
  if (err.description) return `${prefix}: ${err.description}`;
  switch (err.type) {
    case "forbiddenFrom":
      return `${prefix}: that From address is not a verified identity for your account.`;
    case "notFound":
      return `${prefix}: the server no longer has that message.`;
    case "overQuota":
      return `${prefix}: you are over your sending quota.`;
    default:
      return `${prefix} (${err.type}).`;
  }
}

/**
 * A SHORT phrase for the composer's `Not saved · <reason>` footer.
 *
 * Deliberately not `describeError`: that produces a full sentence with a prefix
 * ("Could not save the draft: …"), and the footer already supplies the subject.
 * Two sentences glued together read as a bug in the string, not as an error.
 */
function draftFailureReason(err: unknown): string {
  if (err instanceof JmapTransportError) return "the server could not be reached";
  if (err instanceof JmapAuthError) return "you are signed out";
  const msg = err instanceof Error ? err.message.trim() : String(err).trim();
  return msg === "" ? "unknown error" : msg;
}

function describeError(err: unknown, prefix: string): string {
  if (err instanceof JmapMethodError) return describeSetError({ type: err.type, ...(err.description !== undefined ? { description: err.description } : {}) }, prefix);
  if (err instanceof JmapTransportError) return `${prefix}: the server is unreachable.`;
  return `${prefix}: ${err instanceof Error ? err.message : String(err)}`;
}
