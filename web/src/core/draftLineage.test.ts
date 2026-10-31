// SPDX-License-Identifier: AGPL-3.0-only

// Which draft became which (#610 M2, #612) — core/draftLineage.ts.
//
// The test that earns its place is the cursor one, because it is the harm the
// issue measured: with the draft briefly absent the old rule put the cursor on
// the row BELOW it, and with nothing checked the list's keyboard actions target
// the cursor — so an archive pressed after an autosave went to the neighbour.
// Every "follows" case sits next to the control that must NOT follow, because a
// rule that always jumps to the newest draft would pass the first half alone.

import { describe, expect, it } from "vitest";

import { followSelection, latestRevision, recordSuccessor, repairCursor } from "./draftLineage";

describe("recordSuccessor / latestRevision", () => {
  it("maps a replaced revision to the one that replaced it", () => {
    const m = recordSuccessor({}, "d1", "d2");
    expect(latestRevision(m, "d1")).toBe("d2");
  });

  it("collapses a chain so every past revision names the NEWEST one", () => {
    // A composer open for a minute saves four times; the cursor may be holding
    // any of the ids it has seen, and must land on the one that exists.
    let m = recordSuccessor({}, "d1", "d2");
    m = recordSuccessor(m, "d2", "d3");
    m = recordSuccessor(m, "d3", "d4");
    expect(latestRevision(m, "d1")).toBe("d4");
    expect(latestRevision(m, "d2")).toBe("d4");
    expect(latestRevision(m, "d3")).toBe("d4");
  });

  it("never records the newest revision as replaced", () => {
    let m = recordSuccessor({}, "d1", "d2");
    m = recordSuccessor(m, "d2", "d3");
    expect(latestRevision(m, "d3")).toBe("d3");
    expect(Object.keys(m).sort()).toEqual(["d1", "d2"]);
  });

  it("leaves an id it knows nothing about alone", () => {
    expect(latestRevision(recordSuccessor({}, "d1", "d2"), "m9")).toBe("m9");
  });

  it("keeps two drafts' lineages apart", () => {
    // Two composers in one session (one closed, one opened) must not be welded.
    let m = recordSuccessor({}, "a1", "a2");
    m = recordSuccessor(m, "b1", "b2");
    expect(latestRevision(m, "a1")).toBe("a2");
    expect(latestRevision(m, "b1")).toBe("b2");
  });

  it("does not mutate the map it was given", () => {
    const before = { d1: "d2" };
    recordSuccessor(before, "d2", "d3");
    expect(before).toEqual({ d1: "d2" });
  });
});

describe("repairCursor (#610 M2)", () => {
  const successors = recordSuccessor({}, "draft-old", "draft-new");

  it("keeps a cursor that is still in the list", () => {
    expect(repairCursor(["a", "draft-old", "b"], "draft-old", successors)).toBe("draft-old");
  });

  it("follows a replaced draft to its new revision when something else is on top", () => {
    // Mail arrived while the user typed, so the first row is NOT the draft. The
    // old "first row" rule put the cursor on "just-arrived" — a message the user
    // never chose, and the target of the next keyboard archive.
    const ids = ["just-arrived", "draft-new", "a"];
    expect(repairCursor(ids, "draft-old", successors)).toBe("draft-new");
  });

  it("follows the replacement wherever it sorts, not by position", () => {
    const ids = ["a", "b", "draft-new"];
    expect(repairCursor(ids, "draft-old", successors)).toBe("draft-new");
  });

  it("lands on the new revision when it sorts first — which the OLD rule also did", () => {
    // Recorded because a probe found it: in a date-sorted Drafts list the new
    // revision is first (its receivedAt is the save time), so `ids[0]` is right
    // by coincidence and this case CANNOT tell the two rules apart. With M1
    // closed there is no moment the draft is absent, so this is the common case
    // and it was never the one that moved the cursor. The two cases above are
    // the ones that fail without the succession.
    expect(repairCursor(["draft-new", "a", "b"], "draft-old", successors)).toBe("draft-new");
  });

  it("NEGATIVE CONTROL: a message that genuinely left still falls back to the first row", () => {
    // Archived here, moved elsewhere, destroyed by another client: no lineage,
    // so the rule the list always had applies unchanged.
    expect(repairCursor(["a", "b"], "gone", successors)).toBe("a");
  });

  it("NEGATIVE CONTROL: a cursor the user moved elsewhere is never remapped", () => {
    // The user arrowed down to "b" mid-save. It still exists, so it stays —
    // the lineage of the draft they left is not a reason to drag them back.
    expect(repairCursor(["draft-new", "a", "b"], "b", successors)).toBe("b");
  });

  it("falls back to the first row when the replacement has not arrived in the list", () => {
    // The successor is known but not in THIS list (a view that does not show
    // drafts, or a window it sorted out of): there is nothing to follow it to.
    expect(repairCursor(["a", "b"], "draft-old", successors)).toBe("a");
  });

  it("is null on an empty list", () => {
    expect(repairCursor([], "draft-old", successors)).toBeNull();
    expect(repairCursor([], null, successors)).toBeNull();
  });
});

describe("followSelection", () => {
  it("swaps a checked, replaced draft for its new revision", () => {
    const s = recordSuccessor({}, "d1", "d2");
    expect([...followSelection(new Set(["a", "d1"]), s)].sort()).toEqual(["a", "d2"]);
  });

  it("returns the SAME set when nothing in it was replaced", () => {
    // Identity is what lets the caller skip a state update on every save.
    const sel = new Set(["a", "b"]);
    expect(followSelection(sel, recordSuccessor({}, "d1", "d2"))).toBe(sel);
  });
});
