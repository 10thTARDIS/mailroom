// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The Contacts list and a contact's page (#816), rendered against a fake
// ContactsApi that answers with REAL #828 responses (core/
// contactsFixtures.testdata.ts). What is pinned is what the page SENDS and
// what it SAYS: the query each filter asks for, the bound of a page, the
// user's own name and "Use theirs", the notes limit, the server's VIP rule on
// un-save, the merge's block carry-over and where it lands, Separate only with
// two or more addresses, the recent-mail search over every address, and the
// key card narrowed to this contact.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useSyncExternalStore } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import { MailStore } from "../../../core/store";
import type { Contact, ContactSeparateResult, HeldContactKey, Id } from "../../../core/jmap/types";
import type { ContactsRoute } from "../../../core/routes";
import {
  ADA_MERGED,
  ADA_NAMED,
  ADA_PRE_MERGE,
  GRACE_VIP,
  MERGE_RESULT,
  NEWS_BLOCKED,
  Q_BLOCKED,
  Q_SAVED_BY_NAME,
  Q_SAVED_PAGE1,
  Q_SAVED_PAGE2,
  SEPARATE_RESULT,
  SEPARATE_RESULT_KEPT_NOTES,
  SEPARATE_RESULT_PARTIAL,
} from "../../../core/contactsFixtures.testdata";
import {
  C1_CONTACT_KEY_ALICE,
  C1_CONTACT_KEY_BOB_REMOVED,
  C1_CONTACT_KEY_CAROL,
} from "../../../core/keyHistoryFixtures.testdata";
import type { ContactOutcome, ContactPatch, ContactsApi, ContactsPage, RecentMail } from "./api";

const store = new MailStore(null);
const readCorrespondents = vi.fn(async (): Promise<{ keys: HeldContactKey[]; contacts: Contact[] } | null> => ({
  keys: [],
  contacts: [],
}));

vi.mock("../../state", () => ({
  useApp: () => ({ pushToast: () => 1, client: {}, actions: {} }),
  useMailState: () => useSyncExternalStore(store.subscribe, store.getState),
}));
vi.mock("../settings/keys/api", () => ({
  useKeysApi: () => ({
    readCorrespondents,
    correspondentKeyHistory: async () => ({ ok: true, value: [] }),
    refreshCorrespondentKey: async () => ({ ok: false, error: "failed" }),
    removeCorrespondentKey: async () => ({ ok: true, value: undefined }),
    decideCorrespondentKey: async () => ({ ok: true, value: undefined }),
    checkContactKey: async () => ({ kind: "failed" }),
    importContactKey: async () => ({ kind: "failed" }),
  }),
}));

import { ContactsList } from "./ContactsList";
import { ContactPage } from "./ContactPage";

// -- the fake server --------------------------------------------------------

type QueryArgs = Parameters<ContactsApi["query"]>[0];
let queries: QueryArgs[];
let updates: { id: Id; patch: ContactPatch }[];
let merges: { into: Id; from: Id }[];
let recentAsked: Contact["addresses"][];
let pages: (args: QueryArgs) => ContactsPage;
let current: Contact | null;
let byAddress: Record<string, Contact>;
let separateResult: ContactSeparateResult;

const ok = <T,>(value: T): ContactOutcome<T> => ({ ok: true, value });

function fakeApi(): ContactsApi {
  return {
    query: async (args) => {
      queries.push(args);
      return ok(pages(args));
    },
    get: async () => ok(current),
    byAddress: async (a) => ok(byAddress[a.trim()] ?? null),
    update: async (id, patch) => {
      updates.push({ id, patch });
      if (current) current = { ...current, ...patch } as Contact;
      return ok(undefined);
    },
    merge: async (into, from) => {
      merges.push({ into, from });
      return ok(MERGE_RESULT);
    },
    separate: async () => ok(separateResult),
    recentMail: async (addresses) => {
      recentAsked.push(addresses);
      const mail: RecentMail = { ids: [], total: 0, query: "from:x@example.com", skipped: [] };
      return ok(mail);
    },
  };
}

const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 0));
async function settleAll(): Promise<void> {
  for (let i = 0; i < 6; i++) await settle();
}
async function click(el: Element): Promise<void> {
  flushSync(() => {
    el.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, button: 0 }));
  });
  await settleAll();
}
function type(el: HTMLInputElement | HTMLTextAreaElement, value: string): void {
  const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
  const setter = Object.getOwnPropertyDescriptor(proto, "value")?.set;
  flushSync(() => {
    setter?.call(el, value);
    el.dispatchEvent(new Event("input", { bubbles: true }));
  });
}
function key(el: Element, k: string): void {
  flushSync(() => {
    el.dispatchEvent(new KeyboardEvent("keydown", { key: k, bubbles: true, cancelable: true }));
  });
}
const byText = (sel: string, text: string | RegExp): HTMLElement => {
  const found = [...document.querySelectorAll<HTMLElement>(sel)].find((n) =>
    typeof text === "string" ? n.textContent?.trim() === text : text.test(n.textContent ?? ""),
  );
  if (!found) throw new Error(`no ${sel} with ${String(text)}; page: ${document.body.textContent?.slice(0, 600)}`);
  return found;
};
const maybe = (sel: string, text: string): HTMLElement | undefined =>
  [...document.querySelectorAll<HTMLElement>(sel)].find((n) => n.textContent?.trim() === text);

let root: Root;
beforeEach(() => {
  queries = [];
  updates = [];
  merges = [];
  recentAsked = [];
  current = null;
  byAddress = {};
  separateResult = SEPARATE_RESULT;
  pages = () => ({ list: Q_SAVED_BY_NAME.get.list, total: Q_SAVED_BY_NAME.query.total, position: 0 });
  readCorrespondents.mockClear();
  window.location.hash = "#/contacts";
  const host = document.createElement("div");
  document.body.replaceChildren(host);
  root = createRoot(host);
});
afterEach(() => {
  flushSync(() => root.unmount());
});

async function renderList(route: ContactsRoute): Promise<void> {
  flushSync(() =>
    root.render(<ContactsList api={fakeApi()} route={route} version={0} onChanged={() => {}} onMessage={() => {}} />),
  );
  await settleAll();
}

async function renderPage(contact: Contact, route: ContactsRoute = { page: "contacts", id: contact.id }): Promise<void> {
  current = contact;
  flushSync(() =>
    root.render(
      <ContactPage api={fakeApi()} id={contact.id} route={route} onChanged={() => {}} onMessage={() => {}} />,
    ),
  );
  await settleAll();
}

// -- the list ----------------------------------------------------------------

describe("the Contacts list (#816)", () => {
  it("asks for SAVED contacts by default, sorted by name", async () => {
    await renderList({ page: "contacts" });
    expect(queries[0]?.filter).toEqual({ isSaved: true });
    expect(queries[0]?.sort).toEqual([{ property: "name", isAscending: true }]);
    // The captured rows: alice (no name: the address), and Ada by her own name.
    expect([...document.querySelectorAll(".contacts-row-name")].map((n) => n.textContent)).toEqual([
      "alice@example.org",
      "Countess Ada",
    ]);
  });

  it("Blocked senders asks for isBlocked WITHOUT isSaved, and lists a sender who was never saved", async () => {
    pages = () => ({ list: Q_BLOCKED.get.list, total: Q_BLOCKED.query.total, position: 0 });
    await renderList({ page: "contacts", filter: "blocked" });
    expect(queries[0]?.filter).toEqual({ isBlocked: true });
    expect(byText(".contacts-row-name", "The Grand Theatre")).toBeTruthy();
    expect(byText(".contacts-chip", "Blocked")).toBeTruthy();
  });

  it("the Blocked senders chip navigates to the blocked filter", async () => {
    await renderList({ page: "contacts" });
    expect(byText("button.chip-button", "All saved").getAttribute("aria-pressed")).toBe("true");
    await click(byText("button.chip-button", "Blocked senders"));
    expect(window.location.hash).toBe("#/contacts?filter=blocked");
  });

  it("sorts by last mail newest first when the route says so", async () => {
    await renderList({ page: "contacts", sort: "last" });
    expect(queries[0]?.sort).toEqual([{ property: "lastMailAt", isAscending: false }]);
  });

  it("says the bound of a page, and Show more asks from where it stopped", async () => {
    pages = (args) =>
      args.position === 0
        ? { list: Q_SAVED_PAGE1.get.list, total: Q_SAVED_PAGE1.query.total, position: 0 }
        : { list: Q_SAVED_PAGE2.get.list, total: Q_SAVED_PAGE2.query.total, position: 1 };
    await renderList({ page: "contacts" });
    expect(byText(".contacts-bound", "Showing 1 of 2")).toBeTruthy();
    await click(byText("button", "Show more"));
    expect(queries[1]?.position).toBe(1);
    expect(byText(".contacts-bound", "2 contacts")).toBeTruthy();
    expect(maybe("button", "Show more")).toBeUndefined();
  });

  it("an empty saved list says what saves a contact", async () => {
    pages = () => ({ list: [], total: 0, position: 0 });
    await renderList({ page: "contacts" });
    expect(document.querySelector(".contacts-empty")?.textContent).toBe(
      "People you send mail to appear here automatically. Add anyone else with “Add to contacts” next to their address.",
    );
  });

  it("a server without saved contacts is said so, not listed", async () => {
    const { isSaved: _drop, ...old } = ADA_NAMED;
    pages = () => ({ list: [old as Contact], total: 1, position: 0 });
    await renderList({ page: "contacts" });
    expect(document.body.textContent).toContain("This server cannot list saved contacts.");
    expect(document.querySelector(".contacts-row")).toBeNull();
  });

  it("opening a row keeps the list's filter in the contact's URL", async () => {
    pages = () => ({ list: Q_BLOCKED.get.list, total: 1, position: 0 });
    await renderList({ page: "contacts", filter: "blocked", q: "the" });
    await click(document.querySelector(".contacts-row") as Element);
    expect(window.location.hash).toBe(`#/contacts/${NEWS_BLOCKED.id}?filter=blocked&q=the`);
  });

  it("Add a contact by address finds the contact and saves it", async () => {
    byAddress = { "updates@news.theatre.example.com": NEWS_BLOCKED };
    await renderList({ page: "contacts" });
    await click(byText("button", "Add a contact by address…"));
    const input = document.querySelector(".contacts-add-form input") as HTMLInputElement;
    type(input, "updates@news.theatre.example.com");
    await settleAll();
    await click(byText(".contacts-add-form button", "Add to contacts"));
    expect(updates).toEqual([{ id: NEWS_BLOCKED.id, patch: { isSaved: true } }]);
    expect(window.location.hash).toBe(`#/contacts/${NEWS_BLOCKED.id}`);
  });

  it("an address with no contact is said so, and nothing is written", async () => {
    await renderList({ page: "contacts" });
    await click(byText("button", "Add a contact by address…"));
    type(document.querySelector(".contacts-add-form input") as HTMLInputElement, "nobody@example.net");
    await settleAll();
    await click(byText(".contacts-add-form button", "Add to contacts"));
    expect(updates).toEqual([]);
    expect(document.querySelector('.contacts-add-form [role="alert"]')?.textContent).toContain(
      "There is no mail from or to nobody@example.net yet",
    );
  });
});

// -- hostile names (#816 security review) ------------------------------------

describe("a stranger's name on the Contacts surfaces", () => {
  // DERIVED from the captured theatre: a learned name with an override, and one
  // that is an address.
  const rlo: Contact = { ...NEWS_BLOCKED, name: "\u202Eevil\u202C Theatre", displayName: "\u202Eevil\u202C Theatre" };
  const ceo: Contact = { ...NEWS_BLOCKED, name: "ceo@example.com", displayName: "ceo@example.com" };

  it("the list row shows it without the control, with the real address", async () => {
    pages = () => ({ list: [rlo, ceo], total: 2, position: 0 });
    await renderList({ page: "contacts", filter: "blocked" });
    const rows = [...document.querySelectorAll(".contacts-row")];
    expect(rows[0]?.querySelector(".contacts-row-name")?.textContent).toBe("evil Theatre");
    expect(document.body.textContent).not.toContain("\u202E");
    expect(rows[1]?.querySelector(".contacts-row-name")?.textContent).toBe("ceo@example.com");
    expect(rows[1]?.querySelector(".contacts-row-address")?.textContent).toBe("updates@news.theatre.example.com");
  });

  it("the page's heading shows the real address under an address-shaped name", async () => {
    await renderPage(ceo);
    expect(document.querySelector("h1.contact-name")?.textContent).toBe("ceo@example.com");
    expect(document.querySelector(".contact-real-address")?.textContent).toBe("updates@news.theatre.example.com");
  });

  it("the merge dialog's options and preview use the formatter", async () => {
    byAddress = { "updates@news.theatre.example.com": rlo };
    pages = () => ({ list: [], total: 0, position: 0 });
    await renderPage(ADA_PRE_MERGE);
    await click(byText("button", "Merge with…"));
    const input = document.querySelector('[role="dialog"] input[role="combobox"]') as HTMLInputElement;
    type(input, "updates@news.theatre.example.com");
    await new Promise((r) => setTimeout(r, 260));
    await settleAll();
    const option = document.querySelector(".contact-merge-option") as HTMLElement;
    expect(option.querySelector(".contact-merge-option-name")?.textContent).toBe("evil Theatre");
    expect(option.querySelector(".contact-merge-option-address")?.textContent?.trim()).toBe("updates@news.theatre.example.com");
    key(input, "Enter");
    await settleAll();
    const dialog = document.querySelector('[role="dialog"]') as HTMLElement;
    expect(dialog.textContent).toContain("Merging evil Theatre <updates@news.theatre.example.com> into Countess Ada");
    expect(dialog.textContent).not.toContain("\u202E");
  });
});

// -- one contact ---------------------------------------------------------------

describe("a contact's page (#816)", () => {
  it("shows the user's name large, the learned one small, and Use theirs clears the user's", async () => {
    await renderPage(ADA_NAMED);
    expect(document.querySelector("h1.contact-name")?.textContent).toBe("Countess Ada");
    expect(document.querySelector(".contact-learned")?.textContent).toContain("Their mail says “Ada Lovelace”.");
    await click(byText("button", "Use theirs"));
    expect(updates).toEqual([{ id: ADA_NAMED.id, patch: { userName: null } }]);
  });

  it("Edit name sets userName, and an empty name clears it", async () => {
    await renderPage(ADA_NAMED);
    await click(byText("button", "Edit name"));
    const input = document.querySelector(".contact-name-form input") as HTMLInputElement;
    expect(input.value).toBe("Countess Ada");
    type(input, "  Ada L.  ");
    await click(byText("button", "Save name"));
    expect(updates[0]).toEqual({ id: ADA_NAMED.id, patch: { userName: "Ada L." } });
    await click(byText("button", "Edit name"));
    type(document.querySelector(".contact-name-form input") as HTMLInputElement, "   ");
    await click(byText("button", "Save name"));
    expect(updates[1]).toEqual({ id: ADA_NAMED.id, patch: { userName: null } });
  });

  it("a name over 200 characters is refused before it is sent", async () => {
    await renderPage(ADA_NAMED);
    await click(byText("button", "Edit name"));
    type(document.querySelector(".contact-name-form input") as HTMLInputElement, "x".repeat(201));
    await click(byText("button", "Save name"));
    expect(updates).toEqual([]);
    expect(document.querySelector('.contact-name-form [role="alert"]')?.textContent).toBe(
      "A name can be at most 200 characters.",
    );
  });

  it("notes: a live count, 4,000 characters saves, 4,001 cannot", async () => {
    await renderPage(ADA_NAMED);
    const area = document.querySelector("textarea.contact-notes") as HTMLTextAreaElement;
    expect(area.value).toBe("Met at the\nanalytical engine talk.");
    const save = byText("button", "Save notes") as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    type(area, "n".repeat(4001));
    await settleAll();
    expect(document.querySelector(".contact-notes-count")?.textContent).toBe(
      "4,001 of 4,000 characters: shorten the notes to save them.",
    );
    expect((byText("button", "Save notes") as HTMLButtonElement).disabled).toBe(true);
    type(area, "n".repeat(4000));
    await settleAll();
    expect(document.querySelector(".contact-notes-count")?.textContent).toBe("4,000 of 4,000 characters");
    await click(byText("button", "Save notes"));
    expect(updates).toEqual([{ id: ADA_NAMED.id, patch: { notes: "n".repeat(4000) } }]);
  });

  it("Remove on a VIP says they stay a VIP, and sends only isSaved:false (the server keeps VIP)", async () => {
    await renderPage(GRACE_VIP);
    await click(byText("button", "Remove from contacts…"));
    const dialog = document.querySelector('[role="dialog"]') as HTMLElement;
    expect(dialog.textContent).toContain("Nothing is deleted");
    expect(dialog.textContent).toContain("Grace Hopper stays a VIP");
    await click(byText('[role="dialog"] button', "Remove from contacts"));
    expect(updates).toEqual([{ id: GRACE_VIP.id, patch: { isSaved: false } }]);
  });

  it("Remove on a contact who is not a VIP says nothing about VIP", async () => {
    await renderPage(ADA_NAMED);
    await click(byText("button", "Remove from contacts…"));
    expect(document.querySelector('[role="dialog"]')?.textContent).not.toContain("VIP");
  });

  it("an unsaved contact's page offers Add to contacts", async () => {
    await renderPage(NEWS_BLOCKED);
    expect(document.body.textContent).toContain("Not in your contacts.");
    await click(byText("button.contact-save", "Add to contacts"));
    expect(updates).toEqual([{ id: NEWS_BLOCKED.id, patch: { isSaved: true } }]);
  });

  it("Separate is offered only with two or more addresses", async () => {
    await renderPage(ADA_NAMED);
    expect(maybe("button", "Separate…")).toBeUndefined();
    flushSync(() => root.unmount());
    root = createRoot(document.body.appendChild(document.createElement("div")));
    await renderPage(ADA_MERGED);
    expect(maybe("button", "Separate…")).toBeTruthy();
  });

  // #834: the page says what the separate did to the contact left behind.
  async function separateTheatre(result: ContactSeparateResult): Promise<string> {
    separateResult = result;
    await renderPage(ADA_MERGED);
    await click(byText("button", "Separate…"));
    await click(byText('[role="dialog"] label', "updates@news.theatre.example.com"));
    await click(byText('[role="dialog"] button', "Separate"));
    return document.querySelector(".contact-separated")?.textContent ?? "";
  }

  it("after Separate, says the contact is back as it was and which of your edits were kept", async () => {
    const said = await separateTheatre(SEPARATE_RESULT_KEPT_NOTES);
    expect(said).toContain("This contact is back as it was before the merge. Your edits to notes were kept.");
  });

  it("after a partial Separate, says nothing changed back", async () => {
    const said = await separateTheatre(SEPARATE_RESULT_PARTIAL);
    expect(said).toContain("This contact keeps its details as they are.");
    expect(said).not.toContain("back as it was");
  });

  it("after Separate on a server that does not say, claims nothing", async () => {
    const said = await separateTheatre(SEPARATE_RESULT);
    expect(said).toContain("Split off into a separate contact:");
    expect(said).not.toContain("back as it was");
    expect(said).not.toContain("keeps its details");
  });

  it("says the counts in words, and never a date it does not have", async () => {
    await renderPage({ ...ADA_NAMED, outboundCount: 0, inboundCount: 4, firstMailAt: null });
    expect(document.querySelector(".contact-counts")?.textContent).toBe(
      "You haven’t written to them · 4 other messages from them or copying them",
    );
    expect(document.querySelector(".contact-dates")?.textContent).toBe("Last mail 22 Oct 2026");
  });

  it("asks for recent mail over every address", async () => {
    await renderPage(ADA_MERGED);
    expect(recentAsked.at(-1)?.map((a) => a.address)).toEqual([
      "ada@example.com",
      "updates@news.theatre.example.com",
    ]);
  });

  it("shows only this contact's keys, and its removed key under History", async () => {
    const mine = C1_CONTACT_KEY_ALICE.contactId;
    // DERIVED: bob's captured removed key, re-homed onto this contact.
    const removed = { ...C1_CONTACT_KEY_BOB_REMOVED, contactId: mine };
    readCorrespondents.mockResolvedValue({ keys: [C1_CONTACT_KEY_ALICE, removed, C1_CONTACT_KEY_CAROL], contacts: [] });
    // DERIVED: Ada's captured contact with the id the keys are held under.
    await renderPage({ ...ADA_NAMED, id: mine, addresses: [{ address: "alice@example.org", reachedMeAt: null }] });
    const cards = [...document.querySelectorAll(".correspondent-keys > .correspondent-key-list > .correspondent-key")];
    expect(cards).toHaveLength(1);
    expect(document.querySelector(".contact-keys h2")?.textContent).toBe("Key");
    expect(document.body.textContent).not.toContain(C1_CONTACT_KEY_CAROL.fingerprint.slice(-4));
    const history = document.querySelector(".contact-keys .key-history-toggle") as HTMLElement;
    expect(history.textContent).toContain("(1)");
  });

  it("Merge with…: keyboard selection, the block carry-over said first, then it lands on the merged contact", async () => {
    byAddress = { "updates@news.theatre.example.com": NEWS_BLOCKED };
    pages = () => ({ list: [], total: 0, position: 0 });
    await renderPage(ADA_PRE_MERGE);
    await click(byText("button", "Merge with…"));
    const input = document.querySelector('[role="dialog"] input[role="combobox"]') as HTMLInputElement;
    expect(document.activeElement).toBe(input);
    type(input, "updates@news.theatre.example.com");
    await new Promise((r) => setTimeout(r, 260));
    await settleAll();
    expect(input.getAttribute("aria-expanded")).toBe("true");
    key(input, "ArrowDown");
    key(input, "Enter");
    await settleAll();
    const preview = document.querySelector(".contact-merge-preview") as HTMLElement;
    expect(preview.textContent).toContain("Merging The Grand Theatre into Countess Ada");
    expect(document.querySelector(".contact-merge-block")?.textContent).toBe(
      "Blocked: the block on The Grand Theatre carries over, so new mail from every address goes to Trash",
    );
    window.location.hash = "#/contacts";
    await click(byText('[role="dialog"] button', "Merge The Grand Theatre in"));
    expect(merges).toEqual([{ into: ADA_PRE_MERGE.id, from: NEWS_BLOCKED.id }]);
    expect(window.location.hash).toBe(`#/contacts/${MERGE_RESULT.id}`);
  });
});
