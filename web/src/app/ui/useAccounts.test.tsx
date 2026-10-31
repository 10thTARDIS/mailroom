// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// WHICH ACCOUNTS THE LIST HOLDS (#662).
//
// The list was derived from synced Identities alone, so an account added with
// `--ingest-only` (no identity) never appeared and its controls were
// unreachable. These mount the hook with a stubbed store and client and read what
// it returns — and the Settings list row, which is where the note is printed.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { JSX } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

let identities: Record<string, { accountId: string; email: string }> = {};
let accountRecords: { id: string; name: string; syncEnabled: boolean }[] = [];
const client = {
  // No sign-in provider configured: the Connect buttons (#52) render nothing.
  oauthProviders: () => [] as string[],
  request: vi.fn(async () => [
    { name: "Account/get", value: { list: accountRecords } },
    { name: "Account/getHealth", value: { list: [] } },
  ]),
};

vi.mock("../state", () => ({
  useApp: () => ({ client }),
  useMailState: () => ({ accountId: "A1", identities }),
}));
vi.mock("./settings/nav", async (orig) => ({
  ...(await orig<object>()),
  useKeepScope: () => () => {},
}));
vi.mock("./AddAccountWizard", () => ({ AddAccountWizard: () => null }));

import { forgetAccounts, useAccounts } from "./useAccounts";
import type { AccountsSnapshot } from "./useAccounts";
import { AccountsBody } from "./settings/rows/AccountsRow";

let root: Root;
let host: HTMLElement;
let snap: AccountsSnapshot | null = null;

function Probe(): JSX.Element {
  snap = useAccounts();
  return <></>;
}

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}

async function mount(el: JSX.Element): Promise<void> {
  flushSync(() => root.render(el));
  await settle();
  flushSync(() => root.render(el));
}

beforeEach(() => {
  forgetAccounts();
  snap = null;
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

describe("useAccounts account set", () => {
  it("lists an account that has no identity, flagged as having no sending address", async () => {
    identities = { i1: { accountId: "A1", email: "me@example.com" } };
    accountRecords = [
      { id: "A1", name: "Main", syncEnabled: true },
      { id: "A2", name: "Archive", syncEnabled: true },
    ];
    await mount(<Probe />);
    expect(snap!.accounts.map((a) => [a.id, a.hasIdentity])).toEqual([
      ["A1", true],
      ["A2", false],
    ]);
    expect(snap!.byId.get("A2")?.name).toBe("Archive");
  });

  it("is unchanged for accounts that have identities: same rows, same order", async () => {
    identities = {
      i1: { accountId: "A2", email: "b@example.com" },
      i2: { accountId: "A1", email: "*@example.com" },
      i3: { accountId: "A1", email: "a@example.com" },
    };
    accountRecords = [
      { id: "A2", name: "Second", syncEnabled: true },
      { id: "A1", name: "", syncEnabled: true },
    ];
    await mount(<Probe />);
    expect(snap!.accounts.map((a) => [a.id, a.name, a.hasIdentity])).toEqual([
      ["A1", "a@example.com", true],
      ["A2", "Second", true],
    ]);
  });
});

describe("useAccounts before Account/get answers", () => {
  it("lists the identity-derived accounts only, as it always did", async () => {
    identities = {
      i1: { accountId: "A2", email: "b@example.com" },
      i2: { accountId: "A1", email: "a@example.com" },
    };
    accountRecords = [{ id: "A3", name: "Archive", syncEnabled: true }];
    // Held open for the whole test, then released so it cannot starve the next
    // test's fetch through the module-level in-flight promise.
    let release: () => void = () => {};
    client.request.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          release = () => resolve([]);
        }),
    );
    await mount(<Probe />);
    expect(snap!.loaded).toBe(false);
    expect(snap!.accounts.map((a) => a.id)).toEqual(["A1", "A2"]);
    release();
    await settle();
  });
});

describe("Settings Accounts list", () => {
  it("shows the note on an account with no identity, and only on that one", async () => {
    identities = { i1: { accountId: "A1", email: "me@example.com" } };
    accountRecords = [
      { id: "A1", name: "Main", syncEnabled: true },
      { id: "A2", name: "Archive", syncEnabled: true },
    ];
    await mount(<AccountsBody accountId={null} />);
    const items = [...host.querySelectorAll("li.setting-account")];
    expect(items).toHaveLength(2);
    expect(items[0]!.textContent).not.toContain("No sending address yet.");
    expect(items[1]!.textContent).toContain("Archive");
    expect(items[1]!.textContent).toContain("No sending address yet.");
  });
});
