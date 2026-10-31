// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// VIP and Block on the sender card (#59).
//
// Real MailStore and the real bulk path (../SenderBulk.tsx, ../senderBulk.ts);
// the network and the writes are stand-ins. Pinned: the markers come from the
// contact and are absent until it is read; VIP is a pressed toggle; the block
// form defaults to Junk with "also move existing mail" OFF; a block with the
// box unticked moves nothing; ticked, it counts and confirms before moving
// through moveToJunk; the toast offers Undo that unblocks; Unblock appears
// once blocked.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useSyncExternalStore } from "react";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { Contact, Email, Id, Mailbox } from "../../../core/jmap/types";
import { MailStore, effectiveEmail } from "../../../core/store";
import type { EffectiveEmail } from "../../../core/store";
import type { View } from "../../../core/views";

const store = new MailStore(null);
const ADDRESS = "pest@example.com";
let contact: Contact | null;
const server: Email[] = [];
const allMatchingIds = vi.fn(async (_v: View, _cap?: number, position = 0) => ({
  ids: server.map((e) => e.id).slice(position, position + 1000),
  total: server.length,
}));
const loadEmails = vi.fn(async (ids: Id[]) => {
  store.putEmails(server.filter((e) => ids.includes(e.id)));
  return { notFound: [] as Id[], failed: false };
});
const setContactMarkers = vi.fn(async (_id: Id, patch: Partial<Contact>) => {
  if (contact) contact = { ...contact, ...patch };
  return true;
});
const moveToJunk = vi.fn(async () => true);
const trash = vi.fn(async () => undefined);
const undos: Array<{ label: string; inverse: () => Promise<boolean> }> = [];
const toasts: string[] = [];

vi.mock("../../state", () => ({
  useApp: () => ({
    actions: { relabel: vi.fn(), setContactMarkers, moveToJunk, trash, archive: vi.fn(), restoreToInbox: vi.fn(), untrash: vi.fn(), settle: vi.fn(async () => true) },
    sync: { loadEmails, allMatchingIds },
    client: { contactForAddress: vi.fn(async () => contact) },
    store,
    offerUndo: (label: string, inverse: () => Promise<boolean>) => void undos.push({ label, inverse }),
    pushToast: (t: { message: string }) => (toasts.push(t.message), 1),
  }),
  useMailState: () => useSyncExternalStore(store.subscribe, store.getState),
}));
vi.mock("../shellContext", () => ({ useShell: () => ({ openRuleDraft: () => {} }) }));
vi.mock("../useMediaQuery", async (orig) => ({ ...(await orig<object>()), useMediaQuery: () => false }));

import { SenderPopover } from "./SenderPopover";

function mailbox(id: string, role: Mailbox["role"]): Mailbox {
  return { id, accountId: "A1", name: id, parentId: null, role, sortOrder: 0, totalEmails: 0, unreadEmails: 0 } as Mailbox;
}
function email(i: number): Email {
  return {
    id: `p${i}`,
    blobId: "b",
    threadId: `t${i}`,
    mailboxIds: { INBOX: true },
    keywords: {},
    labelIds: {},
    size: 1,
    receivedAt: "2026-10-01T00:00:00Z",
    from: [{ name: "Pest", email: ADDRESS }],
    subject: `Pest ${i}`,
    preview: "",
    hasAttachment: false,
  } as unknown as Email;
}

const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));
async function click(el: Element): Promise<void> {
  flushSync(() => {
    el.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
  });
  await settle();
  await settle();
}
const byText = (sel: string, text: string): HTMLElement => {
  const found = [...document.querySelectorAll<HTMLElement>(sel)].find((n) => n.textContent?.trim() === text);
  if (!found) throw new Error(`no ${sel} with text ${text}; have ${document.body.textContent}`);
  return found;
};
const maybe = (sel: string, text: string): HTMLElement | undefined =>
  [...document.querySelectorAll<HTMLElement>(sel)].find((n) => n.textContent?.trim() === text);

describe("VIP and Block on the sender card (#59)", () => {
  let root: Root;
  let shown: EffectiveEmail;

  async function open(): Promise<void> {
    flushSync(() => root.render(<SenderPopover email={shown} />));
    await click(document.querySelector("button.message-from-button") as Element);
  }

  beforeEach(() => {
    undos.length = 0;
    toasts.length = 0;
    for (const f of [setContactMarkers, moveToJunk, trash, allMatchingIds, loadEmails]) f.mockClear();
    contact = { id: "C1", displayName: "Pest", addresses: [{ address: ADDRESS, reachedMeAt: null }], isVip: false, isBlocked: false, blockDestination: "junk", blockedAt: null };
    store.setSession({ capabilities: {} } as never, "U1");
    store.putMailboxes([mailbox("INBOX", "inbox"), mailbox("JUNK", "junk"), mailbox("BIN", "trash")]);
    server.length = 0;
    for (let i = 0; i < 3; i++) server.push(email(i));
    store.putEmails(server.slice(0, 1));
    shown = effectiveEmail(store.getState(), "p0") as EffectiveEmail;
    const host = document.createElement("div");
    document.body.replaceChildren(host);
    root = createRoot(host);
  });
  afterEach(() => {
    flushSync(() => root.unmount());
  });

  it("shows no markers against a server that sends none", async () => {
    contact = { id: "C1", displayName: "Pest", addresses: [{ address: ADDRESS, reachedMeAt: null }] };
    await open();
    expect(maybe("button", "VIP")).toBeUndefined();
    expect(maybe("button", `Block ${ADDRESS}…`)).toBeUndefined();
  });

  it("VIP is a pressed toggle that writes the marker", async () => {
    await open();
    const vip = byText("button", "VIP");
    expect(vip.getAttribute("aria-pressed")).toBe("false");
    await click(vip);
    expect(setContactMarkers).toHaveBeenCalledWith("C1", { isVip: true }, expect.any(String));
    expect(byText("button", "VIP").getAttribute("aria-pressed")).toBe("true");
  });

  it("blocks to Junk by default and moves NOTHING unless asked; Undo unblocks", async () => {
    await open();
    await click(byText("button", `Block ${ADDRESS}…`));
    const form = document.querySelector("form.sender-block-form") as HTMLFormElement;
    const junk = form.querySelector('input[value="junk"]') as HTMLInputElement;
    const move = form.querySelector('input[type="checkbox"]') as HTMLInputElement;
    expect(junk.checked).toBe(true);
    expect(move.checked).toBe(false);
    await click(byText("button", "Block"));
    expect(setContactMarkers).toHaveBeenCalledWith("C1", { isBlocked: true, blockDestination: "junk" }, expect.any(String));
    expect(allMatchingIds).not.toHaveBeenCalled();
    expect(moveToJunk).not.toHaveBeenCalled();
    expect(undos.map((u) => u.label)).toEqual([`Blocked ${ADDRESS}. New mail from them goes to Junk.`]);
    // Blocked now: the card says so and offers Unblock.
    expect(document.body.textContent).toContain("Blocked: new mail from this address goes to Junk.");
    expect(maybe("button", `Unblock ${ADDRESS}`)).toBeDefined();
    await undos[0]!.inverse();
    expect(setContactMarkers).toHaveBeenLastCalledWith("C1", { isBlocked: false }, expect.any(String));
  });

  it.each(["example.com", "@example.com", "*.example.com", ".example.com"])(
    "a From of %s offers no sender-wide move: from: would read it as a whole domain",
    async (addr) => {
      server.length = 0;
      server.push({ ...email(9), from: [{ name: "Odd", email: addr }] } as Email);
      store.putEmails(server);
      shown = effectiveEmail(store.getState(), "p9") as EffectiveEmail;
      contact = { ...(contact as Contact), addresses: [{ address: addr, reachedMeAt: null }] };
      await open();
      expect(document.body.textContent).not.toContain(`Move all from ${addr}`);
      await click(byText("button", `Block ${addr}…`));
      expect(document.body.textContent).not.toContain("Also move the mail already here");
    },
  );

  it("with the box ticked, counts and confirms in the card before moving to Trash", async () => {
    await open();
    await click(byText("button", `Block ${ADDRESS}…`));
    const form = document.querySelector("form.sender-block-form") as HTMLFormElement;
    await click(form.querySelector('input[value="trash"]') as Element);
    await click(form.querySelector('input[type="checkbox"]') as Element);
    await click(byText("button", "Block"));
    expect(setContactMarkers).toHaveBeenCalledWith("C1", { isBlocked: true, blockDestination: "trash" }, expect.any(String));
    expect(allMatchingIds).toHaveBeenCalledTimes(1);
    expect(allMatchingIds.mock.calls[0]![0]).toEqual({ kind: "search", query: `from:${ADDRESS}` });
    // Nothing has moved yet: the count is the question.
    expect(trash).not.toHaveBeenCalled();
    expect(document.body.textContent).toContain(`Move all 3 messages from ${ADDRESS} to Trash?`);
    await click(byText("button", "Move to Trash"));
    expect(trash).toHaveBeenCalledWith(["p0", "p1", "p2"]);
  });
});
