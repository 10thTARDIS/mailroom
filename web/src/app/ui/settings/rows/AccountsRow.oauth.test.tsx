// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// Settings › Accounts for an account that signs in at its provider (#52): no
// host/port/password editor; the provider, the address and the sign-in state
// stated; Reconnect only where a reconnect can help (never operator_error);
// Disconnect behind a confirmation, then where to remove the access when the
// provider was not told; and a held Gmail account shows the server's hold
// sentence instead of a Resume button the server would refuse.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { Account, AccountHealth } from "../../../../core/jmap/types";

let records: Partial<Account>[] = [];
let health: Partial<AccountHealth>[] = [];
const client = {
  oauthProviders: () => ["google", "microsoft"],
  accountOAuthStart: vi.fn(async () => ({ authorizationUrl: "https://idp.example.com/auth", expiresAt: "" })),
  accountOAuthDisconnect: vi.fn(async (id: string) => ({ id, provider: "google", revocation: "failed" })),
  request: vi.fn(async () => [
    { name: "Account/get", value: { list: records } },
    { name: "Account/getHealth", value: { list: health } },
  ]),
};
vi.mock("../../../state", () => ({
  useApp: () => ({ client, actions: { updateAccount: vi.fn(async () => true) } }),
  useMailState: () => ({ accountId: "U1", identities: {}, mailboxes: {} }),
}));
vi.mock("../nav", async (orig) => ({ ...(await orig<object>()), useKeepScope: () => () => {} }));
vi.mock("../../AddAccountWizard", async (orig) => ({ ...(await orig<object>()), AddAccountWizard: () => null }));
const startOAuth = vi.fn(async (..._a: unknown[]): Promise<string | null> => null);
vi.mock("../../oauthReturn", async (orig) => ({
  ...(await orig<object>()),
  startOAuth: (...a: unknown[]) => startOAuth(...a),
}));

import { forgetAccounts } from "../../useAccounts";
import { AccountsBody } from "./AccountsRow";

const base = { imapHost: "outlook.office365.com", imapPort: 993, smtpHost: "smtp.office365.com", smtpPort: 587,
  syncEnabled: true, writebackEnabled: true, color: null, authKind: "oauth2" as const };
const MS = { ...base, id: "01JB00000000000000000000A1", name: "Work", providerProfile: "microsoft",
  authProvider: "microsoft", authAddress: "me@example.com" };
const GM = { ...base, id: "01JB00000000000000000000G1", name: "Gmail", providerProfile: "gmail",
  authProvider: "google", authAddress: "me@gmail.example", authState: "active" as const, syncEnabled: false };
const healthFor = (id: string, extra: Partial<AccountHealth> = {}): Partial<AccountHealth> => ({
  id,
  name: id,
  syncStatus: { lastSuccessAt: null, lastAttemptAt: null, lagSeconds: null, lastError: null, lastErrorAt: null, consecutiveFailures: 0 },
  deliveryFailures: { count: 0, recent: [] },
  deadLetters: { count: 0, reasons: [] },
  sendQuota: { sent: 0, limit: 0 },
  writeback: { enabled: true, suppressedIntents: 0 },
  ...extra,
});

let root: Root;
let host: HTMLDivElement;

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}
async function mount(accountId: string | null): Promise<void> {
  flushSync(() => root.render(<AccountsBody accountId={accountId} />));
  await settle();
  flushSync(() => root.render(<AccountsBody accountId={accountId} />));
}
const button = (label: string) => [...document.querySelectorAll("button")].find((b) => b.textContent === label);
const legends = () => [...host.querySelectorAll("legend")].map((l) => l.textContent);

beforeEach(() => {
  forgetAccounts();
  host = document.createElement("div");
  document.body.replaceChildren(host);
  root = createRoot(host);
  vi.clearAllMocks();
});
afterEach(() => flushSync(() => root.unmount()));

describe("an OAuth account's settings", () => {
  it("states provider, address and state, with no server editor", async () => {
    records = [{ ...MS, authState: "active" }];
    health = [healthFor(MS.id, { auth: { kind: "oauth2", provider: "microsoft", state: "active", reason: null, stateChangedAt: null, lastRefreshAt: null } })];
    await mount(MS.id);
    expect(legends()).toContain("Sign-in");
    expect(legends()).not.toContain("Server");
    expect(host.textContent).toContain("Signs in with Microsoft as me@example.com");
    expect(host.textContent).toContain("Working normally.");
    expect(button("Change credentials…")).toBeUndefined();
    expect(host.querySelector("input[type=password]")).toBeNull();
    expect(button("Reconnect")).toBeUndefined();
    expect(button("Disconnect…")).toBeDefined();
  });

  it("needs_reauth: says nothing is lost, offers Reconnect for this account, shows the reason code", async () => {
    records = [{ ...MS, authState: "needs_reauth" }];
    health = [healthFor(MS.id, { auth: { kind: "oauth2", provider: "microsoft", state: "needs_reauth", reason: "invalid_grant", stateChangedAt: null, lastRefreshAt: null } })];
    await mount(MS.id);
    expect(host.textContent).toContain("needs you to sign in again");
    expect(host.textContent).toContain("nothing is lost");
    expect(host.textContent).toContain("invalid_grant");
    flushSync(() => button("Reconnect")!.click());
    await settle();
    expect(startOAuth).toHaveBeenCalledWith(client, "microsoft", MS.id);
  });

  it("operator_error: tells the user to contact the operator, and offers no Reconnect", async () => {
    records = [{ ...MS, authState: "operator_error" }];
    health = [healthFor(MS.id)];
    await mount(MS.id);
    expect(host.textContent).toContain("contact the person who runs this server");
    expect(button("Reconnect")).toBeUndefined();
  });

  it("disconnected: Reconnect, and no Disconnect", async () => {
    records = [{ ...MS, authState: "disconnected", authAddress: null, syncEnabled: false }];
    health = [healthFor(MS.id)];
    await mount(MS.id);
    expect(host.textContent).toContain("Signs in with Microsoft");
    expect(button("Reconnect")).toBeDefined();
    expect(button("Disconnect…")).toBeUndefined();
  });

  it("Disconnect confirms first, then says where to remove access the provider kept", async () => {
    records = [GM];
    health = [healthFor(GM.id)];
    await mount(GM.id);
    flushSync(() => button("Disconnect…")!.click());
    expect(document.querySelector("[role=dialog]")?.textContent).toContain("The mail already here stays");
    expect(client.accountOAuthDisconnect).not.toHaveBeenCalled();
    flushSync(() => button("Disconnect")!.click());
    await settle();
    expect(client.accountOAuthDisconnect).toHaveBeenCalledWith(GM.id);
    expect(host.textContent).toContain("remove it in your Google Account");
  });

  it("a held Gmail account shows the server's hold sentence instead of Resume", async () => {
    records = [GM];
    health = [healthFor(GM.id, { syncHold: { code: "gmail_sync_pending", message: "Gmail sync arrives in the next update; your account is connected and ready." } })];
    await mount(GM.id);
    expect(host.textContent).toContain("Gmail sync arrives in the next update");
    expect(button("Resume sync")).toBeUndefined();
    expect(button("Pause sync")).toBeUndefined();
  });

  it("a syncing Gmail account shows the import progress, and keeps the Pause button", async () => {
    records = [{ ...GM, syncEnabled: true }];
    health = [healthFor(GM.id, {
      gmailImport: {
        messagesOnServer: 12000,
        messagesImported: 3000,
        bytesImported: 1,
        historyComplete: false,
        bytesDownloadedLast24h: 1,
        dailyBudgetBytes: 2,
        remainingEstimateBytes: 5,
        etaDays: 4,
      },
    })];
    await mount(GM.id);
    // Built the same way the component builds it: the test environment's
    // locale may or may not insert thousands separators.
    expect(host.textContent).toContain(
      `${(3000).toLocaleString()} of ${(12000).toLocaleString()} messages so far`,
    );
    expect(host.textContent).toContain("About 4 more days");
    expect(button("Pause sync")).toBeDefined();
  });

  it("an import without a server total states only what is known, and a one-day ETA says within a day", async () => {
    records = [{ ...GM, syncEnabled: true }];
    health = [healthFor(GM.id, {
      gmailImport: {
        messagesOnServer: null,
        messagesImported: 3000,
        bytesImported: 1,
        historyComplete: false,
        bytesDownloadedLast24h: 1,
        dailyBudgetBytes: 2,
        remainingEstimateBytes: null,
        etaDays: 1,
      },
    })];
    await mount(GM.id);
    expect(host.textContent).toContain("Importing your Gmail");
    expect(host.textContent).toContain("messages so far.");
    expect(host.textContent).not.toContain(" of ");
    expect(host.textContent).toContain("Should finish within a day.");
  });

  it("a finished import says done, and not that it is still importing", async () => {
    records = [{ ...GM, syncEnabled: true }];
    health = [healthFor(GM.id, {
      gmailImport: {
        messagesOnServer: 12000,
        messagesImported: 12000,
        bytesImported: 9,
        historyComplete: true,
        bytesDownloadedLast24h: 0,
        dailyBudgetBytes: 2,
        remainingEstimateBytes: 0,
        etaDays: 0,
      },
    })];
    await mount(GM.id);
    expect(host.textContent).toContain("Your Gmail history is fully imported.");
    expect(host.textContent).not.toContain("Importing");
  });

  it("the list says which way the sign-in is broken, and offers Connect", async () => {
    records = [{ ...MS, authState: "needs_reauth" }, { ...GM, id: "01JB00000000000000000000G2", authState: "operator_error", syncEnabled: true }];
    health = [];
    await mount(null);
    expect(host.textContent).toContain("needs reconnecting");
    expect(host.textContent).toContain("server sign-in problem");
    expect(button("Connect Gmail")).toBeDefined();
    expect(button("Connect Outlook")).toBeDefined();
  });

  it("shows an admin the providers refusing this server, and nobody else", async () => {
    records = [{ ...MS, authState: "operator_error" }];
    health = [];
    client.request.mockImplementationOnce(async () => [
      { name: "Account/get", value: { list: records } },
      { name: "Account/getHealth", value: { list: [], operatorNotices: [{ provider: "microsoft", code: "invalid_client", since: "2026-10-27T09:00:00Z", lastSeenAt: "2026-10-27T10:00:00Z", accounts: 2 }] } },
    ]);
    await mount(null);
    expect(host.querySelector(".setting-operator-notices")?.textContent).toContain("Microsoft is refusing this server's sign-in client (invalid_client)");
    forgetAccounts();
    flushSync(() => root.unmount());
    root = createRoot(host);
    await mount(null);
    expect(host.querySelector(".setting-operator-notices")).toBeNull();
  });
});
