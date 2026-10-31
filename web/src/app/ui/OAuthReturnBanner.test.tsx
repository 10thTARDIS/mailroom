// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The sign-in result (#52): each of the eighteen codes gets its own plain
// sentence, the query comes off the URL (so a reload does not repeat it), and
// already_connected offers Reconnect for the account the server named.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

const client = { accountOAuthStart: vi.fn(async () => ({ authorizationUrl: "https://idp.example.com/auth", expiresAt: "" })) };
vi.mock("../state", () => ({ useApp: () => ({ client }) }));

import { OAUTH_RESULT_CODES } from "../../core/oauth";
import { OAuthReturnBanner } from "./OAuthReturnBanner";
import { resetOAuthReturn, takeOAuthReturn } from "./oauthReturn";

const ACCT = "01JB00000000000000000000A1";
let root: Root;
let host: HTMLDivElement;

async function settle(): Promise<void> {
  for (let i = 0; i < 3; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}

function at(url: string): void {
  window.history.replaceState(null, "", url);
  resetOAuthReturn();
}

async function render(): Promise<void> {
  flushSync(() => root.render(<OAuthReturnBanner />));
  await settle();
}

beforeEach(() => {
  host = document.createElement("div");
  document.body.replaceChildren(host);
  root = createRoot(host);
  client.accountOAuthStart.mockClear();
});
afterEach(() => {
  flushSync(() => root.unmount());
  at("/");
});

describe("OAuthReturnBanner", () => {
  it("gives each of the eighteen codes its own sentence, never the raw key", async () => {
    const seen = new Set<string>();
    for (const code of OAUTH_RESULT_CODES) {
      at(`/?oauth=${code}&provider=microsoft&account=${ACCT}#/settings`);
      flushSync(() => root.unmount());
      root = createRoot(host);
      await render();
      const text = host.querySelector(".oauth-result p")?.textContent ?? "";
      expect(text, code).not.toBe("");
      expect(text, code).not.toContain("oauth.result");
      expect(text, code).not.toContain("{provider}");
      seen.add(text);
    }
    expect(seen.size).toBe(18);
  });

  it("names the provider, and takes the query off the URL keeping the hash", async () => {
    at(`/?oauth=connected&provider=google&account=${ACCT}#/settings`);
    await render();
    expect(host.textContent).toContain("Your Google account is connected.");
    expect(window.location.search).toBe("");
    expect(window.location.hash).toBe("#/settings");
    // A reload (a fresh read of the now-clean URL) shows nothing.
    resetOAuthReturn();
    expect(takeOAuthReturn()).toBeNull();
  });

  it("is a region that exists before its text, and says nothing without a result", async () => {
    at("/#/inbox");
    await render();
    const region = host.querySelector(".oauth-result-region");
    expect(region?.getAttribute("role")).toBe("status");
    expect(region?.textContent).toBe("");
  });

  it("dismisses", async () => {
    at("/?oauth=denied&provider=google");
    await render();
    const dismiss = [...host.querySelectorAll("button")].find((b) => b.textContent === "Dismiss")!;
    expect(dismiss.getAttribute("aria-label")).toContain("Dismiss");
    flushSync(() => dismiss.click());
    expect(host.querySelector(".oauth-result")).toBeNull();
  });

  it("offers Reconnect on already_connected, for the named account", async () => {
    at(`/?oauth=already_connected&provider=microsoft&account=${ACCT}`);
    await render();
    const reconnect = [...host.querySelectorAll("button")].find((b) => b.textContent === "Reconnect");
    expect(reconnect).toBeDefined();
    // The navigation itself is jsdom's to refuse; what matters is the call.
    try {
      flushSync(() => reconnect!.click());
      await settle();
    } catch {
      /* jsdom: navigation not implemented */
    }
    expect(client.accountOAuthStart).toHaveBeenCalledWith("microsoft", ACCT);
  });

  it("offers no Reconnect for any other code", async () => {
    at(`/?oauth=connected&provider=microsoft&account=${ACCT}`);
    await render();
    expect([...host.querySelectorAll("button")].some((b) => b.textContent === "Reconnect")).toBe(false);
  });
});
