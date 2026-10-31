// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// #747: a device's notification mode survives off/on. Turning notifications off
// destroys the server row, so the mode is kept client-side and sent on re-enable.

import { beforeEach, describe, expect, it, vi } from "vitest";

import type { JmapClient } from "../core/jmap/client";
import { disablePush, enablePush, setDeviceMode } from "./push";

type Create = Record<string, Record<string, unknown>>;

function fakeClient() {
  const calls: Array<Record<string, unknown>> = [];
  const client = {
    accountId: "a1",
    vapidPublicKey: () => "AAAA",
    webPushSubscriptionSet: vi.fn(async (args: Record<string, unknown>) => {
      calls.push(args);
      if (args.create) return { created: { web: { id: "sub-" + calls.length } } };
      if (args.update) return { updated: Object.fromEntries(Object.keys(args.update as object).map((k) => [k, null])) };
      return { destroyed: args.destroy };
    }),
    webPushSubscriptionGet: vi.fn(),
  };
  return { client: client as unknown as JmapClient, calls };
}

function installBrowser() {
  let sub: { endpoint: string; getKey: () => ArrayBuffer; unsubscribe: () => Promise<boolean> } | null = null;
  const mk = () => ({
    endpoint: "https://push.example/ep",
    getKey: () => new Uint8Array([1, 2, 3]).buffer,
    unsubscribe: async () => {
      sub = null;
      return true;
    },
  });
  const reg = {
    pushManager: {
      getSubscription: async () => sub,
      subscribe: async () => (sub = mk()),
    },
  };
  Object.defineProperty(navigator, "serviceWorker", {
    configurable: true,
    value: { ready: Promise.resolve(reg), getRegistration: async () => reg },
  });
  vi.stubGlobal("PushManager", class {});
  vi.stubGlobal("Notification", { permission: "granted", requestPermission: async () => "granted" });
}

describe("device push mode across re-subscribe (#747)", () => {
  beforeEach(() => {
    localStorage.clear();
    installBrowser();
  });

  it("sends the remembered mode with the new registration after off/on", async () => {
    const { client, calls } = fakeClient();
    await enablePush(client);
    expect((calls[0]?.create as Create).web).not.toHaveProperty("notifyMode");

    const id = localStorage.getItem("mailroom.push.subscriptionId") as string;
    expect(await setDeviceMode(client, id, "vip")).toBe(true);
    await disablePush(client);
    await enablePush(client);

    const recreate = calls.filter((c) => c.create).at(-1)?.create as Create;
    expect(recreate.web?.notifyMode).toBe("vip");
  });

  it("ignores a stored value it does not know", async () => {
    localStorage.setItem("mailroom.push.notifyMode", "bogus");
    const { client, calls } = fakeClient();
    await enablePush(client);
    expect((calls[0]?.create as Create).web).not.toHaveProperty("notifyMode");
  });
});
