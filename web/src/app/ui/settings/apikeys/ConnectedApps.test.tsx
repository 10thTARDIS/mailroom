// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// CONNECTED APPS AND REGISTERED APPS IN SETTINGS → API KEYS (ADR-050, #34),
// rendered for real against a fake API: grants listed apart from keys with how
// each app was identified, a grant's detail (its connections, the revoke
// wording, no token anywhere), ending one connection, and creating and deleting
// an app registered by hand with the return addresses checked first.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { ApiKey, OAuthClient } from "../../../../core/jmap/types";
import { ApiKeysBody } from "../rows/ApiKeysRow";
import type { ApiKeysApi } from "./api";
import { axeViolations } from "./axe.testdata";
import { APP_MANUAL, APP_UNVERIFIED, APP_VERIFIED, CLIENT, KEY } from "./fixtures.testdata";

const REVOKED_APP: ApiKey = { ...APP_UNVERIFIED, id: "01APPREVOKEDREVOKEDREVOK1", name: "Old Tool", revokedAt: "2026-10-01T00:00:00Z" };
const ROWS = [KEY, APP_VERIFIED, APP_UNVERIFIED, APP_MANUAL, REVOKED_APP];

let host: HTMLDivElement;
let root: Root;
const openKey = vi.fn();
const update = vi.fn(async (..._a: unknown[]) => ({ ok: true as const }));
const revoke = vi.fn(async (..._a: unknown[]) => ({ ok: true as const }));
const endConnection = vi.fn(async (..._a: unknown[]) => ({ ok: true as const }));
const createClient = vi.fn(
  async (name: string, uris: readonly string[]): Promise<{ ok: true; client: OAuthClient } | { ok: false; error: { type: string; description?: string } }> => ({
    ok: true,
    client: { id: "mc_newclient001", name, redirectUris: [...uris], createdAt: "2026-11-04T08:00:00Z" },
  }),
);
const deleteClient = vi.fn(async (..._a: unknown[]) => ({ ok: true as const }));
let clients: OAuthClient[] = [];
const api: ApiKeysApi = {
  capability: () => ({}),
  list: async () => ROWS,
  create: async () => ({ ok: false, error: { type: "x" } }),
  update: (id, patch) => update(id, patch),
  revoke: (id) => revoke(id),
  clients: async () => clients,
  createClient: (name, uris) => createClient(name, uris),
  deleteClient: (id) => deleteClient(id),
  endConnection: (id) => endConnection(id),
  events: async () => ({ list: [], suppressedRefusals: [] }),
  pending: async () => [],
  decide: async () => ({ ok: false, error: { type: "notFound" } }),
  subjects: async () => new Map(),
};
const settle = async (): Promise<void> => {
  // Several turns: a write, the re-read it triggers, and the render of that.
  for (let i = 0; i < 4; i++) await new Promise<void>((r) => setTimeout(r, 0));
};

beforeEach(() => {
  clients = [];
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  vi.clearAllMocks();
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

async function render(apiKeyId: string | null): Promise<void> {
  flushSync(() =>
    root.render(
      <section aria-label="Settings">
        <h3>API keys and AI tools</h3>
        <ApiKeysBody
          reading={{ kind: "loaded", keys: ROWS }}
          reload={() => {}}
          api={api}
          waiting={[]}
          refreshPending={() => {}}
          accounts={[{ id: "A1", name: "Work" }]}
          apiKeyId={apiKeyId}
          openKey={openKey}
          origin="https://mail.example.com"
        />
      </section>,
    ),
  );
  await settle();
}
const text = () => document.body.textContent ?? "";
const button = (label: string) => [...document.body.querySelectorAll("button")].find((b) => b.textContent === label)!;
const apps = () => [...host.querySelectorAll(".apikeys-page > .apikey-app-list > li")].map((li) => li.textContent ?? "");
const keysList = () => [...host.querySelectorAll(".apikey-list:not(.apikey-app-list) > li")].map((li) => li.textContent ?? "");

function type(el: HTMLInputElement | HTMLTextAreaElement, value: string): void {
  const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
  const setter = Object.getOwnPropertyDescriptor(proto, "value")!.set!;
  flushSync(() => {
    setter.call(el, value);
    el.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

describe("the Connected apps list", () => {
  beforeEach(() => render(null));

  it("lists apps apart from keys: no app among the keys, no key among the apps", () => {
    const k = keysList();
    expect(k).toHaveLength(1);
    expect(k[0]).toContain("Helper");
    expect(k[0]).toContain("mr_abcdefg…");
    expect(apps()).toHaveLength(3);
    for (const a of apps()) {
      expect(a).toContain("Connected app");
      expect(a).not.toContain("mr_");
    }
    expect([...host.querySelectorAll("h4")].map((h) => h.textContent)).toContain("Connected apps");
  });

  it("shows a verified app as from its host, with where it returns to, permissions, limits, created and last used", () => {
    const a = apps()[0]!;
    expect(a).toContain("Example Assistant");
    expect(a).toContain("From app.example.com (verified)");
    expect(a).toContain("Returns to app.example.com");
    expect(a).toContain("Read mail, Search mail, Send mail");
    expect(a).toContain("Every account");
    expect(a).toContain("60 calls a minute, 500 a day");
    expect(a).toContain("Active");
    expect(a).toContain("Created ");
    expect(a).toContain("last used ");
  });

  it("says plainly when an app's name is its own word", () => {
    const a = apps()[1]!;
    expect(a).toContain("Mystery Tool");
    expect(a).toContain("Name given by the app, not verified");
    expect(a).not.toContain("verified)");
    expect(a).toContain("Returns to tool.example.com");
    expect(a).toContain("never used");
  });

  it("marks an app the person registered, and a suspended one with its reason", () => {
    const a = apps()[2]!;
    expect(a).toContain("My Script");
    expect(a).toContain("Registered by you");
    expect(a).toContain("Returns to 127.0.0.1");
    expect(a).toContain("Suspended: mass delete: more than 50 deletes in 10 minutes");
  });

  it("keeps a revoked app out of the list, under a disclosure", () => {
    expect(text()).toContain("1 disconnected app");
    expect(host.querySelectorAll(".apikey-app-list")[0]!.textContent).not.toContain("Old Tool");
    expect(host.querySelector("details.apikey-revoked")!.textContent).toContain("Old Tool");
  });

  it("opens an app's detail", () => {
    const open = host.querySelector<HTMLButtonElement>('.apikey-app-list button[aria-label="Details for Example Assistant"]')!;
    flushSync(() => open.click());
    expect(openKey).toHaveBeenCalledWith(APP_VERIFIED.id);
  });

  it("passes axe", async () => {
    expect(await axeViolations()).toEqual([]);
  });
});

describe("an app's detail", () => {
  beforeEach(() => render(APP_VERIFIED.id));

  it("reuses the key's screen: facts, editing, suspend and revoke, activity", () => {
    expect(host.querySelector(".apikey-detail-name")!.textContent).toBe("Example Assistant");
    expect(host.querySelector(".apikey-edit")).not.toBeNull();
    expect(button("Suspend…")).toBeDefined();
    expect(button("Revoke…")).toBeDefined();
    expect([...host.querySelectorAll("h4")].map((h) => h.textContent)).toContain("Activity");
    expect(text()).toContain("From app.example.com (verified)");
    expect(text()).toContain("Returns to");
    expect(text()).toContain("60 calls a minute, 500 a day");
  });

  it("has no token to show or copy, no hint, and no key setup guide", () => {
    expect(text()).toContain("there is no key to show or copy");
    expect(text()).not.toContain("Key starts with");
    expect(text()).not.toContain("mr_");
    expect(text()).not.toContain("Connect an AI tool");
    expect(host.querySelector(".setup-guide")).toBeNull();
    expect(host.querySelector(".copy-control")).toBeNull();
  });

  it("lists the app's connections with when each was made, refreshed and expires", () => {
    const rows = [...host.querySelectorAll(".apikey-family")].map((li) => li.textContent ?? "");
    expect(rows).toHaveLength(2);
    expect(rows[0]).toContain("Connected ");
    expect(rows[0]).toContain("Last refreshed ");
    expect(rows[0]).toContain("Expires ");
    expect(rows[0]).toContain("End this connection");
  });

  it("ends exactly the connection asked for, and not the app", async () => {
    const buttons = [...host.querySelectorAll<HTMLButtonElement>(".apikey-family button")];
    flushSync(() => buttons[1]!.click());
    await settle();
    expect(endConnection).toHaveBeenCalledTimes(1);
    expect(endConnection).toHaveBeenCalledWith("01FAMFAMFAMFAMFAMFAMFAMFA2");
    expect(revoke).not.toHaveBeenCalled();
    expect(host.querySelector('[role="status"]')!.textContent).toBe("Connection ended.");
  });

  it("a failed end says so and ends nothing", async () => {
    endConnection.mockResolvedValueOnce({ ok: false, error: { type: "notFound" } } as never);
    flushSync(() => host.querySelector<HTMLButtonElement>(".apikey-family button")!.click());
    await settle();
    expect(host.querySelector('[role="alert"]')!.textContent).toContain("notFound");
  });

  it("revoking says every connection ends immediately, and names the app", async () => {
    flushSync(() => button("Revoke…").click());
    const dlg = document.body.querySelector('[role="dialog"]')!;
    expect(document.getElementById(dlg.getAttribute("aria-labelledby")!)?.textContent).toBe('Revoke "Example Assistant"?');
    expect(dlg.textContent).toContain("Every connection of this app ends immediately");
    expect(dlg.textContent).toContain("can't be undone");
    expect(dlg.textContent).not.toContain("this key");
    expect(await axeViolations()).toEqual([]);
    flushSync(() => button("Revoke app").click());
    await settle();
    expect(revoke).toHaveBeenCalledWith(APP_VERIFIED.id);
  });

  it("suspending uses the app's words", () => {
    flushSync(() => button("Suspend…").click());
    expect(document.body.querySelector('[role="dialog"]')!.textContent).toContain("Every call from this app will be refused");
  });

  it("passes axe", async () => {
    expect(await axeViolations()).toEqual([]);
  });
});

describe("a suspended app and an app with no connections", () => {
  it("says what to do when there are no connections", async () => {
    await render(APP_UNVERIFIED.id);
    expect(text()).toContain("No active connections");
    expect(host.querySelector(".apikey-family")).toBeNull();
  });

  it("offers reactivation in the app's words", async () => {
    await render(APP_MANUAL.id);
    flushSync(() => button("Reactivate…").click());
    expect(document.body.querySelector('[role="dialog"]')!.textContent).toContain("Reactivating lets the app make calls again");
  });
});

describe("apps you register yourself", () => {
  const nameInput = () => host.querySelector<HTMLInputElement>(".apikey-client-form input[type=text]")!;
  const uris = () => host.querySelector<HTMLTextAreaElement>(".apikey-client-form textarea")!;

  it("explains when it is needed", async () => {
    await render(null);
    expect(text()).toContain("only if a tool cannot identify itself");
    expect(text()).toContain("You haven't registered any apps.");
  });

  it("lists existing ones with the client id, copyable, and their return addresses", async () => {
    clients = [CLIENT];
    await render(null);
    const li = host.querySelector(".apikey-client")!;
    expect(li.textContent).toContain("My Script");
    expect(li.textContent).toContain("mc_manual00001");
    expect(li.textContent).toContain("http://127.0.0.1/callback, https://script.example.com/cb");
    expect(li.textContent).toContain("Copy client ID");
  });

  it("creates one and then shows its client id, which stays visible in the list", async () => {
    await render(null);
    type(nameInput(), "  My Script ");
    type(uris(), "https://script.example.com/cb\nhttp://127.0.0.1/callback");
    clients = [{ id: "mc_newclient001", name: "My Script", redirectUris: ["https://script.example.com/cb"], createdAt: "2026-11-04T08:00:00Z" }];
    flushSync(() => button("Register app").click());
    await settle();
    await settle();
    expect(createClient).toHaveBeenCalledWith("My Script", ["https://script.example.com/cb", "http://127.0.0.1/callback"]);
    const banner = host.querySelector(".apikey-client-created")!;
    expect(banner.textContent).toContain('Registered "My Script"');
    expect(banner.textContent).toContain("mc_newclient001");
    expect(banner.textContent).toContain("not a secret");
    expect(host.querySelector(".apikey-client-list")!.textContent).toContain("mc_newclient001");
    expect(nameInput().value).toBe("");
    expect(await axeViolations()).toEqual([]);
  });

  it("refuses a bad return address before sending anything, naming it", async () => {
    await render(null);
    type(nameInput(), "My Script");
    type(uris(), "https://script.example.com/cb\nhttp://script.example.com/cb");
    flushSync(() => button("Register app").click());
    await settle();
    expect(createClient).not.toHaveBeenCalled();
    expect(host.querySelector('.apikey-client-form [role="alert"]')!.textContent).toBe(
      "http://script.example.com/cb must start with https://, or with http:// for localhost, 127.0.0.1 or [::1].",
    );
    expect(await axeViolations()).toEqual([]);
  });

  it("refuses an empty name and no addresses", async () => {
    await render(null);
    flushSync(() => button("Register app").click());
    expect(host.querySelector('.apikey-client-form [role="alert"]')!.textContent).toContain("Give the app a name");
    type(nameInput(), "App");
    flushSync(() => button("Register app").click());
    expect(host.querySelector('.apikey-client-form [role="alert"]')!.textContent).toContain("Give at least one return address");
    expect(createClient).not.toHaveBeenCalled();
  });

  it("shows the server's refusal in its words", async () => {
    createClient.mockResolvedValueOnce({ ok: false, error: { type: "forbidden", description: "you already have 20 registered apps, the most allowed" } });
    await render(null);
    type(nameInput(), "App");
    type(uris(), "https://app.example.com/cb");
    flushSync(() => button("Register app").click());
    await settle();
    expect(host.querySelector('.apikey-client-form [role="alert"]')!.textContent).toBe("you already have 20 registered apps, the most allowed");
  });

  it("deletes one after asking, and says what that does and does not do", async () => {
    clients = [CLIENT];
    await render(null);
    flushSync(() => host.querySelector<HTMLButtonElement>('button[aria-label="Delete My Script"]')!.click());
    const dlg = document.body.querySelector('[role="dialog"]')!;
    expect(document.getElementById(dlg.getAttribute("aria-labelledby")!)?.textContent).toBe('Delete "My Script"?');
    expect(dlg.textContent).toContain("revoke it under Connected apps");
    expect(deleteClient).not.toHaveBeenCalled();
    clients = [];
    flushSync(() => button("Delete app").click());
    await settle();
    await settle();
    expect(deleteClient).toHaveBeenCalledWith("mc_manual00001");
    expect(host.querySelector(".apikey-client")).toBeNull();
    expect(text()).toContain("Deleted.");
  });
});
