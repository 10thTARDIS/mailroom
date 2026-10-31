// SPDX-License-Identifier: AGPL-3.0-only

// The browser half of the sign-in result (#52): read once, cleaned off the URL
// once, and a sign-in started only toward an https address.

import { beforeEach, describe, expect, it, vi } from "vitest";

import { START_BAD_URL, resetOAuthReturn, startOAuth, takeOAuthReturn } from "./oauthReturn";

function fakes(search: string) {
  const loc = { pathname: "/", search, hash: "#/settings" };
  const hist = { state: { k: 1 }, replaceState: vi.fn() };
  return { loc, hist };
}

beforeEach(() => resetOAuthReturn());

describe("takeOAuthReturn", () => {
  it("reads the result and replaces the URL without the query, keeping path, other params and hash", () => {
    const { loc, hist } = fakes("?keep=1&oauth=connected&provider=google");
    expect(takeOAuthReturn(loc, hist)?.code).toBe("connected");
    expect(hist.replaceState).toHaveBeenCalledWith({ k: 1 }, "", "/?keep=1#/settings");
  });

  it("reads once: a second call neither re-reads nor touches the URL", () => {
    const { loc, hist } = fakes("?oauth=denied");
    takeOAuthReturn(loc, hist);
    const again = fakes("?oauth=expired");
    expect(takeOAuthReturn(again.loc, again.hist)?.code).toBe("denied");
    expect(again.hist.replaceState).not.toHaveBeenCalled();
  });

  it("cleans a query it refuses to show, and leaves a clean URL alone", () => {
    const bad = fakes("?oauth=whatever&provider=x");
    expect(takeOAuthReturn(bad.loc, bad.hist)).toBeNull();
    expect(bad.hist.replaceState).toHaveBeenCalledWith({ k: 1 }, "", "/#/settings");
    resetOAuthReturn();
    const clean = fakes("");
    expect(takeOAuthReturn(clean.loc, clean.hist)).toBeNull();
    expect(clean.hist.replaceState).not.toHaveBeenCalled();
  });
});

describe("startOAuth", () => {
  it("asks the server and goes to the https address it returns", async () => {
    const client = { accountOAuthStart: vi.fn(async () => ({ authorizationUrl: "https://idp.example.com/a?x=1", expiresAt: "" })) };
    const go = vi.fn();
    expect(await startOAuth(client, "google", "ACC", go)).toBeNull();
    expect(client.accountOAuthStart).toHaveBeenCalledWith("google", "ACC");
    expect(go).toHaveBeenCalledWith("https://idp.example.com/a?x=1");
  });

  it("refuses to go anywhere that is not https", async () => {
    const go = vi.fn();
    for (const url of ["http://idp.example.com/", "javascript:alert(1)", ""]) {
      const client = { accountOAuthStart: vi.fn(async () => ({ authorizationUrl: url, expiresAt: "" })) };
      expect(await startOAuth(client, "google", undefined, go)).toBe(START_BAD_URL);
    }
    expect(go).not.toHaveBeenCalled();
  });

  it("returns the server's refusal rather than throwing", async () => {
    const client = { accountOAuthStart: vi.fn(async () => Promise.reject(new Error("that provider is not configured"))) };
    expect(await startOAuth(client, "yahoo", undefined, vi.fn())).toBe("that provider is not configured");
  });
});
