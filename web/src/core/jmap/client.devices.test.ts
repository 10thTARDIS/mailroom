// SPDX-License-Identifier: AGPL-3.0-only

// SignedInDevice/get and /set on the wire (#937): what the client SENDS (the
// capability in `using`, ids null, "*others*") and that it never throws.

import { describe, expect, it } from "vitest";

import { JmapClient } from "./client";
import { CAP_DEVICES } from "./types";

function session(withCap: boolean) {
  return {
    apiUrl: "/jmap/api",
    downloadUrl: "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
    uploadUrl: "/jmap/upload/{accountId}",
    eventSourceUrl: "/jmap/eventsource",
    capabilities: withCap ? { [CAP_DEVICES]: {} } : {},
    accounts: { acct: {} },
    primaryAccounts: { "urn:ietf:params:jmap:mail": "acct" },
    state: "s1",
  };
}

interface Sent {
  using: string[];
  methodCalls: [string, Record<string, unknown>, string][];
}

async function clientWith(
  withCap: boolean,
  respond: (name: string) => Response | "throw",
): Promise<{ client: JmapClient; sent: Sent[] }> {
  const sent: Sent[] = [];
  const fakeFetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    if (String(input).includes("/jmap/session")) {
      return new Response(JSON.stringify(session(withCap)), { status: 200, headers: { "Content-Type": "application/json" } });
    }
    const body = JSON.parse(String(init?.body)) as Sent;
    sent.push(body);
    const r = respond(body.methodCalls[0]![0]);
    if (r === "throw") throw new Error("network down");
    return r;
  }) as unknown as typeof fetch;
  const client = new JmapClient({ fetch: fakeFetch });
  await client.connect();
  return { client, sent };
}

const answer = (name: string, value: unknown): Response =>
  new Response(JSON.stringify({ methodResponses: [[name, value, "d"]], sessionState: "s1" }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

const DEVICE = {
  id: "S1",
  description: "Firefox on Linux",
  createdAt: "2026-10-01T00:00:00Z",
  lastSeenAt: "2026-10-30T00:00:00Z",
  lastAddr: "203.0.113.7",
  isCurrent: true,
};

describe("signed-in devices on the wire", () => {
  it("devicesCapability follows the session", async () => {
    expect((await clientWith(true, () => "throw")).client.devicesCapability()).not.toBeNull();
    expect((await clientWith(false, () => "throw")).client.devicesCapability()).toBeNull();
  });

  it("get names the capability and asks for every id", async () => {
    const { client, sent } = await clientWith(true, (n) => answer(n, { list: [DEVICE] }));
    expect(await client.signedInDevices()).toEqual([DEVICE]);
    expect(sent[0]!.using).toContain(CAP_DEVICES);
    expect(sent[0]!.methodCalls[0]![0]).toBe("SignedInDevice/get");
    expect(sent[0]!.methodCalls[0]![1]).toEqual({ accountId: "acct", ids: null });
  });

  it("destroy sends the ids, and \"others\" sends the wildcard", async () => {
    const { client, sent } = await clientWith(true, (n) =>
      answer(n, { destroyed: ["S2"], notDestroyed: { S3: { type: "notFound" } } }),
    );
    expect(await client.signOutDevices(["S2", "S3"])).toEqual({ destroyed: ["S2"], notDestroyed: ["S3"] });
    expect(sent[0]!.using).toContain(CAP_DEVICES);
    expect(sent[0]!.methodCalls[0]![0]).toBe("SignedInDevice/set");
    expect(sent[0]!.methodCalls[0]![1]).toEqual({ accountId: "acct", destroy: ["S2", "S3"] });
    await client.signOutDevices("others");
    expect(sent[1]!.methodCalls[0]![1]).toEqual({ accountId: "acct", destroy: ["*others*"] });
  });

  it("returns null, and makes no request, without the capability", async () => {
    const { client, sent } = await clientWith(false, (n) => answer(n, { list: [] }));
    expect(await client.signedInDevices()).toBeNull();
    expect(await client.signOutDevices("others")).toBeNull();
    expect(sent).toHaveLength(0);
  });

  it("returns null on a transport failure and on a method error, never throwing", async () => {
    const down = await clientWith(true, () => "throw");
    expect(await down.client.signedInDevices()).toBeNull();
    expect(await down.client.signOutDevices(["S2"])).toBeNull();
    const refused = await clientWith(true, () => answer("error", { type: "forbidden" }));
    expect(await refused.client.signedInDevices()).toBeNull();
    expect(await refused.client.signOutDevices("others")).toBeNull();
  });
});
