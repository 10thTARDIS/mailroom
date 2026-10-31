// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The ways INTO a contact's page (#816), each against captured #828 contacts:
//   * the sender card: a saved sender's name links to the page; anyone else
//     gets "Add to contacts", which on success becomes "In your contacts" —
//     the same link — where the button was;
//   * a compose recipient chip: its name opens "Open contact" or "Add to
//     contacts", and Escape closes only that;
//   * Settings › Senders: links to the VIP and Blocked filters;
//   * the recent-mail search the page runs covers every address and goes
//     through the search path (sync.allMatchingIds) with a from:/to: query.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useSyncExternalStore } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { Contact, Email, Id, Mailbox } from "../../../core/jmap/types";
import { MailStore, effectiveEmail } from "../../../core/store";
import type { EffectiveEmail } from "../../../core/store";
import type { View } from "../../../core/views";
import { ADA_MERGED, ADA_NAMED, NEWS_BLOCKED, RECENT_QUERY } from "../../../core/contactsFixtures.testdata";

const store = new MailStore(null);
let contact: Contact | null | undefined;
const setContactMarkers = vi.fn(async (_id: Id, patch: Partial<Contact>) => {
  if (contact) contact = { ...contact, ...patch };
  return true;
});
const allMatchingIds = vi.fn(async (_v: View, _cap?: number) => ({ ids: RECENT_QUERY.ids, total: RECENT_QUERY.total }));
const loadEmails = vi.fn(async (_ids: Id[]) => ({ notFound: [] as Id[], failed: false }));

// Stable, as the real ones are: a fresh client per render would re-run every
// effect that depends on it.
const client = { contactForAddress: vi.fn(async () => contact) };
const actions = { relabel: vi.fn(), setContactMarkers };
const sync = { loadEmails, allMatchingIds };
vi.mock("../../state", () => ({
  useApp: () => ({
    actions,
    sync,
    client,
    store,
    offerUndo: () => {},
    pushToast: () => 1,
  }),
  useMailState: () => useSyncExternalStore(store.subscribe, store.getState),
}));
vi.mock("../shellContext", () => ({ useShell: () => ({ openRuleDraft: () => {} }) }));
vi.mock("../useMediaQuery", async (orig) => ({ ...(await orig<object>()), useMediaQuery: () => false }));

import { SenderPopover } from "../pane/SenderPopover";
import { RecipientChips } from "../compose/RecipientChips";
import { SendersBody } from "../settings/rows/SendersRow";
import { CorrespondentKeys } from "../settings/keys/CorrespondentKeys";
import type { KeysApi } from "../settings/keys/api";
import { C1_CONTACT_KEY_ALICE } from "../../../core/keyHistoryFixtures.testdata";
import { formatRoute } from "../../../core/routes";
import { useContactsApi } from "./api";
import type { ContactsApi } from "./api";

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
const byText = (sel: string, text: string): HTMLElement => {
  const found = [...document.querySelectorAll<HTMLElement>(sel)].find((n) => n.textContent?.trim() === text);
  if (!found) throw new Error(`no ${sel} with ${text}; page: ${document.body.textContent?.slice(0, 500)}`);
  return found;
};
const maybe = (sel: string, text: string): HTMLElement | undefined =>
  [...document.querySelectorAll<HTMLElement>(sel)].find((n) => n.textContent?.trim() === text);

function mailbox(id: string, role: Mailbox["role"]): Mailbox {
  return { id, accountId: "A1", name: id, parentId: null, role, sortOrder: 0, totalEmails: 0, unreadEmails: 0 } as Mailbox;
}
function emailFrom(address: string, name: string): Email {
  return {
    id: "m1",
    blobId: "b",
    threadId: "t1",
    mailboxIds: { INBOX: true },
    keywords: {},
    labelIds: {},
    size: 1,
    receivedAt: "2026-10-22T04:36:09Z",
    from: [{ name, email: address }],
    subject: "The season",
    preview: "",
    hasAttachment: false,
  } as unknown as Email;
}

let root: Root;
beforeEach(() => {
  setContactMarkers.mockClear();
  allMatchingIds.mockClear();
  loadEmails.mockClear();
  store.setSession({ capabilities: {} } as never, "U1");
  store.putMailboxes([mailbox("INBOX", "inbox")]);
  window.location.hash = "#/inbox";
  const host = document.createElement("div");
  document.body.replaceChildren(host);
  root = createRoot(host);
});
afterEach(() => {
  flushSync(() => root.unmount());
});

async function openPopover(address: string, name: string): Promise<void> {
  store.putEmails([emailFrom(address, name)]);
  const shown = effectiveEmail(store.getState(), "m1") as EffectiveEmail;
  flushSync(() => root.render(<SenderPopover email={shown} />));
  await click(document.querySelector("button.message-from-button") as Element);
}

describe("the sender card (#816)", () => {
  it("Add to contacts saves the sender and becomes a link to their page", async () => {
    contact = NEWS_BLOCKED;
    await openPopover("updates@news.theatre.example.com", "The Grand Theatre");
    expect(document.querySelector("a.sender-popover-contact")).toBeNull();
    await click(byText("button", "Add to contacts"));
    expect(setContactMarkers).toHaveBeenCalledWith(NEWS_BLOCKED.id, { isSaved: true }, expect.any(String));
    expect(maybe("button", "Add to contacts")).toBeUndefined();
    const link = byText("a", "In your contacts");
    expect(link.getAttribute("href")).toBe(`#/contacts/${NEWS_BLOCKED.id}`);
    // And the name at the top is the same link now.
    expect(document.querySelector("a.sender-popover-contact")?.getAttribute("href")).toBe(`#/contacts/${NEWS_BLOCKED.id}`);
  });

  it("a saved sender's name links to their page, and following it navigates", async () => {
    contact = ADA_NAMED;
    await openPopover("ada@example.com", "Ada Lovelace");
    const name = document.querySelector("a.sender-popover-contact") as HTMLAnchorElement;
    expect(name.textContent).toBe("Ada Lovelace");
    await click(name);
    expect(window.location.hash).toBe(`#/contacts/${ADA_NAMED.id}`);
  });

  it("says nothing about contacts against a server without saved contacts", async () => {
    const { isSaved: _drop, ...old } = ADA_NAMED;
    contact = old as Contact;
    await openPopover("ada@example.com", "Ada Lovelace");
    expect(maybe("button", "Add to contacts")).toBeUndefined();
    expect(maybe("a", "In your contacts")).toBeUndefined();
  });
});

describe("a compose recipient chip (#816)", () => {
  async function renderChip(): Promise<HTMLButtonElement> {
    flushSync(() =>
      root.render(
        <RecipientChips
          label="To"
          chips={[{ address: "updates@news.theatre.example.com", name: "The Grand Theatre" }]}
          onChips={() => {}}
          factFor={() => "unknown"}
          onNeedFact={() => {}}
          keysAvailable={false}
        />,
      ),
    );
    await settleAll();
    return document.querySelector("button.compose-chip-name") as HTMLButtonElement;
  }

  it("the name opens Add to contacts for an unsaved one, which becomes Open contact", async () => {
    contact = NEWS_BLOCKED;
    const name = await renderChip();
    expect(name.getAttribute("aria-expanded")).toBe("false");
    await click(name);
    expect(name.getAttribute("aria-expanded")).toBe("true");
    await click(byText(".compose-chip-panel button", "Add to contacts"));
    expect(setContactMarkers).toHaveBeenCalledWith(NEWS_BLOCKED.id, { isSaved: true }, expect.any(String));
    expect(byText(".compose-chip-panel a", "Open contact").getAttribute("href")).toBe(`#/contacts/${NEWS_BLOCKED.id}`);
  });

  it("Escape closes the panel and puts focus back on the name", async () => {
    contact = ADA_NAMED;
    const name = await renderChip();
    await click(name);
    const link = byText(".compose-chip-panel a", "Open contact");
    link.focus();
    const ev = new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true });
    flushSync(() => link.dispatchEvent(ev));
    expect(name.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(name);
  });

  it("an address with no contact says so and offers nothing", async () => {
    contact = null;
    const name = await renderChip();
    await click(name);
    expect(document.querySelector(".compose-chip-panel")?.textContent).toBe(
      "No mail with this address yet, so there is no contact to open.",
    );
    expect(document.querySelector(".compose-chip-panel button, .compose-chip-panel a")).toBeNull();
  });
});

describe("Settings › Senders (#816)", () => {
  it("links to the Contacts page's VIP and Blocked filters", async () => {
    flushSync(() =>
      root.render(
        <SendersBody senders={{ vips: { list: [], total: 2 }, blocked: { list: [], total: 1 }, reload: () => {} }} />,
      ),
    );
    await settleAll();
    expect(document.body.textContent).toContain("VIPs and blocked senders are now in Contacts");
    expect(byText("a", "VIPs (2)").getAttribute("href")).toBe("#/contacts?filter=vip");
    const blocked = byText("a", "Blocked senders (1)");
    expect(blocked.getAttribute("href")).toBe("#/contacts?filter=blocked");
    await click(blocked);
    expect(window.location.hash).toBe("#/contacts?filter=blocked");
    // The old "mark an address as VIP" form moved to Contacts.
    expect(document.querySelector("form.senders-add")).toBeNull();
  });
});

describe("Settings → Keys › a correspondent's key (#816)", () => {
  it("links to the contact the key is held under", async () => {
    const keysApi = {
      readCorrespondents: async () => ({ keys: [C1_CONTACT_KEY_ALICE], contacts: [] }),
    } as unknown as KeysApi;
    flushSync(() =>
      root.render(
        <CorrespondentKeys
          api={keysApi}
          onChanged={() => {}}
          onOpenContact={(id) => ({
            href: formatRoute({ page: "contacts", id }),
            open: () => {
              window.location.hash = formatRoute({ page: "contacts", id });
            },
          })}
        />,
      ),
    );
    await settleAll();
    const link = byText(".correspondent-key-contact a", "Open contact");
    expect(link.getAttribute("href")).toBe("#/contacts/" + C1_CONTACT_KEY_ALICE.contactId);
    expect(link.getAttribute("aria-label")).toMatch(/^Open contact for /);
    await click(link);
    expect(window.location.hash).toBe("#/contacts/" + C1_CONTACT_KEY_ALICE.contactId);
  });
});

describe("the recent-mail search (#816)", () => {
  it("goes through the search path, from: and to: for every address, newest ten", async () => {
    let api: ContactsApi | null = null;
    function Probe(): null {
      api = useContactsApi();
      return null;
    }
    flushSync(() => root.render(<Probe />));
    const out = await (api as unknown as ContactsApi).recentMail(ADA_MERGED.addresses);
    expect(allMatchingIds).toHaveBeenCalledTimes(1);
    const [view, cap] = allMatchingIds.mock.calls[0] as [View, number];
    expect(view).toEqual({
      kind: "search",
      query:
        "from:ada@example.com OR to:ada@example.com OR from:updates@news.theatre.example.com OR to:updates@news.theatre.example.com",
    });
    expect(cap).toBe(10);
    expect(loadEmails).toHaveBeenCalledWith(RECENT_QUERY.ids);
    expect(out).toEqual({
      ok: true,
      value: { ids: RECENT_QUERY.ids, total: RECENT_QUERY.total, query: (view as { query: string }).query, skipped: [] },
    });
  });
});
