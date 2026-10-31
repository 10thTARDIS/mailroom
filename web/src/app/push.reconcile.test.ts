// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// #937: push reconciliation on connect. Sign-out and the session sweep delete
// the server-side subscription; the browser keeps its own, and the Settings
// toggle reads the browser alone. Reconcile puts the server row back, once per
// load, and never prompts.

import { beforeEach, describe, expect, it, vi } from "vitest";

import type { JmapClient } from "../core/jmap/client";
import { reconcilePush, resetPushReconcileForTests } from "./push";

const SUB_ID_KEY = "mailroom.push.subscriptionId";

function setup(opts: { permission?: string; browserSub?: boolean; serverIds?: string[]; getFails?: boolean }) {
  const requestPermission = vi.fn(async () => "granted");
  const sub = {
    endpoint: "https://push.example/ep",
    getKey: () => new Uint8Array([1, 2, 3]).buffer,
    unsubscribe: vi.fn(async () => true),
  };
  const reg = { pushManager: { getSubscription: async () => (opts.browserSub === false ? null : sub) } };
  Object.defineProperty(navigator, "serviceWorker", {
    configurable: true,
    value: { ready: Promise.resolve(reg), getRegistration: async () => reg },
  });
  vi.stubGlobal("PushManager", class {});
  vi.stubGlobal("Notification", { permission: opts.permission ?? "granted", requestPermission });
  const client = {
    accountId: "a1",
    vapidPublicKey: () => "AAAA",
    webPushSubscriptionGet: vi.fn(async () => {
      if (opts.getFails) throw new Error("down");
      return { accountId: "a1", list: (opts.serverIds ?? []).map((id) => ({ id })) };
    }),
    webPushSubscriptionSet: vi.fn(async () => ({ created: { web: { id: "new-id" } } })),
  };
  return { client: client as unknown as JmapClient, fake: client, requestPermission, sub };
}

beforeEach(() => {
  localStorage.clear();
  resetPushReconcileForTests();
});

describe("reconcilePush", () => {
  it("re-registers a browser subscription the server no longer lists, without prompting", async () => {
    localStorage.setItem(SUB_ID_KEY, "old-id");
    const { client, fake, requestPermission, sub } = setup({ serverIds: ["someone-else"] });
    await reconcilePush(client);
    expect(fake.webPushSubscriptionSet).toHaveBeenCalledTimes(1);
    const arg = fake.webPushSubscriptionSet.mock.calls[0] as unknown as [{ create: { web: { endpoint: string } } }];
    expect(arg[0].create.web.endpoint).toBe(sub.endpoint);
    expect(localStorage.getItem(SUB_ID_KEY)).toBe("new-id");
    expect(requestPermission).not.toHaveBeenCalled();
  });

  it("re-registers when the browser has a subscription but remembers no server id", async () => {
    const { client, fake } = setup({ serverIds: [] });
    await reconcilePush(client);
    expect(fake.webPushSubscriptionSet).toHaveBeenCalledTimes(1);
  });

  it("does nothing when the server still lists it", async () => {
    localStorage.setItem(SUB_ID_KEY, "old-id");
    const { client, fake } = setup({ serverIds: ["old-id"] });
    await reconcilePush(client);
    expect(fake.webPushSubscriptionGet).toHaveBeenCalledTimes(1);
    expect(fake.webPushSubscriptionSet).not.toHaveBeenCalled();
  });

  it.each(["default", "denied"])("does nothing when permission is %s", async (permission) => {
    localStorage.setItem(SUB_ID_KEY, "old-id");
    const { client, fake, requestPermission } = setup({ permission });
    await reconcilePush(client);
    expect(fake.webPushSubscriptionGet).not.toHaveBeenCalled();
    expect(fake.webPushSubscriptionSet).not.toHaveBeenCalled();
    expect(requestPermission).not.toHaveBeenCalled();
  });

  it("does nothing when the browser holds no subscription", async () => {
    localStorage.setItem(SUB_ID_KEY, "old-id");
    const { client, fake } = setup({ browserSub: false });
    await reconcilePush(client);
    expect(fake.webPushSubscriptionGet).not.toHaveBeenCalled();
    expect(fake.webPushSubscriptionSet).not.toHaveBeenCalled();
  });

  it("runs once per page load", async () => {
    localStorage.setItem(SUB_ID_KEY, "old-id");
    const { client, fake } = setup({ serverIds: ["old-id"] });
    await reconcilePush(client);
    await reconcilePush(client);
    expect(fake.webPushSubscriptionGet).toHaveBeenCalledTimes(1);
  });

  it("is silent on failure and keeps the browser subscription", async () => {
    localStorage.setItem(SUB_ID_KEY, "old-id");
    const debug = vi.spyOn(console, "debug").mockImplementation(() => undefined);
    const { client, sub } = setup({ getFails: true });
    await expect(reconcilePush(client)).resolves.toBeUndefined();
    expect(debug).toHaveBeenCalled();
    expect(sub.unsubscribe).not.toHaveBeenCalled();
    debug.mockRestore();
  });
});
