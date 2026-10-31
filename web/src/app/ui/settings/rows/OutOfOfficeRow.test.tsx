// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The Out of office row (#50). The server is a stand-in that records what it
// was asked. Pinned: every control has a label; an enabled reply is checked
// with the provider when the row opens and drift shows as a warning; a save
// sends the whole form; a refusal shows the server's sentence in an alert and
// keeps what was typed; the form's own checks stop a bad save before any
// request; an account the provider cannot serve shows why and no form.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { JSX } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { OutOfOffice, OutOfOfficeUpdate } from "../../../../core/jmap/types";

function obj(over: Partial<OutOfOffice> = {}): OutOfOffice {
  return {
    id: "A1", accountName: "Main", isEnabled: false, fromDate: null, toDate: null, subject: "Away",
    textBody: "I am away.", replyDays: 7, fromAddress: "me@example.com", contactsOnly: false,
    addresses: ["me@example.com", "alt@example.com"], isSupported: true, serverState: "off", serverDetail: "",
    checkedAt: null, contactsIncluded: null, contactsTotal: null, maxContacts: 2000, ...over,
  };
}

let list: OutOfOffice[] | null = [];
const outOfOfficeGet = vi.fn(async () => list);
const outOfOfficeCheck = vi.fn(async (_id: string): Promise<OutOfOffice | null> => null);
const outOfOfficeSet = vi.fn(
  async (_id: string, _u: OutOfOfficeUpdate): Promise<{ ok: true; value: OutOfOffice } | { ok: false; error: string }> => ({
    ok: true,
    value: obj(),
  }),
);

vi.mock("../../../state", () => ({
  useApp: () => ({ client: { outOfOfficeGet, outOfOfficeCheck, outOfOfficeSet } }),
  useMailState: () => ({ accountId: "U1" }),
}));

import { OutOfOfficeBody, useOutOfOffice } from "./OutOfOfficeRow";

function Harness(): JSX.Element {
  const oof = useOutOfOffice();
  return <OutOfOfficeBody oof={oof} />;
}

const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 0));
let host: HTMLDivElement;
let root: Root;

async function mount(): Promise<void> {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  flushSync(() => root.render(<Harness />));
  for (let i = 0; i < 4; i++) await settle();
}

function setValue(el: HTMLInputElement | HTMLTextAreaElement, value: string): void {
  const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, "value")?.set?.call(el, value);
  flushSync(() => el.dispatchEvent(new Event("input", { bubbles: true })));
}

function byLabel(text: string): HTMLInputElement {
  const label = [...host.querySelectorAll("label")].find((l) => l.textContent?.includes(text));
  if (!label) throw new Error(`no label "${text}"`);
  const el = label.htmlFor ? (host.querySelector(`#${CSS.escape(label.htmlFor)}`) as HTMLInputElement) : label.querySelector("input");
  if (!el) throw new Error(`label "${text}" names no control`);
  return el as HTMLInputElement;
}

async function submit(): Promise<void> {
  const form = host.querySelector("form") as HTMLFormElement;
  flushSync(() => form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true })));
  for (let i = 0; i < 4; i++) await settle();
}

beforeEach(() => {
  list = [obj()];
  vi.clearAllMocks();
});
afterEach(() => {
  root.unmount();
  host.remove();
});

describe("Out of office row", () => {
  it("labels every control", async () => {
    await mount();
    for (const control of host.querySelectorAll("input, textarea, select")) {
      const id = control.getAttribute("id");
      const labelled = (id && host.querySelector(`label[for="${CSS.escape(id)}"]`)) || control.closest("label");
      expect(labelled, `${control.tagName} #${id ?? ""} has no label`).toBeTruthy();
    }
    expect(host.querySelector("form")?.getAttribute("aria-labelledby")).toBeTruthy();
  });

  it("does not ask the provider about a reply that is off", async () => {
    await mount();
    expect(outOfOfficeCheck).not.toHaveBeenCalled();
  });

  it("checks an enabled reply when the row opens, and shows drift as a warning", async () => {
    list = [obj({ isEnabled: true, serverState: "active" })];
    outOfOfficeCheck.mockResolvedValueOnce(
      obj({ isEnabled: true, serverState: "displaced", serverDetail: 'The filter script "webmail" is now the active script.' }),
    );
    await mount();
    expect(outOfOfficeCheck).toHaveBeenCalledTimes(1);
    const status = host.querySelector(".oof-status") as HTMLElement;
    expect(status.textContent).toContain('"webmail"');
    expect(status.classList.contains("warn")).toBe(true);
  });

  it("sends the whole form", async () => {
    await mount();
    flushSync(() => byLabel("Send an automatic reply").click());
    setValue(byLabel("Subject"), "On leave");
    setValue(byLabel("Message") as unknown as HTMLTextAreaElement, "Back soon.");
    flushSync(() => byLabel("Only reply to people").click());
    await submit();
    expect(outOfOfficeSet).toHaveBeenCalledTimes(1);
    const [id, update] = outOfOfficeSet.mock.calls[0] as [string, OutOfOfficeUpdate];
    expect(id).toBe("A1");
    expect(update).toEqual({
      isEnabled: true, fromDate: null, toDate: null, subject: "On leave", textBody: "Back soon.",
      replyDays: 7, fromAddress: "me@example.com", contactsOnly: true,
    });
  });

  it("shows a refusal in an alert and keeps what was typed", async () => {
    outOfOfficeSet.mockResolvedValueOnce({
      ok: false,
      error: 'Out of office was not turned on: the filter script "webmail" is active on your mail server.',
    });
    await mount();
    flushSync(() => byLabel("Send an automatic reply").click());
    setValue(byLabel("Subject"), "Typed subject");
    await submit();
    const alert = host.querySelector('[role="alert"]');
    expect(alert?.textContent).toContain('"webmail" is active');
    expect(byLabel("Subject").value).toBe("Typed subject");
    expect(byLabel("Send an automatic reply").checked).toBe(true);
  });

  it("stops a bad form before any request", async () => {
    await mount();
    flushSync(() => byLabel("Send an automatic reply").click());
    setValue(byLabel("Message") as unknown as HTMLTextAreaElement, "   ");
    await submit();
    expect(outOfOfficeSet).not.toHaveBeenCalled();
    expect(host.querySelector('[role="alert"]')?.textContent).toContain("Write the message");
  });

  it("an account the provider cannot serve says why and offers no form", async () => {
    list = [obj({ isSupported: false, unsupportedReason: "Gmail offers no ManageSieve service" })];
    await mount();
    expect(host.querySelector("form")).toBeNull();
    expect(host.textContent).toContain("Gmail offers no ManageSieve service");
  });

  it("a server without the API says so, never 'off'", async () => {
    list = null;
    await mount();
    expect(host.textContent).toContain("This server cannot set an out-of-office reply.");
  });
});
