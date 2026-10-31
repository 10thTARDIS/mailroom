// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The search box (#724) in a real DOM: contact and area suggestions through the
// ARIA combobox, the debounced live search, and the search-options form —
// opened, pre-filled from the query, submitted as a query string, closed with
// focus put back.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useState, useSyncExternalStore } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { Area, Contact, Email } from "../../core/jmap/types";
import { MailStore } from "../../core/store";

const store = new MailStore(null);
const contactAutocomplete = vi.fn(async (_text: string): Promise<Contact[]> => []);

vi.mock("../state", () => ({
  useApp: () => ({
    actions: { saveSavedSearch: async () => true },
    client: { contactAutocomplete },
    pushToast: () => 1,
  }),
  useMailState: () => useSyncExternalStore(store.subscribe, store.getState),
}));
vi.mock("../searchHistory", () => ({
  loadSearchHistory: () => [],
  clearSearchHistory: () => [],
}));

import { SEARCH_DEBOUNCE_MS, SearchBox } from "./SearchBox";

const onSubmit = vi.fn((_q: string) => {});
const onLive = vi.fn((_q: string, _prefix: boolean) => {});
let current = "";

function Harness({ initial }: { initial: string }) {
  const [value, setValue] = useState(initial);
  current = value;
  return <SearchBox value={value} onChange={setValue} onSubmit={onSubmit} onLive={onLive} onClear={() => {}} />;
}

let root: Root;
let host: HTMLElement;

function mount(initial = ""): HTMLInputElement {
  flushSync(() => root.render(<Harness initial={initial} />));
  return host.querySelector("input.search-input") as HTMLInputElement;
}

function type(input: HTMLInputElement, text: string): void {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
  flushSync(() => {
    input.focus();
    setter?.call(input, text);
    input.setSelectionRange(text.length, text.length);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

function key(el: Element, k: string): void {
  flushSync(() => {
    el.dispatchEvent(new KeyboardEvent("keydown", { key: k, bubbles: true, cancelable: true }));
  });
}

function click(el: Element): void {
  flushSync(() => {
    el.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
  });
}

const options = (): HTMLElement[] => [...host.querySelectorAll<HTMLElement>('[role="option"]')];

function sender(id: string, name: string, email: string): Email {
  return {
    id,
    blobId: "b",
    threadId: "t" + id,
    mailboxIds: {},
    keywords: {},
    size: 1,
    receivedAt: "2026-10-01T00:00:00Z",
    from: [{ name, email }],
    subject: id,
    preview: "",
    hasAttachment: false,
  } as unknown as Email;
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => setTimeout(() => cb(0), 0));
  onSubmit.mockClear();
  onLive.mockClear();
  contactAutocomplete.mockReset();
  contactAutocomplete.mockResolvedValue([]);
  store.setSession({ capabilities: {} } as never, "U1");
  store.putEmails([sender("m1", "Alice Adams", "alice@example.com")]);
  store.setList({ key: "k", ids: ["m1"], total: 1 });
  store.putAreas(
    [
      { id: "A1", name: "Work", position: 1, labelId: "L1", tabs: [], labelIds: [] } as unknown as Area,
      { id: "A2", name: "Side Projects", position: 2, labelId: "L2", tabs: [], labelIds: [] } as unknown as Area,
    ],
    true,
  );
  host = document.createElement("div");
  document.body.replaceChildren(host);
  root = createRoot(host);
});
afterEach(() => {
  flushSync(() => root.unmount());
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("contact suggestions (combobox)", () => {
  it("offers directory contacts for from:, keyboard-navigable, inserting the exact address", async () => {
    contactAutocomplete.mockResolvedValue([
      { id: "c1", displayName: "Dora Explorer", addresses: [{ address: "dora@example.org", reachedMeAt: null }] },
    ]);
    const input = mount();
    type(input, "from:do");
    await vi.advanceTimersByTimeAsync(200);
    expect(contactAutocomplete).toHaveBeenCalledWith("do");
    // The answer's state update renders on React's scheduler.
    await vi.advanceTimersByTimeAsync(50);

    // A combobox that owns a listbox.
    expect(input.getAttribute("role")).toBe("combobox");
    expect(input.getAttribute("aria-expanded")).toBe("true");
    const list = host.querySelector('[role="listbox"]');
    expect(list?.id).toBe(input.getAttribute("aria-controls"));
    expect(options().map((o) => o.textContent)).toEqual(["dora@example.orgDora Explorer"]);

    // Nothing is chosen until the user moves to it.
    expect(input.hasAttribute("aria-activedescendant")).toBe(false);
    key(input, "ArrowDown");
    const activeId = input.getAttribute("aria-activedescendant");
    expect(activeId).toBe(options()[0]?.id);
    expect(options()[0]?.getAttribute("aria-selected")).toBe("true");

    key(input, "Enter");
    expect(current).toBe("from:dora@example.org");
    expect(onSubmit).toHaveBeenCalledWith("from:dora@example.org");
  });

  it("offers loaded senders for alias: and deliveredto: too, and Escape closes the list without touching the text", () => {
    const input = mount();
    type(input, "deliveredto:ali");
    expect(options().map((o) => o.querySelector(".search-option-label")?.textContent)).toEqual(["alice@example.com"]);
    key(input, "Escape");
    expect(input.getAttribute("aria-expanded")).toBe("false");
    expect(host.querySelector('[role="listbox"]')).toBeNull();
    expect(current).toBe("deliveredto:ali");
  });

  it("suggests area names after area:, quoted where needed", () => {
    const input = mount();
    type(input, "area:si");
    expect(options().map((o) => o.textContent)).toEqual(["Side Projects"]);
    key(input, "ArrowDown");
    key(input, "Tab");
    expect(current).toBe('area:"Side Projects"');
  });
});

describe("live results", () => {
  it(`waits ${SEARCH_DEBOUNCE_MS} ms after the LAST keystroke, then searches once with the final word as a prefix`, async () => {
    const input = mount();
    type(input, "inv");
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS - 50);
    type(input, "invo");
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS - 1);
    expect(onLive).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(onLive).toHaveBeenCalledTimes(1);
    expect(onLive).toHaveBeenCalledWith("invo", true);
  });

  it("does not search a half-typed operator, and Enter cancels the pending live search", async () => {
    const input = mount();
    type(input, "from:");
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS * 2);
    expect(onLive).not.toHaveBeenCalled();

    type(input, "invoice");
    key(input, "Enter");
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS * 2);
    expect(onSubmit).toHaveBeenCalledWith("invoice");
    expect(onLive).not.toHaveBeenCalled();
  });
});

describe("search-options form", () => {
  const button = (): HTMLButtonElement => host.querySelector('button[aria-controls="search-options"]') as HTMLButtonElement;
  const field = (label: string): HTMLInputElement | HTMLSelectElement => {
    const l = [...host.querySelectorAll("form.search-options label")].find((n) => n.textContent === label);
    const el = l ? document.getElementById(l.getAttribute("for") ?? "") : null;
    if (!el) throw new Error("no field labelled " + label);
    return el as HTMLInputElement | HTMLSelectElement;
  };

  it("opens in the page, labelled, focused on the first field, pre-filled from the query", async () => {
    mount('from:alice@example.com invoice OR receipt');
    expect(button().getAttribute("aria-label")).toBe("Search options");
    expect(button().getAttribute("aria-expanded")).toBe("false");
    click(button());
    await vi.advanceTimersByTimeAsync(0);

    const form = host.querySelector("form.search-options") as HTMLFormElement;
    expect(form).not.toBeNull();
    expect(form.id).toBe("search-options");
    expect(button().getAttribute("aria-expanded")).toBe("true");
    // Named by its visible heading; no dialog of any kind.
    expect(document.getElementById(form.getAttribute("aria-labelledby") ?? "")?.textContent).toBe("Search options");
    expect(host.querySelector("dialog, [role='dialog']")).toBeNull();
    expect(document.activeElement).toBe(field("From"));

    // A top-level OR: nothing can be lifted safely, so ALL of it is in the words.
    expect(field("From").value).toBe("");
    expect(field("Has the words").value).toBe("from:alice@example.com invoice OR receipt");
  });

  it("gives every control its visible label as its accessible name", () => {
    mount("");
    click(button());
    const controls = [...host.querySelectorAll<HTMLInputElement>("form.search-options input, form.search-options select")];
    expect(controls.length).toBe(11);
    for (const c of controls) {
      expect(c.labels?.length, `${c.id} has no <label for>`).toBe(1);
      expect(c.labels?.[0]?.textContent?.trim()).not.toBe("");
    }
  });

  it("writes the query string from the fields and runs it through the box", async () => {
    mount("is:unread");
    click(button());
    const set = (label: string, v: string): void => {
      const el = field(label);
      const proto = el instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
      flushSync(() => {
        Object.getOwnPropertyDescriptor(proto, "value")?.set?.call(el, v);
        el.dispatchEvent(new Event(el instanceof HTMLSelectElement ? "change" : "input", { bubbles: true }));
      });
    };
    set("From", "Acme Billing");
    set("Date within", "1w");
    set("Area", "Side Projects");
    set("Has attachment", "pdf");
    flushSync(() => (host.querySelector("#search-options-anywhere") as HTMLInputElement).click());
    flushSync(() => (host.querySelector("form.search-options") as HTMLFormElement).requestSubmit());

    const q = 'from:"Acme Billing" is:unread newer_than:1w area:"Side Projects" has:pdf in:anywhere';
    expect(onSubmit).toHaveBeenCalledWith(q);
    expect(current).toBe(q);
    expect(host.querySelector("form.search-options")).toBeNull();
  });

  it("Escape closes it without changing the query and puts focus back on the button", async () => {
    mount("from:x");
    click(button());
    key(field("Subject"), "Escape");
    await vi.advanceTimersByTimeAsync(0);
    expect(host.querySelector("form.search-options")).toBeNull();
    expect(document.activeElement).toBe(button());
    expect(current).toBe("from:x");
    expect(onSubmit).not.toHaveBeenCalled();
  });
});
