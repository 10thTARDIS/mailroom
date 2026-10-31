// SPDX-License-Identifier: AGPL-3.0-only

// The sign-in result in the URL, and which accounts need reconnecting (#52).

import { describe, expect, it } from "vitest";

import type { Account } from "./jmap/types";
import {
  OAUTH_RESULT_CODES,
  accountsNeedingReconnect,
  canDisconnect,
  canReconnect,
  oauthResultTone,
  parseOAuthReturn,
  reconnectDismissKey,
  revocationKind,
  searchWithoutOAuth,
} from "./oauth";

const ACCT = "01JB00000000000000000000A1";

function acct(id: string, authState: Account["authState"], authKind: Account["authKind"] = "oauth2"): Account {
  return {
    id,
    name: id,
    providerProfile: "microsoft",
    imapHost: "",
    imapPort: 993,
    smtpHost: "",
    smtpPort: 587,
    syncEnabled: true,
    writebackEnabled: true,
    color: null,
    authKind,
    authState,
    authProvider: "microsoft",
  };
}

describe("parseOAuthReturn", () => {
  it("reads every one of the eighteen codes, with the provider and account", () => {
    expect(OAUTH_RESULT_CODES).toHaveLength(18);
    for (const code of OAUTH_RESULT_CODES) {
      expect(parseOAuthReturn(`?oauth=${code}&provider=google&account=${ACCT}`)).toEqual({
        code,
        provider: "google",
        account: ACCT,
      });
    }
  });

  it("ignores a code this server never sends, so a crafted link cannot choose the words", () => {
    expect(parseOAuthReturn("?oauth=pwned&provider=google")).toBeNull();
    expect(parseOAuthReturn("?provider=google")).toBeNull();
    expect(parseOAuthReturn("")).toBeNull();
  });

  it("drops a provider or account it does not recognise, keeping the result", () => {
    expect(parseOAuthReturn("?oauth=denied&provider=yahoo&account=../../x")).toEqual({
      code: "denied",
      provider: null,
      account: null,
    });
  });
});

describe("searchWithoutOAuth", () => {
  it("removes exactly the callback's three parameters", () => {
    expect(searchWithoutOAuth(`?oauth=connected&provider=google&account=${ACCT}`)).toBe("");
    expect(searchWithoutOAuth("?keep=1&oauth=denied&provider=microsoft")).toBe("?keep=1");
    expect(searchWithoutOAuth("?keep=1")).toBe("?keep=1");
  });
});

describe("oauthResultTone", () => {
  it("is ok for a connect, info for a duplicate, and an error for everything else", () => {
    expect(oauthResultTone("connected")).toBe("ok");
    expect(oauthResultTone("reconnected")).toBe("ok");
    expect(oauthResultTone("already_connected")).toBe("info");
    const errors = OAUTH_RESULT_CODES.filter((c) => oauthResultTone(c) === "error");
    expect(errors).toHaveLength(15);
  });
});

describe("which accounts the reconnect controls are for", () => {
  it("offers Reconnect for needs_reauth and disconnected, never for operator_error", () => {
    expect(canReconnect(acct("a", "needs_reauth"))).toBe(true);
    expect(canReconnect(acct("a", "disconnected"))).toBe(true);
    expect(canReconnect(acct("a", "operator_error"))).toBe(false);
    expect(canReconnect(acct("a", "active"))).toBe(false);
    expect(canReconnect(acct("a", null, "password"))).toBe(false);
  });

  it("offers Disconnect while a grant exists", () => {
    expect(canDisconnect(acct("a", "active"))).toBe(true);
    expect(canDisconnect(acct("a", "needs_reauth"))).toBe(true);
    expect(canDisconnect(acct("a", "operator_error"))).toBe(true);
    expect(canDisconnect(acct("a", "disconnected"))).toBe(false);
    expect(canDisconnect(acct("a", null, "password"))).toBe(false);
  });

  it("lists only needs_reauth accounts for the banner, in id order", () => {
    const list = [acct("c", "needs_reauth"), acct("a", "operator_error"), acct("b", "needs_reauth"), acct("d", "active")];
    expect(accountsNeedingReconnect(list).map((a) => a.id)).toEqual(["b", "c"]);
  });

  it("keys a dismissal by the set of accounts, whatever their order", () => {
    expect(reconnectDismissKey([{ id: "b" }, { id: "a" }])).toBe(reconnectDismissKey([{ id: "a" }, { id: "b" }]));
    expect(reconnectDismissKey([{ id: "a" }])).not.toBe(reconnectDismissKey([{ id: "a" }, { id: "b" }]));
  });

  it("reads anything but revoked as access the user must remove themselves", () => {
    expect(revocationKind("revoked")).toBe("revoked");
    for (const r of ["failed", "unsupported", "not_configured", ""]) expect(revocationKind(r)).toBe("manual");
  });
});
