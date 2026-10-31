// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The reconnect banner (#52): shown for an account waiting for its user to
// sign in again, never for operator_error, dismissed for the session and for
// that set of accounts only, and its Reconnect goes straight to the provider
// for one account or to Settings for several.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { Account } from "../../core/jmap/types";

let records: Partial<Account>[] = [];
const client = {
  oauthProviders: () => ["google", "microsoft"],
  accountOAuthStart: vi.fn(async () => ({ authorizationUrl: "https://idp.example.com/auth", expiresAt: "" })),
  request: vi.fn(async () => [
    { name: "Account/get", value: { list: records } },
    { name: "Account/getHealth", value: { list: [] } },
  ]),
};
const navigate = vi.fn();
vi.mock("../state", () => ({
  useApp: () => ({ client }),
  useMailState: () => ({ accountId: "U1", identities: {} }),
}));
vi.mock("../routing", () => ({ navigate: (...a: unknown[]) => navigate(...a) }));

import { RECONNECT_DISMISSED_KEY, ReconnectBanner } from "./ReconnectBanner";
import { forgetAccounts } from "./useAccounts";

const base = { providerProfile: "microsoft", imapHost: "", imapPort: 993, smtpHost: "", smtpPort: 587,
  syncEnabled: true, writebackEnabled: true, color: null, authKind: "oauth2" as const, authProvider: "microsoft" };
const A = { ...base, id: "01JB00000000000000000000A1", name: "Work mail", authState: "needs_reauth" as const };
const B = { ...base, id: "01JB00000000000000000000B1", name: "Home mail", authState: "needs_reauth" as const };
const OP = { ...base, id: "01JB00000000000000000000C1", name: "Broken server", authState: "operator_error" as const };

let root: Root;
let host: HTMLDivElement;

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}
async function mount(): Promise<void> {
  flushSync(() => root.render(<ReconnectBanner />));
  await settle();
  flushSync(() => root.render(<ReconnectBanner />));
}
async function remount(): Promise<void> {
  flushSync(() => root.unmount());
  forgetAccounts();
  root = createRoot(host);
  await mount();
}
const button = (label: string) => [...host.querySelectorAll("button")].find((b) => b.textContent === label);

beforeEach(() => {
  sessionStorage.clear();
  forgetAccounts();
  host = document.createElement("div");
  document.body.replaceChildren(host);
  root = createRoot(host);
  vi.clearAllMocks();
});
afterEach(() => {
  flushSync(() => root.unmount());
});

describe("ReconnectBanner", () => {
  it("names the one account that needs signing in again", async () => {
    records = [A];
    await mount();
    expect(host.querySelector(".reconnect-banner")?.textContent).toContain("Work mail needs you to sign in again");
    expect(host.querySelector(".reconnect-banner-region")?.getAttribute("role")).toBe("status");
  });

  it("says nothing for operator_error or an active account: a reconnect cannot fix the server", async () => {
    records = [OP, { ...A, authState: "active" }];
    await mount();
    expect(host.querySelector(".reconnect-banner")).toBeNull();
    expect(host.querySelector(".reconnect-banner-region")).not.toBeNull();
  });

  it("stays dismissed for the session, and comes back when another account joins", async () => {
    records = [A];
    await mount();
    flushSync(() => button("Dismiss")!.click());
    expect(host.querySelector(".reconnect-banner")).toBeNull();
    expect(sessionStorage.getItem(RECONNECT_DISMISSED_KEY)).toBe(A.id);
    await remount();
    expect(host.querySelector(".reconnect-banner")).toBeNull();
    records = [A, B];
    await remount();
    expect(host.querySelector(".reconnect-banner")?.textContent).toContain("2 accounts need you to sign in again");
  });

  it("reconnects one account straight away, and sends several to Settings", async () => {
    records = [A];
    await mount();
    flushSync(() => button("Reconnect")!.click());
    await settle();
    expect(client.accountOAuthStart).toHaveBeenCalledWith("microsoft", A.id);

    records = [A, B];
    await remount();
    flushSync(() => button("Review accounts")!.click());
    expect(navigate).toHaveBeenCalledWith({ page: "settings", section: "accounts" });
  });
});
