// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// SETTINGS → API KEYS (#49), rendered for real against a fake API: the list's
// facts (scope summary with presets named, restriction, state with reason,
// last used, usage against the budget), a key's detail (revoke names the key,
// reactivate says what tripped it), and the activity (plain words, suppressed
// refusals as "+N more", anomalies highlighted). Both screens pass axe.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { ApiKey, ApiKeyEvent } from "../../../../core/jmap/types";
import type { ApiKeysApi } from "../apikeys/api";
import { axeViolations } from "../apikeys/axe.testdata";
import { KEY } from "../apikeys/fixtures.testdata";
import { ApiKeysBody, apiKeysFacts } from "./ApiKeysRow";

const SUSPENDED: ApiKey = {
  ...KEY,
  id: "01SUSPENDEDSUSPENDEDSUSPEN",
  name: "Night bot",
  scopes: [...KEY.scopes, "mail:move", "labels:write", "mail:draft", "mail:delete"],
  accountIds: ["A1"],
  confirmScopes: ["mail:delete"],
  suspendedAt: "2026-10-23T02:00:00Z",
  suspendedReason: "mass delete: more than 50 deletes in 10 minutes",
  lastUsedAt: "2026-10-23T02:00:00Z",
};
const REVOKED: ApiKey = { ...KEY, id: "01REVOKEDREVOKEDREVOKEDREV", name: "Old", revokedAt: "2026-10-01T00:00:00Z" };

const EVENTS: ApiKeyEvent[] = [
  {
    id: "E3", keyId: SUSPENDED.id, at: "2026-10-23T02:00:00Z", method: "Email/set", outcome: "anomaly", cost: 0, deletes: 0, sends: 0,
    summary: { threshold: 50, windowMinutes: 10, suspended: true }, clientIp: null, pendingActionId: null, errorType: null, anomaly: "mass_delete",
  },
  {
    id: "E2", keyId: SUSPENDED.id, at: "2026-10-23T01:59:00Z", method: "Email/query", outcome: "ok", cost: 1, deletes: 0, sends: 0,
    summary: null, clientIp: null, pendingActionId: null, errorType: null, anomaly: null,
  },
];

let host: HTMLDivElement;
let root: Root;
const openKey = vi.fn();
const update = vi.fn(async (..._a: unknown[]) => ({ ok: true as const }));
const revoke = vi.fn(async (..._a: unknown[]) => ({ ok: true as const }));
const api: ApiKeysApi = {
  capability: () => ({ massDeleteThreshold: 50, massDeleteWindowSeconds: 600 }),
  list: async () => [KEY, SUSPENDED, REVOKED],
  create: async () => ({ ok: false, error: { type: "x" } }),
  update: (id, patch) => update(id, patch),
  revoke: (id) => revoke(id),
  events: async () => ({
    list: EVENTS,
    suppressedRefusals: [{ keyId: SUSPENDED.id, minute: "2026-10-23T01:58:00Z", outcome: "denied", method: "Email/set", reason: "scope", count: 12 }],
  }),
  pending: async () => [],
  decide: async () => ({ ok: false, error: { type: "notFound" } }),
  subjects: async () => new Map(),
};
const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 0));

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  vi.clearAllMocks();
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

function render(apiKeyId: string | null): void {
  flushSync(() =>
    root.render(
      <section aria-label="Settings">
        <h3>API keys and AI tools</h3>
        <ApiKeysBody
          reading={{ kind: "loaded", keys: [KEY, SUSPENDED, REVOKED] }}
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
}
const text = () => document.body.textContent ?? "";
const button = (label: string) => [...document.body.querySelectorAll("button")].find((b) => b.textContent === label)!;

describe("the state line", () => {
  it("counts active and suspended keys and what is waiting, and says when it cannot ask", () => {
    expect(apiKeysFacts({ kind: "loaded", keys: [KEY, SUSPENDED, REVOKED] }, [])).toEqual([
      { text: "1 active" },
      { text: "1 suspended", tone: "warn" },
    ]);
    expect(apiKeysFacts({ kind: "asking" }, null)).toEqual([]);
    expect(apiKeysFacts({ kind: "unavailable" }, null)).toEqual([{ text: "not available on this server" }]);
  });
});

describe("the list", () => {
  beforeEach(() => render(null));

  it("states each key's access, restriction, state, last use and usage", () => {
    const items = [...host.querySelectorAll(".apikey-list > li")].map((li) => li.textContent ?? "");
    expect(items[0]).toContain("Helper");
    expect(items[0]).toContain("Read only");
    expect(items[0]).toContain("Every account");
    expect(items[0]).toContain("Active");
    expect(items[0]).toContain("never used");
    expect(items[0]).toContain("12 of 500 used in the last 24 hours");
    expect(items[1]).toContain("Assistant + Delete mail (to Trash)");
    expect(items[1]).toContain("Only Work");
    expect(items[1]).toContain("Suspended: mass delete: more than 50 deletes in 10 minutes");
    expect(text()).toContain("1 revoked key");
  });

  it("puts approvals first, then keys, then Create, then the setup guide", () => {
    const headings = [...host.querySelectorAll("h4")].map((h) => h.textContent);
    expect(headings).toEqual(["Waiting for your approval", "Your keys", "Connect an AI tool"]);
    expect(text()).toContain("https://mail.example.com/mcp");
    expect(text()).toContain("Authorization: Bearer mr_your_key_here");
  });

  it("opens a key's detail", () => {
    flushSync(() => button("Details").click());
    expect(openKey).toHaveBeenCalledWith(KEY.id);
  });

  it("passes axe", async () => {
    expect(await axeViolations()).toEqual([]);
  });
});

describe("a key's detail", () => {
  beforeEach(async () => {
    render(SUSPENDED.id);
    await settle();
  });

  it("shows the activity in words, with suppressed refusals and the anomaly highlighted", () => {
    const rows = [...host.querySelectorAll(".apikey-event")];
    expect(rows.map((r) => r.textContent)).toEqual([
      expect.stringContaining("Unusual activity: tried to delete more than 50 messages in 10 minutes. The key was suspended."),
      expect.stringContaining("Email/query Allowed"),
      expect.stringContaining("+12 more refused calls (Email/set: the key doesn't have permission)"),
    ]);
    expect(rows[0]!.className).toContain("is-warn");
    expect(rows[2]!.className).toContain("is-suppressed");
  });

  it("reactivating says what tripped it, then clears the suspension", async () => {
    flushSync(() => button("Reactivate…").click());
    const dlg = document.body.querySelector('[role="dialog"]')!;
    expect(dlg.textContent).toContain("It was suspended");
    expect(dlg.textContent).toContain("mass delete: more than 50 deletes in 10 minutes");
    expect(dlg.textContent).toContain("tried to delete more than 50 messages in 10 minutes");
    expect(await axeViolations()).toEqual([]);
    flushSync(() => button("Reactivate").click());
    await settle();
    expect(update).toHaveBeenCalledWith(SUSPENDED.id, { suspended: false });
  });

  it("revoking asks with the key's name", async () => {
    flushSync(() => button("Revoke…").click());
    const dlg = document.body.querySelector('[role="dialog"]')!;
    expect(document.getElementById(dlg.getAttribute("aria-labelledby")!)?.textContent).toBe('Revoke "Night bot"?');
    expect(dlg.textContent).toContain("can't be undone");
    expect(document.activeElement?.textContent).toBe("Cancel");
    flushSync(() => button("Revoke key").click());
    await settle();
    expect(revoke).toHaveBeenCalledWith(SUSPENDED.id);
  });

  it("saves only what changed", async () => {
    const rate = [...host.querySelectorAll<HTMLInputElement>(".apikey-edit input[type=text]")][1]!;
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
    flushSync(() => {
      setter.call(rate, "30");
      rate.dispatchEvent(new Event("input", { bubbles: true }));
    });
    flushSync(() => button("Save").click());
    await settle();
    expect(update).toHaveBeenCalledWith(SUSPENDED.id, { ratePerMinute: 30 });
  });

  it("passes axe", async () => {
    expect(await axeViolations()).toEqual([]);
  });
});
