// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// Learned placement's off switch in Settings (#714): a checkbox whose
// accessible name is its visible text (WCAG 2.5.3), reflecting the server's
// DeskPreferences.learnedPlacementEnabled, and writing the flipped value
// through the one DeskPreferences write path.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { Area, DeskPreferences, LearningStatus } from "../../../../core/jmap/types";
import { MailStore } from "../../../../core/store";
import type { MailState } from "../../../../core/store";

let state: MailState;
const updateDeskPreferences = vi.fn(async () => true);
const learningStatus = vi.fn(async (): Promise<LearningStatus | null> => null);
const ruleSuggestion = vi.fn(async (): Promise<unknown> => ({ suggest: true, reason: "mixed", earlier: 0 }));
const createRule = vi.fn(async (_input: unknown): Promise<unknown> => ({ id: "R1" }));
const pushToast = vi.fn((_t: unknown) => 1);
// One stable object: the component's effect depends on `actions`.
const appValue = {
  actions: { updateDeskPreferences, learningStatus, ruleSuggestion, createRule },
  store: { getState: () => state },
  sync: {},
  offerUndo: vi.fn(),
  pushToast,
};
vi.mock("../../../state", () => ({
  useApp: () => appValue,
  useMailState: () => state,
}));

import { LearnedPlacementToggle } from "./LearnedPlacementToggle";

function prefs(over: Partial<DeskPreferences>): DeskPreferences {
  return {
    id: "U1",
    lineThreshold: 0.5,
    drawerDefaultDays: 30,
    trayNagDays: 7,
    drawerSweepEnabled: false,
    setupCompletedAt: "2026-10-01T00:00:00Z",
    ...over,
  };
}

let root: Root;
let host: HTMLElement;
beforeEach(() => {
  updateDeskPreferences.mockClear();
  learningStatus.mockReset();
  learningStatus.mockImplementation(async () => null);
  ruleSuggestion.mockClear();
  createRule.mockClear();
  pushToast.mockClear();
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

function area(id: string, name: string): Area {
  return { id, name, position: 1, labelId: "L-" + id, tabs: [], labelIds: [], autoArchive: true, autoArchiveDays: null, hiddenFromRail: false };
}

function mount(p: DeskPreferences | null): void {
  const store = new MailStore(null);
  if (p) store.setDeskPreferences(p);
  store.putAreas([area("A-ALERTS", "Alerts"), area("A-NEWS", "News")]);
  state = store.getState();
  flushSync(() => root.render(<LearnedPlacementToggle />));
}

function box(): HTMLInputElement | null {
  return host.querySelector('input[type="checkbox"]');
}

describe("LearnedPlacementToggle (#714)", () => {
  it("shows the server's ON as a checked box whose name is its visible text", () => {
    mount(prefs({ learnedPlacementEnabled: true }));
    const input = box();
    expect(input?.checked).toBe(true);
    const label = input?.closest("label");
    // WCAG 2.5.3: the accessible name comes from the wrapping label's text,
    // with no aria-label to diverge from it.
    expect(input?.getAttribute("aria-label")).toBeNull();
    expect(label?.textContent).toBe("Sort new mail into areas by learning from your moves");
  });

  it("switching it off writes learnedPlacementEnabled: false through DeskPreferences", () => {
    mount(prefs({ learnedPlacementEnabled: true }));
    flushSync(() => box()?.click());
    expect(updateDeskPreferences).toHaveBeenCalledTimes(1);
    expect(updateDeskPreferences).toHaveBeenCalledWith({ learnedPlacementEnabled: false }, expect.any(String));
  });

  it("shows OFF as unchecked, and switching it on writes true", () => {
    mount(prefs({ learnedPlacementEnabled: false }));
    expect(box()?.checked).toBe(false);
    flushSync(() => box()?.click());
    expect(updateDeskPreferences).toHaveBeenCalledWith({ learnedPlacementEnabled: true }, expect.any(String));
  });

  it("draws nothing against a server that does not send the switch", () => {
    mount(prefs({}));
    expect(box()).toBeNull();
    mount(null);
    expect(box()).toBeNull();
  });
});

const settle = async (): Promise<void> => {
  await new Promise((r) => setTimeout(r, 0));
};

function status(over: Partial<LearningStatus> = {}): LearningStatus {
  return {
    enabled: true,
    placed: 37,
    corrected: 2,
    kept: 30,
    open: 5,
    senders: [],
    moreSenders: 0,
    sendersTruncated: false,
    ...over,
  };
}

async function mountStatus(st: LearningStatus | null, on = true): Promise<void> {
  learningStatus.mockImplementation(async () => st);
  mount(prefs({ learnedPlacementEnabled: on }));
  await settle();
}

describe("the status block under the switch", () => {
  it("says how many messages it filed and how many the user moved", async () => {
    await mountStatus(status());
    expect(host.textContent).toContain("In the last 30 days learned sorting filed 37 messages for you.");
    expect(host.textContent).toContain("You moved 2 of them somewhere else.");
  });

  it("says plainly when it filed nothing, and never mentions moves then", async () => {
    await mountStatus(status({ placed: 0, corrected: 0 }));
    expect(host.textContent).toContain("Learned sorting hasn’t filed anything in the last 30 days.");
    expect(host.textContent).not.toContain("You moved");
  });

  it("omits the moved sentence when nothing was moved", async () => {
    await mountStatus(status({ corrected: 0 }));
    expect(host.textContent).toContain("filed 37 messages");
    expect(host.textContent).not.toContain("You moved");
  });

  it("is hidden when the switch is off, and does not even ask", async () => {
    await mountStatus(status(), false);
    expect(learningStatus).not.toHaveBeenCalled();
    expect(host.querySelector(".learned-status")).toBeNull();
    expect(host.textContent).not.toContain("learned sorting filed");
  });

  it("is hidden when the server says it is off", async () => {
    await mountStatus(status({ enabled: false }));
    expect(host.querySelector(".learned-status")).toBeNull();
  });

  it("a failed status call renders nothing and raises no toast", async () => {
    await mountStatus(null);
    expect(host.querySelector(".learned-status")).toBeNull();
    expect(pushToast).not.toHaveBeenCalled();
    // Control: the same mount with an answer does render the block.
    flushSync(() => root.unmount());
    root = createRoot(host);
    await mountStatus(status());
    expect(host.querySelector(".learned-status")).not.toBeNull();
  });
});

describe("senders you keep moving by hand", () => {
  const sender = (over = {}) => ({ address: "noreply@example.com", areaId: "A-ALERTS", reason: "unauthenticated", filings: 3, ...over });

  it("lists each sender with its area and a plain reason", async () => {
    await mountStatus(
      status({ senders: [sender(), sender({ address: "news@example.com", areaId: "A-NEWS", reason: "mixed" })] }),
    );
    expect(host.querySelector("h4")?.textContent).toBe("Senders you keep moving by hand");
    const items = [...host.querySelectorAll("li")].map((li) => li.textContent);
    expect(items[0]).toContain("noreply@example.com → Alerts · not confirmed as genuine");
    expect(items[1]).toContain("news@example.com → News · filed in more than one place");
    expect(host.querySelector("li strong")?.textContent).toBe("noreply@example.com");
  });

  it("hides the heading when there are none", async () => {
    await mountStatus(status());
    expect(host.querySelector("h4")).toBeNull();
  });

  it("says how many more there are, and that it is at least that many when cut", async () => {
    await mountStatus(status({ senders: [sender()], moreSenders: 3 }));
    expect(host.textContent).toContain("and 3 more");
    flushSync(() => root.unmount());
    root = createRoot(host);
    await mountStatus(status({ senders: [sender()], moreSenders: 20, sendersTruncated: true }));
    expect(host.textContent).toContain("and at least 20 more");
  });

  it("Make a rule runs the shared flow: the rule is created, then the line goes", async () => {
    await mountStatus(status({ senders: [sender()] }));
    const button = host.querySelector("li button") as HTMLButtonElement;
    expect(button.textContent).toBe("Make a rule");
    flushSync(() => button.click());
    await settle();
    expect(ruleSuggestion).toHaveBeenCalledWith("A-ALERTS", "noreply@example.com");
    expect(createRule).toHaveBeenCalledTimes(1);
    expect(createRule.mock.calls[0]?.[0]).toMatchObject({
      conditions: { all: [{ field: "from", op: "equals", value: "noreply@example.com" }] },
      actions: [{ type: "addLabel", label: "L-A-ALERTS" }],
    });
    expect(pushToast).toHaveBeenCalledWith(expect.objectContaining({ message: "Rule made." }));
    expect(host.querySelector("li")).toBeNull();
  });

  it("offers to move earlier mail, as the toast does", async () => {
    ruleSuggestion.mockImplementation(async () => ({ suggest: true, reason: "mixed", earlier: 4 }));
    await mountStatus(status({ senders: [sender()] }));
    flushSync(() => (host.querySelector("li button") as HTMLButtonElement).click());
    await settle();
    const toast = pushToast.mock.calls.map((c) => c[0] as { action?: { label: string } }).find((x) => x.action);
    expect(toast?.action?.label).toBe("Move them");
  });

  it("keeps the line when the rule could not be made", async () => {
    createRule.mockImplementation(async () => null);
    await mountStatus(status({ senders: [sender()] }));
    flushSync(() => (host.querySelector("li button") as HTMLButtonElement).click());
    await settle();
    expect(host.querySelector("li")).not.toBeNull();
  });
});
