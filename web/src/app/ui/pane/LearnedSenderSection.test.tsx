// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The Learned sorting section of the sender card (#167): Forget asks first,
// then uses the right scope and shows the fresh state; trust is offered only
// where it can matter and calls the method.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { Area, LearnedSender, LearnedSenderEvidence } from "../../../core/jmap/types";
import { MailStore } from "../../../core/store";

const store = new MailStore(null);
let current: LearnedSender | null;
const learnedSender = vi.fn(async (_a: string) => current);
const forgetLearnedSender = vi.fn(async (_a: string, _s: "address" | "domain"): Promise<LearnedSender | null> => null);
const trustLearnedSender = vi.fn(async (_a: string, _t: boolean): Promise<LearnedSender | null> => null);
const pushToast = vi.fn((_t: unknown) => 1);
const appValue = { actions: { learnedSender, forgetLearnedSender, trustLearnedSender }, pushToast };
vi.mock("../../state", () => ({ useApp: () => appValue, useMailState: () => store.getState() }));

import { LearnedSenderSection } from "./LearnedSenderSection";

const ev = (over: Partial<LearnedSenderEvidence>): LearnedSenderEvidence => ({
  areaId: null, filings: 0, verdict: "none", acting: false, reason: "none", ...over,
});
const mk = (over: Partial<LearnedSender>): LearnedSender => ({
  sender: "alice@example.com",
  address: ev({}),
  domain: { domain: "example.com", ...ev({}) },
  decidedBy: "none",
  trusted: false,
  forgottenAt: null,
  ...over,
});
const acting = (decidedBy: "address" | "domain"): LearnedSender =>
  mk({
    decidedBy,
    address: ev({ areaId: "AU", filings: 4, verdict: "confident", acting: decidedBy === "address", reason: decidedBy === "address" ? null : "thin" }),
    domain: { domain: "example.com", ...ev({ areaId: "AU", filings: 6, verdict: "confident", acting: decidedBy === "domain", reason: null }) },
  });

const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 0));
async function click(el: Element): Promise<void> {
  flushSync(() => {
    el.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
  });
  await settle();
  await settle();
}
const button = (text: string): HTMLButtonElement | undefined =>
  [...document.querySelectorAll("button")].find((b) => b.textContent?.trim() === text);

let root: Root;
beforeEach(() => {
  store.putAreas([{ id: "AU", name: "Updates", position: 1, labelId: "L", tabs: [], labelIds: [], autoArchive: true, autoArchiveDays: null, hiddenFromRail: false } as Area]);
  for (const f of [learnedSender, forgetLearnedSender, trustLearnedSender, pushToast]) f.mockClear();
  const host = document.createElement("div");
  document.body.replaceChildren(host);
  root = createRoot(host);
});
afterEach(() => flushSync(() => root.unmount()));

async function mount(s: LearnedSender | null): Promise<void> {
  current = s;
  flushSync(() => root.render(<LearnedSenderSection address="alice@example.com" messageId="m1" />));
  await settle();
  await settle();
}

describe("LearnedSenderSection", () => {
  it("draws nothing when the server gives no answer", async () => {
    await mount(null);
    expect(document.body.textContent).toBe("");
  });

  it("says what it learned", async () => {
    await mount(acting("address"));
    expect(document.body.textContent).toContain("Learned sorting");
    expect(document.body.textContent).toContain("Files new mail from this sender to Updates (learned from 4 of your moves).");
  });

  it("Forget asks first and calls nothing until confirmed", async () => {
    await mount(acting("address"));
    await click(button("Forget what was learned")!);
    expect(document.body.textContent).toContain("Forget what Mailroom learned about this sender? New mail from them will be sorted by the desk until you move one again.");
    expect(forgetLearnedSender).not.toHaveBeenCalled();
    // Focus is on the safe choice.
    expect(document.activeElement).toBe(button("Keep it"));
    await click(button("Keep it")!);
    expect(forgetLearnedSender).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(button("Forget what was learned"));
  });

  it("confirmed Forget uses the address scope and shows the fresh state", async () => {
    forgetLearnedSender.mockResolvedValueOnce(mk({}));
    await mount(acting("address"));
    await click(button("Forget what was learned")!);
    await click(button("Forget")!);
    expect(forgetLearnedSender).toHaveBeenCalledWith("alice@example.com", "address");
    expect(document.body.textContent).toContain("Nothing learned about this sender yet.");
    expect(button("Forget what was learned")).toBeUndefined();
  });

  it("uses the domain scope when the domain decided", async () => {
    forgetLearnedSender.mockResolvedValueOnce(mk({}));
    await mount(acting("domain"));
    await click(button("Forget what was learned")!);
    await click(button("Forget")!);
    expect(forgetLearnedSender).toHaveBeenCalledWith("alice@example.com", "domain");
  });

  it("a failed Forget says so and keeps the state", async () => {
    await mount(acting("address"));
    await click(button("Forget what was learned")!);
    await click(button("Forget")!);
    expect(pushToast).toHaveBeenCalledWith(expect.objectContaining({ kind: "error" }));
    expect(document.body.textContent).toContain("learned from 4 of your moves");
  });

  it("Escape in the question cancels the question only", async () => {
    await mount(acting("address"));
    await click(button("Forget what was learned")!);
    const group = document.querySelector(".learned-sender-confirm")!;
    flushSync(() => {
      group.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
    });
    expect(document.querySelector(".learned-sender-confirm")).toBeNull();
    expect(forgetLearnedSender).not.toHaveBeenCalled();
  });

  it("offers trust for an unverified sender, as a labelled switch, and calls it", async () => {
    trustLearnedSender.mockResolvedValueOnce(mk({ trusted: true, address: ev({ areaId: "AU", filings: 3, acting: true, reason: null }) }));
    await mount(mk({ address: ev({ filings: 3, verdict: "confident", reason: "unauthenticated" }) }));
    const sw = document.querySelector<HTMLInputElement>('input[role="switch"]')!;
    expect(sw.closest("label")?.textContent).toBe("Trust this sender for sorting");
    expect(document.body.textContent).toContain("Only for senders you know, such as your own servers. It changes sorting only; warnings still show.");
    expect(sw.checked).toBe(false);
    await click(sw);
    expect(trustLearnedSender).toHaveBeenCalledWith("alice@example.com", true);
    expect(document.querySelector<HTMLInputElement>('input[role="switch"]')!.checked).toBe(true);
  });

  it("keeps the switch once trusted, so it can be turned off", async () => {
    await mount(mk({ trusted: true, address: ev({ areaId: "AU", filings: 3, acting: true, reason: null }), decidedBy: "address" }));
    expect(document.querySelector('input[role="switch"]')).not.toBeNull();
  });

  it("offers no trust for an authenticated sender", async () => {
    await mount(acting("address"));
    expect(document.querySelector('input[role="switch"]')).toBeNull();
    expect(document.body.textContent).not.toContain("Trust this sender");
  });
});
