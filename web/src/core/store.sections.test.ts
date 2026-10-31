// SPDX-License-Identifier: AGPL-3.0-only

// The store's half of the desk (#375): several named lists at once, each read
// through the same optimistic overlay as the single list.
//
// The desk shows five or six lists side by side, so the two things that must be
// true of them are things a single list never had to prove:
//
//   * one section's rows cannot leak into another's — the account switcher and
//     the areas both key on the section key, and a fresh key inheriting the
//     previous section's ids would attribute mail to the wrong area;
//   * a row the user just acted on leaves the section it was in AT ONCE, and for
//     the right reason. Archiving, unflagging from the tray and snoozing are
//     three different overlay facts, and each has to be observed separately —
//     otherwise a filter that catches only one of them looks like it works.
//
// `visibleIds` has to keep behaving exactly as before: it now shares its
// ordering and its away-check with sectionIds, and a refactor that quietly
// changed the inbox's order would be a much bigger bug than the feature.

import { beforeEach, describe, expect, it } from "vitest";

import { MailStore, sectionIds, visibleIds } from "./store";
import type { Email } from "./jmap/types";

function email(id: string, over: Partial<Email> = {}): Email {
  return {
    id,
    blobId: "b",
    threadId: `t-${id}`,
    mailboxIds: {},
    keywords: {},
    size: 1,
    receivedAt: "2026-07-01T00:00:00Z",
    sentAt: null,
    subject: id,
    preview: "",
    hasAttachment: false,
    from: null,
    messageId: [],
    inReplyTo: [],
    references: [],
    deliveredTo: null,
    identityId: null,
    ...over,
  } as unknown as Email;
}

const SURFACE = "all/surface/A1";
const OTHER = "all/surface/A2";
const TRAY = "all/tray/none";

let store: MailStore;
beforeEach(() => {
  store = new MailStore(null);
});

function loadSection(key: string, ids: string[], total = ids.length): void {
  store.setSection({ key, ids, total, limit: 50, loading: false, error: null, loadedAt: 1 });
}

describe("sections are independent lists", () => {
  it("keeps each key's rows to itself", () => {
    loadSection(SURFACE, ["m1", "m2"]);
    loadSection(OTHER, ["m3"]);
    const st = store.getState();
    expect(st.sections[SURFACE]?.ids).toEqual(["m1", "m2"]);
    expect(st.sections[OTHER]?.ids).toEqual(["m3"]);
  });

  it("merges a patch for a key it already has", () => {
    loadSection(SURFACE, ["m1", "m2"], 40);
    store.setSection({ key: SURFACE, loading: true });
    const s = store.getState().sections[SURFACE];
    // Still showing what it had while the refresh is in flight.
    expect(s?.ids).toEqual(["m1", "m2"]);
    expect(s?.total).toBe(40);
    expect(s?.loading).toBe(true);
  });

  it("starts a fresh key from empty rather than from another section", () => {
    loadSection(SURFACE, ["m1", "m2"], 40);
    store.setSection({ key: OTHER, loading: true });
    const s = store.getState().sections[OTHER];
    expect(s?.ids).toEqual([]);
    expect(s?.total).toBe(0);
    expect(s?.loadedAt).toBeNull();
  });

  it("reports a window that is a bound rather than a total", () => {
    loadSection(SURFACE, ["m1", "m2"], 900);
    const s = store.getState().sections[SURFACE];
    expect(s?.ids.length).toBeLessThan(s?.total ?? 0);
  });

  it("drops only the sections the predicate names", () => {
    loadSection(SURFACE, ["m1"]);
    loadSection("ACC1/surface/A1", ["m2"]);
    store.dropSections((k) => !k.startsWith("ACC1/"));
    expect(Object.keys(store.getState().sections)).toEqual(["ACC1/surface/A1"]);
  });

  it("an unknown section is empty, not an error", () => {
    expect(sectionIds(store.getState(), "nope", { move: null })).toEqual([]);
  });
});

describe("a row leaves its section the moment the user acts", () => {
  beforeEach(() => {
    store.putEmails([email("m1"), email("m2"), email("m3")]);
    loadSection(SURFACE, ["m1", "m2", "m3"]);
  });

  it("NEGATIVE CONTROL: with no overlay every row stays", () => {
    // Without this, each assertion below could be passing against a selector
    // that returns nothing at all.
    expect(sectionIds(store.getState(), SURFACE, { move: "inbox" })).toEqual(["m1", "m2", "m3"]);
  });

  it("hides an optimistic archive", () => {
    store.applyMove(["m2"], "archive");
    expect(sectionIds(store.getState(), SURFACE, { move: "inbox" })).toEqual(["m1", "m3"]);
  });

  it("hides an optimistic trash", () => {
    store.applyMove(["m3"], "trash");
    expect(sectionIds(store.getState(), SURFACE, { move: "inbox" })).toEqual(["m1", "m2"]);
  });

  it("hides an optimistic unflag from the tray", () => {
    store.putEmails([email("f1", { keywords: { $flagged: true } })]);
    loadSection(TRAY, ["f1", "m1"]);
    const tray = { move: null as null, keyword: { atom: "$flagged", present: true }, snoozed: null };
    expect(sectionIds(store.getState(), TRAY, tray)).toEqual(["f1", "m1"]);
    store.applyKeyword(["f1"], "$flagged", false);
    expect(sectionIds(store.getState(), TRAY, tray)).toEqual(["m1"]);
  });

  it("keeps a row whose keyword intent AGREES with the section", () => {
    loadSection(TRAY, ["m1"]);
    store.applyKeyword(["m1"], "$flagged", true);
    expect(
      sectionIds(store.getState(), TRAY, {
        move: null,
        keyword: { atom: "$flagged", present: true },
        snoozed: null,
      }),
    ).toEqual(["m1"]);
  });

  it("hides a row snoozed away from a surface or a drawer", () => {
    const later = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    store.applySnooze(["m1"], later);
    expect(sectionIds(store.getState(), SURFACE, { move: "inbox" })).toEqual(["m2", "m3"]);
  });

  it("does NOT hide a snoozed row from the tray, whose query never excluded it", () => {
    // The tray asks `hasKeyword: $flagged` and nothing else — the same choice
    // the Starred view makes. Hiding rows the query returned would make the
    // strip's own count disagree with the total printed beside it.
    const later = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    loadSection(TRAY, ["m1", "m2"]);
    store.applySnooze(["m1"], later);
    expect(
      sectionIds(store.getState(), TRAY, {
        move: null,
        keyword: { atom: "$flagged", present: true },
        snoozed: null,
      }),
    ).toEqual(["m1", "m2"]);
  });

  it("keeps a message whose snooze has already passed", () => {
    const past = new Date(Date.now() - 60 * 60 * 1000).toISOString();
    store.applySnooze(["m1"], past);
    expect(sectionIds(store.getState(), SURFACE, { move: "inbox" })).toContain("m1");
  });

  it("never adds a message the section's query did not return", () => {
    // A local archive cannot put a message into some other area's section: a
    // client does not know which area would claim it, and inventing membership
    // would file mail somewhere the server would not.
    store.putEmails([email("elsewhere")]);
    store.applyMove(["elsewhere"], "archive");
    expect(sectionIds(store.getState(), SURFACE, { move: "inbox" })).not.toContain("elsewhere");
    expect(sectionIds(store.getState(), SURFACE, { move: null })).not.toContain("elsewhere");
  });
});

describe("a section's order", () => {
  it("puts pinned rows first, then newest effective arrival", () => {
    store.putEmails([
      email("new", { receivedAt: "2026-07-05T00:00:00Z" }),
      email("old", { receivedAt: "2026-07-01T00:00:00Z" }),
      email("pinned", {
        receivedAt: "2026-06-01T00:00:00Z",
        isPinned: true,
        pinnedAt: "2026-07-04T00:00:00Z",
      }),
    ]);
    loadSection(SURFACE, ["old", "new", "pinned"]);
    expect(sectionIds(store.getState(), SURFACE, { move: "inbox" })).toEqual([
      "pinned",
      "new",
      "old",
    ]);
  });

  it("sorts a returned message by when it came back, not by its age", () => {
    store.putEmails([
      email("fresh", { receivedAt: "2026-07-05T00:00:00Z" }),
      email("returned", {
        receivedAt: "2026-01-01T00:00:00Z",
        returnedAt: "2026-07-06T00:00:00Z",
      }),
    ]);
    loadSection(SURFACE, ["fresh", "returned"]);
    expect(sectionIds(store.getState(), SURFACE, { move: "inbox" })).toEqual([
      "returned",
      "fresh",
    ]);
  });

  it("puts a JUST-pinned row first before the server has agreed", () => {
    store.putEmails([
      email("a", { receivedAt: "2026-07-05T00:00:00Z" }),
      email("b", { receivedAt: "2026-07-01T00:00:00Z" }),
    ]);
    loadSection(SURFACE, ["a", "b"]);
    store.applyPin(["b"], true);
    expect(sectionIds(store.getState(), SURFACE, { move: "inbox" })).toEqual(["b", "a"]);
  });
});

describe("visibleIds is unchanged by the refactor", () => {
  // sectionIds took visibleIds' away-check, pin reading and comparator out into
  // shared functions. These re-assert the single list's behaviour at the two
  // points where a shared helper could have changed it.
  it("still orders pinned first, then effective arrival", () => {
    store.putEmails([
      email("new", { receivedAt: "2026-07-05T00:00:00Z" }),
      email("old", { receivedAt: "2026-07-01T00:00:00Z" }),
      email("pin", {
        receivedAt: "2026-06-01T00:00:00Z",
        isPinned: true,
        pinnedAt: "2026-07-04T00:00:00Z",
      }),
      email("back", { receivedAt: "2026-01-01T00:00:00Z", returnedAt: "2026-07-06T00:00:00Z" }),
    ]);
    store.setList({
      key: "role:inbox",
      ids: ["old", "new", "pin", "back"],
      total: 4,
      limit: 50,
      loading: false,
      error: null,
      searchUnavailable: false,
    });
    expect(visibleIds(store.getState(), "inbox")).toEqual(["pin", "back", "new", "old"]);
  });

  it("still drops snoozed mail from a view that filters on it, and only there", () => {
    const later = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    store.putEmails([email("a"), email("b")]);
    store.setList({
      key: "role:inbox",
      ids: ["a", "b"],
      total: 2,
      limit: 50,
      loading: false,
      error: null,
      searchUnavailable: false,
    });
    store.applySnooze(["a"], later);
    expect(visibleIds(store.getState(), "inbox", false)).toEqual(["b"]);
    expect(visibleIds(store.getState(), "inbox", null)).toEqual(["a", "b"]);
    expect(visibleIds(store.getState(), null, true)).toEqual(["a"]);
  });
});
