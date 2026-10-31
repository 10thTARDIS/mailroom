// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// Follow-up reminders in Settings (#58): absent against a server without
// them; each control named by its visible words; every write is the WHOLE
// followUp object through the one DeskPreferences write path; the delay is
// offered only while the inbound reminder is on.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { DeskPreferences, FollowUpSettings as Settings } from "../../../../core/jmap/types";
import { MailStore } from "../../../../core/store";
import type { MailState } from "../../../../core/store";

let state: MailState;
const updateDeskPreferences = vi.fn(async () => true);
vi.mock("../../../state", () => ({
  useApp: () => ({ actions: { updateDeskPreferences } }),
  useMailState: () => state,
}));

import { FollowUpSettings } from "./FollowUpSettings";

function prefs(followUp: Settings | undefined): DeskPreferences {
  return {
    id: "U1",
    lineThreshold: 0.5,
    drawerDefaultDays: 30,
    trayNagDays: 7,
    drawerSweepEnabled: false,
    setupCompletedAt: "2026-10-01T00:00:00Z",
    ...(followUp ? { followUp } : {}),
  };
}

let root: Root;
let host: HTMLElement;
beforeEach(() => {
  updateDeskPreferences.mockClear();
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

function mount(f: Settings | undefined): void {
  const store = new MailStore(null);
  store.setDeskPreferences(prefs(f));
  state = store.getState();
  flushSync(() => root.render(<FollowUpSettings />));
}

const boxes = () => [...host.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')];

describe("FollowUpSettings (#58)", () => {
  it("is absent against a server that does not send followUp", () => {
    mount(undefined);
    expect(host.childElementCount).toBe(0);
  });

  it("shows the defaults: unanswered-mail on, waiting-on-you off, no delay picker", () => {
    mount({ outbound: true, inbound: false, inboundDays: 3 });
    const [out, inb] = boxes();
    expect(out!.checked).toBe(true);
    expect(inb!.checked).toBe(false);
    expect(host.querySelector("select")).toBeNull();
    for (const b of boxes()) {
      expect(b.getAttribute("aria-label")).toBeNull();
      expect(b.closest("label")?.textContent?.length).toBeGreaterThan(10);
    }
  });

  it("opting in writes the whole object", () => {
    mount({ outbound: true, inbound: false, inboundDays: 3 });
    flushSync(() => boxes()[1]!.click());
    expect(updateDeskPreferences).toHaveBeenCalledWith(
      { followUp: { outbound: true, inbound: true, inboundDays: 3 } },
      expect.any(String),
    );
  });

  it("offers the delay while the inbound reminder is on, named by its label", () => {
    mount({ outbound: false, inbound: true, inboundDays: 3 });
    const select = host.querySelector("select")!;
    expect(select.value).toBe("3");
    expect(select.closest("label")?.textContent).toContain("Remind me after");
    select.value = "7";
    flushSync(() => select.dispatchEvent(new Event("change", { bubbles: true })));
    expect(updateDeskPreferences).toHaveBeenCalledWith(
      { followUp: { outbound: false, inbound: true, inboundDays: 7 } },
      expect.any(String),
    );
  });

  it("keeps a stored delay that is not one of the offered choices", () => {
    mount({ outbound: true, inbound: true, inboundDays: 4 });
    const values = [...host.querySelectorAll("option")].map((o) => o.value);
    expect(values).toContain("4");
    expect(host.querySelector("select")!.value).toBe("4");
  });
});
