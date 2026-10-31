// SPDX-License-Identifier: AGPL-3.0-only

// Offline vs. stale vs. error (#375 increment 6).
//
// The reason this function exists is the reason these tests are mostly about
// PRECEDENCE: all three states are true at once fairly often — a laptop that
// lost its wifi has a failed request AND an account whose health is hours old
// AND no connection — and each one sends the reader somewhere different. Saying
// "sync is behind" to someone whose own connection dropped points them at the
// server; saying "offline" to someone whose provider is refusing Mailroom's
// credentials points them at their router. Only one of them is the cause.
//
// The other half is the nulls. `AccountSyncStatus` is nullable in every field
// that matters, precisely for accounts that have never worked, and those are
// the accounts most likely to be the one named — so "never synced" must not
// come out the far end as "synced 0 s ago".

import { describe, expect, it } from "vitest";

import { STALE_AFTER_SECONDS, headerState } from "./headerState";
import type { HeaderAccount, HeaderStateInput } from "./headerState";

const NOW = 1_700_000_000_000;

function input(over: Partial<HeaderStateInput> = {}): HeaderStateInput {
  return {
    conn: "online",
    syncError: null,
    syncErrorKind: null,
    queueDepth: 0,
    lastSyncAt: NOW - 30_000,
    accounts: [],
    ...over,
  };
}

function account(over: Partial<HeaderAccount> = {}): HeaderAccount {
  return { name: "Personal", status: "ok", lagSeconds: 10, ...over };
}

describe("headerState — the plain branches", () => {
  it("is ok when the connection is up, nothing failed, and every account is current", () => {
    expect(headerState(input({ accounts: [account()] }), NOW)).toEqual({ kind: "ok" });
  });

  it("is offline when the connection state says so, and carries what the user needs", () => {
    const state = headerState(
      input({ conn: "offline", queueDepth: 3, lastSyncAt: NOW - 840_000 }),
      NOW,
    );
    expect(state).toEqual({ kind: "offline", lastSyncAt: NOW - 840_000, queued: 3 });
  });

  it("is offline for a TRANSPORT failure while the connection is not believed online", () => {
    const state = headerState(
      input({ conn: "connecting", syncError: "jmap: fetch failed", syncErrorKind: "transport" }),
      NOW,
    );
    expect(state.kind).toBe("offline");
  });

  it("NEGATIVE CONTROL: a transport blip on a live connection is not offline", () => {
    // One dropped request while the poll still believes it is online is a blip
    // the next tick clears. Flashing the whole header for it would be the
    // banner that cries wolf.
    const state = headerState(
      input({ conn: "online", syncError: "jmap: fetch failed", syncErrorKind: "transport" }),
      NOW,
    );
    expect(state.kind).toBe("error");
  });

  it("NEGATIVE CONTROL: a SERVER refusal off-line-state is not offline", () => {
    const state = headerState(
      input({ conn: "connecting", syncError: "unknownMethod", syncErrorKind: "server" }),
      NOW,
    );
    expect(state).toEqual({ kind: "error", message: "unknownMethod" });
  });

  it("is stale when an account is further behind than the threshold", () => {
    const state = headerState(
      input({ accounts: [account({ lagSeconds: STALE_AFTER_SECONDS + 1 })] }),
      NOW,
    );
    expect(state).toEqual({
      kind: "stale",
      account: "Personal",
      lagSeconds: STALE_AFTER_SECONDS + 1,
    });
  });

  it("NEGATIVE CONTROL: exactly at the threshold is not yet stale", () => {
    const state = headerState(
      input({ accounts: [account({ lagSeconds: STALE_AFTER_SECONDS })] }),
      NOW,
    );
    expect(state).toEqual({ kind: "ok" });
  });

  it("is stale for a failing or paused account whatever its lag", () => {
    for (const status of ["failing", "paused"] as const) {
      const state = headerState(input({ accounts: [account({ status, lagSeconds: 5 })] }), NOW);
      expect(state.kind).toBe("stale");
    }
  });

  it("is an error when the server answered and said no", () => {
    const state = headerState(
      input({ syncError: "Area/getSummary: unknownMethod", syncErrorKind: "server" }),
      NOW,
    );
    expect(state).toEqual({ kind: "error", message: "Area/getSummary: unknownMethod" });
  });
});

describe("headerState — precedence", () => {
  it("offline beats stale", () => {
    const state = headerState(
      input({
        conn: "offline",
        queueDepth: 1,
        accounts: [account({ status: "failing", lagSeconds: 99_999 })],
      }),
      NOW,
    );
    expect(state.kind).toBe("offline");
  });

  it("offline beats error", () => {
    const state = headerState(
      input({ conn: "offline", syncError: "boom", syncErrorKind: "server" }),
      NOW,
    );
    expect(state.kind).toBe("offline");
  });

  it("stale beats error", () => {
    const state = headerState(
      input({
        syncError: "boom",
        syncErrorKind: "server",
        accounts: [account({ status: "failing" })],
      }),
      NOW,
    );
    expect(state.kind).toBe("stale");
  });

  it("offline beats both at once", () => {
    const state = headerState(
      input({
        conn: "offline",
        syncError: "boom",
        syncErrorKind: "server",
        accounts: [account({ status: "failing" })],
      }),
      NOW,
    );
    expect(state.kind).toBe("offline");
  });
});

describe("headerState — which account gets named", () => {
  it("names the failing one over the paused one", () => {
    const state = headerState(
      input({
        accounts: [
          account({ name: "Paused", status: "paused", lagSeconds: 99_999 }),
          account({ name: "Failing", status: "failing", lagSeconds: 60 }),
        ],
      }),
      NOW,
    );
    expect(state).toMatchObject({ kind: "stale", account: "Failing" });
  });

  it("names the furthest behind when two are equally bad", () => {
    const state = headerState(
      input({
        accounts: [
          account({ name: "Near", status: "failing", lagSeconds: 1_000 }),
          account({ name: "Far", status: "failing", lagSeconds: 90_000 }),
        ],
      }),
      NOW,
    );
    expect(state).toMatchObject({ kind: "stale", account: "Far" });
  });

  it("ignores the healthy accounts entirely", () => {
    const state = headerState(
      input({
        accounts: [
          account({ name: "Fine", lagSeconds: 3 }),
          account({ name: "Behind", lagSeconds: STALE_AFTER_SECONDS + 60 }),
          account({ name: "Also fine", lagSeconds: 0 }),
        ],
      }),
      NOW,
    );
    expect(state).toMatchObject({ kind: "stale", account: "Behind" });
  });
});

describe("headerState — only the clauses the health data carries", () => {
  it("dates the outage when there is a recorded error to date", () => {
    const state = headerState(
      input({
        accounts: [
          account({
            status: "failing",
            lastError: "dial tcp: connection refused",
            lastErrorAt: "2026-09-07T09:02:00Z",
          }),
        ],
      }),
      NOW,
    );
    expect(state).toMatchObject({ kind: "stale", since: "2026-09-07T09:02:00Z" });
  });

  it("omits the date when there is an error with no timestamp on it", () => {
    const state = headerState(
      input({ accounts: [account({ status: "failing", lastError: "refused", lastErrorAt: null })] }),
      NOW,
    );
    expect(state).not.toHaveProperty("since");
  });

  it("omits the date when the account is merely paused, with no error at all", () => {
    const state = headerState(input({ accounts: [account({ status: "paused" })] }), NOW);
    expect(state).not.toHaveProperty("since");
  });

  it("reports a null lag rather than zero for an account that has never synced", () => {
    // The whole point: an account that has NEVER synced is the one most likely
    // to be named, and "last synced 0 s ago" about it would be an invented
    // number in the sentence the user is reading to find out what is wrong.
    const state = headerState(
      input({ accounts: [account({ status: "failing", lagSeconds: null })] }),
      NOW,
    );
    expect(state).toMatchObject({ kind: "stale", lagSeconds: null });
  });

  it("carries a retry cadence when one is supplied, and omits it otherwise", () => {
    const withCadence = headerState(
      input({ accounts: [account({ status: "failing", retryEverySeconds: 240 })] }),
      NOW,
    );
    expect(withCadence).toMatchObject({ retryEverySeconds: 240 });
    // Which is what the app actually passes today: `Account/getHealth` reports
    // what HAPPENED, not the cadence it will happen at, so the clause is simply
    // absent rather than filled with an interval nobody measured.
    const without = headerState(
      input({ accounts: [account({ status: "failing", retryEverySeconds: null })] }),
      NOW,
    );
    expect(without).not.toHaveProperty("retryEverySeconds");
  });
});
