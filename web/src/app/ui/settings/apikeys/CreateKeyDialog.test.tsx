// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// CREATE A KEY, SEE ITS TOKEN ONCE (#49), rendered for real:
//
//   * send + delete together is refused CLIENT-SIDE until the phrase is typed:
//     the Create button stays aria-disabled and a click sends nothing;
//   * the token is shown once, in a labelled modal that only the Done button
//     closes (no ✕, Escape and the backdrop do nothing), and after Done it is
//     nowhere: not in the DOM, not in storage, not in the URL, not in a log,
//     and not in what `onCreated` was handed;
//   * both phases pass axe and trap Tab.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { ApiKey, ApiKeySettings, ApiKeysCapability } from "../../../../core/jmap/types";
import type { ApiKeysApi } from "./api";
import { axeViolations } from "./axe.testdata";
import { CreateKeyDialog } from "./CreateKeyDialog";
import { KEY } from "./fixtures.testdata";

const TOKEN = "mr_" + "q7".repeat(26);
const CAP: ApiKeysCapability = {
  scopes: ["mail:read", "mail:send", "mail:draft", "mail:move", "mail:delete", "labels:read", "labels:write", "search:read", "contacts:read", "contacts:write"],
  confirmableScopes: ["mail:send", "mail:delete"],
  defaultRatePerMinute: 60,
  maxRatePerMinute: 600,
  defaultDailyBudget: 500,
  maxDailyBudget: 20000,
};

let host: HTMLDivElement;
let root: Root;
const create = vi.fn(async (s: ApiKeySettings) => ({ ok: true as const, key: { ...KEY, name: s.name ?? "", scopes: s.scopes ?? [] } as ApiKey, token: TOKEN }));
const onCreated = vi.fn();
const onClose = vi.fn();
const pageKeyDown = vi.fn();
const api: ApiKeysApi = {
  capability: () => CAP,
  list: async () => [],
  create: (s) => create(s),
  update: async () => ({ ok: true }),
  revoke: async () => ({ ok: true }),
  events: async () => null,
  pending: async () => [],
  decide: async () => ({ ok: false, error: { type: "notFound" } }),
  subjects: async () => new Map(),
};
const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 0));
// The document as markup, read (never written) through a serializer.
const markup = (): string => new XMLSerializer().serializeToString(document.body);

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  vi.clearAllMocks();
  flushSync(() =>
    root.render(
      <div onKeyDown={pageKeyDown}>
        <CreateKeyDialog
          api={api}
          accounts={[{ id: "A1", name: "Work" }]}
          origin="https://mail.example.com"
          onCreated={onCreated}
          onClose={onClose}
        />
      </div>,
    ),
  );
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
  vi.restoreAllMocks();
});

const q = <T extends Element>(sel: string): T => document.body.querySelector(sel) as T;
const submit = () => q<HTMLButtonElement>(".apikey-create-submit");

function setValue(input: HTMLInputElement, value: string): void {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
  flushSync(() => {
    setter.call(input, value);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}
function nameInput(): HTMLInputElement {
  return q<HTMLInputElement>(".apikey-create input[type='text']");
}
function click(el: Element): void {
  flushSync(() => (el as HTMLElement).click());
}
function chooseCustom(): void {
  click(q('input[type="radio"][value="custom"]'));
}
function tick(scope: string): void {
  click(q(`.apikey-scopes input[type="checkbox"][value="${scope}"]`));
}

describe("the form", () => {
  it("is a named modal dialog, starts on the name, and passes axe", async () => {
    const dlg = q('[role="dialog"]');
    expect(dlg.getAttribute("aria-modal")).toBe("true");
    expect(document.getElementById(dlg.getAttribute("aria-labelledby")!)?.textContent).toBe("Create an API key");
    expect(document.activeElement).toBe(nameInput());
    expect(await axeViolations()).toEqual([]);
  });

  it("offers presets, and Custom shows the server's scope list", () => {
    expect(document.body.textContent).toContain("Read only");
    expect(document.body.textContent).toContain("Organise");
    expect(document.body.textContent).toContain("Assistant");
    chooseCustom();
    const boxes = [...document.body.querySelectorAll<HTMLInputElement>(".apikey-scopes input[type=checkbox]")].map((b) => b.value);
    expect(boxes.sort()).toEqual([...(CAP.scopes ?? [])].sort());
  });
});

describe("send and delete together — the typed confirmation, enforced client-side", () => {
  beforeEach(() => {
    setValue(nameInput(), "Everything");
    chooseCustom();
    tick("mail:send");
    tick("mail:delete");
  });

  it("asks for the phrase with the server's wording, and holds the button", async () => {
    expect(document.body.textContent).toContain("An agent with both can mail anyone as you and then remove the trace.");
    expect(submit().getAttribute("aria-disabled")).toBe("true");
    expect(submit().disabled).toBe(false);
    expect(document.getElementById(submit().getAttribute("aria-describedby")!)?.textContent).toContain("Type the phrase");
    click(submit());
    await settle();
    expect(create).not.toHaveBeenCalled();
  });

  it("a wrong phrase sends nothing; the right one sends the acknowledgement", async () => {
    setValue(q<HTMLInputElement>(".apikey-both-confirm"), "yes please");
    click(submit());
    await settle();
    expect(create).not.toHaveBeenCalled();
    setValue(q<HTMLInputElement>(".apikey-both-confirm"), "Send and Delete");
    expect(submit().getAttribute("aria-disabled")).toBe("false");
    click(submit());
    await settle();
    expect(create).toHaveBeenCalledTimes(1);
    const sent = create.mock.calls[0]![0];
    expect(sent.acknowledgeSendAndDelete).toBe(true);
    expect(sent.scopes).toEqual(expect.arrayContaining(["mail:send", "mail:delete"]));
    // "Ask me first" defaulted on for both.
    expect(sent.confirmScopes).toEqual(["mail:send", "mail:delete"]);
  });
});

describe("the token, shown once", () => {
  async function createKey(): Promise<void> {
    setValue(nameInput(), "Desktop assistant");
    click(submit());
    await settle();
    await settle();
  }

  it("shows the token, says it won't be shown again, and fills the setup snippet", async () => {
    await createKey();
    const dlg = q('[role="dialog"]');
    expect(document.getElementById(dlg.getAttribute("aria-labelledby")!)?.textContent).toBe(
      "Your new key: Desktop assistant",
    );
    expect(q<HTMLInputElement>(".apikey-token-value").value).toBe(TOKEN);
    expect(document.body.textContent).toContain("You won't see it again");
    expect(document.body.textContent).toContain(`"MAILROOM_AUTH": "Bearer ${TOKEN}"`);
    expect(document.body.textContent).toContain("https://mail.example.com/mcp");
    expect(document.activeElement).toBe(q(".apikey-token-value"));
    expect(await axeViolations()).toEqual([]);
  });

  it("only Done closes it: no ✕, and Escape or a click outside do nothing (nor reach the page)", async () => {
    await createKey();
    expect(q('[role="dialog"] [aria-label="Close"]')).toBeNull();
    const shell = vi.fn();
    window.addEventListener("keydown", shell);
    try {
      q(".apikey-token-value").dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
    } finally {
      window.removeEventListener("keydown", shell);
    }
    flushSync(() => q(".modal-backdrop").dispatchEvent(new MouseEvent("mousedown", { bubbles: true })));
    expect(onClose).not.toHaveBeenCalled();
    expect(shell).not.toHaveBeenCalled();
    expect(pageKeyDown).not.toHaveBeenCalled();
    expect(q(".apikey-token-value")).not.toBeNull();
    click(q(".apikey-token-done"));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("traps Tab inside the token dialog", async () => {
    await createKey();
    const dlg = q<HTMLElement>('[role="dialog"]');
    const done = q<HTMLButtonElement>(".apikey-token-done");
    done.focus();
    const tab = new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true });
    done.dispatchEvent(tab);
    expect(tab.defaultPrevented).toBe(true);
    expect(dlg.contains(document.activeElement)).toBe(true);
    expect(pageKeyDown).not.toHaveBeenCalled();
  });

  it("is never persisted: not in storage, the URL, a log, or what onCreated receives — and gone after Done", async () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    const logs = (["log", "info", "warn", "error", "debug"] as const).map((m) => vi.spyOn(console, m));
    const pushState = vi.spyOn(history, "pushState");
    const replaceState = vi.spyOn(history, "replaceState");
    await createKey();
    expect(markup()).toContain(TOKEN); // shown while open

    // Handed up WITHOUT the token.
    expect(onCreated).toHaveBeenCalledTimes(1);
    expect(JSON.stringify(onCreated.mock.calls)).not.toContain(TOKEN);
    // Nothing wrote it anywhere.
    for (const call of setItem.mock.calls) expect(JSON.stringify(call)).not.toContain(TOKEN);
    for (const spy of logs) for (const call of spy.mock.calls) expect(JSON.stringify(call)).not.toContain(TOKEN);
    for (const call of [...pushState.mock.calls, ...replaceState.mock.calls]) expect(JSON.stringify(call)).not.toContain(TOKEN);
    expect(location.href).not.toContain(TOKEN);
    const stored = [localStorage, sessionStorage].flatMap((s) => Object.keys(s).map((k) => s.getItem(k) ?? ""));
    expect(stored.join("\n")).not.toContain(TOKEN);

    // Done closes the dialog; the parent unmounts it (as ApiKeysBody does), and
    // nothing of the token remains in the document.
    click(q(".apikey-token-done"));
    expect(onClose).toHaveBeenCalled();
    flushSync(() => root.render(<div />));
    expect(markup()).not.toContain(TOKEN);
    expect(markup()).not.toContain(TOKEN.slice(3, 20));
  });
});
