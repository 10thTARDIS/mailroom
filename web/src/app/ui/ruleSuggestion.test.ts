// SPDX-License-Identifier: AGPL-3.0-only

// "Always put mail from {address} in {area}?" after a manual move.
//
// Real MailStore, real moveToArea, real refileChanges; the server (the
// suggestion, the rule write, the id enumeration) and the relabel write are the
// stand-ins. Each negative case is paired with a positive one in the same
// harness, so "no toast" cannot be the harness never producing one.

import { describe, expect, it, vi } from "vitest";

import type { LabelChange } from "../../core/actions";
import type { AreaRuleSuggestion, Area, Email, Id, Label } from "../../core/jmap/types";
import { MailStore } from "../../core/store";
import type { Toast } from "../state";
import { movableSenderAddress, offerRuleSuggestion } from "./ruleSuggestion";

function label(id: string): Label {
  return { id, name: id, color: null, parentId: null, mirrorToImap: false, keyword: null, tab: null };
}
function area(id: string, name: string, position: number, labelId: string): Area {
  return { id, name, position, labelId, tabs: [], labelIds: [], autoArchive: true, autoArchiveDays: null, hiddenFromRail: false };
}
function email(id: string, from: string | null, labelIds: Id[] = []): Email {
  return {
    id,
    blobId: "b" + id,
    threadId: "t" + id,
    mailboxIds: {},
    keywords: {},
    labelIds: Object.fromEntries(labelIds.map((l) => [l, true])),
    size: 1,
    receivedAt: "2026-10-01T00:00:00Z",
    subject: id,
    preview: "",
    hasAttachment: false,
    from: from === null ? [] : [{ name: "Someone", email: from }],
  } as Email;
}

const SUGGEST = (over: Partial<AreaRuleSuggestion> = {}): AreaRuleSuggestion => ({
  suggest: true,
  reason: "mixed",
  address: "news@example.com",
  areaId: "A-NEWS",
  filings: 3,
  earlier: 0,
  ...over,
});

function harness(emails: Email[], suggestion: () => Promise<AreaRuleSuggestion | null> = async () => SUGGEST()) {
  const store = new MailStore(null);
  store.putLabels([label("L-WORK"), label("L-NEWS")]);
  store.putAreas([area("A-WORK", "Work", 1, "L-WORK"), area("A-NEWS", "News", 2, "L-NEWS")]);
  store.putEmails(emails);
  const toasts: Array<Omit<Toast, "id">> = [];
  const ruleSuggestion = vi.fn(async (_areaId: Id, _address: string) => suggestion());
  const dismissRuleSuggestion = vi.fn(async (_areaId: Id, _address: string) => undefined);
  const createRule = vi.fn(async (input: { name: string }) => ({ id: "R1", ...input }) as never);
  const relabel = vi.fn(async (changes: LabelChange[]) => {
    for (const c of changes) {
      for (const l of c.remove) store.applyLabelId([c.id], l.id, false);
      for (const l of c.add) store.applyLabelId([c.id], l.id, true);
    }
  });
  const allMatchingIds = vi.fn(async (_view: unknown) => ({ ids: [] as Id[], total: 0 }));
  const loadEmails = vi.fn(async (ids: Id[]) => ({ notFound: [] as Id[], failed: false, ids }));
  const undos: string[] = [];
  const deps = {
    store,
    sync: { loadEmails, allMatchingIds } as never,
    actions: { relabel, ruleSuggestion, dismissRuleSuggestion, createRule },
    offerUndo: (l: string) => void undos.push(l),
    pushToast: (t: Omit<Toast, "id">) => (toasts.push(t), 1),
  };
  return { store, deps, toasts, ruleSuggestion, dismissRuleSuggestion, createRule, relabel, allMatchingIds, undos };
}

const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

describe("the offer after a manual move", () => {
  it("is made when every moved message is from one sender and the server says suggest", async () => {
    const h = harness([email("m1", "news@example.com"), email("m2", "News@Example.com")]);
    offerRuleSuggestion(h.deps, ["m1", "m2"], "A-NEWS", 2);
    await settle();
    expect(h.ruleSuggestion).toHaveBeenCalledWith("A-NEWS", "news@example.com");
    expect(h.toasts).toHaveLength(1);
    const t = h.toasts[0]!;
    expect(t.kind).toBe("info");
    expect(t.message).toBe("Always put mail from news@example.com in News?");
    expect(t.action?.label).toBe("Make a rule");
    expect(t.secondary?.label).toBe("Not now");
    // It is a second toast: the move's own Undo was never touched.
    expect(h.undos).toEqual([]);
  });

  it("is not asked for a mixed-sender selection", async () => {
    const h = harness([email("m1", "news@example.com"), email("m2", "other@example.com")]);
    offerRuleSuggestion(h.deps, ["m1", "m2"], "A-NEWS", 2);
    await settle();
    expect(h.ruleSuggestion).not.toHaveBeenCalled();
    expect(h.toasts).toEqual([]);
  });

  it("is not asked for a From that is not a well-formed address, or an unreadable message", async () => {
    const h = harness([email("m1", "example.com"), email("m2", null), email("m3", "a b@example.com")]);
    for (const ids of [["m1"], ["m2"], ["m3"], ["gone"]]) offerRuleSuggestion(h.deps, ids, "A-NEWS", 1);
    await settle();
    expect(h.ruleSuggestion).not.toHaveBeenCalled();
    expect(h.toasts).toEqual([]);
    expect(movableSenderAddress("a@example.com")).toBe(true);
    expect(movableSenderAddress("@example.com")).toBe(false);
    expect(movableSenderAddress("*@example.com")).toBe(false);
  });

  it("is not asked when nothing actually moved", async () => {
    const h = harness([email("m1", "news@example.com")]);
    offerRuleSuggestion(h.deps, ["m1"], "A-NEWS", 0);
    await settle();
    expect(h.ruleSuggestion).not.toHaveBeenCalled();
  });

  it("shows nothing when the server says no, or the call fails, and never an error", async () => {
    for (const answer of [
      async () => SUGGEST({ suggest: false, reason: "learned" }),
      async () => null,
      async () => {
        throw new Error("boom");
      },
    ]) {
      const h = harness([email("m1", "news@example.com")], answer);
      offerRuleSuggestion(h.deps, ["m1"], "A-NEWS", 1);
      await settle();
      expect(h.ruleSuggestion).toHaveBeenCalledTimes(1);
      expect(h.toasts).toEqual([]);
    }
  });
});

describe("answering the offer", () => {
  async function offered(over: Partial<AreaRuleSuggestion> = {}, emails: Email[] = [email("m1", "news@example.com")]) {
    const h = harness(emails, async () => SUGGEST(over));
    offerRuleSuggestion(h.deps, ["m1"], "A-NEWS", 1);
    await settle();
    expect(h.toasts).toHaveLength(1);
    return h;
  }

  it("Make a rule creates the rule-editor shape, then offers Move them only when earlier mail exists", async () => {
    const h = await offered({ earlier: 4 });
    h.toasts[0]!.action!.run();
    await settle();
    expect(h.createRule).toHaveBeenCalledWith({
      name: "Mail from news@example.com → News",
      priority: 100,
      conditions: { all: [{ field: "from", op: "equals", value: "news@example.com" }] },
      actions: [{ type: "addLabel", label: "L-NEWS" }],
      enabled: true,
    });
    expect(h.toasts).toHaveLength(2);
    expect(h.toasts[1]!.message).toBe("Rule made. Also move the 4 earlier messages from news@example.com to News?");
    expect(h.toasts[1]!.action?.label).toBe("Move them");
    // Nothing has moved: the second tap has not happened.
    expect(h.allMatchingIds).not.toHaveBeenCalled();
    expect(h.relabel).not.toHaveBeenCalled();

    // Control for the next case: the same flow with no earlier mail says so plainly.
    const none = await offered({ earlier: 0 });
    none.toasts[0]!.action!.run();
    await settle();
    expect(none.toasts[1]!.message).toBe("Rule made.");
    expect(none.toasts[1]!.action).toBeUndefined();
  });

  it("says nothing more when the rule could not be made", async () => {
    const h = await offered({ earlier: 4 });
    h.createRule.mockResolvedValueOnce(null as never);
    h.toasts[0]!.action!.run();
    await settle();
    expect(h.toasts).toHaveLength(1);
  });

  it("Move them moves the earlier messages not already in the area, through the one move path", async () => {
    const h = await offered(
      { earlier: 2 },
      [
        email("m1", "news@example.com", ["L-NEWS"]),
        email("e1", "news@example.com", ["L-WORK"]),
        email("e2", "news@example.com"),
      ],
    );
    h.allMatchingIds.mockResolvedValueOnce({ ids: ["m1", "e1", "e2"], total: 3 });
    h.toasts[0]!.action!.run();
    await settle();
    h.toasts[1]!.action!.run();
    await settle();
    expect(h.allMatchingIds).toHaveBeenCalledWith({ kind: "search", query: "from:news@example.com" });
    expect(h.relabel).toHaveBeenCalledTimes(1);
    const moved = (h.relabel.mock.calls[0]![0] as LabelChange[]).map((c) => c.id).sort();
    expect(moved).toEqual(["e1", "e2"]);
    // moveToArea offered its own Undo for this move.
    expect(h.undos).toHaveLength(1);
  });

  it("Not now records the dismissal and shows nothing further", async () => {
    const h = await offered();
    h.toasts[0]!.secondary!.run();
    await settle();
    expect(h.dismissRuleSuggestion).toHaveBeenCalledWith("A-NEWS", "news@example.com");
    expect(h.createRule).not.toHaveBeenCalled();
    expect(h.toasts).toHaveLength(1);
  });
});
