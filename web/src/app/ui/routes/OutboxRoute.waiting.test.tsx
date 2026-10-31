// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The Outbox's waiting rows (#52): a message held for a reconnect says so,
// names its account and offers Reconnect beside Cancel; one held for the
// provider says that instead, with Cancel and no Reconnect.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { EmailSubmission } from "../../../core/jmap/types";

const ACC = "01JB00000000000000000000A1";
let submissions: Record<string, EmailSubmission> = {};
const actions = { cancelSubmission: vi.fn(async () => true), dismissSubmission: vi.fn(async () => true) };
vi.mock("../../state", () => ({
  useApp: () => ({ actions, sync: { loadEmails: vi.fn() }, pushToast: vi.fn(), offerUndo: vi.fn(), client: {} }),
  useMailState: () => ({
    submissions,
    emails: { e1: { id: "e1", subject: "Hello", to: [{ name: null, email: "friend@example.net" }] } },
    identities: { i1: { id: "i1", email: "me@example.com", accountId: ACC } },
  }),
}));
vi.mock("../../routing", () => ({ navigate: vi.fn(), useRoute: () => ({ page: "outbox" }) }));
vi.mock("../useAccounts", () => ({
  useAccounts: () => ({
    byId: new Map([[ACC, { id: ACC, name: "Work (Outlook)", record: { id: ACC, authProvider: "microsoft" } }]]),
  }),
}));
const startOAuth = vi.fn(async (..._a: unknown[]): Promise<string | null> => null);
vi.mock("../oauthReturn", async (orig) => ({ ...(await orig<object>()), startOAuth: (...a: unknown[]) => startOAuth(...a) }));

import { OutboxRoute } from "./OutboxRoute";

let root: Root;
let host: HTMLDivElement;
const held = (why: "reconnect" | "provider"): EmailSubmission => ({
  id: "01JBQZ3K7WX2N4Y6P8R0S00001", identityId: "i1", emailId: "e1", threadId: null,
  sendAt: new Date(Date.now() - 60_000).toISOString(), undoStatus: "pending", mailroomState: "scheduled",
  mailroomHeld: why,
});
const buttons = () => [...host.querySelectorAll("button")].map((b) => b.textContent);

beforeEach(() => {
  host = document.createElement("div");
  document.body.replaceChildren(host);
  root = createRoot(host);
  startOAuth.mockClear();
});
afterEach(() => flushSync(() => root.unmount()));

describe("OutboxRoute waiting rows", () => {
  it("names the account to reconnect, offers Reconnect and keeps Cancel", async () => {
    submissions = { a: held("reconnect") };
    flushSync(() => root.render(<OutboxRoute />));
    expect(host.querySelector(".outbox-row-state")?.textContent).toBe("Waiting for you to reconnect Work (Outlook)");
    expect(host.textContent).not.toContain("Sending");
    expect(buttons()).toEqual(expect.arrayContaining(["Cancel", "Reconnect"]));
    flushSync(() => [...host.querySelectorAll("button")].find((b) => b.textContent === "Reconnect")!.click());
    await Promise.resolve();
    expect(startOAuth).toHaveBeenCalledWith({}, "microsoft", ACC);
  });

  it("says the provider is unavailable, with Cancel and no Reconnect", () => {
    submissions = { a: held("provider") };
    flushSync(() => root.render(<OutboxRoute />));
    expect(host.querySelector(".outbox-row-state")?.textContent).toContain("Sign-in to Microsoft is temporarily unavailable");
    expect(buttons()).toContain("Cancel");
    expect(buttons()).not.toContain("Reconnect");
  });
});
