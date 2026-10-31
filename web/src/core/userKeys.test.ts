// SPDX-License-Identifier: AGPL-3.0-only

// Settings → Keys' non-UI half (#796, #795), against responses captured from
// the real server (keyFixtures.testdata.ts): the error mapping (no raw codes,
// no guessed causes), the expiry facts (an absent fact is not "never"), the
// warning-window judgement (the server's flag first, its threshold second,
// nothing invented), the revoke and backup gates, and the correspondents'
// rows.

import { afterEach, describe, expect, it, vi } from "vitest";

import { JmapMethodError, JmapTransportError } from "./jmap/client";
import {
  CAPTURED_AT,
  CONTACT_KEY_ALICE,
  CONTACT_KEY_CAROL_EXPIRED,
  CONTACTS_FOR_KEYS,
  IDENTITY_READER,
  METHOD_ERRORS,
  SET_ERRORS,
  USER_KEYS_WKD,
  USER_KEY_BOUND,
  USER_KEY_BOUND_PUBLISHED,
  USER_KEY_REVOKED,
  USER_KEY_TEN_DAYS,
} from "./keyFixtures.testdata";
import {
  backupPassphraseProblem,
  contactKeySource,
  correspondentRows,
  downloadArmoredKey,
  expiryFact,
  extendDays,
  fingerprintTail,
  fixedIdentities,
  generateExpiryDays,
  isExpiringSoon,
  keyChips,
  keyDateShort,
  keyErrorOf,
  keyOutcome,
  keySetErrorOf,
  keySetOutcome,
  publishBlock,
  revokeConfirmMatches,
} from "./userKeys";
import type { Identity } from "./jmap/types";

const methodError = (e: { type: string; description?: string }) => new JmapMethodError("c", e);

describe("errors map to the closed set", () => {
  it("each documented method error keeps its own type", () => {
    expect(keyErrorOf(methodError(METHOD_ERRORS.importPublic))).toBe("publicKeyRefused");
    expect(keyErrorOf(methodError(METHOD_ERRORS.importGarbage))).toBe("invalidKey");
    expect(keyErrorOf(methodError(METHOD_ERRORS.exportShort))).toBe("invalidArguments");
    for (const type of ["keyManagementUnavailable", "notFound", "rateLimited", "wildcardIdentity", "tooLarge"]) {
      expect(keyErrorOf(methodError({ type }))).toBe(type);
    }
  });

  it("an unknown type is `failed`, and a dead connection `unavailable`", () => {
    expect(keyErrorOf(methodError({ type: "serverFail", description: "boom" }))).toBe("failed");
    expect(keyErrorOf(methodError({ type: "somethingNew" }))).toBe("failed");
    expect(keyErrorOf(new JmapTransportError("request failed"))).toBe("unavailable");
    expect(keyErrorOf(new Error("x"))).toBe("failed");
  });

  it("each documented SetError keeps its own type; others are `failed`", () => {
    expect(keySetErrorOf(SET_ERRORS.notPublishable)).toBe("notPublishable");
    expect(keySetErrorOf(SET_ERRORS.keyIsDefault)).toBe("keyIsDefault");
    expect(keySetErrorOf(SET_ERRORS.keyIsPublished)).toBe("keyIsPublished");
    expect(keySetErrorOf(SET_ERRORS.oneChangeAtATime)).toBe("oneChangeAtATime");
    expect(keySetErrorOf({ type: "forbidden" })).toBe("failed");
  });

  it("keyOutcome and keySetOutcome never throw", async () => {
    await expect(keyOutcome(async () => 1)).resolves.toEqual({ ok: true, value: 1 });
    await expect(keyOutcome(() => Promise.reject(methodError({ type: "rateLimited" })))).resolves.toEqual({
      ok: false,
      error: "rateLimited",
    });
    await expect(keySetOutcome(async () => SET_ERRORS.keyIsPublished)).resolves.toEqual({
      ok: false,
      error: "keyIsPublished",
    });
    await expect(keySetOutcome(async () => null)).resolves.toEqual({ ok: true, value: undefined });
  });
});

describe("expiry", () => {
  it("states expires / expired / never only when the server said", () => {
    expect(expiryFact(USER_KEY_BOUND)).toEqual({ kind: "expires", at: "2028-10-20T09:50:21Z" });
    expect(expiryFact({ ...USER_KEY_BOUND, expired: true })).toEqual({ kind: "expired", at: "2028-10-20T09:50:21Z" });
    expect(expiryFact({ ...USER_KEY_BOUND, expiresAt: null })).toEqual({ kind: "never" });
    // A server before #795 sends neither field: nothing is claimed.
    const old = { ...USER_KEY_BOUND } as Record<string, unknown>;
    delete old["expiresAt"];
    delete old["expired"];
    expect(expiryFact(old as never)).toEqual({ kind: "unknown" });
  });

  it("the server's expiringSoon wins; the capability's threshold judges only a flagless date", () => {
    expect(isExpiringSoon(USER_KEY_TEN_DAYS, 30, CAPTURED_AT)).toBe(true);
    expect(isExpiringSoon(USER_KEY_BOUND, 30, CAPTURED_AT)).toBe(false);
    // The server's flag is not second-guessed by a bigger client threshold.
    expect(isExpiringSoon(USER_KEY_BOUND, 1000, CAPTURED_AT)).toBe(false);
    const flagless = { expiresAt: USER_KEY_TEN_DAYS.expiresAt, usable: true };
    expect(isExpiringSoon(flagless, 30, CAPTURED_AT)).toBe(true);
    expect(isExpiringSoon(flagless, 5, CAPTURED_AT)).toBe(false);
    // No threshold from the server: no judgement at all.
    expect(isExpiringSoon(flagless, null, CAPTURED_AT)).toBe(false);
  });

  it("short dates carry the year only when it differs", () => {
    expect(keyDateShort("2026-10-31T09:50:21Z", CAPTURED_AT)).toBe("31 Oct");
    expect(keyDateShort("2028-10-20T09:50:21Z", CAPTURED_AT)).toBe("20 Oct 2028");
  });
});

describe("chips and publishing", () => {
  const bound: Identity = { ...IDENTITY_READER, signingKeyId: USER_KEY_BOUND.id };

  it("a bound, published key signs as its address and says published", () => {
    expect(keyChips(USER_KEY_BOUND_PUBLISHED, [bound], 30, CAPTURED_AT)).toEqual([
      { kind: "signsAs", address: "reader@example.com" },
      { kind: "published" },
    ]);
  });

  it("an unbound ten-day key says it expires soon; a revoked one says revoked and nothing about expiry", () => {
    expect(keyChips(USER_KEY_TEN_DAYS, [bound], 30, CAPTURED_AT)).toEqual([{ kind: "expiringSoon" }]);
    expect(keyChips({ ...USER_KEY_REVOKED, expiringSoon: true }, [bound], 30, CAPTURED_AT)).toEqual([{ kind: "revoked" }]);
    expect(keyChips({ ...USER_KEY_TEN_DAYS, decryptOnly: true }, [], 30, CAPTURED_AT)).toEqual([{ kind: "retired" }]);
  });

  it("publishing is blocked with the reason the server would give", () => {
    expect(publishBlock(USER_KEY_BOUND, USER_KEYS_WKD.wkdConfigured)).toBeNull();
    expect(publishBlock(USER_KEY_TEN_DAYS, true)).toBe("notBound");
    expect(publishBlock(USER_KEY_BOUND, false)).toBe("wkdOff");
    expect(publishBlock(USER_KEY_BOUND, null)).toBe("unknown");
    expect(publishBlock({ ...USER_KEY_BOUND, wkdServable: false }, true)).toBe("notServable");
    expect(publishBlock(USER_KEY_REVOKED, true)).toBe("retired");
  });

  it("only fixed identities can carry a key", () => {
    const wild = { ...IDENTITY_READER, id: "W", email: "*@example.com", isWildcard: true } as Identity;
    expect(fixedIdentities([wild, IDENTITY_READER]).map((i) => i.id)).toEqual([IDENTITY_READER.id]);
  });
});

describe("the deliberate steps", () => {
  it("revoke asks for the last 8 hex digits, forgiving only case and spaces", () => {
    const fpr = USER_KEY_TEN_DAYS.fingerprint;
    expect(fingerprintTail(fpr)).toBe("1A905815");
    // The server's own refusal names the same tail (captured).
    expect(METHOD_ERRORS.revokeWrongConfirm.description).toContain(fingerprintTail(fpr));
    expect(revokeConfirmMatches(fpr, "1A905815")).toBe(true);
    expect(revokeConfirmMatches(fpr, "1a90 5815")).toBe(true);
    expect(revokeConfirmMatches(fpr, "1A90581")).toBe(false);
    expect(revokeConfirmMatches(fpr, "1A9058155")).toBe(false);
    expect(revokeConfirmMatches(fpr, "00000000")).toBe(false);
    expect(revokeConfirmMatches(fpr, "")).toBe(false);
  });

  it("a backup passphrase needs 12 characters (counted as the server counts) and a matching repeat", () => {
    expect(backupPassphraseProblem("short", "short")).toBe("tooShort");
    expect(backupPassphraseProblem("correct horse battery", "correct horse batterY")).toBe("mismatch");
    expect(backupPassphraseProblem("correct horse battery", "correct horse battery")).toBeNull();
    // 11 emoji are 22 UTF-16 units but 11 characters: still too short.
    expect(backupPassphraseProblem("😀".repeat(11), "😀".repeat(11))).toBe("tooShort");
    expect(backupPassphraseProblem("😀".repeat(12), "😀".repeat(12))).toBeNull();
  });

  it("extend: one or two years, or 1..3650 whole days", () => {
    expect(extendDays("1y", "")).toBe(365);
    expect(extendDays("2y", "")).toBe(730);
    expect(extendDays("custom", "90")).toBe(90);
    expect(extendDays("custom", "3650")).toBe(3650);
    expect(extendDays("custom", "99999")).toBeNull(); // the server refuses it too (captured)
    expect(extendDays("custom", "0")).toBeNull();
    expect(extendDays("custom", "1.5")).toBeNull();
    expect(extendDays("custom", "")).toBeNull();
  });

  it("generate: two years is the server's default (nothing sent), never is 0", () => {
    expect(generateExpiryDays("2y")).toBeUndefined();
    expect(generateExpiryDays("1y")).toBe(365);
    expect(generateExpiryDays("never")).toBe(0);
  });
});

describe("the backup download", () => {
  afterEach(() => vi.restoreAllMocks());

  it("goes through a Blob URL that is revoked straight after the click", () => {
    const create = vi.fn(() => "blob:key");
    const revoke = vi.fn();
    vi.stubGlobal("URL", { createObjectURL: create, revokeObjectURL: revoke });
    const clicked: { href: string; download: string }[] = [];
    const anchor = {
      href: "",
      download: "",
      rel: "",
      style: { display: "" },
      click() {
        clicked.push({ href: this.href, download: this.download });
        // Not yet revoked at the moment of the click.
        expect(revoke).not.toHaveBeenCalled();
      },
      remove: vi.fn(),
    };
    const doc = { createElement: () => anchor, body: { appendChild: vi.fn() } } as unknown as Document;
    downloadArmoredKey("-----BEGIN PGP PRIVATE KEY BLOCK-----", "mailroom-key-1a905815.asc", doc);
    expect(clicked).toEqual([{ href: "blob:key", download: "mailroom-key-1a905815.asc" }]);
    expect(revoke).toHaveBeenCalledWith("blob:key");
    expect(anchor.remove).toHaveBeenCalled();
    vi.unstubAllGlobals();
  });
});

describe("correspondents' keys", () => {
  it("names each key by its contact or address, sorted, with its source", () => {
    const rows = correspondentRows([CONTACT_KEY_CAROL_EXPIRED, CONTACT_KEY_ALICE], CONTACTS_FOR_KEYS);
    expect(rows.map((r) => r.name)).toEqual(["alice@example.org", "carol@example.org"]);
    expect(rows[0]?.addresses).toEqual(["alice@example.org"]);
    expect(contactKeySource(CONTACT_KEY_ALICE)).toBe("imported");
    expect(contactKeySource({ source: "autocrypt", authenticated: true })).toBe("autocryptAuthenticated");
    expect(contactKeySource({ source: "autocrypt", authenticated: false })).toBe("autocrypt");
    expect(contactKeySource({ source: "wkd", authenticated: false })).toBe("wkd");
  });

  it("a contact's display name wins over the address", () => {
    const named = CONTACTS_FOR_KEYS.map((c) => (c.id === CONTACT_KEY_ALICE.contactId ? { ...c, displayName: "Alice" } : c));
    expect(correspondentRows([CONTACT_KEY_ALICE], named)[0]?.name).toBe("Alice");
  });
});
