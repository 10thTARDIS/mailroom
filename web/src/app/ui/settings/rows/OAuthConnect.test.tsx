// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// Connect Gmail / Connect Outlook (#52): offered only for the providers the
// server lists; the interstitial says what access means, and the provider's
// particular hurdles, BEFORE anything leaves the page; it is a named modal
// that holds focus, closes on Escape back to its opener, and Continue starts
// the sign-in for that provider.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

let providers: string[] = [];
const client = { oauthProviders: () => providers, accountOAuthStart: vi.fn() };
vi.mock("../../../state", () => ({ useApp: () => ({ client }) }));
const startOAuth = vi.fn(async (..._a: unknown[]): Promise<string | null> => null);
vi.mock("../../oauthReturn", async (orig) => ({
  ...(await orig<object>()),
  startOAuth: (...a: unknown[]) => startOAuth(...a),
}));

import { OAuthConnectButtons } from "./OAuthConnect";

let root: Root;
let host: HTMLDivElement;

async function settle(): Promise<void> {
  for (let i = 0; i < 3; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}
const button = (label: string) => [...document.querySelectorAll("button")].find((b) => b.textContent === label);
const dialog = () => document.querySelector<HTMLElement>("[role=dialog]");

beforeEach(() => {
  host = document.createElement("div");
  document.body.replaceChildren(host);
  root = createRoot(host);
  startOAuth.mockClear();
});
afterEach(() => flushSync(() => root.unmount()));

describe("OAuthConnectButtons", () => {
  it("renders nothing when the server offers no provider", () => {
    providers = [];
    flushSync(() => root.render(<OAuthConnectButtons />));
    expect(host.childElementCount).toBe(0);
  });

  it("offers exactly the providers the server lists, in a named group", () => {
    providers = ["microsoft"];
    flushSync(() => root.render(<OAuthConnectButtons />));
    expect(button("Connect Outlook")).toBeDefined();
    expect(button("Connect Gmail")).toBeUndefined();
    expect(host.querySelector("[role=group]")?.getAttribute("aria-label")).toBe("Connect an account by signing in");
    // An unknown provider name from a newer server is not offered words it has none for.
    providers = ["google", "yahoo"];
    flushSync(() => root.render(<OAuthConnectButtons />));
    expect([...host.querySelectorAll(".setting-oauth-buttons button")].map((b) => b.textContent)).toEqual(["Connect Gmail"]);
  });

  it("explains Google's access and its unverified-app warning before anything leaves the page", async () => {
    providers = ["google", "microsoft"];
    flushSync(() => root.render(<OAuthConnectButtons />));
    flushSync(() => button("Connect Gmail")!.click());
    const d = dialog()!;
    expect(d.getAttribute("aria-modal")).toBe("true");
    const title = document.getElementById(d.getAttribute("aria-labelledby")!);
    expect(title?.textContent).toBe("Connect your Gmail account");
    expect(d.textContent).toContain("read, send, organise and delete all the mail");
    expect(d.textContent).toContain("Whoever runs this Mailroom server");
    expect(d.textContent).toContain("unverified");
    expect(d.textContent).not.toContain("IMAP");
    expect(startOAuth).not.toHaveBeenCalled();
    // Cancel has focus first.
    expect(document.activeElement?.textContent).toBe("Cancel");
    flushSync(() => button("Continue to Google")!.click());
    await settle();
    expect(startOAuth).toHaveBeenCalledWith(client, "google");
  });

  it("tells Outlook.com users to switch IMAP on, and work users about admin approval", () => {
    providers = ["microsoft"];
    flushSync(() => root.render(<OAuthConnectButtons />));
    flushSync(() => button("Connect Outlook")!.click());
    const d = dialog()!;
    expect(d.textContent).toContain("switch on IMAP first");
    expect(d.textContent).toContain("administrator may have to approve");
    expect(d.textContent).not.toContain("unverified");
  });

  it("holds focus inside, and Escape closes it back to the button that opened it", async () => {
    providers = ["microsoft"];
    flushSync(() => root.render(<OAuthConnectButtons />));
    const opener = button("Connect Outlook")!;
    opener.focus();
    flushSync(() => opener.click());
    const last = button("Continue to Microsoft")!;
    last.focus();
    flushSync(() => {
      last.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true }));
    });
    expect(dialog()!.contains(document.activeElement)).toBe(true);
    flushSync(() => {
      document.activeElement!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
    });
    expect(dialog()).toBeNull();
    await settle();
    expect(document.activeElement).toBe(button("Connect Outlook"));
  });

  it("shows why a sign-in could not start, and stays open", async () => {
    providers = ["google"];
    startOAuth.mockResolvedValueOnce("that provider is not configured on this server");
    flushSync(() => root.render(<OAuthConnectButtons />));
    flushSync(() => button("Connect Gmail")!.click());
    flushSync(() => button("Continue to Google")!.click());
    await settle();
    expect(dialog()?.querySelector("[role=alert]")?.textContent).toContain("not configured");
  });
});
