// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// SETTINGS → ACTIVITY (ADR-053), rendered for real against a fake API: the list,
// the source and date filters (and what they ask the server for), Load more
// (appending, never repeating), the empty states, the honest bound, the links
// that vanish with their object, and axe.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";
import { useState } from "react";
import type { JSX } from "react";

import type { ActivityEntry, ActivityPage } from "../../../../core/jmap/types";
import type { ActivityApi, ActivityQuery } from "../activity/api";
import { dateRange, entryLink, entryWords, rangeIsBackwards } from "../activity/model";
import { axeViolations } from "../apikeys/axe.testdata";
import { ACTIVITY_PAGE, ActivityBody, NO_FILTERS, activityFacts, useActivity } from "./ActivityRow";
import type { ActivityFilters } from "./ActivityRow";

const MSG = "01JMESSAGE00000000000000AA";
const KEY = "01JKEY0000000000000000000A";

function entry(over: Partial<ActivityEntry> & Pick<ActivityEntry, "id" | "kind" | "source">): ActivityEntry {
  return {
    at: "2026-11-02T10:00:00Z",
    outcome: null,
    keyName: null,
    method: null,
    reason: null,
    purpose: null,
    fingerprint: null,
    device: null,
    count: null,
    object: null,
    ...over,
  };
}

const DECRYPT = entry({
  id: "decryption:1",
  kind: "decryption",
  source: "decryption",
  outcome: "decrypted",
  purpose: "body",
  fingerprint: "AABBCCDD11223344",
  object: { type: "Email", id: MSG, exists: true },
});
const DECRYPT_GONE = entry({
  id: "decryption:2",
  kind: "decryption",
  source: "decryption",
  outcome: "decrypted",
  purpose: "attachment",
  fingerprint: "AABBCCDD11223344",
  object: { type: "Email", id: "01JGONE0000000000000000000", exists: false },
});
const REFUSED = entry({
  id: "ai_tool:3",
  kind: "ai_tool",
  source: "ai_tool",
  outcome: "denied",
  reason: "scope",
  keyName: "Mail helper",
  method: "Email/set",
  object: { type: "ApiKey", id: KEY, exists: true },
});
const PURGE = entry({
  id: "purge:4",
  kind: "purge",
  source: "purge",
  object: { type: "Email", id: "01JPURGED000000000000000AA", exists: false },
});
const SIGNIN = entry({ id: "sign_in:5", kind: "sign_in", source: "session", device: "Firefox on Linux" });

let host: HTMLDivElement;
let root: Root;
const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 0));
const calls: ActivityQuery[] = [];

function page(list: ActivityEntry[], next: string | null = null, covers: ActivityPage["covers"] = {}): ActivityPage {
  return { list, next, limit: ACTIVITY_PAGE, covers };
}

function Harness({ api, ready = true }: { api: ActivityApi; ready?: boolean }): JSX.Element {
  const [filters, setFilters] = useState<ActivityFilters>(NO_FILTERS);
  const { reading, loadMore } = useActivity(api, ready, filters);
  return (
    <section aria-label="Settings">
      <h3>Activity</h3>
      <p id="facts">{activityFacts(reading).map((f) => f.text)}</p>
      <ActivityBody reading={reading} loadMore={loadMore} filters={filters} setFilters={setFilters} />
    </section>
  );
}

function fakeApi(handler: (q: ActivityQuery) => ActivityPage | null, cap: Record<string, unknown> | null = {}): ActivityApi {
  return {
    capability: () => cap,
    list: async (q) => {
      calls.push(q);
      return handler(q);
    },
  };
}

async function mount(api: ActivityApi, ready = true): Promise<void> {
  flushSync(() => root.render(<Harness api={api} ready={ready} />));
  await settle();
  await settle();
}

const text = () => document.body.textContent ?? "";
const button = (label: string) => [...document.body.querySelectorAll("button")].find((b) => b.textContent === label);
const items = () => [...document.body.querySelectorAll(".activity-list > li")].map((li) => li.textContent ?? "");

function check(label: RegExp): HTMLInputElement {
  const l = [...document.body.querySelectorAll("label")].find((x) => label.test(x.textContent ?? ""));
  return l!.querySelector("input")!;
}

async function change(input: HTMLInputElement, value: string): Promise<void> {
  const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
  flushSync(() => {
    set.call(input, value);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
  await settle();
  await settle();
}

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  calls.length = 0;
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
  vi.restoreAllMocks();
});

describe("the states", () => {
  it("says the server does not offer it, and asks nothing", async () => {
    await mount(fakeApi(() => page([]), null));
    expect(text()).toContain("The activity list is not available on this server.");
    expect(document.getElementById("facts")!.textContent).toBe("not available on this server");
    expect(calls).toEqual([]);
  });

  it("asks nothing until the session has landed", async () => {
    await mount(fakeApi(() => page([])), false);
    expect(calls).toEqual([]);
    expect(text()).toContain("Reading your activity");
  });

  it("says a failed read failed, rather than that nothing happened", async () => {
    await mount(fakeApi(() => null));
    expect(text()).toContain("Could not read your activity.");
    expect(text()).not.toContain("Nothing has been recorded yet");
  });

  it("explains an empty list, and an empty filtered list differently", async () => {
    await mount(fakeApi(() => page([])));
    expect(text()).toContain("Nothing has been recorded yet.");
    await change(check(/Deletes/), "");
    flushSync(() => check(/Deletes/).click());
    await settle();
    await settle();
    expect(text()).toContain("Nothing matches these filters.");
  });
});

describe("the list", () => {
  it("shows one line per entry, with the words, the kind and the time", async () => {
    await mount(fakeApi(() => page([REFUSED, DECRYPT, PURGE, SIGNIN])));
    const rows = items();
    expect(rows).toHaveLength(4);
    expect(rows[0]).toContain("AI tools");
    expect(rows[0]).toContain("AI tool Mail helper was refused: it does not have permission.");
    expect(rows[1]).toContain("An encrypted message was decrypted with your key ending 11223344.");
    expect(rows[2]).toContain("A message was permanently deleted.");
    expect(rows[3]).toContain("Signed in on Firefox on Linux.");
    expect(rows[0]).toContain("2 Nov 2026");
  });

  it("links an object that exists, and drops the link when it is gone, keeping the entry", async () => {
    await mount(fakeApi(() => page([DECRYPT, DECRYPT_GONE, REFUSED, PURGE])));
    const rows = [...document.body.querySelectorAll(".activity-list > li")];
    expect(rows[0]!.querySelector("a")!.getAttribute("href")).toContain(MSG);
    expect(rows[0]!.querySelector("a")!.textContent).toBe("Open message");
    expect(rows[1]!.querySelector("a")).toBeNull();
    expect(rows[1]!.textContent).toContain("attachment");
    expect(rows[2]!.querySelector("a")!.getAttribute("href")).toContain(KEY);
    expect(rows[3]!.querySelector("a")).toBeNull();
  });

  it("names a key that no longer exists with a fixed phrase, and never shows message content", async () => {
    const e = entry({ id: "ai_tool:9", kind: "ai_tool", source: "ai_tool", outcome: "anomaly", keyName: null });
    await mount(fakeApi(() => page([e])));
    expect(items()[0]).toContain("AI tool a key that no longer exists was flagged for unusual activity.");
    expect(text()).not.toMatch(/subject/i);
  });

  it("isolates a name a person typed (bidi controls stripped, shown as its own text)", async () => {
    const e = entry({ id: "ai_tool:8", kind: "ai_tool", source: "ai_tool", keyName: "evil‮name" });
    await mount(fakeApi(() => page([e])));
    const bdi = document.body.querySelector(".activity-list bdi")!;
    expect(bdi.textContent).toBe("evilname");
    expect(items()[0]).not.toContain("‮");
  });

  it("passes axe", async () => {
    await mount(fakeApi(() => page([REFUSED, DECRYPT, DECRYPT_GONE, PURGE, SIGNIN], "cursor-1")));
    expect(await axeViolations()).toEqual([]);
  });
});

describe("the bound", () => {
  it("says it shows the most recent N and that there are older ones, never a total", async () => {
    await mount(fakeApi(() => page([REFUSED, DECRYPT], "c1")));
    expect(text()).toContain("Showing the 2 most recent entries. There are older ones.");
    expect(text()).not.toMatch(/of \d+|total/i);
    expect(button("Load more")).toBeDefined();
  });

  it("says that is everything at the end, and offers no Load more", async () => {
    await mount(fakeApi(() => page([REFUSED, DECRYPT], null)));
    expect(text()).toContain("Showing 2 entries. That is everything recorded for these filters.");
    expect(button("Load more")).toBeUndefined();
  });

  it("says how far back sign-ins go when sessions are shown", async () => {
    await mount(fakeApi(() => page([SIGNIN], null, { session: "2026-10-30T09:00:00Z" })));
    expect(text()).toContain("Sign-in history goes back to 30 Oct 2026");
    await act_toggle(/Sign-ins/);
    expect(text()).not.toContain("Sign-in history goes back");
  });
});

async function act_toggle(label: RegExp): Promise<void> {
  flushSync(() => check(label).click());
  await settle();
  await settle();
}

describe("Load more", () => {
  it("asks with the cursor, appends, and never shows an id twice", async () => {
    const api = fakeApi((q) => (q.before ? page([DECRYPT, PURGE], null) : page([REFUSED, DECRYPT], "cursor-1")));
    await mount(api);
    expect(items()).toHaveLength(2);
    flushSync(() => button("Load more")!.click());
    await settle();
    await settle();
    expect(calls[1]).toMatchObject({ before: "cursor-1", limit: ACTIVITY_PAGE });
    // DECRYPT came back on page two: shown once.
    expect(items()).toHaveLength(3);
    expect(button("Load more")).toBeUndefined();
    expect(text()).toContain("Showing 3 entries. That is everything");
  });

  it("keeps what is shown when a further page fails, and says so", async () => {
    const api = fakeApi((q) => (q.before ? null : page([REFUSED], "c")));
    await mount(api);
    flushSync(() => button("Load more")!.click());
    await settle();
    await settle();
    expect(items()).toHaveLength(1);
    expect(text()).toContain("Could not load more. What is shown is unchanged.");
    expect(button("Load more")).toBeDefined();
  });
});

describe("the filters", () => {
  it("asks for no source filter when all are on, and for the chosen ones otherwise", async () => {
    await mount(fakeApi(() => page([REFUSED])));
    expect(calls[0]!.sources).toBeUndefined();
    await act_toggle(/Decryptions/);
    await act_toggle(/Deletes/);
    expect(calls.at(-1)!.sources).toEqual(["ai_tool", "session", "bulk"]);
  });

  it("starts again from the first page when a filter changes (no stale cursor)", async () => {
    await mount(fakeApi((q) => page([REFUSED], q.before ? null : "c1")));
    flushSync(() => button("Load more")!.click());
    await settle();
    await settle();
    await act_toggle(/Deletes/);
    expect(calls.at(-1)!.before).toBeUndefined();
  });

  it("turns the dates into a UTC range whose last day is included whole", async () => {
    await mount(fakeApi(() => page([REFUSED])));
    const dates = [...document.body.querySelectorAll<HTMLInputElement>("input[type=date]")];
    await change(dates[0]!, "2026-10-01");
    await change(dates[1]!, "2026-10-31");
    expect(calls.at(-1)).toMatchObject({ since: "2026-10-01T00:00:00Z", until: "2026-11-01T00:00:00Z" });
    flushSync(() => button("Clear dates")!.click());
    await settle();
    await settle();
    expect(calls.at(-1)!.since).toBeUndefined();
    expect(calls.at(-1)!.until).toBeUndefined();
  });

  it("asks nothing for backwards dates or for no source, and says why", async () => {
    await mount(fakeApi(() => page([REFUSED])));
    const dates = [...document.body.querySelectorAll<HTMLInputElement>("input[type=date]")];
    await change(dates[0]!, "2026-10-31");
    const before = calls.length;
    await change(dates[1]!, "2026-10-01");
    expect(calls.length).toBe(before);
    expect(text()).toContain("The first date is after the last date");
    await change(dates[0]!, "");
    for (const l of [/AI tools/, /Decryptions/, /Deletes/, /Sign-ins/, /Bulk changes/]) await act_toggle(l);
    expect(text()).toContain("Choose at least one kind of activity to show.");
  });

  it("the filter form passes axe", async () => {
    await mount(fakeApi(() => page([])));
    expect(await axeViolations()).toEqual([]);
  });
});

describe("the model", () => {
  it("builds the UTC range and spots a backwards one", () => {
    expect(dateRange("", "")).toEqual({});
    expect(dateRange("2026-02-28", "2026-02-28")).toEqual({ since: "2026-02-28T00:00:00Z", until: "2026-03-01T00:00:00Z" });
    expect(dateRange("junk", "")).toEqual({});
    expect(rangeIsBackwards("2026-02-02", "2026-02-01")).toBe(true);
    expect(rangeIsBackwards("2026-02-01", "")).toBe(false);
  });

  it("links by object type, and only while the object exists", () => {
    expect(entryLink(DECRYPT)!.route).toEqual({ page: "all", m: MSG });
    expect(entryLink(REFUSED)!.route).toEqual({ page: "settings", section: "api-keys", apiKeyId: KEY });
    expect(entryLink(DECRYPT_GONE)).toBeNull();
    expect(entryLink(SIGNIN)).toBeNull();
  });

  it("describes a kind it does not know rather than showing nothing", () => {
    const w = entryWords({ ...SIGNIN, kind: "future_kind" as never });
    expect(w.template).toContain("cannot describe");
  });

  it("counts a bulk action in words", () => {
    const rule = entry({ id: "rule_apply:6", kind: "rule_apply", source: "bulk", outcome: "rule.apply", keyName: "Newsletters", count: 1 });
    expect(entryWords(rule).template).toBe("Rule {name} was applied to 1 existing message.");
    expect(entryWords({ ...rule, count: 12, outcome: "rule.apply_undo" }).template).toBe("Rule {name} was undone for 12 messages.");
  });
});
