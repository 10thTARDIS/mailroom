// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// A MODIFIED CLICK ANYWHERE ON A DESK ROW SELECTS (#708). The requirement is to
// build a selection by Shift-clicking (a range) and Ctrl/Cmd-clicking (one row
// at a time) on any part of a message, not only on its sender. A plain click
// keeps its meaning: the sender selects, the rest opens.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { Email } from "../../../core/jmap/types";
import { MailStore, effectiveEmail } from "../../../core/store";
import type { EffectiveEmail } from "../../../core/store";
import { DeskRow } from "./DeskRow";

function row(): EffectiveEmail {
  const store = new MailStore(null);
  store.putEmails([
    {
      id: "m1",
      blobId: "b",
      threadId: "t",
      mailboxIds: {},
      keywords: {},
      size: 1,
      receivedAt: "2026-10-01T00:00:00Z",
      subject: "Disk usage above 90% on host-3",
      preview: "Alert fired at 14:02",
      from: [{ name: "Monitoring", email: "alerts@example.com" }],
      hasAttachment: false,
    } as Email,
  ]);
  const e = effectiveEmail(store.getState(), "m1");
  if (!e) throw new Error("no effective email");
  return e;
}

const onOpen = vi.fn();
const onToggleSelect = vi.fn();
let root: Root;
let host: HTMLElement;

beforeEach(() => {
  onOpen.mockClear();
  onToggleSelect.mockClear();
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root.render(
      <DeskRow
        email={row()}
        selected={false}
        isCursor={false}
        isOpen={false}
        threadCount={null}
        account={null}
        queued={false}
        now={Date.parse("2026-10-02T00:00:00Z")}
        onOpen={onOpen}
        onToggleSelect={onToggleSelect}
      />,
    ),
  );
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

function el(selector: string): HTMLElement {
  const found = host.querySelector<HTMLElement>(selector);
  if (!found) throw new Error("no element " + selector);
  return found;
}

function click(target: HTMLElement, mods: MouseEventInit = {}): void {
  flushSync(() => {
    target.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, ...mods }));
  });
}

describe("DeskRow modifier-click selection (#708)", () => {
  it("a plain click on the subject opens and does not select", () => {
    click(el(".desk-main"));
    expect(onOpen).toHaveBeenCalledWith("m1");
    expect(onToggleSelect).not.toHaveBeenCalled();
  });

  it("a plain click on the sender selects and does not open (unchanged)", () => {
    click(el(".desk-sender"));
    expect(onToggleSelect).toHaveBeenCalledWith("m1", false);
    expect(onOpen).not.toHaveBeenCalled();
  });

  for (const [name, mods, shift] of [
    ["Ctrl", { ctrlKey: true }, false],
    ["Cmd", { metaKey: true }, false],
    ["Shift", { shiftKey: true }, true],
  ] as const) {
    it(`${name}+click on the subject selects (range: ${shift}) and does not open`, () => {
      click(el(".desk-main"), mods);
      expect(onToggleSelect).toHaveBeenCalledTimes(1);
      expect(onToggleSelect).toHaveBeenCalledWith("m1", shift);
      expect(onOpen).not.toHaveBeenCalled();
    });

    it(`${name}+click on the sender selects exactly once`, () => {
      click(el(".desk-sender"), mods);
      expect(onToggleSelect).toHaveBeenCalledTimes(1);
      expect(onToggleSelect).toHaveBeenCalledWith("m1", shift);
    });

    it(`${name}+click on the marks cell selects`, () => {
      click(el(".desk-meta"), mods);
      expect(onToggleSelect).toHaveBeenCalledWith("m1", shift);
      expect(onOpen).not.toHaveBeenCalled();
    });
  }
});
