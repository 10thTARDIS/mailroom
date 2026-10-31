// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE PENDING-APPROVALS BANNER (#49): a polite live region that exists before
// it has anything to say; "N actions waiting for your approval" once
// PendingAction/get reports some, linking to Settings → API keys; nothing at
// all on a server without the capability; polled only while the tab is
// visible.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { PendingAction } from "../../core/jmap/types";
import { HELD_DELETE, HOSTILE_SEND } from "./settings/apikeys/fixtures.testdata";

const navigate = vi.fn();
vi.mock("../routing", () => ({ navigate: (...a: unknown[]) => navigate(...a) }));
const pendingActionsGet = vi.fn(async (_s?: readonly string[]): Promise<PendingAction[] | null> => [HOSTILE_SEND, HELD_DELETE]);
let advertised = true;
let client = { pendingActionsGet, hasCapability: () => advertised };
vi.mock("../state", () => ({
  useApp: () => ({ client }),
  useMailState: () => ({ accountId: "U1" }),
}));

import { POLL_MS } from "./pendingApprovals";
import { PendingApprovalsBanner } from "./PendingApprovalsBanner";
import { axeViolations } from "./settings/apikeys/axe.testdata";

let host: HTMLDivElement;
let root: Root;
const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

beforeEach(() => {
  vi.clearAllMocks();
  advertised = true;
  // A fresh client object per test: the store is keyed by client.
  client = { pendingActionsGet, hasCapability: () => advertised };
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
  vi.useRealTimers();
});

function render(): void {
  flushSync(() => root.render(<PendingApprovalsBanner />));
}

describe("the banner", () => {
  it("is a polite live region that says how many are waiting and links to the list", async () => {
    render();
    const region = host.querySelector('[role="status"]')!;
    expect(region).not.toBeNull();
    await settle();
    await settle();
    expect(pendingActionsGet).toHaveBeenCalledWith(["pending"]);
    expect(region.textContent).toContain("2 actions waiting for your approval");
    const link = [...host.querySelectorAll("button")].find((b) => b.textContent === "Review")!;
    flushSync(() => link.click());
    expect(navigate).toHaveBeenCalledWith({ page: "settings", section: "api-keys" });
    expect(await axeViolations()).toEqual([]);
  });

  it("the region exists, empty, while nothing is waiting", async () => {
    pendingActionsGet.mockResolvedValueOnce([]);
    render();
    await settle();
    await settle();
    const region = host.querySelector('[role="status"]')!;
    expect(region).not.toBeNull();
    expect(region.textContent).toBe("");
  });

  it("asks nothing and says nothing on a server without API keys", async () => {
    advertised = false;
    render();
    await settle();
    expect(pendingActionsGet).not.toHaveBeenCalled();
    expect(host.textContent).toBe("");
  });

  it("polls while visible, not while hidden", async () => {
    vi.useFakeTimers();
    render();
    await vi.advanceTimersByTimeAsync(0);
    expect(pendingActionsGet).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(POLL_MS);
    expect(pendingActionsGet).toHaveBeenCalledTimes(2);
    const vis = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    await vi.advanceTimersByTimeAsync(POLL_MS * 3);
    expect(pendingActionsGet).toHaveBeenCalledTimes(2);
    vis.mockReturnValue("visible");
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(0);
    expect(pendingActionsGet).toHaveBeenCalledTimes(3);
    vis.mockRestore();
  });
});
