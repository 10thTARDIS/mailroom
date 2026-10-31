// SPDX-License-Identifier: AGPL-3.0-only

// The Contacts page's decisions (#816), against REAL #828 responses
// (contactsFixtures.testdata.ts) wherever the server's behaviour is the thing
// being followed.

import { describe, expect, it } from "vitest";

import {
  contactIdentity,
  contactLabel,
  contactMethodErrorOf,
  contactName,
  contactSetErrorOf,
  contactsQueryArgs,
  learnedNameBeside,
  mergePreview,
  notesLength,
  recentMailQuery,
  separateAllowed,
  separateOutcome,
} from "./contacts";
import {
  ADA_MERGED,
  ADA_NAMED,
  ADA_PRE_MERGE,
  ADA_SEPARATED,
  ADA_THEIRS,
  ALAN_WITH_KEY,
  ERR_MERGE_NOT_FOUND,
  ERR_MERGE_SELF,
  ERR_MERGE_STALE,
  ERR_SEPARATE_ALL,
  GRACE_VIP_UNSAVED,
  NEWS_BLOCKED,
  Q_VIP_ANY,
  Q_VIP_SAVED,
  SET_NOTES_TOO_LONG,
  SET_USERNAME_TOO_LONG,
  SEPARATE_RESULT,
  SEPARATE_RESULT_KEPT_NOTES,
  SEPARATE_RESULT_PARTIAL,
  SET_VIP_AND_UNSAVE,
} from "./contactsFixtures.testdata";
import { formatRoute, parseRoute, escapeTarget, isListRoute } from "./routes";

const refusal = (r: { notUpdated?: Record<string, unknown> | null }) =>
  Object.values(r.notUpdated ?? {})[0] as { type: string; properties?: string[] };

describe("which Contact/query each view of the list sends (#816)", () => {
  it("lists only SAVED contacts by default, by name", () => {
    expect(contactsQueryArgs({ filter: "saved", sort: "name", text: "", position: 0 })).toEqual({
      filter: { isSaved: true },
      sort: [{ property: "name", isAscending: true }],
      position: 0,
      limit: 50,
    });
  });

  it("Blocked senders asks for isBlocked WITHOUT isSaved (blocked senders are usually not saved)", () => {
    const args = contactsQueryArgs({ filter: "blocked", sort: "name", text: "", position: 0 });
    expect(args.filter).toEqual({ isBlocked: true });
    expect("isSaved" in args.filter).toBe(false);
    // The captured fact behind it: the blocked theatre was never saved.
    expect(NEWS_BLOCKED.isBlocked).toBe(true);
    expect(NEWS_BLOCKED.isSaved).toBe(false);
  });

  it("VIP asks for every VIP, because the server keeps VIP when a VIP is un-saved", () => {
    const args = contactsQueryArgs({ filter: "vip", sort: "name", text: "", position: 0 });
    expect(args.filter).toEqual({ isVip: true });
    // Captured at the same moment: Grace was un-saved as a VIP. {isVip} finds
    // her; {isSaved, isVip} would have found nobody.
    expect(GRACE_VIP_UNSAVED.isVip).toBe(true);
    expect(GRACE_VIP_UNSAVED.isSaved).toBe(false);
    expect(Q_VIP_ANY.query.ids).toEqual([GRACE_VIP_UNSAVED.id]);
    expect(Q_VIP_SAVED.query.total).toBe(0);
  });

  it("Has a key stays among saved contacts", () => {
    expect(contactsQueryArgs({ filter: "key", sort: "name", text: "", position: 0 }).filter).toEqual({
      isSaved: true,
      hasKey: true,
    });
  });

  it("Last mail sorts newest first, sending the direction rather than trusting the default", () => {
    expect(contactsQueryArgs({ filter: "saved", sort: "last", text: "", position: 0 }).sort).toEqual([
      { property: "lastMailAt", isAscending: false },
    ]);
  });

  it("carries the text trimmed, and the page window", () => {
    const args = contactsQueryArgs({ filter: "saved", sort: "name", text: "  ada ", position: 50 });
    expect(args.filter).toEqual({ isSaved: true, text: "ada" });
    expect(args.position).toBe(50);
    expect(args.limit).toBe(50);
  });
});

describe("names", () => {
  it("is the server's name, else the address", () => {
    expect(contactName(ADA_NAMED)).toBe("Countess Ada");
    expect(contactName({ ...ADA_NAMED, name: "", displayName: "" })).toBe("ada@example.com");
  });

  it("shows the learned name beside the user's own, and not once it is cleared", () => {
    expect(learnedNameBeside(ADA_NAMED)).toBe("Ada Lovelace");
    expect(ADA_THEIRS.userName).toBeNull();
    expect(learnedNameBeside(ADA_THEIRS)).toBeNull();
  });
});

describe("a stranger's text goes through one formatter (#816 security review)", () => {
  // DERIVED: the captured theatre contact with a hostile learned name.
  const rlo = { ...NEWS_BLOCKED, name: "\u202Eevil\u202C Theatre", displayName: "\u202Eevil\u202C Theatre" };
  const ceo = { ...NEWS_BLOCKED, name: "ceo@example.com", displayName: "ceo@example.com" };

  it("strips bidi controls from the name, and then shows the real address beside it", () => {
    expect(contactName(rlo)).toBe("evil Theatre");
    expect(contactIdentity(rlo).showAddress).toBe(true);
    expect(contactLabel(rlo)).toBe("evil Theatre <updates@news.theatre.example.com>");
  });

  it("a name that looks like an address shows the real address beside it", () => {
    expect(contactIdentity(ceo)).toEqual({
      name: "ceo@example.com",
      address: "updates@news.theatre.example.com",
      showAddress: true,
    });
  });

  it("an ordinary name does not", () => {
    expect(contactIdentity(NEWS_BLOCKED).showAddress).toBe(false);
    expect(contactLabel(NEWS_BLOCKED)).toBe("The Grand Theatre");
  });

  it("strips controls from addresses and the learned name too", () => {
    const addr = { ...ADA_NAMED, addresses: [{ address: "ada\u202E@example.com", reachedMeAt: null }] };
    expect(contactIdentity(addr).address).toBe("ada@example.com");
    expect(learnedNameBeside({ ...ADA_NAMED, displayName: "Ada\u200F Lovelace" })).toBe("Ada Lovelace");
  });
});

describe("notes length is counted as the server counts it", () => {
  it("counts code points, and a CRLF as one", () => {
    expect(notesLength("a\r\nb")).toBe(3);
    expect(notesLength("😀")).toBe(1);
    expect(notesLength("m".repeat(4000))).toBe(4000);
  });
});

describe("recent mail covers EVERY address", () => {
  it("ORs from: and to: for each one", () => {
    const q = recentMailQuery(ADA_MERGED.addresses);
    expect(q.query).toBe(
      "from:ada@example.com OR to:ada@example.com OR from:updates@news.theatre.example.com OR to:updates@news.theatre.example.com",
    );
    expect(q.skipped).toEqual([]);
  });

  it("leaves out, and reports, an address the syntax cannot carry", () => {
    const q = recentMailQuery([
      { address: "ok@example.com", reachedMeAt: null },
      { address: "has space@example.com", reachedMeAt: null },
      { address: "example.com", reachedMeAt: null },
    ]);
    expect(q.query).toBe("from:ok@example.com OR to:ok@example.com");
    expect(q.skipped).toEqual(["has space@example.com", "example.com"]);
  });
});

describe("what a merge combines (ADR-041 §4)", () => {
  it("says the block on the merged-in contact carries over, and the server agreed", () => {
    const p = mergePreview(ADA_PRE_MERGE, NEWS_BLOCKED);
    expect(p.block).toEqual({ destination: "trash", from: "from" });
    // Captured: after the merge Ada IS blocked, to Trash.
    expect(ADA_MERGED.isBlocked).toBe(true);
    expect(ADA_MERGED.blockDestination).toBe("trash");
    expect(p.addresses).toEqual(ADA_MERGED.addresses.map((a) => a.address));
    expect(p.name).toBe(ADA_MERGED.name);
    expect(p.saved).toBe(true);
  });

  it("no block on either: none carries", () => {
    expect(mergePreview(ADA_PRE_MERGE, { ...NEWS_BLOCKED, isBlocked: false }).block).toBeNull();
  });

  it("the stricter destination among the blocked sides", () => {
    const junk = { ...ADA_PRE_MERGE, isBlocked: true, blockDestination: "junk" as const };
    expect(mergePreview(junk, NEWS_BLOCKED).block).toEqual({ destination: "trash", from: "both" });
    expect(mergePreview(junk, { ...NEWS_BLOCKED, isBlocked: false }).block).toEqual({ destination: "junk", from: "into" });
  });

  it("counts the keys that move, and joined notes over the limit", () => {
    expect(mergePreview(ADA_PRE_MERGE, ALAN_WITH_KEY).keysFrom).toBe(1);
    const long = { ...NEWS_BLOCKED, notes: "x".repeat(3990) };
    expect(mergePreview(ADA_PRE_MERGE, long).notesTooLong).toBe(true);
    expect(mergePreview(ADA_PRE_MERGE, { ...NEWS_BLOCKED, notes: "short" }).notesJoined).toBe(true);
  });
});

describe("separate", () => {
  it("needs at least one address and never all of them", () => {
    const ids = ADA_MERGED.addresses.map((a) => a.id as string);
    expect(separateAllowed(ADA_MERGED, new Set())).toBe("none");
    expect(separateAllowed(ADA_MERGED, new Set(ids))).toBe("all");
    expect(separateAllowed(ADA_MERGED, new Set([ids[1] as string]))).toBe("ok");
    expect(ADA_SEPARATED.addresses).toHaveLength(1);
  });

  it("reads what the separate did to the contact left (#834)", () => {
    expect(separateOutcome(SEPARATE_RESULT)).toBeNull();
    expect(separateOutcome(SEPARATE_RESULT_PARTIAL)).toEqual({ kind: "unchanged" });
    expect(separateOutcome(SEPARATE_RESULT_KEPT_NOTES)).toEqual({ kind: "restored", kept: ["notes"] });
    // The page's order; the learned name and anything unknown are not the user's edits.
    expect(
      separateOutcome({ ...SEPARATE_RESULT, restored: true, keptFields: ["isBlocked", "displayName", "notes", "later"] }),
    ).toEqual({ kind: "restored", kept: ["notes", "isBlocked"] });
  });
});

describe("refusals are named, never printed as codes", () => {
  it("maps the captured Contact/set refusals by the property the server named", () => {
    expect(contactSetErrorOf(refusal(SET_VIP_AND_UNSAVE))).toBe("vipConflict");
    expect(contactSetErrorOf(refusal(SET_USERNAME_TOO_LONG))).toBe("userNameTooLong");
    expect(contactSetErrorOf(refusal(SET_NOTES_TOO_LONG))).toBe("notesTooLong");
    expect(contactSetErrorOf({ type: "notFound" })).toBe("notFound");
  });

  it("maps the captured merge and separate errors", () => {
    expect(contactMethodErrorOf(ERR_MERGE_SELF)).toBe("invalid");
    expect(contactMethodErrorOf(ERR_MERGE_NOT_FOUND)).toBe("notFound");
    expect(contactMethodErrorOf(ERR_MERGE_STALE)).toBe("stateMismatch");
    expect(contactMethodErrorOf(ERR_SEPARATE_ALL)).toBe("invalid");
    expect(contactMethodErrorOf({ type: "notesTooLong" })).toBe("notesTooLong");
    expect(contactMethodErrorOf(new Error("socket"))).toBe("failed");
  });
});

describe("the contacts routes", () => {
  it("round-trip the list's filter, sort and text, on the list and on a contact", () => {
    for (const hash of [
      "#/contacts",
      "#/contacts?filter=vip",
      "#/contacts?filter=blocked&sort=last&q=ada",
      "#/contacts/01M5GYA96GBQWF3MN8Q7PR9T1M?filter=key",
    ]) {
      expect(formatRoute(parseRoute(hash))).toBe(hash);
    }
    expect(parseRoute("#/contacts?filter=saved")).toEqual({ page: "contacts" });
    expect(parseRoute("#/contacts?filter=nonsense")).toEqual({ page: "contacts" });
  });

  it("Escape on a contact returns to the list it came from; on the list it does nothing", () => {
    const r = parseRoute("#/contacts/01M5GYA96GBQWF3MN8Q7PR9T1M?filter=blocked&q=the");
    expect(escapeTarget(r)).toEqual({ page: "contacts", filter: "blocked", q: "the" });
    expect(escapeTarget(parseRoute("#/contacts"))).toBeNull();
  });

  it("is not a message list, so the mail list underneath is kept", () => {
    expect(isListRoute(parseRoute("#/contacts"))).toBe(false);
  });
});
