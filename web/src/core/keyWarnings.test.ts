// SPDX-License-Identifier: AGPL-3.0-only

// The desk's key-expiry banner logic (#795), against captured keys: which keys
// are warned about, and dismissal per key PER STATE — a dismissed "expires on"
// comes back as "expired" when the key lapses, and storage that throws loses
// the memory, never the desk.

import { describe, expect, it } from "vitest";

import {
  CAPTURED_AT,
  IDENTITY_READER,
  USER_KEY_BOUND,
  USER_KEY_REVOKED,
  USER_KEY_TEN_DAYS,
} from "./keyFixtures.testdata";
import {
  DISMISSED_KEY_WARNINGS,
  dismissWarning,
  loadDismissedWarnings,
  ownKeyWarnings,
  visibleWarnings,
  warningKey,
} from "./keyWarnings";

function memoryStorage(): Storage & { data: Map<string, string> } {
  const data = new Map<string, string>();
  return {
    data,
    get length() {
      return data.size;
    },
    clear: () => data.clear(),
    getItem: (k: string) => data.get(k) ?? null,
    key: (i: number) => [...data.keys()][i] ?? null,
    removeItem: (k: string) => void data.delete(k),
    setItem: (k: string, v: string) => void data.set(k, v),
  };
}

const ids = [IDENTITY_READER];

describe("which keys are warned about", () => {
  it("a usable key inside the server's window, and nothing else of the captured set", () => {
    const w = ownKeyWarnings([USER_KEY_BOUND, USER_KEY_TEN_DAYS, USER_KEY_REVOKED], ids, 30, CAPTURED_AT);
    expect(w).toEqual([
      {
        keyId: USER_KEY_TEN_DAYS.id,
        state: "expiring",
        at: "2026-10-31T09:50:21Z",
        address: null,
        fingerprint: USER_KEY_TEN_DAYS.fingerprint,
      },
    ]);
  });

  it("a BOUND key that has expired is warned about, with its address; an unbound expired one is not", () => {
    const expiredBound = { ...USER_KEY_BOUND, expired: true, usable: false, expiringSoon: false };
    const expiredUnbound = { ...USER_KEY_TEN_DAYS, expired: true, usable: false, expiringSoon: false };
    const w = ownKeyWarnings([expiredUnbound, expiredBound], ids, 30, CAPTURED_AT);
    expect(w.map((x) => [x.keyId, x.state, x.address])).toEqual([[USER_KEY_BOUND.id, "expired", "reader@example.com"]]);
  });

  it("a retired or revoked key is never warned about, even inside the window", () => {
    const retired = { ...USER_KEY_TEN_DAYS, decryptOnly: true };
    const revoked = { ...USER_KEY_REVOKED, expiringSoon: true };
    expect(ownKeyWarnings([retired, revoked], ids, 30, CAPTURED_AT)).toEqual([]);
  });
});

describe("dismissal, per key per state", () => {
  it("hides the dismissed warning only, and remembers it in storage", () => {
    const store = memoryStorage();
    const w = ownKeyWarnings([USER_KEY_TEN_DAYS], ids, 30, CAPTURED_AT);
    const dismissed = dismissWarning(w[0]!, w, store);
    expect(visibleWarnings(w, dismissed)).toEqual([]);
    expect(JSON.parse(store.data.get(DISMISSED_KEY_WARNINGS)!)).toEqual([`${USER_KEY_TEN_DAYS.id}:expiring`]);
    expect(visibleWarnings(w, loadDismissedWarnings(store))).toEqual([]);
  });

  it("comes back when the state changes from expiring to expired", () => {
    const store = memoryStorage();
    const bound = { ...USER_KEY_TEN_DAYS, identityId: IDENTITY_READER.id };
    const soon = ownKeyWarnings([bound], ids, 30, CAPTURED_AT);
    dismissWarning(soon[0]!, soon, store);
    const lapsed = ownKeyWarnings([{ ...bound, expired: true, usable: false, expiringSoon: false }], ids, 30, CAPTURED_AT);
    expect(warningKey(lapsed[0]!)).toBe(`${bound.id}:expired`);
    expect(visibleWarnings(lapsed, loadDismissedWarnings(store))).toHaveLength(1);
  });

  it("drops entries for warnings that no longer exist when it writes", () => {
    const store = memoryStorage();
    store.setItem(DISMISSED_KEY_WARNINGS, JSON.stringify(["gone:expiring"]));
    const w = ownKeyWarnings([USER_KEY_TEN_DAYS], ids, 30, CAPTURED_AT);
    dismissWarning(w[0]!, w, store);
    expect(JSON.parse(store.data.get(DISMISSED_KEY_WARNINGS)!)).toEqual([`${USER_KEY_TEN_DAYS.id}:expiring`]);
  });

  it("storage that throws or holds junk loses the memory, not the banner", () => {
    const throwing = {
      getItem: () => {
        throw new Error("denied");
      },
      setItem: () => {
        throw new Error("denied");
      },
    };
    const w = ownKeyWarnings([USER_KEY_TEN_DAYS], ids, 30, CAPTURED_AT);
    expect(loadDismissedWarnings(throwing).size).toBe(0);
    expect(() => dismissWarning(w[0]!, w, throwing)).not.toThrow();
    const junk = memoryStorage();
    junk.setItem(DISMISSED_KEY_WARNINGS, "{not json");
    expect(loadDismissedWarnings(junk).size).toBe(0);
    junk.setItem(DISMISSED_KEY_WARNINGS, JSON.stringify({ a: 1 }));
    expect(loadDismissedWarnings(junk).size).toBe(0);
  });
});
