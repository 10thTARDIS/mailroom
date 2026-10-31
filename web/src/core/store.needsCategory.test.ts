// SPDX-License-Identifier: AGPL-3.0-only

// Tests for the store's half of the needs-a-category surface (#176): the queue
// ordering, and the `answered` flag that takes a row out of the list on the
// keystroke instead of on the next poll.
//
// The retirement rule is the subtle one and it got written wrongly first: every
// other optimistic intent in this overlay can be checked against a property on
// the Email, and this one cannot — queue membership depends on a dismissal
// tombstone the server deliberately does not expose. Retiring it against
// `suggestedLabels` being empty looked right and was exactly backwards, because
// for most of the queue that condition is TRUE BEFORE the user acts.

import { beforeEach, describe, expect, it } from "vitest";

import { MailStore, effectiveEmail, topConfidence, visibleIds } from "./store";
import type { Email } from "./jmap/types";

function email(id: string, opts: { confidence?: number; receivedAt?: string } = {}): Email {
  return {
    id,
    blobId: "b",
    threadId: "t",
    mailboxIds: {},
    keywords: {},
    size: 1,
    receivedAt: opts.receivedAt ?? "2026-07-01T00:00:00Z",
    sentAt: null,
    subject: "s",
    preview: "p",
    hasAttachment: false,
    from: null,
    messageId: [],
    inReplyTo: [],
    references: [],
    deliveredTo: null,
    identityId: null,
    suggestedLabels:
      opts.confidence === undefined
        ? null
        : [{ labelId: "L", confidence: opts.confidence, reasons: null }],
  } as unknown as Email;
}

let store: MailStore;
beforeEach(() => {
  store = new MailStore(null);
});

function loadQueue(ids: string[], queriedAt = Date.now()): void {
  store.setList(
    {
      key: "needsCategory",
      ids,
      total: ids.length,
      limit: 500,
      loading: false,
      error: null,
      searchUnavailable: false,
    },
    queriedAt,
  );
}

describe("topConfidence", () => {
  it("is the message's BEST open question, mirroring the server's max()", () => {
    store.putEmails([
      {
        ...email("m"),
        suggestedLabels: [
          { labelId: "A", confidence: 0.2, reasons: null },
          { labelId: "B", confidence: 0.8, reasons: null },
        ],
      } as unknown as Email,
    ]);
    expect(topConfidence(store.getState(), "m")).toBeCloseTo(0.8);
  });

  it("is -1 with no questions, so unsuggested mail sorts last like DESC NULLS LAST", () => {
    store.putEmails([email("m")]);
    expect(topConfidence(store.getState(), "m")).toBe(-1);
  });
});

describe("visibleIds, confidence order", () => {
  it("orders most-confident first, then by arrival", () => {
    store.putEmails([
      email("weak", { confidence: 0.3, receivedAt: "2026-07-09T00:00:00Z" }),
      email("strong", { confidence: 0.9, receivedAt: "2026-07-01T00:00:00Z" }),
      email("none", { receivedAt: "2026-07-05T00:00:00Z" }),
    ]);
    loadQueue(["weak", "strong", "none"]);
    // Deliberately adversarial: arrival order is the exact reverse for the top
    // two, so an accidental receivedAt sort cannot pass this.
    expect(visibleIds(store.getState(), null, null, "confidence")).toEqual([
      "strong",
      "weak",
      "none",
    ]);
  });

  it("drops an answered message immediately", () => {
    store.putEmails([email("a", { confidence: 0.5 }), email("b", { confidence: 0.4 })]);
    loadQueue(["a", "b"]);
    store.applyAnswered(["a"]);
    expect(visibleIds(store.getState(), null, null, "confidence")).toEqual(["b"]);
  });

  it("does not drop answered messages from OTHER views", () => {
    store.putEmails([email("a", { confidence: 0.5 })]);
    loadQueue(["a"]);
    store.applyAnswered(["a"]);
    expect(visibleIds(store.getState(), null, null, "arrival")).toEqual(["a"]);
  });
});

describe("effectiveEmail suggestions", () => {
  it("reports no open questions once answered, whatever the server last said", () => {
    store.putEmails([email("a", { confidence: 0.5 })]);
    expect(effectiveEmail(store.getState(), "a")?.suggestions).toHaveLength(1);
    store.applyAnswered(["a"]);
    const e = effectiveEmail(store.getState(), "a");
    expect(e?.suggestions).toHaveLength(0);
    expect(e?.topConfidence).toBeNull();
  });

  it("orders suggestions most-confident first, so the leading one matches the server's", () => {
    store.putEmails([
      {
        ...email("m"),
        suggestedLabels: [
          { labelId: "A", confidence: 0.2, reasons: null },
          { labelId: "B", confidence: 0.8, reasons: null },
        ],
      } as unknown as Email,
    ]);
    expect(effectiveEmail(store.getState(), "m")?.suggestions[0]?.labelId).toBe("B");
  });
});

describe("clearAnswered (#182)", () => {
  it("un-hides a row that is still in the list", () => {
    store.putEmails([email("a", { confidence: 0.5 }), email("b", { confidence: 0.4 })]);
    loadQueue(["a", "b"]);
    store.applyAnswered(["a"]);
    expect(visibleIds(store.getState(), null, null, "confidence")).toEqual(["b"]);
    store.clearAnswered(["a"]);
    expect(visibleIds(store.getState(), null, null, "confidence")).toEqual(["a", "b"]);
  });

  it("restores the message's open questions", () => {
    store.putEmails([email("a", { confidence: 0.5 })]);
    store.applyAnswered(["a"]);
    expect(effectiveEmail(store.getState(), "a")?.suggestions).toHaveLength(0);
    store.clearAnswered(["a"]);
    expect(effectiveEmail(store.getState(), "a")?.suggestions).toHaveLength(1);
  });

  it("drops an overlay entry that no longer says anything", () => {
    store.putEmails([email("a")]);
    store.applyAnswered(["a"]);
    expect(store.getState().overlay["a"]).toBeDefined();
    store.clearAnswered(["a"]);
    expect(store.getState().overlay["a"]).toBeUndefined();
  });

  it("keeps an overlay entry that still carries other intent", () => {
    store.putEmails([email("a")]);
    store.applyKeyword(["a"], "$flagged", true);
    store.applyAnswered(["a"]);
    store.clearAnswered(["a"]);
    const ov = store.getState().overlay["a"];
    expect(ov?.answered).toBeUndefined();
    expect(ov?.keywords["$flagged"]).toBe(true);
  });

  it("is a no-op on a message that was never answered", () => {
    store.putEmails([email("a")]);
    store.applyKeyword(["a"], "$seen", true);
    const before = store.getState().overlay["a"];
    store.clearAnswered(["a"]);
    expect(store.getState().overlay["a"]).toBe(before);
  });

  it("cannot on its own return a row the server has already dropped", () => {
    // The reason MailActions.undoBatchLabel has to remove the label and refresh
    // rather than just flip the flag: once the queue is re-queried, the answered
    // message is not in list.ids at all, and visibleIds only ever orders and
    // filters what the query returned.
    store.putEmails([email("a", { confidence: 0.5 }), email("b", { confidence: 0.4 })]);
    loadQueue(["a", "b"]);
    store.applyAnswered(["a"]);
    loadQueue(["b"], 1_000); // a fresh query, without "a"
    store.clearAnswered(["a"]);
    expect(visibleIds(store.getState(), null, null, "confidence")).toEqual(["b"]);
  });
});

describe("answered retirement", () => {
  it("survives a query that was ISSUED BEFORE the answer", () => {
    // A poll overlapping a batch dismissal must not flash two dozen rows back.
    store.putEmails([email("a", { confidence: 0.5 })]);
    loadQueue(["a"], 1_000);
    store.applyAnswered(["a"]); // updatedAt = now, far beyond 1_000
    loadQueue(["a"], 2_000);
    expect(visibleIds(store.getState(), null, null, "confidence")).toEqual([]);
  });

  it("is retired by a query issued AFTER the answer that still returns the message", () => {
    // The write did not take — a failed dismiss, a rolled-back label — and
    // hiding work that is not done is worse than a flicker.
    store.putEmails([email("a", { confidence: 0.5 })]);
    store.applyAnswered(["a"]);
    loadQueue(["a"], Date.now() + 10_000);
    expect(visibleIds(store.getState(), null, null, "confidence")).toEqual(["a"]);
  });

  it("is NOT retired by another view's query, which has no opinion about the queue", () => {
    store.putEmails([email("a", { confidence: 0.5 })]);
    loadQueue(["a"]);
    store.applyAnswered(["a"]);
    store.setList(
      {
        key: "role:inbox",
        ids: ["a"],
        total: 1,
        limit: 50,
        loading: false,
        error: null,
        searchUnavailable: false,
      },
      Date.now() + 10_000,
    );
    loadQueue(["a"], 1_000); // an older queue query: must not resurrect it either
    expect(visibleIds(store.getState(), null, null, "confidence")).toEqual([]);
  });

  it("is NOT retired by putEmails, whose only evidence is a property that was already empty", () => {
    // Most of the queue is unlabelled mail with no suggestions, so
    // `suggestedLabels.length === 0` is true before the user acts.
    store.putEmails([email("a")]);
    loadQueue(["a"]);
    store.applyAnswered(["a"]);
    store.putEmails([email("a")]);
    expect(visibleIds(store.getState(), null, null, "confidence")).toEqual([]);
  });
});
