// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// REPLIES AND FORWARDS QUOTE WITH FORMATTING (ADR-051 §7, #92), through the real
// composer and the real editor: what the opening document holds, what a send and a
// saved draft carry, and what stays exactly as it was (a plain original, plain-text
// mode, the signature's place, an encrypted original).

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
vi.setConfig({ testTimeout: 60_000 });

const identity = { id: "I1", accountId: "A1", email: "me@example.com", name: "Me", isWildcard: false };
const mailState: { identities: Record<string, unknown>; mailboxes: object; emails: object; composeSnippets: unknown[]; session: null } = {
  identities: { I1: identity },
  mailboxes: {},
  emails: {},
  composeSnippets: [],
  session: null,
};
const actions = {
  sendMessage: vi.fn(async () => null),
  discardDraft: vi.fn(async () => true),
  saveDraft: vi.fn(async () => ({ ok: false as const, reason: "test", retiredPrevious: false })),
  cancelSubmission: vi.fn(async () => true),
  releaseAttachment: vi.fn(),
};
const client = {
  hasCapability: () => false,
  pgpSigning: () => null,
  pgpEncrypt: () => null,
  pgpKeyExpiryWarningDays: () => null,
  recipientKeyLookup: async () => null,
  contactForAddress: async () => null,
};
vi.mock("../state", () => ({
  useApp: () => ({ actions, client, pushToast: vi.fn(), offerUndo: vi.fn() }),
  useMailState: () => mailState,
}));
vi.mock("./useListVerbs", () => ({ requestPaneLeave: vi.fn() }));

import { Compose, formattedQuoteOf, openingBodies, quotedText } from "./Compose";
import { t } from "../i18n";

const part = (over: Record<string, unknown>) => ({
  partId: "1", blobId: "B1", size: 100, name: null, type: "text/html", charset: null, disposition: null, cid: null, ...over,
});
const htmlPart = part({ partId: "h", blobId: "BH", type: "text/html" });
const textPart = part({ partId: "t", blobId: "BT", type: "text/plain" });

const HTML = '<p>Hello <strong>Bob</strong>, see <a href="https://example.com/doc">the doc</a>.</p><ul><li>one</li><li>two</li></ul>';

function parentOf(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "P1",
    threadId: "T1",
    mailboxIds: {},
    keywords: {},
    isSeen: true,
    messageId: ["<p1@example.com>"],
    references: [],
    from: [{ name: "Alice", email: "alice@example.net" }],
    to: [{ name: null, email: "me@example.com" }],
    cc: [],
    subject: "the plan",
    receivedAt: "2026-03-02T10:00:00Z",
    attachments: [],
    textBody: [textPart],
    htmlBody: [htmlPart],
    bodyValues: {
      h: { value: HTML, isEncodingProblem: false, isTruncated: false },
      t: { value: "Hello Bob, see the doc.\n* one\n* two", isEncodingProblem: false, isTruncated: false },
    },
    ...over,
  };
}

let root: Root;
let host: HTMLElement;

const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 15));
/** Waits for a condition, polling; never a fixed sleep. */
async function until(cond: () => boolean, what: string): Promise<void> {
  for (let i = 0; i < 1500; i += 1) {
    if (cond()) return;
    await act(async () => settle());
  }
  throw new Error(`timed out waiting for ${what}`);
}

type Intent = "reply" | "replyAll" | "forward";
async function open(kind: Intent, parent: Record<string, unknown>): Promise<void> {
  await act(async () => {
    root.render(<Compose intent={{ kind, parent: parent as never }} placement="full" scope={null} onClose={vi.fn()} />);
  });
  await until(() => host.querySelector(".ProseMirror") !== null, "the editor");
}

/** What a Send would carry: the body HTML and the attachments. */
async function sent(): Promise<{ html: string; attachments: Array<Record<string, unknown>> }> {
  // Recipients: a forward has none yet.
  actions.sendMessage.mockClear();
  const form = host.querySelector("form") as HTMLFormElement;
  await act(async () => {
    form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  });
  const call = actions.sendMessage.mock.calls[0] as unknown as [{ html?: string; attachments?: Array<Record<string, unknown>> }] | undefined;
  if (!call) throw new Error("nothing was sent (the form refused)");
  return { html: call[0].html ?? "", attachments: call[0].attachments ?? [] };
}

const inert = (h: string): Document => new DOMParser().parseFromString(h, "text/html");
const header = (p: Record<string, unknown>): string =>
  t("compose.quoteHeader", { who: "Alice", date: new Date(p["receivedAt"] as string).toLocaleString() });

beforeEach(() => {
  vi.clearAllMocks();
  mailState.composeSnippets = [];
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
});

describe("an HTML original is quoted with its formatting", () => {
  for (const kind of ["reply", "replyAll", "forward"] as const) {
    it(`${kind}: the quote keeps bold, the link and the list, under the unchanged attribution`, async () => {
      const parent = parentOf();
      await open(kind, parent);
      if (kind === "forward") {
        // A forward has no recipient yet; the quote is what is under test.
        const surface = host.querySelector(".ProseMirror") as HTMLElement;
        expect(surface.querySelector("blockquote strong")?.textContent).toBe("Bob");
        expect(surface.querySelector('blockquote a[href="https://example.com/doc"]')).not.toBeNull();
        expect(surface.querySelectorAll("blockquote li")).toHaveLength(2);
        expect(surface.textContent).toContain(header(parent));
        return;
      }
      const { html } = await sent();
      const d = inert(html);
      expect(d.querySelector("blockquote strong")?.textContent).toBe("Bob");
      expect(d.querySelector('blockquote a[href="https://example.com/doc"]')).not.toBeNull();
      expect(d.querySelectorAll("blockquote li")).toHaveLength(2);
      expect(html).toContain(`<p>${header(parent)}</p><blockquote>`);
    });
  }

  it("the forward's subject and the reply's subject are unchanged", async () => {
    await open("forward", parentOf());
    expect((host.querySelector('input[type="text"][value="Fwd: the plan"], input[value="Fwd: the plan"]'))).not.toBeNull();
  });

  it("the quote stays last, below the signature (opening order unchanged)", () => {
    const parent = parentOf() as never;
    const q = formattedQuoteOf(parent);
    expect(q).not.toBeNull();
    const out = openingBodies(parent, { html: "<p>-- Me</p>", text: "-- Me" }, q);
    const i = (s: string): number => out.html.indexOf(s);
    expect(i("<p>-- Me</p>")).toBeGreaterThan(0);
    expect(i("<p>-- Me</p>")).toBeLessThan(i("<blockquote>"));
    expect(out.html.startsWith("<p><br></p><p>-- Me</p><p>On ") || out.html.startsWith("<p><br></p><p>-- Me</p><p>")).toBe(true);
    // One blank line to type in, not two.
    expect(out.html.match(/^<p><br><\/p><p>-- Me<\/p>/)).not.toBeNull();
    expect(out.html).not.toMatch(/<p><br><\/p><p>-- Me<\/p><p><br><\/p>/);
  });

  it("the picture note and the truncation note are our own words", async () => {
    const parent = parentOf({
      bodyValues: {
        h: { value: '<p>Look</p><img data-mailroom-remote-src="/jmap/proxy/A/M/t" data-mailroom-blocked="">', isEncodingProblem: false, isTruncated: true },
        t: { value: "Look", isEncodingProblem: false, isTruncated: false },
      },
    });
    await open("reply", parent);
    const { html } = await sent();
    expect(html).toContain(t("compose.quotePictureOmitted"));
    expect(html).toContain(t("compose.quoteRestOmitted"));
    expect(html).not.toContain("proxy");
  });
});

describe("what does not change", () => {
  it("a plain-only original keeps today's escaped plain-text quote", async () => {
    const parent = parentOf({
      textBody: [textPart],
      htmlBody: [textPart],
      bodyValues: { t: { value: "line <one>\nline two", isEncodingProblem: false, isTruncated: false } },
    });
    expect(formattedQuoteOf(parent as never)).toBeNull();
    const out = openingBodies(parent as never, null, null);
    expect(out.html).toBe(`<p><br></p><p>${header(parent)}</p><blockquote>line &lt;one&gt;<br>line two</blockquote>`);
    await open("reply", parent);
    const { html } = await sent();
    // Pinned as it is today: with no edit, the seed itself is what is sent.
    expect(html).toContain("<blockquote>line &lt;one&gt;<br>line two</blockquote>");
  });

  it("plain-text mode keeps the text quote, formatted original or not", () => {
    const parent = parentOf();
    const expected = `\n\n${header(parent)}\n> Hello Bob, see the doc.\n> * one\n> * two\n`;
    expect(quotedText(parent as never)).toBe(expected);
    expect(openingBodies(parent as never, null, formattedQuoteOf(parent as never)).text).toBe(expected);
    expect(openingBodies(parent as never, { html: "<p>s</p>", text: "-- s" }, formattedQuoteOf(parent as never)).text).toBe(`\n\n-- s${expected}`);
  });

  it("an HTML original with nothing left to quote falls back to its plain text", () => {
    const parent = parentOf({
      bodyValues: {
        h: { value: '<img data-mailroom-remote-src="/jmap/proxy/A/M/t" data-mailroom-blocked="">', isEncodingProblem: false, isTruncated: false },
        t: { value: "only text", isEncodingProblem: false, isTruncated: false },
      },
    });
    expect(formattedQuoteOf(parent as never)).toBeNull();
    expect(openingBodies(parent as never, null, null).html).toContain("<blockquote>only text</blockquote>");
  });

  describe("an encrypted original (ADR-037) is quoted exactly as before", () => {
    // PINNED against a literal, so that formatted quoting cannot change it unnoticed:
    // the decrypted body is quoted as its plain text, escaped, never as HTML, and no
    // part of it is carried as an attachment.
    const decrypted = (over: Record<string, unknown> = {}) =>
      parentOf({
        encrypted: true,
        decrypted: true,
        bodyValues: {
          h: { value: "<p><strong>secret</strong> <img src=\"cid:s@example.com\"></p>", isEncodingProblem: false, isTruncated: false },
          t: { value: "secret <plan>", isEncodingProblem: false, isTruncated: false },
        },
        attachments: [part({ partId: "i", blobId: "BI", type: "image/png", cid: "s@example.com" })],
        ...over,
      });

    for (const [name, over] of [
      ["decrypted", {}],
      ["could not be decrypted", { decrypted: false }],
      ["encrypted, no attempt made", { decrypted: null }],
    ] as const) {
      it(`${name}: no formatted quote, the old plain one, to the byte`, () => {
        const parent = decrypted(over) as never;
        expect(formattedQuoteOf(parent)).toBeNull();
        const out = openingBodies(parent, null, formattedQuoteOf(parent));
        expect(out.html).toBe(`<p><br></p><p>${header(decrypted(over))}</p><blockquote>secret &lt;plan&gt;</blockquote>`);
        expect(out.text).toBe(`\n\n${header(decrypted(over))}\n> secret <plan>\n`);
      });
    }

    it("through the composer: plain text in the quote, nothing carried, no picture", async () => {
      await open("reply", decrypted());
      const { html, attachments } = await sent();
      expect(html).toContain("<blockquote>secret &lt;plan&gt;</blockquote>");
      expect(html).not.toContain("<strong>");
      expect(html).not.toContain("cid:");
      expect(attachments).toEqual([]);
    });
  });
});

describe("pictures from the original", () => {
  const withCid = (over: Record<string, unknown> = {}) =>
    parentOf({
      attachments: [
        part({ partId: "i", blobId: "BI", type: "image/png", name: "logo.png", cid: "logo@example.com", size: 500, disposition: "inline" }),
        part({ partId: "f", blobId: "BF", type: "application/pdf", name: "plan.pdf", size: 900, disposition: "attachment" }),
      ],
      bodyValues: {
        h: { value: '<p>Hi</p><p><img src="cid:logo@example.com" alt="Logo"></p>', isEncodingProblem: false, isTruncated: false },
        t: { value: "Hi", isEncodingProblem: false, isTruncated: false },
      },
      ...over,
    });

  it("an inline picture listed in htmlBody (RFC 8621: not in attachments) is carried too", async () => {
    const parent = withCid({
      attachments: [],
      htmlBody: [htmlPart, part({ partId: "i", blobId: "BI", type: "image/png", name: "logo.png", cid: "logo@example.com", size: 500, disposition: "inline" })],
    });
    await open("reply", parent);
    const { attachments } = await sent();
    expect(attachments).toEqual([
      { blobId: "BI", type: "image/png", name: "logo.png", cid: "logo@example.com", disposition: "inline" },
    ]);
  });

  it("a cid: picture the original lists is carried as the reply's own inline attachment", async () => {
    await open("reply", withCid());
    const { html, attachments } = await sent();
    expect(inert(html).querySelector("blockquote img")?.getAttribute("src")).toBe("cid:logo@example.com");
    expect(attachments).toEqual([
      { blobId: "BI", type: "image/png", name: "logo.png", cid: "logo@example.com", disposition: "inline" },
    ]);
  });

  it("a forward carries the file as before, and the inline picture once (not also as a file)", async () => {
    await open("forward", withCid());
    // A forward has no recipient: give it one the way a person would not need to, by
    // reading the attachments the composer WOULD send from the tiles it shows.
    const names = Array.from(host.querySelectorAll("li, .tile, [data-tile]")).map((e) => e.textContent ?? "").join("|");
    expect(names).toContain("plan.pdf");
    expect(names).not.toContain("logo.png");
  });

  it("with the picture deleted from the body, it is not sent (nothing orphaned travels)", async () => {
    await open("reply", withCid());
    const surface = host.querySelector(".ProseMirror") as HTMLElement & { editor?: { commands: { setContent: (h: string, opts?: { emitUpdate?: boolean }) => boolean } } };
    expect(surface.editor).toBeDefined();
    expect((await sent()).attachments).toHaveLength(1); // control: it travels while the body names it
    await act(async () => {
      surface.editor?.commands.setContent("<p>my reply, quote deleted</p>", { emitUpdate: true });
    });
    expect((await sent()).attachments).toEqual([]);
  });

  it("a withheld picture is dropped with the note", async () => {
    const parent = withCid({ attachmentScan: { parts: {}, quarantined: ["i"] } });
    await open("reply", parent);
    const { html, attachments } = await sent();
    expect(html).not.toContain("cid:");
    expect(html).toContain(t("compose.quotePictureOmitted"));
    expect(attachments).toEqual([]);
  });
});

describe("a saved draft of a quote", () => {
  it("is what the editor made of it, and reopens as the same document", async () => {
    const parent = parentOf();
    await open("reply", parent);
    // Touch the composer as a person would (an input), then close it: the unmount
    // flush saves.
    const subject = host.querySelector('input[value="Re: the plan"]') as HTMLInputElement;
    await act(async () => {
      subject.dispatchEvent(new Event("input", { bubbles: true }));
    });
    await act(async () => root.unmount());
    expect(actions.saveDraft).toHaveBeenCalled();
    const saved = (actions.saveDraft.mock.calls[0] as unknown as [{ html: string }])[0].html;
    const d = inert(saved);
    expect(d.querySelector("blockquote strong")?.textContent).toBe("Bob");
    expect(saved).not.toContain("xmlns");
    root = createRoot(host); // so afterEach can unmount
  });
});
