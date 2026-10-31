// SPDX-License-Identifier: AGPL-3.0-only

// Re-filing is a label SWAP (#704). The case that earns this file its place is
// "Promotions -> Updates": the server resolves the label-fed area by POSITION,
// so a re-file that only added Updates' label left the message in Promotions.

import { describe, expect, it } from "vitest";

import type { LabelChange } from "./actions";
import type { Area, Email, Id, Label } from "./jmap/types";
import { invertChanges, refileChanges } from "./refile";
import { MailStore, effectiveEmail } from "./store";

function label(id: string, keyword: string | null = null): Label {
  return { id, name: id, color: null, parentId: null, mirrorToImap: keyword !== null, keyword, tab: null };
}

function area(id: string, position: number, labelId: string): Area {
  return {
    id,
    name: id,
    position,
    labelId,
    tabs: [],
    labelIds: [],
    autoArchive: true,
    autoArchiveDays: null,
    hiddenFromRail: false,
  };
}

function email(id: string, labelIds: Id[], keywords: string[] = []): Email {
  return {
    id,
    blobId: "b" + id,
    threadId: "t" + id,
    mailboxIds: {},
    keywords: Object.fromEntries(keywords.map((k) => [k, true])),
    labelIds: Object.fromEntries(labelIds.map((l) => [l, true])),
    size: 1,
    receivedAt: "2026-10-01T00:00:00Z",
    subject: id,
    preview: "",
    hasAttachment: false,
  } as Email;
}

function setup(emails: Email[]): MailStore {
  const store = new MailStore(null);
  store.putLabels([label("L-PRI"), label("L-PROMO"), label("L-UPD"), label("L-TOPIC", "MailroomLabel-topic")]);
  store.putAreas([area("A-PRI", 1, "L-PRI"), area("A-PROMO", 2, "L-PROMO"), area("A-UPD", 3, "L-UPD")]);
  store.putEmails(emails);
  return store;
}

const ids = (ls: Label[]): Id[] => ls.map((l) => l.id).sort();
const shape = (cs: LabelChange[]) => cs.map((c) => ({ id: c.id, add: ids(c.add), remove: ids(c.remove) }));

/** Applies changes to a plain label-set model — what the server would hold. */
function applyTo(sets: Record<Id, Set<Id>>, cs: LabelChange[]): void {
  for (const c of cs) {
    const s = sets[c.id] ?? new Set<Id>();
    for (const l of c.remove) s.delete(l.id);
    for (const l of c.add) s.add(l.id);
    sets[c.id] = s;
  }
}

describe("refileChanges (#704)", () => {
  it("Promotions -> Updates REMOVES Promotions' label, or the earlier area would keep the message", () => {
    const st = setup([email("m1", ["L-PROMO"])]).getState();
    expect(shape(refileChanges(st, ["m1"], "A-UPD"))).toEqual([{ id: "m1", add: ["L-UPD"], remove: ["L-PROMO"] }]);
  });

  it("Updates -> Promotions swaps the same way", () => {
    const st = setup([email("m1", ["L-UPD"])]).getState();
    expect(shape(refileChanges(st, ["m1"], "A-PROMO"))).toEqual([{ id: "m1", add: ["L-PROMO"], remove: ["L-UPD"] }]);
  });

  it("leaves non-area labels alone — a topic label is the user's, not the area's bookkeeping", () => {
    const st = setup([email("m1", ["L-PROMO", "L-TOPIC"], ["MailroomLabel-topic"])]).getState();
    expect(shape(refileChanges(st, ["m1"], "A-UPD"))).toEqual([{ id: "m1", add: ["L-UPD"], remove: ["L-PROMO"] }]);
  });

  it("computes PER MESSAGE across a mixed selection, and skips one already there", () => {
    const st = setup([
      email("m1", ["L-PROMO"]),
      email("m2", []),
      email("m3", ["L-UPD"]),
      email("m4", ["L-UPD", "L-PRI"]),
    ]).getState();
    expect(shape(refileChanges(st, ["m1", "m2", "m3", "m4"], "A-UPD"))).toEqual([
      { id: "m1", add: ["L-UPD"], remove: ["L-PROMO"] },
      { id: "m2", add: ["L-UPD"], remove: [] },
      // m3 is exactly in Updates already: nothing to do, nothing to undo.
      { id: "m4", add: [], remove: ["L-PRI"] },
    ]);
  });

  it("reads PENDING label changes, so a re-file right after another one swaps from where it now is", () => {
    const store = setup([email("m1", ["L-PROMO"])]);
    store.applyLabelId(["m1"], "L-PROMO", false);
    store.applyLabelId(["m1"], "L-PRI", true);
    expect(shape(refileChanges(store.getState(), ["m1"], "A-UPD"))).toEqual([
      { id: "m1", add: ["L-UPD"], remove: ["L-PRI"] },
    ]);
  });

  it("is empty when the target area's label is not loaded", () => {
    const store = setup([email("m1", ["L-PROMO"])]);
    store.putAreas([area("A-GHOST", 4, "L-NOT-LOADED")]);
    expect(refileChanges(store.getState(), ["m1"], "A-GHOST")).toEqual([]);
  });
});

describe("invertChanges (#704)", () => {
  it("restores EXACTLY each message's prior label set", () => {
    const before: Record<Id, Id[]> = {
      m1: ["L-PROMO", "L-TOPIC"],
      m2: [],
      m3: ["L-UPD"],
      m4: ["L-UPD", "L-PRI"],
    };
    const store = setup(Object.entries(before).map(([id, ls]) => email(id, ls)));
    const changes = refileChanges(store.getState(), Object.keys(before), "A-UPD");
    const sets: Record<Id, Set<Id>> = {};
    for (const [id, ls] of Object.entries(before)) sets[id] = new Set(ls);
    applyTo(sets, changes);
    // Every message is now in Updates and nowhere else among the areas.
    for (const id of Object.keys(before)) {
      expect([...(sets[id] ?? [])].filter((l) => l !== "L-TOPIC").sort()).toEqual(["L-UPD"]);
    }
    applyTo(sets, invertChanges(changes));
    for (const [id, ls] of Object.entries(before)) {
      expect([...(sets[id] ?? [])].sort()).toEqual([...ls].sort());
    }
  });

  it("an undo through the overlay shows the prior labels again", () => {
    const store = setup([email("m1", ["L-PROMO"])]);
    const changes = refileChanges(store.getState(), ["m1"], "A-UPD");
    for (const c of [...changes, ...invertChanges(changes)]) {
      for (const l of c.remove) store.applyLabelId([c.id], l.id, false);
      for (const l of c.add) store.applyLabelId([c.id], l.id, true);
    }
    const e = effectiveEmail(store.getState(), "m1");
    expect(Object.keys(e?.labelIds ?? {}).sort()).toEqual(["L-PROMO"]);
  });
});
