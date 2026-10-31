// SPDX-License-Identifier: AGPL-3.0-only

// When the composer may fetch the message it is about (#612) — ./subjectFetch.ts.
//
// The harm is a composer that sits on "Opening…" for ever: on a reload the
// read used to fire before the session existed, fail, and never be asked again.
// Every "fetch" case sits next to the "do not fetch" case that bounds it,
// because "always fetch" passes the first half and is a request per render.

import { describe, expect, it } from "vitest";

import { shouldFetchSubject } from "./subjectFetch";
import type { SubjectFetchInput } from "./subjectFetch";

const base: SubjectFetchInput = {
  wantId: "d1",
  needsBody: true,
  hasBody: false,
  sessionReady: true,
  phase: "idle",
  failedAtSync: null,
  lastSyncAt: 100,
};

describe("shouldFetchSubject (#612)", () => {
  it("does NOT fetch before the session exists — the reload that hung", () => {
    expect(shouldFetchSubject({ ...base, sessionReady: false, lastSyncAt: null })).toBe(false);
  });

  it("fetches once the session exists", () => {
    // The same composer one render later, after AppProvider connected.
    expect(shouldFetchSubject({ ...base, sessionReady: true })).toBe(true);
  });

  it("does not fetch twice while one read is in flight, or after it answered", () => {
    expect(shouldFetchSubject({ ...base, phase: "inflight" })).toBe(false);
    expect(shouldFetchSubject({ ...base, phase: "done" })).toBe(false);
  });

  it("retries a FAILED read once a later sync has succeeded", () => {
    expect(shouldFetchSubject({ ...base, phase: "failed", failedAtSync: 100, lastSyncAt: 160 })).toBe(true);
  });

  it("NEGATIVE CONTROL: does not retry a failed read until the connection has proven itself", () => {
    // Same sync stamp as the failure: nothing has succeeded since, so asking
    // again would be a request per render against a server that is down.
    expect(shouldFetchSubject({ ...base, phase: "failed", failedAtSync: 100, lastSyncAt: 100 })).toBe(false);
    expect(shouldFetchSubject({ ...base, phase: "failed", failedAtSync: null, lastSyncAt: null })).toBe(false);
  });

  it("fetches nothing a composer does not need", () => {
    expect(shouldFetchSubject({ ...base, wantId: null })).toBe(false); // blank composer
    expect(shouldFetchSubject({ ...base, needsBody: false })).toBe(false); // a reply
    expect(shouldFetchSubject({ ...base, hasBody: true })).toBe(false); // already loaded
  });
});
