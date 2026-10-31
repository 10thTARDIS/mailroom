// SPDX-License-Identifier: AGPL-3.0-only

// A SEND THAT COULD NOT REACH THE SERVER, held for replay (#62 — offline compose).
//
// Everything else in the offline queue is an INTENT DELTA on an object the server
// already has: `Email/set` with `{"keywords/$seen": true}` twice is the same as once,
// so the runner can replay it freely and the only question is whether it ever lands.
// A send is not that. It CREATES two objects and hands bytes to an MTA, and the
// failure that makes a retry necessary is precisely the one that cannot be
// distinguished from success:
//
//     client ──── EmailSubmission/set ───▶ server: submission created, send scheduled
//            ◀╌╌╌╌╌╌ response lost ╌╌╌╌╌╌
//
// The laptop closed, the proxy timed out, the tunnel dropped. The client saw a
// transport error and knows nothing about what the server did. It must retry — and a
// naive retry sends the message a second time.
//
// So a queued send carries an IDEMPOTENCY KEY, generated once when the user hits
// Send and reused by every replay of that one action. The server records it on the
// submission under a per-user unique index and answers a repeat with the SUBMISSION
// IT ALREADY MADE (internal/store/submissions.go CreateSubmission), so the retry
// above resolves to "already done" instead of a second message.
//
// TWO PHASES, and the boundary is durable. RFC 8620 resolves result references only
// for top-level arguments, so the submission's `emailId` cannot back-reference the
// `Email/set` that created the draft (internal/api/request.go resolveReferences) —
// the draft id has to come back to the client first. `draftId` below is where it is
// written, INTO THE QUEUED ACTION, before the submission is attempted. A replay that
// finds it set skips phase 1 rather than creating a second draft.
//
// The window that remains, stated rather than implied: if the process dies after the
// draft is created but before `draftId` is persisted, the replay creates a second
// DRAFT. It does not send twice — the key still collapses the submissions onto the
// first draft's — so the artifact is one orphaned draft in the Drafts folder, which is
// visible and deletable. That is the trade taken deliberately: duplicate drafts are
// recoverable, duplicate mail is not.
//
// Attachments are referenced by `blobId`, not held here. An upload needs the network,
// so a send with attachments only ever gets queued AFTER its uploads succeeded, which
// means those blob ids are real server-side rows; nothing reclaims an uploaded blob
// while it is unreferenced (internal/purge reclaims only the blobs of purged
// messages). The queue therefore never holds message bytes — only the small JSON
// envelope below.

import type { Id, PatchObject } from "./jmap/types";

/**
 * A send waiting to be replayed. Stored as the `payload` of a queued action whose
 * entity is `EmailSubmission`.
 */
export interface QueuedSend {
  /** The `Email/set` create arguments for the draft, replayed verbatim. */
  draft: Record<string, unknown>;
  /**
   * The server-assigned draft id, once phase 1 has landed. Non-null means the
   * draft EXISTS and must not be created again.
   */
  draftId: Id | null;
  /** False for a "save draft" that never reached the server: phase 2 is skipped. */
  submit: boolean;
  /** The subject, so the toast that eventually reports the send can name the message
   * the user wrote minutes or hours earlier. */
  subject: string;
  /** Passed through to `EmailSubmission/set` when set (see MailActions.sendMessage). */
  identityId?: Id;
  /** Send Later's absolute instant, resolved when the user acted — not at drain time. */
  sendAt?: string;
  /** The key that makes a replay safe. Generated once per send; never regenerated. */
  idempotencyKey: string;
  /** SEND & ARCHIVE (#648): passed through to `EmailSubmission/set` as
   * `archiveOnSuccess`, so a held send archives its conversation only once the
   * server has actually sent it — the same rule as an immediate send. */
  archiveOnSuccess?: Id[];
  /** ENCRYPT (#47, ADR-038): passed through to `EmailSubmission/set` as `encrypt`.
   * ⚠ Carried so a send held while offline is STILL encrypted when it replays: a
   * queued send that lost this flag would go out in the clear. */
  encrypt?: boolean;
}

/**
 * The server's bound on an idempotency key
 * (`submissions_idempotency_key_bounded`, migration 00025). A key outside it is
 * REFUSED rather than trimmed — a truncated key could collide with another
 * action's and suppress a real send — so the generator below must stay inside it.
 */
export const KEY_MIN = 8;
export const KEY_MAX = 200;

/**
 * A fresh idempotency key for one send.
 *
 * Randomness, not a counter: two tabs of the same account share the queue's
 * storage but not its module state, and a counter would have them minting the same
 * key for different messages — which the server would read as a replay and answer
 * by suppressing the second send. Silently losing mail is the worst failure this
 * file can produce, so the key is drawn from the CSPRNG when there is one.
 *
 * The fallback is for a plain-http context, where `crypto.randomUUID` is absent
 * (it requires a secure context). It is weaker, and it is still per-send unique in
 * practice; the alternative — refusing to queue the send at all — would lose the
 * message for certain rather than probably.
 */
export function newIdempotencyKey(): string {
  const c: Crypto | undefined = globalThis.crypto;
  if (typeof c?.randomUUID === "function") return `send-${c.randomUUID()}`;
  if (typeof c?.getRandomValues === "function") {
    const bytes = c.getRandomValues(new Uint8Array(16));
    let hex = "";
    for (const b of bytes) hex += b.toString(16).padStart(2, "0");
    return `send-${hex}`;
  }
  return `send-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}-${Math.random().toString(36).slice(2, 12)}`;
}

/** True when `key` is one the server will accept. */
export function usableIdempotencyKey(key: string): boolean {
  return key.length >= KEY_MIN && key.length <= KEY_MAX;
}

/** Reads a queued action's payload back as a send, or null if it is not one. */
export function asQueuedSend(payload: PatchObject | null): QueuedSend | null {
  if (!payload || typeof payload !== "object") return null;
  const p = payload as Partial<QueuedSend>;
  if (typeof p.draft !== "object" || p.draft === null) return null;
  if (typeof p.idempotencyKey !== "string" || !usableIdempotencyKey(p.idempotencyKey)) return null;
  return {
    draft: p.draft as Record<string, unknown>,
    draftId: typeof p.draftId === "string" && p.draftId !== "" ? p.draftId : null,
    submit: p.submit !== false,
    subject: typeof p.subject === "string" ? p.subject : "",
    ...(typeof p.identityId === "string" ? { identityId: p.identityId } : {}),
    ...(typeof p.sendAt === "string" ? { sendAt: p.sendAt } : {}),
    idempotencyKey: p.idempotencyKey,
    ...(Array.isArray(p.archiveOnSuccess) &&
    p.archiveOnSuccess.length > 0 &&
    p.archiveOnSuccess.every((id) => typeof id === "string")
      ? { archiveOnSuccess: p.archiveOnSuccess }
      : {}),
    ...(p.encrypt === true ? { encrypt: true } : {}),
  };
}
