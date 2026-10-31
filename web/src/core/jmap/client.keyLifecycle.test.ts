// SPDX-License-Identifier: AGPL-3.0-only

// The v1.93 final-scope JMAP calls (#815, #813, #812, #817, #818) on the wire:
// what the client SENDS (an exact date as expiresAt and never alongside
// expiresDays, which the server refuses; includeRemoved; replace's expiry) and
// what it READS back from responses captured from the C1 server (the bind
// warning, a publish's null update, history, refresh).

import { describe, expect, it } from "vitest";

import { JmapClient, JmapMethodError } from "./client";
import { CAP_PGP } from "./types";
import {
  C1_BIND_LISTED,
  C1_BIND_NOT_LISTED,
  C1_HISTORY_BOB,
  C1_METHOD_ERRORS,
  C1_PUBLISH_UPDATED,
  C1_REFRESH_ALICE_NOT_FOUND,
} from "../keyHistoryFixtures.testdata";

const SESSION = {
  apiUrl: "/jmap/api",
  downloadUrl: "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
  uploadUrl: "/jmap/upload/{accountId}",
  eventSourceUrl: "/jmap/eventsource",
  capabilities: { [CAP_PGP]: { encrypt: true, keyManagement: true, keyExpiryWarningDays: 30 } },
  accounts: { acct: {} },
  primaryAccounts: { "urn:ietf:params:jmap:mail": "acct" },
  state: "s1",
};

type Call = [string, Record<string, unknown>, string];

/** A client whose API answers with `answer(call)` and records every call. */
async function clientAnswering(answer: (call: Call) => [string, unknown]): Promise<{ client: JmapClient; calls: Call[] }> {
  const calls: Call[] = [];
  const fakeFetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url.includes("/jmap/session")) {
      return new Response(JSON.stringify(SESSION), { status: 200, headers: { "Content-Type": "application/json" } });
    }
    const body = JSON.parse(String(init?.body)) as { methodCalls: Call[] };
    const responses = body.methodCalls.map((c) => {
      calls.push(c);
      const [name, value] = answer(c);
      return [name, value, c[2]];
    });
    return new Response(JSON.stringify({ methodResponses: responses, sessionState: "s1" }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  }) as unknown as typeof fetch;
  const client = new JmapClient({ fetch: fakeFetch });
  await client.connect();
  return { client, calls };
}

const ok = (value: unknown) => (c: Call): [string, unknown] => [c[0], value];

describe("exact expiry on the wire (#813)", () => {
  it("generate sends expiresAt alone, never with expiresDays", async () => {
    const { client, calls } = await clientAnswering(ok({ id: "K", fingerprint: "F", boundTo: null, key: {} }));
    await client.userKeyGenerate("I", { expiresAt: "2026-11-04", expiresDays: 10, bind: true });
    expect(calls[0]![1]).toEqual({ accountId: "acct", identityId: "I", expiresAt: "2026-11-04", bind: true });
    await client.userKeyGenerate("I", { expiresDays: 365 });
    expect(calls[1]![1]).toEqual({ accountId: "acct", identityId: "I", expiresDays: 365 });
    await client.userKeyGenerate("I", {});
    expect(calls[2]![1]).toEqual({ accountId: "acct", identityId: "I" });
  });

  it("extend sends days or expiresAt, exactly one", async () => {
    const { client, calls } = await clientAnswering(ok({ id: "K", expiresAt: null, key: {} }));
    await client.userKeyExtend("K", 90);
    await client.userKeyExtend("K", { expiresAt: "2026-11-30" });
    expect(calls.map((c) => c[1])).toEqual([
      { accountId: "acct", id: "K", days: 90 },
      { accountId: "acct", id: "K", expiresAt: "2026-11-30" },
    ]);
  });

  it("replace sends the id and the chosen expiry", async () => {
    const { client, calls } = await clientAnswering(ok({ id: "N" }));
    await client.userKeyReplace("K", { expiresAt: "2027-11-25" });
    await client.userKeyReplace("K", {});
    expect(calls.map((c) => [c[0], c[1]])).toEqual([
      ["UserKey/replace", { accountId: "acct", id: "K", expiresAt: "2027-11-25" }],
      ["UserKey/replace", { accountId: "acct", id: "K" }],
    ]);
  });
});

describe("what a bind says back (#812)", () => {
  it("keeps the server's warning on a key that does not name the address", async () => {
    const id = Object.keys(C1_BIND_NOT_LISTED)[0]!;
    const { client } = await clientAnswering(ok({ updated: C1_BIND_NOT_LISTED, notUpdated: null }));
    expect(await client.userKeySet(id, { identityId: "I" })).toEqual({
      ok: true,
      updated: { boundAddressListed: false, warning: "addressNotInKey" },
    });
  });

  it("a key that names it, and a publish (a null update), carry no warning", async () => {
    const listed = Object.keys(C1_BIND_LISTED)[0]!;
    const published = Object.keys(C1_PUBLISH_UPDATED)[0]!;
    let { client } = await clientAnswering(ok({ updated: C1_BIND_LISTED }));
    expect(await client.userKeySet(listed, { identityId: "I" })).toEqual({ ok: true, updated: { boundAddressListed: true } });
    ({ client } = await clientAnswering(ok({ updated: C1_PUBLISH_UPDATED })));
    expect(await client.userKeySet(published, { wkdPublished: true })).toEqual({ ok: true, updated: {} });
    // The old entry point still answers null for success.
    expect(await client.userKeyUpdate(published, { wkdPublished: true })).toBeNull();
  });
});

describe("correspondents' keys (#815, #818)", () => {
  it("asks for removed keys only when told to", async () => {
    const { client, calls } = await clientAnswering((c) => [c[0], { list: [] }]);
    await client.contactKeysRead({ includeRemoved: true });
    await client.contactKeysRead();
    const gets = calls.filter((c) => c[0] === "ContactKey/get").map((c) => c[1]);
    expect(gets).toEqual([{ accountId: "acct", includeRemoved: true }, { accountId: "acct" }]);
  });

  it("history and refresh read the captured answers; a refusal throws its type", async () => {
    const { client } = await clientAnswering((c) =>
      c[0] === "ContactKey/history"
        ? [c[0], { id: c[1]["id"], list: C1_HISTORY_BOB }]
        : c[1]["id"] === "again"
          ? ["error", C1_METHOD_ERRORS.refreshAgain]
          : [c[0], C1_REFRESH_ALICE_NOT_FOUND],
    );
    expect(await client.contactKeyHistory("B")).toEqual(C1_HISTORY_BOB);
    expect((await client.contactKeyRefresh("A")).result).toBe("notFound");
    const err = await client.contactKeyRefresh("again").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(JmapMethodError);
    expect((err as JmapMethodError).type).toBe("rateLimited");
  });
});
