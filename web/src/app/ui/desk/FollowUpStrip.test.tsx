// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The desk's Follow up strip (#58): nothing at all when nothing is due; a row
// per nudge that opens the message; Done and Snooze in a group NAMED BY ITS
// ROW, so "Done" says which conversation it closes.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { FollowUpItem } from "../../../core/jmap/types";
import { FollowUpStrip } from "./FollowUpStrip";
import { followUpText } from "./followUp";

const NOW = Date.parse("2026-10-18T12:00:00Z");
const DAY = 24 * 60 * 60 * 1000;

function item(over: Partial<FollowUpItem>): FollowUpItem {
  return {
    emailId: "E1",
    threadId: "T1",
    kind: "outbound",
    since: new Date(NOW - 10 * DAY).toISOString(),
    dueAt: new Date(NOW - 3 * DAY).toISOString(),
    subject: "Plans",
    who: { name: "", email: "ann@example.org" },
    ...over,
  };
}

let root: Root;
let host: HTMLElement;
beforeEach(() => {
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

function mount(items: FollowUpItem[], pending: Set<string> = new Set()) {
  const onOpen = vi.fn();
  const onDone = vi.fn();
  const onSnooze = vi.fn();
  flushSync(() =>
    root.render(
      <FollowUpStrip items={items} now={NOW} pending={pending} onOpen={onOpen} onDone={onDone} onSnooze={onSnooze} />,
    ),
  );
  return { onOpen, onDone, onSnooze };
}

describe("followUpText", () => {
  it("names the day for unanswered mail, and who it went to", () => {
    const text = followUpText(item({}), NOW);
    expect(text.startsWith("No one answered your mail to ann@example.org from ")).toBe(true);
    expect(text).not.toContain("{");
  });
  it("counts whole days, rounded down, for mail waiting on the user", () => {
    const it4 = item({ kind: "inbound", since: new Date(NOW - 4.9 * DAY).toISOString(), who: { name: "Bob", email: "bob@example.org" } });
    expect(followUpText(it4, NOW)).toBe("You haven't replied to Bob in 4 days");
    const it1 = item({ kind: "inbound", since: new Date(NOW - 1.2 * DAY).toISOString(), who: { name: "", email: "" } });
    expect(followUpText(it1, NOW)).toBe("You haven't replied to this in 1 day");
  });
});

describe("FollowUpStrip (#58)", () => {
  it("draws nothing at all when nothing is due", () => {
    mount([]);
    expect(host.childElementCount).toBe(0);
  });

  it("draws a titled section with one row per nudge", () => {
    mount([item({}), item({ emailId: "E2", kind: "inbound", subject: "" })]);
    const section = host.querySelector("section.desk-followup");
    expect(section?.getAttribute("aria-labelledby")).toBe("desk-followup-title");
    expect(host.querySelector("#desk-followup-title")?.textContent).toBe("Follow up 2");
    expect(host.querySelectorAll("li.desk-followup-item")).toHaveLength(2);
    expect(host.querySelectorAll(".desk-followup-subject")[1]?.textContent).toBe("(no subject)");
  });

  it("names each action group by its own row", () => {
    mount([item({}), item({ emailId: "E2" })]);
    const groups = [...host.querySelectorAll<HTMLElement>('[role="group"]')];
    expect(groups).toHaveLength(2);
    for (const g of groups) {
      const id = g.getAttribute("aria-labelledby");
      expect(id).toBeTruthy();
      const row = host.querySelector(`#${id}`);
      // The group's name is the row's own text, inside the same list item.
      expect(row?.closest("li")).toBe(g.closest("li"));
      expect(row?.textContent).toContain("Plans");
    }
  });

  it("opens, marks done and snoozes the row it is on", () => {
    const cb = mount([item({}), item({ emailId: "E2" })]);
    const second = host.querySelectorAll("li")[1]!;
    flushSync(() => second.querySelector<HTMLButtonElement>(".desk-followup-open")!.click());
    expect(cb.onOpen).toHaveBeenCalledWith(expect.objectContaining({ emailId: "E2" }));
    const [done, snooze] = [...second.querySelectorAll<HTMLButtonElement>(".desk-followup-actions button")];
    expect(done!.textContent).toBe("Done");
    expect(snooze!.getAttribute("aria-haspopup")).toBe("dialog");
    flushSync(() => done!.click());
    flushSync(() => snooze!.click());
    expect(cb.onDone).toHaveBeenCalledWith(expect.objectContaining({ emailId: "E2" }));
    expect(cb.onSnooze).toHaveBeenCalledWith(expect.objectContaining({ emailId: "E2" }));
  });

  it("disables a row's actions while its Done is in flight", () => {
    mount([item({}), item({ emailId: "E2" })], new Set(["E1"]));
    const [first, second] = [...host.querySelectorAll("li")];
    expect([...first!.querySelectorAll("button[disabled]")]).toHaveLength(2);
    expect([...second!.querySelectorAll("button[disabled]")]).toHaveLength(0);
  });
});
