// SPDX-License-Identifier: AGPL-3.0-only

// WHEN THE COMPOSER MAY FETCH THE MESSAGE IT IS ABOUT (#612, the half v1.73.3 missed).
//
// A resumed draft and a forward both need the message's BODY, which only an
// Email/get that asked for it provides, so ComposeRoute fetches it. It fetched
// exactly once, on mount, and never again — and on a COLD LOAD (a reload, or a
// pasted `?compose=draft:<id>`) mount is before there is a session at all:
// React runs a child's effects before its parent's, and the session is opened by
// AppProvider's effect, the composer's ancestor. The read threw "not connected",
// loadEmails swallowed it, and the composer sat on "Opening…" for ever.
//
// v1.73.3 fixed the URL so a reload named a draft that exists, and did not check
// that a reload could load anything at all. It could not, and never had: before
// that release the URL named a destroyed draft, so the two failures were
// indistinguishable and the issue blamed the one it could see.
//
// So the read waits for the session, and a read that FAILED (as opposed to one
// the server answered with "no such message") is asked again once the
// connection has proven itself since — the next successful sync — rather than
// never. It is a pure function because the composer is JSX and there is no DOM
// in this project's tests (app/ui/routes/ComposeRoute.tsx wires it).

import type { Id } from "../../../core/jmap/types";

export type SubjectFetchPhase = "idle" | "inflight" | "done" | "failed";

export interface SubjectFetchInput {
  /** The message the composer is about, or null for a blank composer. */
  wantId: Id | null;
  /** A forward, a resumed draft, or a reply/reply-all (#646) needs the body;
   * only a brand-new message does not. */
  needsBody: boolean;
  /** The body is already in the store — nothing to fetch. */
  hasBody: boolean;
  /** A session exists: the id every read is scoped to has been set. */
  sessionReady: boolean;
  phase: SubjectFetchPhase;
  /** `lastSyncAt` when the last attempt failed. */
  failedAtSync: number | null;
  /** The store's `lastSyncAt` now. */
  lastSyncAt: number | null;
}

export function shouldFetchSubject(i: SubjectFetchInput): boolean {
  if (!i.wantId || !i.needsBody || i.hasBody) return false;
  // THE COLD-LOAD CASE: nothing can be read before the session exists, and a
  // read attempted anyway is the one that used to be spent and never repeated.
  if (!i.sessionReady) return false;
  if (i.phase === "inflight" || i.phase === "done") return false;
  // A failure is retried only once a later sync has succeeded: retrying on a
  // timer against a server that is down would be a request per render.
  if (i.phase === "failed") return i.lastSyncAt !== null && i.lastSyncAt !== i.failedAtSync;
  return true;
}
