// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE REVOKE DIALOG (#796), rendered for real: it says what revoking does and
// that it cannot be undone; its confirm button is UNAVAILABLE (aria-disabled,
// still focusable, described) until a reason is chosen and the fingerprint's
// last 8 characters are typed; a click while unavailable sends nothing; Tab
// stays inside; Escape closes; a refusal is a sentence, not a code.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { UserKey } from "../../../../core/jmap/types";
import { RevokeKeyDialog } from "./RevokeKeyDialog";
import { USER_KEY_BOUND } from "../../../../core/keyFixtures.testdata";

let host: HTMLDivElement;
let root: Root;
const onRevoke = vi.fn(async (..._a: unknown[]) => ({ ok: true as const }));
const onClose = vi.fn();
// The settings page behind the dialog: a React ancestor with its own keydown.
const pageKeyDown = vi.fn();
const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));
const KEY: UserKey = USER_KEY_BOUND;
const TAIL = KEY.fingerprint.slice(-8);

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  vi.clearAllMocks();
  flushSync(() =>
    root.render(
      <div onKeyDown={pageKeyDown}>
        <RevokeKeyDialog keyItem={KEY} address="reader@example.com" onRevoke={onRevoke} onClose={onClose} />
      </div>,
    ),
  );
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

const submit = () => document.body.querySelector(".key-revoke-submit") as HTMLButtonElement;
const confirmInput = () => document.body.querySelector(".key-revoke-confirm") as HTMLInputElement;
const text = () => document.body.textContent ?? "";

function type(value: string): void {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
  flushSync(() => {
    setter.call(confirmInput(), value);
    confirmInput().dispatchEvent(new Event("input", { bubbles: true }));
  });
}
function chooseReason(value: string): void {
  const r = document.body.querySelector(`input[type="radio"][value="${value}"]`) as HTMLInputElement;
  flushSync(() => r.click());
}

describe("what it says", () => {
  it("is a named modal dialog that says what revoking does and that it cannot be undone", () => {
    const dlg = document.body.querySelector('[role="dialog"]')!;
    expect(dlg.getAttribute("aria-modal")).toBe("true");
    const heading = document.getElementById(dlg.getAttribute("aria-labelledby")!)!;
    expect(heading.textContent).toBe("Revoke the key for reader@example.com?");
    expect(text()).toContain("must not be used any more");
    expect(text()).toContain("This can't be undone.");
    expect(text()).toContain("can still be opened here");
  });

  it("labels the confirm input with the characters to type, grouped", () => {
    const label = document.body.querySelector(`label[for="${confirmInput().id}"]`)!;
    expect(label.textContent).toContain(`${TAIL.slice(0, 4)} ${TAIL.slice(4)}`);
  });
});

describe("the confirm gating", () => {
  it("is aria-disabled (not disabled) and described until a reason and the tail are given", () => {
    expect(submit().getAttribute("aria-disabled")).toBe("true");
    // Still a tab stop: a disabled attribute would hide it from the keyboard.
    expect(submit().disabled).toBe(false);
    const why = document.getElementById(submit().getAttribute("aria-describedby")!)!;
    expect(why.textContent).toContain("Choose a reason");
    type(TAIL);
    expect(submit().getAttribute("aria-disabled")).toBe("true");
    chooseReason("compromised");
    expect(submit().getAttribute("aria-disabled")).toBe("false");
    expect(why.textContent).toContain("Ready");
  });

  it("does not match a wrong tail, and forgives case and spaces in the right one", () => {
    chooseReason("superseded");
    type("0000 0000");
    expect(submit().getAttribute("aria-disabled")).toBe("true");
    expect(document.getElementById(submit().getAttribute("aria-describedby")!)!.textContent).toContain(
      "once the characters match",
    );
    type(`${TAIL.slice(0, 4).toLowerCase()} ${TAIL.slice(4).toLowerCase()}`);
    expect(submit().getAttribute("aria-disabled")).toBe("false");
  });

  it("a click while unavailable sends nothing", async () => {
    chooseReason("retired");
    flushSync(() => submit().click());
    await settle();
    expect(onRevoke).not.toHaveBeenCalled();
    type("ABCD");
    flushSync(() => submit().click());
    await settle();
    expect(onRevoke).not.toHaveBeenCalled();
  });

  it("once ready, sends the reason and the tail upper-cased without spaces, then closes", async () => {
    chooseReason("superseded");
    type(`${TAIL.slice(0, 4).toLowerCase()} ${TAIL.slice(4)}`);
    flushSync(() => submit().click());
    await settle();
    expect(onRevoke).toHaveBeenCalledWith("superseded", TAIL);
    await settle();
    expect(onClose).toHaveBeenCalled();
  });

  it("a refusal is a sentence and the dialog stays", async () => {
    onRevoke.mockResolvedValueOnce({ ok: false, error: "rateLimited" } as never);
    chooseReason("compromised");
    type(TAIL);
    flushSync(() => submit().click());
    await settle();
    await settle();
    expect(onClose).not.toHaveBeenCalled();
    expect(document.body.querySelector('[role="alert"]')?.textContent).toBe(
      "Too many key operations in a row. Wait a minute and try again.",
    );
    expect(text()).not.toContain("rateLimited");
  });
});

describe("focus", () => {
  it("starts on Cancel, and Tab wraps at both edges without leaving", () => {
    expect(document.activeElement?.textContent).toBe("Cancel");
    const dlg = document.body.querySelector('[role="dialog"]') as HTMLElement;
    // jsdom has no layout, so the trap's visibility filter falls back to every
    // focusable (focusTrap.ts says why): first is the close button, last is
    // the revoke button.
    submit().focus();
    const tab = new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true });
    submit().dispatchEvent(tab);
    expect(tab.defaultPrevented).toBe(true);
    expect(dlg.contains(document.activeElement)).toBe(true);
    expect(document.activeElement?.getAttribute("aria-label")).toBe("Close");
    const back = new KeyboardEvent("keydown", { key: "Tab", shiftKey: true, bubbles: true, cancelable: true });
    document.activeElement!.dispatchEvent(back);
    expect(back.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(submit());
  });

  it("does not let Tab or Escape reach the settings page or the shell behind it", () => {
    // The shell listens on window for Escape; the settings page traps Tab in
    // its own onKeyDown. Neither may see a key this dialog handled.
    const shell = vi.fn();
    window.addEventListener("keydown", shell);
    try {
      submit().focus();
      submit().dispatchEvent(new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true }));
      submit().dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
    } finally {
      window.removeEventListener("keydown", shell);
    }
    expect(pageKeyDown).not.toHaveBeenCalled();
    expect(shell).not.toHaveBeenCalled();
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
