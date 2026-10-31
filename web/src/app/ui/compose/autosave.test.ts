// SPDX-License-Identifier: AGPL-3.0-only

// Autosave's two decisions (#448).
//
// The failures these guard are both invisible: a draft saved on every keystroke
// (which, because a JMAP draft is edited by create-plus-destroy, is a new server
// object per keystroke), and a draft never saved at all. Neither errors.

import { describe, expect, it } from "vitest";

import { autosaveDecision, draftDirty, draftEmpty } from "./autosave";
import type { AutosaveState, DraftSnapshot } from "./autosave";

function snap(over: Partial<DraftSnapshot> = {}): DraftSnapshot {
  return {
    from: "house@example.com",
    to: ["friend@example.com"],
    cc: [],
    bcc: [],
    subject: "Hello",
    text: "Hi there",
    html: "",
    attachmentIds: [],
    ...over,
  };
}

function state(over: Partial<AutosaveState> = {}): AutosaveState {
  return {
    dirty: true,
    empty: false,
    // True by default so every case below keeps asserting what it always did;
    // the #614 cases set it false on purpose.
    touched: true,
    saving: false,
    lastAttemptAt: null,
    now: 1_000_000,
    intervalMs: 15_000,
    onBlur: false,
    ...over,
  };
}

describe("draftDirty", () => {
  it("is true with nothing saved yet", () => {
    expect(draftDirty(null, snap())).toBe(true);
  });

  it("is false for an identical snapshot rebuilt from scratch", () => {
    // The property that matters: the composer rebuilds this object on every
    // render, so equal CONTENT must read as clean or autosave never stops.
    expect(draftDirty(snap(), snap())).toBe(false);
  });

  it("notices every field it is responsible for", () => {
    const base = snap();
    const changes: Partial<DraftSnapshot>[] = [
      { from: "personal@example.com" },
      { to: ["other@example.com"] },
      { to: ["friend@example.com", "other@example.com"] },
      { cc: ["c@example.com"] },
      { bcc: ["b@example.com"] },
      { subject: "Hello there" },
      { text: "Hi there!" },
      { html: "<p>Hi there</p>" },
      { attachmentIds: ["G1"] },
    ];
    for (const change of changes) {
      expect(draftDirty(base, snap(change))).toBe(true);
    }
  });

  it("notices a reordered recipient list", () => {
    // Order is what goes in the To header, so it is a change to the message.
    expect(draftDirty(snap({ to: ["a@x.example", "b@x.example"] }), snap({ to: ["b@x.example", "a@x.example"] }))).toBe(
      true,
    );
  });
});

describe("draftEmpty", () => {
  // The scenario is kept and the expectation INVERTED (#601), not deleted.
  //
  // This asserted `true` — no recipients meant empty however much had been
  // typed — because internal/api/draft.go refused such a draft. The reasoning
  // was sound and the cost was not: a FORWARD opens with no recipients (a reply
  // seeds them from the parent, a forward cannot), so a covering note written
  // before choosing who needs it was discarded on close, by every path,
  // including the flush that exists to stop exactly that. The server now
  // accepts it and the rule lives at submission, where it is true.
  it("is FALSE with no recipients once something has been typed (#601)", () => {
    expect(draftEmpty(snap({ to: [], cc: [], bcc: [], subject: "Ideas", text: "lots" }))).toBe(
      false,
    );
  });

  // ⚠ AND THE HALF THAT MUST NOT MOVE. An untouched composer still saves
  // nothing — otherwise opening and closing a compose window litters Drafts,
  // and a draft costs a round trip, a server-side version and an IMAP APPEND.
  it("is still true for a composer with no recipients AND nothing typed", () => {
    expect(draftEmpty(snap({ to: [], cc: [], bcc: [], subject: " ", text: "", html: "" }))).toBe(
      true,
    );
  });

  it("is true for a recipient and nothing else", () => {
    expect(draftEmpty(snap({ subject: "  ", text: "", html: "" }))).toBe(true);
  });

  it("is false once there is a subject, a body or an attachment", () => {
    expect(draftEmpty(snap({ subject: "Hi", text: "", html: "" }))).toBe(false);
    expect(draftEmpty(snap({ subject: "", text: "x", html: "" }))).toBe(false);
    expect(draftEmpty(snap({ subject: "", text: "", html: "<p>x</p>" }))).toBe(false);
    expect(draftEmpty(snap({ subject: "", text: "", html: "", attachmentIds: ["G1"] }))).toBe(false);
  });

  it("counts a Cc-only or Bcc-only recipient as a recipient", () => {
    expect(draftEmpty(snap({ to: [], cc: ["c@example.com"], subject: "Hi" }))).toBe(false);
    expect(draftEmpty(snap({ to: [], bcc: ["b@example.com"], subject: "Hi" }))).toBe(false);
  });
});

describe("autosaveDecision", () => {
  it("saves on the first dirty tick", () => {
    expect(autosaveDecision(state())).toEqual({ save: true, reason: "interval" });
  });

  it("refuses an empty composer even on blur", () => {
    expect(autosaveDecision(state({ empty: true, onBlur: true }))).toEqual({
      save: false,
      reason: "empty",
    });
  });

  it("refuses an UNTOUCHED reply or forward — non-empty and dirty — even on blur (#614)", () => {
    // What a reply or forward looks like the moment it opens: a subject and a
    // quote (not empty), nothing saved yet (dirty), and nothing done by the user.
    // It used to be saved on the first tick, leaving a draft behind for every
    // reply merely opened.
    const opened = state({ empty: false, dirty: true, touched: false });
    expect(autosaveDecision(opened)).toEqual({ save: false, reason: "untouched" });
    expect(autosaveDecision({ ...opened, onBlur: true })).toEqual({
      save: false,
      reason: "untouched",
    });
  });

  it("NEGATIVE CONTROL: saves the same composer once the user has touched it (#614)", () => {
    // Identical except for the one input, so the guard above is the ONLY thing
    // that refused it — not emptiness, cleanliness or the interval.
    expect(autosaveDecision(state({ empty: false, dirty: true, touched: true }))).toEqual({
      save: true,
      reason: "interval",
    });
  });

  it("still saves #601's case: a forward's covering note with no recipients", () => {
    // The note is typed, so the composer is touched and non-empty. The #614 guard
    // must not bring back the bug #601 fixed.
    const note = snap({ to: [], cc: [], bcc: [], subject: "Fwd: x", text: "for you" });
    expect(draftEmpty(note)).toBe(false);
    expect(
      autosaveDecision(state({ empty: draftEmpty(note), dirty: true, touched: true, onBlur: true })),
    ).toEqual({ save: true, reason: "blur" });
  });

  it("refuses a clean composer even on blur", () => {
    expect(autosaveDecision(state({ dirty: false, onBlur: true }))).toEqual({
      save: false,
      reason: "clean",
    });
  });

  it("refuses while a save is in flight", () => {
    // Two overlapping saves each destroy the draft id they were handed, and the
    // second's destroy names one the first has already retired.
    expect(autosaveDecision(state({ saving: true, onBlur: true }))).toEqual({
      save: false,
      reason: "in-flight",
    });
  });

  it("waits out the interval on a tick, and does not on a blur", () => {
    const inside = { lastAttemptAt: 995_000, now: 1_000_000, intervalMs: 15_000 };
    expect(autosaveDecision(state(inside))).toEqual({ save: false, reason: "too-soon" });
    expect(autosaveDecision(state({ ...inside, onBlur: true }))).toEqual({
      save: true,
      reason: "blur",
    });
  });

  it("saves once the interval has elapsed", () => {
    expect(autosaveDecision(state({ lastAttemptAt: 985_000, now: 1_000_000 }))).toEqual({
      save: true,
      reason: "interval",
    });
  });

  it("counts the interval from the ATTEMPT, so a failing server is retried and not hammered", () => {
    // lastAttemptAt is set whether or not the save succeeded; a decision that
    // used the last SUCCESS would fire on every tick while the server said no.
    const justFailed = state({ lastAttemptAt: 999_000, now: 1_000_000 });
    expect(autosaveDecision(justFailed).save).toBe(false);
  });
});
