// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The quiet-hours form (#59): every control is named by its visible text, the
// whole object is saved (the server wants all six fields), a period with no
// length cannot be saved, and the form starts from the browser's zone until a
// setting has been saved.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { QuietHours } from "../../../../core/jmap/types";

vi.mock("../../../state", () => ({ useApp: () => ({}), useMailState: () => ({}) }));

import { QuietHoursForm } from "./NotificationControls";

const stored: QuietHours = { enabled: false, start: 1320, end: 420, days: [0, 1, 2, 3, 4, 5, 6], timeZone: "UTC", allowVip: true };
const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

function setValue(input: HTMLInputElement | HTMLSelectElement, value: string): void {
  const proto = input instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, "value")!.set!.call(input, value);
  flushSync(() => {
    input.dispatchEvent(new Event(input instanceof HTMLSelectElement ? "change" : "input", { bubbles: true }));
  });
}
function clickEl(el: Element): void {
  flushSync(() => {
    el.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
  });
}
const labelled = (text: string): HTMLInputElement | HTMLSelectElement => {
  const label = [...document.querySelectorAll("label")].find((l) => l.textContent?.trim().startsWith(text));
  const input = label?.querySelector("input, select");
  if (!input) throw new Error(`no control labelled ${text}: ${document.body.textContent}`);
  return input as HTMLInputElement;
};

describe("QuietHoursForm (#59)", () => {
  let root: Root;
  const save = vi.fn(async (_q: QuietHours) => true);
  beforeEach(() => {
    save.mockClear();
    const host = document.createElement("div");
    document.body.replaceChildren(host);
    root = createRoot(host);
    vi.spyOn(Intl.DateTimeFormat.prototype, "resolvedOptions").mockReturnValue({ timeZone: "Europe/London" } as Intl.ResolvedDateTimeFormatOptions);
  });
  afterEach(() => {
    flushSync(() => root.unmount());
    vi.restoreAllMocks();
  });

  it("names every control by its visible label and saves the whole object", async () => {
    flushSync(() => root.render(<QuietHoursForm stored={stored} save={save} />));
    // Every input/select sits inside its label: no aria-label to drift.
    for (const c of document.querySelectorAll("form input, form select")) {
      expect(c.closest("label"), `${c.tagName} ${c.getAttribute("type")} is not inside a label`).not.toBeNull();
      expect(c.hasAttribute("aria-label")).toBe(false);
    }
    // A never-saved setting starts from the browser's zone, not UTC.
    expect((labelled("Time zone") as HTMLSelectElement).value).toBe("Europe/London");

    clickEl(labelled("Turn on quiet hours"));
    setValue(labelled("From"), "23:30");
    setValue(labelled("Until"), "06:15");
    clickEl(labelled("Saturday"));
    clickEl(labelled("Sunday"));
    clickEl(labelled("Allow VIPs during quiet hours"));
    clickEl([...document.querySelectorAll("button")].find((b) => b.textContent === "Save quiet hours")!);
    await settle();
    expect(save).toHaveBeenCalledWith({
      enabled: true,
      start: 23 * 60 + 30,
      end: 6 * 60 + 15,
      days: [1, 2, 3, 4, 5],
      timeZone: "Europe/London",
      allowVip: false,
    });
  });

  it("a period with no length is said and cannot be saved", () => {
    flushSync(() => root.render(<QuietHoursForm stored={{ ...stored, enabled: true }} save={save} />));
    setValue(labelled("Until"), "22:00");
    expect(document.querySelector('[role="alert"]')?.textContent).toContain("no quiet period");
    const button = [...document.querySelectorAll("button")].find((b) => b.textContent === "Save quiet hours")!;
    expect(button.disabled).toBe(true);
  });
});
