// SPDX-License-Identifier: AGPL-3.0-only

// A sign-in gate in front of Mailroom (a reverse proxy with forward auth) answers
// with a redirect once its own session expires. That must read as "signed out",
// which sends the browser through the gate to sign in again, and never as
// "offline", where Retry can never succeed.

import { describe, expect, it } from "vitest";

import { JmapAuthError, JmapClient, JmapTransportError } from "./client";

const SESSION = {
  apiUrl: "/jmap",
  downloadUrl: "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
  uploadUrl: "/jmap/upload/{accountId}/",
  eventSourceUrl: "/jmap/eventsource/",
  capabilities: {},
  accounts: { acct: {} },
  primaryAccounts: { "urn:ietf:params:jmap:mail": "acct" },
  username: "u",
  state: "s1",
};

// What fetch hands back for a redirect under `redirect: "manual"` in a browser.
function opaqueRedirect(): Response {
  return { type: "opaqueredirect", status: 0, ok: false, headers: new Headers() } as unknown as Response;
}

function json(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
}

describe("a sign-in gate in front of Mailroom", () => {
  it("asks fetch not to follow redirects, so a gate's redirect cannot turn into a CORS failure", async () => {
    const seen: RequestInit[] = [];
    const fakeFetch = (async (_url: RequestInfo | URL, init?: RequestInit) => {
      seen.push(init ?? {});
      return seen.length === 1 ? json(SESSION) : json({ methodResponses: [], sessionState: "s1" });
    }) as unknown as typeof fetch;
    const client = new JmapClient({ fetch: fakeFetch });
    await client.connect();
    await client.request([]);
    expect(seen.map((i) => i.redirect)).toEqual(["manual", "manual"]);
  });

  it("reads a redirect on the session request as signed out", async () => {
    const client = new JmapClient({ fetch: (async () => opaqueRedirect()) as unknown as typeof fetch });
    await expect(client.connect()).rejects.toBeInstanceOf(JmapAuthError);
  });

  it("reads a redirect on an API request as signed out", async () => {
    let n = 0;
    const fakeFetch = (async () => (n++ === 0 ? json(SESSION) : opaqueRedirect())) as unknown as typeof fetch;
    const client = new JmapClient({ fetch: fakeFetch });
    await client.connect();
    await expect(client.request([])).rejects.toBeInstanceOf(JmapAuthError);
  });

  it("reads a visible 302 the same way (a non-browser fetch that exposes it)", async () => {
    const client = new JmapClient({
      fetch: (async () => new Response(null, { status: 302, headers: { Location: "https://sso.example.com/" } })) as unknown as typeof fetch,
    });
    await expect(client.connect()).rejects.toBeInstanceOf(JmapAuthError);
  });

  // CONTROL: a genuinely failed network request is still "offline", so the test
  // above is not passing because every failure now reads as signed out.
  it("still reads a network failure as a transport error", async () => {
    const client = new JmapClient({
      fetch: (async () => {
        throw new TypeError("Failed to fetch");
      }) as unknown as typeof fetch,
    });
    const err = await client.connect().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(JmapTransportError);
    expect(err).not.toBeInstanceOf(JmapAuthError);
  });
});
