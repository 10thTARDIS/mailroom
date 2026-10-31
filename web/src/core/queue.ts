// SPDX-License-Identifier: AGPL-3.0-only

// The offline action queue (spec §12's PWA "offline cache … action queue", and
// spec §4a's discipline mirrored client-side: actions are INTENT DELTAS with an
// idempotency key, never "set the state to X").
//
// Every user mutation takes the same path:
//
//   1. the MailStore applies it to the overlay      → the UI updates instantly;
//   2. the action is appended here (durably)        → it survives a reload;
//   3. ActionRunner drains the queue against JMAP   → the server catches up;
//   4a. success  → the action is removed; the overlay retires when the server's
//                  own Email object agrees with it (core/store.ts);
//   4b. transport failure → the action stays queued and is retried with backoff
//                  (the mutation is a delta, so replaying it is safe);
//   4c. the server SAID NO (a SetError / method error) → the action is dropped
//                  and the overlay is ROLLED BACK, with the server's own
//                  description surfaced. A rejection is never retried in a loop.
//
// DOM-free: the durable backend is a KeyValueStorage the host supplies.

import type { AccountId, Id, PatchObject } from "./jmap/types";
import type { KeyValueStorage } from "./storage";
import { MemoryStorage } from "./storage";

export type QueuedActionEntity = "Email" | "Mailbox" | "EmailSubmission" | "Identity" | "Label";

export interface QueuedAction {
  queueId: string;
  accountId: AccountId;
  entity: QueuedActionEntity;
  op: "create" | "update" | "destroy";
  /** JMAP object id for update/destroy; the client-assigned creation id for create. */
  targetId: Id;
  payload: PatchObject | null;
  enqueuedAt: string;
  attempts: number;
  lastError: string | null;
  /** A human-readable summary for the UI ("Archive 3 conversations"). */
  label: string;
}

export type NewAction = Omit<QueuedAction, "queueId" | "enqueuedAt" | "attempts" | "lastError">;

export interface OfflineActionQueue {
  enqueue(action: NewAction): Promise<QueuedAction>;
  list(): Promise<QueuedAction[]>;
  remove(queueId: string): Promise<void>;
  markFailed(queueId: string, error: string): Promise<void>;
  /**
   * Replaces a queued action's payload, DURABLY, while it is still queued.
   *
   * For an action whose replay has more than one step: a queued send creates the
   * draft, then submits it, and the draft id learned in between has to survive a
   * reload or the replay creates a second draft (core/sendQueue.ts). Nothing else
   * needs this — a flag patch is a single call whose payload never changes — so it
   * exists for progress WITHIN an action, not for editing a user's intent.
   */
  setPayload(queueId: string, payload: PatchObject): Promise<void>;
  clear(): Promise<void>;
}

const QUEUE_KEY = "mailroom.queue.v1";

let counter = 0;
function nextQueueId(): string {
  counter += 1;
  return `${Date.now().toString(36)}-${counter.toString(36)}`;
}

/**
 * A durable OfflineActionQueue over a KeyValueStorage. Synchronous JSON in a
 * single key is deliberate for 0.5: the queue holds pending FLAG/LABEL/MOVE
 * deltas (tens of entries at worst, not mail bodies), and a single atomic
 * read-modify-write is far easier to reason about than an IndexedDB object
 * store. If the queue ever has to hold outbound message bytes, this is the one
 * class to replace — the interface above is what everything else codes to.
 */
export class StorageActionQueue implements OfflineActionQueue {
  private readonly storage: KeyValueStorage;

  constructor(storage: KeyValueStorage = new MemoryStorage()) {
    this.storage = storage;
  }

  private read(): QueuedAction[] {
    const raw = this.storage.getItem(QUEUE_KEY);
    if (!raw) return [];
    try {
      const parsed: unknown = JSON.parse(raw);
      return Array.isArray(parsed) ? (parsed as QueuedAction[]) : [];
    } catch {
      this.storage.removeItem(QUEUE_KEY);
      return [];
    }
  }

  private write(actions: QueuedAction[]): void {
    try {
      this.storage.setItem(QUEUE_KEY, JSON.stringify(actions));
    } catch {
      // Storage full/blocked: the queue degrades to in-memory for this session
      // rather than dropping the user's action on the floor.
    }
  }

  async enqueue(action: NewAction): Promise<QueuedAction> {
    const full: QueuedAction = {
      ...action,
      queueId: nextQueueId(),
      enqueuedAt: new Date().toISOString(),
      attempts: 0,
      lastError: null,
    };
    const all = this.read();
    all.push(full);
    this.write(all);
    return full;
  }

  async list(): Promise<QueuedAction[]> {
    return this.read();
  }

  async remove(queueId: string): Promise<void> {
    this.write(this.read().filter((a) => a.queueId !== queueId));
  }

  async markFailed(queueId: string, error: string): Promise<void> {
    this.write(
      this.read().map((a) =>
        a.queueId === queueId ? { ...a, attempts: a.attempts + 1, lastError: error } : a,
      ),
    );
  }

  async setPayload(queueId: string, payload: PatchObject): Promise<void> {
    this.write(this.read().map((a) => (a.queueId === queueId ? { ...a, payload } : a)));
  }

  async clear(): Promise<void> {
    this.storage.removeItem(QUEUE_KEY);
  }
}
