// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { conversationIds, foldPlan } from "./fold";

const read = (id: string) => ({ id, isSeen: true });
const unread = (id: string) => ({ id, isSeen: false });

describe("foldPlan", () => {
  it("expands only the opened message when everything is read", () => {
    const got = foldPlan([read("a"), read("b"), read("c")], "b");
    expect([...got]).toEqual(["b"]);
  });

  it("expands both when two messages are unread", () => {
    const got = foldPlan([unread("a"), unread("b")], "a");
    expect(got.has("a")).toBe(true);
    expect(got.has("b")).toBe(true);
  });

  it("expands a single message whatever its state and whoever is open", () => {
    expect([...foldPlan([read("only")], "only")]).toEqual(["only"]);
    expect([...foldPlan([read("only")], null)]).toEqual(["only"]);
    // An openId the thread does not contain: the pane's message id can lag the
    // thread by a render, and one folded line reads as a broken pane.
    expect([...foldPlan([read("only")], "elsewhere")]).toEqual(["only"]);
  });

  it("expands the unread ones alongside the opened one", () => {
    const got = foldPlan([read("a"), unread("b"), read("c"), unread("d")], "c");
    expect([...got].sort()).toEqual(["b", "c", "d"]);
  });

  it("folds every read message when the opened one is not in the thread", () => {
    // NEGATIVE CONTROL for the single-message rule above: with more than one
    // message there is no rescue, and the plan is exactly "the unread ones".
    const got = foldPlan([read("a"), read("b")], "elsewhere");
    expect([...got]).toEqual([]);
  });

  it("is empty for an empty thread rather than throwing", () => {
    expect([...foldPlan([], "a")]).toEqual([]);
  });
});

describe("conversationIds (#630)", () => {
  it("is the conversation as given when the opened message is in it", () => {
    expect(conversationIds(["a", "b", "c"], "b")).toEqual(["a", "b", "c"]);
  });

  it("adds the opened message when the conversation left it out (a trashed message opened from Trash)", () => {
    expect(conversationIds(["a", "c"], "b")).toEqual(["a", "c", "b"]);
  });

  it("is just the opened message when there is no conversation yet, or it came back empty", () => {
    expect(conversationIds(undefined, "b")).toEqual(["b"]);
    expect(conversationIds([], "b")).toEqual(["b"]);
  });

  it("does not modify the conversation it was handed", () => {
    const thread = ["a"];
    conversationIds(thread, "b");
    expect(thread).toEqual(["a"]);
  });
});
