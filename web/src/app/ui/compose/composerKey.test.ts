// SPDX-License-Identifier: AGPL-3.0-only

// Which composer a compose URL belongs to (#612) — ./composerKey.ts.
//
// The harm on each side: a key that CHANGES on the composer's own save remounts
// it every fifteen seconds and throws away focus, caret and undo; a key that
// does NOT change when the user opens some other draft keeps the old composer
// on screen under the new URL. Both halves are asserted, because the obvious
// implementations each pass one of them.

import { describe, expect, it } from "vitest";

import { composeParam } from "../../../core/routes";
import { composerKey } from "./composerKey";

describe("composerKey (#612)", () => {
  it("is the URL's own spelling for every composer that saved nothing", () => {
    const none = new Map<string, string>();
    expect(composerKey("new", none)).toBe("new");
    expect(composerKey({ reply: "p1" }, none)).toBe(composeParam({ reply: "p1" }));
    expect(composerKey({ forward: "p1" }, none)).toBe(composeParam({ forward: "p1" }));
    expect(composerKey({ draft: "d1" }, none)).toBe(composeParam({ draft: "d1" }));
  });

  it("keeps a blank composer's key once its URL moves to the draft it saved", () => {
    const adopted = new Map([["d1", "new"]]);
    expect(composerKey({ draft: "d1" }, adopted)).toBe(composerKey("new", adopted));
  });

  it("keeps a reply's key across every save, not just the first", () => {
    const key = composeParam({ reply: "p1" });
    const adopted = new Map([
      ["d1", key],
      ["d2", key],
      ["d3", key],
    ]);
    for (const d of ["d1", "d2", "d3"]) expect(composerKey({ draft: d }, adopted)).toBe(key);
  });

  it("keeps a resumed draft's key when it saves its next revision", () => {
    const key = composeParam({ draft: "d1" });
    expect(composerKey({ draft: "d2" }, new Map([["d2", key]]))).toBe(key);
  });

  it("NEGATIVE CONTROL: a draft this composer did not save opens a composer of its own", () => {
    // Clicked in the list, pasted, from another device: not adopted, so a
    // different key and a fresh composer, even while another is open.
    const adopted = new Map([["d1", "new"]]);
    expect(composerKey({ draft: "d9" }, adopted)).toBe(composeParam({ draft: "d9" }));
    expect(composerKey({ draft: "d9" }, adopted)).not.toBe("new");
  });

  it("NEGATIVE CONTROL: adoption applies to drafts only", () => {
    // An id adopted as a draft says nothing about replying to or forwarding a
    // message with that id — those are different composers by definition.
    const adopted = new Map([["d1", "new"]]);
    expect(composerKey({ reply: "d1" }, adopted)).toBe(composeParam({ reply: "d1" }));
    expect(composerKey({ forward: "d1" }, adopted)).toBe(composeParam({ forward: "d1" }));
  });
});
