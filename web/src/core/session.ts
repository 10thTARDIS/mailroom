// SPDX-License-Identifier: AGPL-3.0-only

// Session/state-string cursor types for the Mailroom client-core (spec
// docs/mailroom-spec.md §13/§14). Mirrors the RFC 8620 §2 Session object as
// internal/api/session.go actually serves it.

import type { AccountId, StateString } from "./jmap/types";
import { CAP_CORE, CAP_DESK } from "./jmap/types";

/** A JMAP account entry within the Session object (RFC 8620 §2). */
export interface SessionAccount {
  name: string;
  isPersonal: boolean;
  isReadOnly: boolean;
  accountCapabilities: Record<string, unknown>;
}

/** The RFC 8620 §2 Session object. Same-origin, cookie-authenticated.
 *
 * `downloadUrl`, `uploadUrl` and `eventSourceUrl` are TEMPLATES, and (since #85)
 * every one of them is routed: attachments download, compose uploads, and change
 * detection subscribes to the EventSource stream instead of polling. They are
 * expanded in core/jmap/blobs.ts — no module hardcodes a Mailroom URL. */
export interface JmapSession {
  capabilities: Record<string, unknown>;
  accounts: Record<AccountId, SessionAccount>;
  primaryAccounts: Record<string, AccountId>;
  username: string;
  apiUrl: string;
  downloadUrl: string;
  uploadUrl: string;
  eventSourceUrl: string;
  /** Mailroom extension, not RFC 8620 (#131): the inline-rendering route for one
   * MIME part. Optional so an older server (or a fixture) simply yields no
   * preview affordance rather than a broken one. */
  previewUrl?: string;
  state: StateString;
}

/** What to assume for `maxCallsInRequest` when the session does not advertise
 * it. Deliberately small: a batch OVER the server's limit is refused whole
 * (internal/api/service.go answers a request-level 400), so under-guessing
 * costs one extra round trip and over-guessing costs the entire operation. */
export const DEFAULT_MAX_CALLS_IN_REQUEST = 8;

/**
 * How many method calls this server takes in one request (RFC 8620 §2).
 *
 * Read rather than assumed by anything that builds a batch whose size depends
 * on the data — a desk with many areas, a bulk archive with many chunks. The
 * whole batch is refused when it is too long, so the count has to be bounded by
 * what the server said rather than by what looked reasonable.
 */
export function maxCallsInRequest(session: JmapSession | null): number {
  const core = session?.capabilities?.[CAP_CORE] as { maxCallsInRequest?: number } | undefined;
  const n = core?.maxCallsInRequest;
  return typeof n === "number" && n >= 1 ? Math.floor(n) : DEFAULT_MAX_CALLS_IN_REQUEST;
}

/** What to assume for `maxObjectsInGet` when the session does not advertise
 * it. Small for the reason DEFAULT_MAX_CALLS_IN_REQUEST is: under-guessing
 * costs one more Email/get, over-guessing asks for a refusal. */
export const DEFAULT_MAX_OBJECTS_IN_GET = 100;

/**
 * How many ids one `/get` may name on this server (RFC 8620 §2).
 *
 * Read by the desk (#665), whose single Email/get hydrates every section at
 * once: the sum of the sections' windows has to fit, or the ids are fetched in
 * chunks this size.
 */
export function maxObjectsInGet(session: JmapSession | null): number {
  const core = session?.capabilities?.[CAP_CORE] as { maxObjectsInGet?: number } | undefined;
  const n = core?.maxObjectsInGet;
  return typeof n === "number" && n >= 1 ? Math.floor(n) : DEFAULT_MAX_OBJECTS_IN_GET;
}

/**
 * Whether this server answers `Area/getDesk` (#665): the whole desk — the
 * counts and every section's page — from one resolution of every message's
 * area, instead of `Area/getSummary` plus an `Email/query` per section that
 * each resolve it again.
 *
 * Read from the `getDesk` flag of the `urn:mailroom:desk` capability object
 * (internal/api/session.go deskCapability), because an older server advertises
 * the same URN without the method; asking it would cost a failed call.
 */
export function hasDeskSnapshot(session: JmapSession | null): boolean {
  const desk = session?.capabilities?.[CAP_DESK] as { getDesk?: unknown } | undefined;
  return desk?.getDesk === true;
}

/** The JMAP object types Mailroom tracks a `/changes` cursor for.
 *
 * `Area` and `DeskPreferences` (#375, urn:mailroom:desk) are here for the same
 * reason as the rest: both append change_log rows and both serve `/changes`
 * (internal/api/service.go), and both decide what the desk DRAWS — an area
 * renamed or the line moved on another device changes which mail is on the
 * surface here.
 *
 * `ClientPreferences` (#375 increment 5) is here for the same reason and, since
 * #474, is pushed like the rest — see PUSH_TYPES below. */
export type ChangeType =
  | "Email"
  | "Mailbox"
  | "Thread"
  | "Label"
  | "Identity"
  | "EmailSubmission"
  | "Area"
  | "DeskPreferences"
  | "ClientPreferences";

export const CHANGE_TYPES: readonly ChangeType[] = [
  "Email",
  "Mailbox",
  "Thread",
  "Label",
  "Identity",
  "EmailSubmission",
  "Area",
  "DeskPreferences",
  "ClientPreferences",
];

/**
 * The types the EVENT STREAM will accept in `?types=` — ALL of them, since
 * #474.
 *
 * ⚠ THIS IS NOT A TIDINESS ALIAS, AND GETTING IT WRONG BREAKS PUSH ENTIRELY.
 * `internal/api/eventsource.go` lists the pushable types in `eventSourceTypes`
 * and `parseEventTypes` answers an unknown name with a request-level 400 —
 * "this server does not push changes for …" — which is refused WHOLE. So a
 * name in here that the server does not know does not cost one type's
 * freshness, it costs the entire EventSource connection and drops the client
 * back to polling for everything. The two lists have to agree, and this
 * constant is the seam where that is asserted (core/sync.desk.test.ts).
 *
 * It was a strict subset until #474: `ClientPreferences` had its own /changes
 * method and its own change_log rows but had simply been left off the server's
 * list, so appearance changed on the desktop reached the laptop on the fallback
 * poll rather than instantly. `store.ObjectClientPreferences` is in
 * `eventSourceTypes` now, so the subset is the whole set — kept as its own name
 * rather than collapsed into CHANGE_TYPES because the two mean different
 * things: one is "what this client tracks a cursor for", the other is "what
 * this server will push", and the next type added could easily be in the first
 * and not yet the second.
 */
export const PUSH_TYPES: readonly ChangeType[] = CHANGE_TYPES;

/**
 * SyncCursor holds the per-type state strings this client has synced to, so
 * `Foo/changes` calls know where to resume (spec §13's "opaque per-type state
 * strings with exactly-correct `/changes` deltas", backed by the change log,
 * spec §14). A null entry means "never synced this type" — the first fetch of
 * that type establishes the cursor from its `state`/`queryState`.
 */
export type SyncCursor = Record<ChangeType, StateString | null> & {
  accountId: AccountId;
};

/**
 * The RFC 8620 §7.3 StateChange object: what the EventSource stream pushes when
 * a type's state moves. It carries the NEW STATE, not the changes themselves —
 * the client answers it with `Foo/changes`, which is the only authority on what
 * actually moved (and the same call the polling fallback makes).
 */
export interface StateChange {
  "@type": "StateChange";
  changed: Record<AccountId, Partial<Record<ChangeType, StateString>>>;
}

export function emptySyncCursor(accountId: AccountId): SyncCursor {
  return {
    accountId,
    Email: null,
    Mailbox: null,
    Thread: null,
    Label: null,
    Identity: null,
    EmailSubmission: null,
    Area: null,
    DeskPreferences: null,
    ClientPreferences: null,
  };
}
