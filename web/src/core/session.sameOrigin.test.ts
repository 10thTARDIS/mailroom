// SPDX-License-Identifier: AGPL-3.0-only

// The server's session URLs are absolute (RFC 8620 §2); the web app must keep
// working against them, and must send every request to the page's own origin.

import { describe, expect, it } from "vitest";

import { JmapClient } from "./jmap/client";
import { sameOriginSession } from "./session";
import type { JmapSession } from "./session";

const ABSOLUTE = {
  apiUrl: "https://mail.example.com/jmap",
  downloadUrl: "https://mail.example.com/jmap/download/{accountId}/{blobId}/{name}?type={type}",
  uploadUrl: "https://mail.example.com/jmap/upload/{accountId}/",
  eventSourceUrl:
    "https://mail.example.com/jmap/eventsource/?types={types}&closeafter={closeafter}&ping={ping}",
  previewUrl: "http://mail.example.com:8090/jmap/preview/{accountId}/{blobId}/{name}",
  capabilities: {},
  accounts: { acct: {} },
  primaryAccounts: { "urn:ietf:params:jmap:mail": "acct" },
  username: "u",
  state: "s1",
} as unknown as JmapSession;

describe("sameOriginSession", () => {
  it("keeps the path and query and leaves the template braces alone", () => {
    const s = sameOriginSession(ABSOLUTE);
    expect(s.apiUrl).toBe("/jmap");
    expect(s.downloadUrl).toBe("/jmap/download/{accountId}/{blobId}/{name}?type={type}");
    expect(s.uploadUrl).toBe("/jmap/upload/{accountId}/");
    expect(s.eventSourceUrl).toBe(
      "/jmap/eventsource/?types={types}&closeafter={closeafter}&ping={ping}",
    );
    expect(s.previewUrl).toBe("/jmap/preview/{accountId}/{blobId}/{name}");
  });

  it("leaves a relative session as it is", () => {
    const rel = { ...ABSOLUTE, apiUrl: "/jmap", downloadUrl: "/jmap/download/{blobId}" };
    const s = sameOriginSession(rel);
    expect(s.apiUrl).toBe("/jmap");
    expect(s.downloadUrl).toBe("/jmap/download/{blobId}");
  });

  it("makes a connected client call the api and build blob URLs on the page's origin", async () => {
    const urls: string[] = [];
    const fakeFetch = (async (input: RequestInfo | URL) => {
      const url = String(input);
      urls.push(url);
      if (url.includes("session")) {
        return new Response(JSON.stringify(ABSOLUTE), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      return new Response(JSON.stringify({ methodResponses: [], sessionState: "s1" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }) as unknown as typeof fetch;
    const client = new JmapClient({ fetch: fakeFetch });
    await client.connect();
    expect(client.blobUrl("b1", "a.txt", null)).toBe("/jmap/download/acct/b1/a.txt?type=");
    await client.request([{ name: "Core/echo", args: {}, id: "c0" }]).catch(() => undefined);
    const apiCalls = urls.filter((u) => !u.includes("session"));
    expect(apiCalls.length).toBeGreaterThan(0);
    for (const u of apiCalls) expect(u.startsWith("/")).toBe(true);
  });
});
