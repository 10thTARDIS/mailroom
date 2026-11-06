// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE HOSTILE-ORIGINAL SUITE (ADR-051 §7). The hostile-paste payloads, and the
// ones only a quote has (parked and proxied pictures, a cid: with no part, a
// scripted link, a form, an iframe srcdoc, a style block, a base, a refresh), are
// delivered as RECEIVED MAIL: built into a message, parsed, and run through the
// server's real reader sanitiser (internal/api/sanitize_quote_received_test.go
// writes the fixture). The reply is then opened in the real composer. Asserted,
// for the live editor, the editor's getHTML(), what a send carries and what a
// saved draft carries:
//
//   * nothing ran (a flag every payload with a handler would set stays false,
//     after the events a browser would fire on everything that came out);
//   * nothing loaded (no fetch, XHR or beacon, and no resource element pointing
//     off-host, observed as it happens);
//   * none of the payloads are in the document.
//
// THE ORACLE CAN FAIL, and the flag can be tripped: the negative controls put the
// same payloads into the live page by the route the composer never takes, and a
// planted payload into a clean document. Without them "nothing ran" is also what
// a harness that cannot run handlers reports.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";
import { RECEIVED, SOURCES, staleness } from "./quoteTestkit";
import type { ReceivedCase } from "./quoteTestkit";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
vi.setConfig({ testTimeout: 60_000 });

const identity = { id: "I1", accountId: "A1", email: "me@example.com", name: "Me", isWildcard: false };
const mailState = { identities: { I1: identity }, mailboxes: {}, emails: {}, composeSnippets: [], session: null };
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
vi.mock("../../state", () => ({
  useApp: () => ({ actions, client, pushToast: vi.fn(), offerUndo: vi.fn() }),
  useMailState: () => mailState,
}));
vi.mock("../useListVerbs", () => ({ requestPaneLeave: vi.fn() }));

import { Compose } from "../Compose";
import { formatQuote } from "./quote";
import { NOTES } from "./quoteTestkit";

const pwned = (): boolean => document.title === "pwned";
const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 15));

async function until(cond: () => boolean, what: string): Promise<void> {
  for (let i = 0; i < 1500; i += 1) {
    if (cond()) return;
    await act(async () => settle());
  }
  throw new Error(`timed out waiting for ${what}`);
}

/** Dispatch the events a browser would, on every element under `root` (and root). */
function fireEverything(root: Element): void {
  for (const el of [root, ...Array.from(root.querySelectorAll("*"))]) {
    for (const type of ["error", "load", "click", "mouseover", "focus", "focusin", "toggle"]) {
      el.dispatchEvent(new Event(type, { bubbles: false, cancelable: true }));
    }
  }
}

/** The forbidden route: markup parsed in the LIVE document, handlers armed. */
function live(context: Element, html: string): DocumentFragment {
  const range = document.createRange();
  range.selectNodeContents(context);
  return range.createContextualFragment(html);
}

const inert = (h: string): Document => new DOMParser().parseFromString(h, "text/html");

// ---- the request observer ----------------------------------------------------------

const RESOURCE_TAGS = "img, source, link, script, iframe, video, audio, object, embed, input, track";
const OFF_HOST = /^\s*(https?:)?\/\//i;

interface Observer {
  requests: string[];
  stop: () => void;
}

/** Records every fetch, XHR and beacon, and every resource element that comes to
 * point at another host, as it happens (a MutationObserver sees an attribute set
 * on a live element whether or not the test environment would then load it). */
function observeRequests(): Observer {
  const requests: string[] = [];
  const realFetch = globalThis.fetch;
  const realOpen = XMLHttpRequest.prototype.open;
  const realBeacon = (navigator as { sendBeacon?: unknown }).sendBeacon;
  globalThis.fetch = ((input: RequestInfo | URL) => {
    requests.push(`fetch ${String(input)}`);
    return Promise.reject(new Error("blocked by the test"));
  }) as typeof fetch;
  XMLHttpRequest.prototype.open = function (this: XMLHttpRequest, method: string, url: string | URL): void {
    requests.push(`xhr ${method} ${String(url)}`);
  } as typeof XMLHttpRequest.prototype.open;
  (navigator as { sendBeacon?: unknown }).sendBeacon = (url: string) => {
    requests.push(`beacon ${url}`);
    return false;
  };
  const note = (el: Element): void => {
    if (!el.matches(RESOURCE_TAGS)) return;
    for (const a of ["src", "srcset", "href", "data", "poster", "background"]) {
      const v = el.getAttribute(a);
      if (v && OFF_HOST.test(v)) requests.push(`${el.localName}[${a}] ${v}`);
    }
  };
  const mo = new MutationObserver((records) => {
    for (const r of records) {
      if (r.type === "attributes" && r.target instanceof Element) note(r.target);
      r.addedNodes.forEach((n) => {
        if (n instanceof Element) {
          note(n);
          n.querySelectorAll(RESOURCE_TAGS).forEach(note);
        }
      });
    }
  });
  mo.observe(document, { subtree: true, childList: true, attributes: true });
  return {
    requests,
    stop: () => {
      mo.disconnect();
      globalThis.fetch = realFetch;
      XMLHttpRequest.prototype.open = realOpen;
      (navigator as { sendBeacon?: unknown }).sendBeacon = realBeacon;
    },
  };
}

// ---- the oracle -----------------------------------------------------------------------

const DANGEROUS = "script, style, iframe, object, embed, form, input, textarea, select, button, svg, math, meta, link, base, frame, video, audio, source";
const PAYLOAD_TEXT = /evil\.example|tracker\.example|javascript:|pwned|\/jmap\/proxy\/|data-mailroom|srcdoc|http-equiv|\bon[a-z]+\s*=|url\(|expression\(|<(script|style|iframe|object|embed|form|input|svg|meta|base)\b/i;

/** Everything that must not be in a document the quote produced. Written out here,
 * independently of the code under test. */
function assertQuoteClean(root: ParentNode, where: string): void {
  expect(root.querySelectorAll(DANGEROUS).length, `${where}: dangerous element`).toBe(0);
  for (const el of Array.from(root.querySelectorAll("*"))) {
    for (const attr of Array.from(el.attributes)) {
      expect(attr.name.startsWith("on"), `${where}: ${el.tagName} has ${attr.name}`).toBe(false);
      expect(attr.name.startsWith("data-mailroom"), `${where}: ${el.tagName} has ${attr.name}`).toBe(false);
      expect(["srcset", "srcdoc", "formaction", "background"], `${where}: ${el.tagName} has ${attr.name}`).not.toContain(attr.name);
      if (attr.name === "href") expect(attr.value, `${where}: href`).toMatch(/^(https?:|mailto:)/i);
      if (attr.name === "src") expect(attr.value, `${where}: src`).toMatch(/^(cid:|data:image\/)/i);
      if (attr.name === "style") expect(attr.value, `${where}: style`).not.toMatch(/url\(|expression|position|@import|evil/i);
    }
  }
  const ser = new XMLSerializer();
  const markup = Array.from(root.childNodes)
    .map((n) => ser.serializeToString(n))
    .join("")
    .replace(/ class="ProseMirror-[a-z]+"/g, "");
  expect(markup, `${where}: markup`).not.toMatch(PAYLOAD_TEXT);
}

// ---- composer plumbing -------------------------------------------------------------

let root: Root;
let host: HTMLElement;
let obs: Observer;

beforeEach(() => {
  vi.clearAllMocks();
  document.title = "calm";
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  obs = observeRequests();
});
afterEach(async () => {
  obs.stop();
  await act(async () => root.unmount());
  host.remove();
});

function parentWith(html: string, over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "P1", threadId: "T1", mailboxIds: {}, keywords: {}, isSeen: true, messageId: ["<p1@example.com>"], references: [],
    from: [{ name: "Alice", email: "alice@example.net" }], to: [{ name: null, email: "me@example.com" }], cc: [],
    subject: "hostile", receivedAt: "2026-03-02T10:00:00Z", attachments: [],
    textBody: [{ partId: "t", blobId: "BT", size: 5, name: null, type: "text/plain", charset: null, disposition: null, cid: null }],
    htmlBody: [{ partId: "h", blobId: "BH", size: 5, name: null, type: "text/html", charset: null, disposition: null, cid: null }],
    bodyValues: {
      h: { value: html, isEncodingProblem: false, isTruncated: false },
      t: { value: "text", isEncodingProblem: false, isTruncated: false },
    },
    ...over,
  };
}

async function openReply(parent: Record<string, unknown>, kind: "reply" | "replyAll" = "reply"): Promise<void> {
  await act(async () => {
    root.render(<Compose intent={{ kind, parent: parent as never }} placement="full" scope={null} onClose={vi.fn()} />);
  });
  await until(() => host.querySelector(".ProseMirror") !== null, "the editor");
}

async function sendHtml(): Promise<string> {
  actions.sendMessage.mockClear();
  await act(async () => {
    (host.querySelector("form") as HTMLFormElement).dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  });
  const call = actions.sendMessage.mock.calls[0] as unknown as [{ html?: string }] | undefined;
  if (!call) throw new Error("nothing was sent");
  return call[0].html ?? "";
}

/** The HTML a saved draft would carry: touched as a person would, then closed (the
 * unmount flush saves). Null when no save was made. */
async function savedHtml(): Promise<string | null> {
  actions.saveDraft.mockClear();
  const surface = host.querySelector(".ProseMirror") as HTMLElement;
  await act(async () => {
    surface.dispatchEvent(new Event("input", { bubbles: true }));
  });
  await act(async () => root.unmount());
  root = createRoot(host);
  const call = actions.saveDraft.mock.calls[0] as unknown as [{ html?: string }] | undefined;
  return call ? (call[0].html ?? "") : null;
}

// ---- the suite ----------------------------------------------------------------------

const HOSTILE = RECEIVED.filter((c) => c.group === "hostile");

describe("the fixture is what it claims to be", () => {
  it("is not stale against its sources (the half the Go test cannot check)", () => {
    expect(staleness()).toEqual([]);
  });

  it("holds every hand-written source, hostile ones included, and the formats", () => {
    expect(SOURCES.length).toBeGreaterThanOrEqual(24);
    expect(HOSTILE.length).toBeGreaterThanOrEqual(20);
    expect(RECEIVED.filter((c) => c.group === "format").length).toBeGreaterThan(30);
  });

  it("the reader's output is what the quote's rules are about: parked, proxied, srcset-free", () => {
    const remote = RECEIVED.find((c) => c.name === "remote image") as ReceivedCase;
    expect(remote.html).toMatch(/data-mailroom-remote-src="\/jmap\/proxy\//);
    expect(remote.html).not.toContain(" src=");
  });
});

describe("the harness can see a handler run and a request go out (negative controls)", () => {
  for (const c of RECEIVED.filter((x) => x.live)) {
    it(`${c.name}: the SOURCE put in the live page by the forbidden route trips the flag`, async () => {
      const box = document.createElement("div");
      document.body.appendChild(box);
      box.appendChild(live(box, c.source));
      fireEverything(box);
      await settle();
      box.remove();
      expect(pwned()).toBe(true);
    });
  }

  it("the request observer sees a fetch, an XHR, a beacon and a remote picture", async () => {
    await fetch("https://evil.example/a").catch(() => undefined);
    new XMLHttpRequest().open("GET", "https://evil.example/b");
    navigator.sendBeacon("https://evil.example/c");
    const box = document.createElement("div");
    document.body.appendChild(box);
    const img = document.createElement("img");
    box.appendChild(img);
    img.setAttribute("src", "https://evil.example/d.png");
    await settle();
    box.remove();
    expect(obs.requests.map((r) => r.split(" ")[0])).toEqual(expect.arrayContaining(["fetch", "xhr", "beacon", "img[src]"]));
  });

  it("the cleanliness oracle can fail: each payload, planted in a clean document, is reported", () => {
    const planted = [
      '<img src="https://evil.example/t.png">',
      '<img src="cid:a@x" onerror="x()">',
      '<a href="javascript:x()">x</a>',
      "<iframe></iframe>",
      '<p style="background:url(https://evil.example/x)">x</p>',
      '<img data-mailroom-remote-src="/jmap/proxy/A/M/t">',
      "<p>see /jmap/proxy/A/M/t</p>",
      "<p>pwned</p>",
    ];
    const clean = document.createElement("div");
    clean.appendChild(live(clean, "<p>clean</p>"));
    expect(() => assertQuoteClean(clean, "control")).not.toThrow();
    for (const html of planted) {
      const box = document.createElement("div");
      box.appendChild(live(box, `<p>clean</p>${html}`));
      expect(() => assertQuoteClean(box, "control"), html).toThrow();
    }
  });
});

describe("replying to a hostile original", () => {
  for (const c of HOSTILE) {
    it(`${c.name}: nothing runs, nothing loads, nothing dangerous is in the quote, the send or the saved draft`, async () => {
      await openReply(parentWith(c.html));
      const surface = host.querySelector(".ProseMirror") as HTMLElement;
      // Control: the quote really is there (a suite that quotes nothing proves nothing).
      expect(surface.querySelector("blockquote"), "the reply opened with a quote").not.toBeNull();
      fireEverything(surface);
      await settle();

      expect(pwned()).toBe(false);
      assertQuoteClean(surface, "live editor");

      const sent = await sendHtml();
      assertQuoteClean(inert(sent).body, "the send");

      const saved = await savedHtml();
      expect(saved, "a save was made").not.toBeNull();
      assertQuoteClean(inert(saved as string).body, "the saved draft");

      expect(obs.requests, "no request went out").toEqual([]);
    });
  }

  it("the layout and format fixtures are clean too (the same oracle over everything the reader produced)", async () => {
    for (const c of RECEIVED.filter((x) => x.group !== "hostile")) {
      const q = formatQuote({ html: c.html, truncated: false, parts: [], withheld: () => false, canCarry: true, notes: NOTES });
      const body = inert(`<blockquote>${q.html}</blockquote>`).body;
      assertQuoteClean(body, c.name);
    }
    expect(obs.requests).toEqual([]);
  });

  it("control: a hostile original's TEXT is still quoted, so the suite is not passing because everything is dropped", async () => {
    const c = HOSTILE.find((x) => x.name === "remote image") as ReceivedCase;
    await openReply(parentWith(c.html));
    const sent = await sendHtml();
    expect(inert(sent).querySelector("blockquote")?.textContent).toContain("before");
    expect(inert(sent).querySelector("blockquote")?.textContent).toContain("after");
    expect(sent).toContain(NOTES.picture);
  });

  it("control: a cid: picture the original DOES list is kept, clean, and carried", async () => {
    const html = '<p>pic</p><img src="cid:a@inline.mailroom" onerror="document.title=\'pwned\'" onload="document.title=\'pwned\'">';
    const parent = parentWith(html, {
      attachments: [{ partId: "i", blobId: "BI", size: 10, name: "a.png", type: "image/png", charset: null, disposition: "inline", cid: "a@inline.mailroom" }],
    });
    await openReply(parent);
    const surface = host.querySelector(".ProseMirror") as HTMLElement;
    fireEverything(surface);
    await settle();
    expect(pwned()).toBe(false);
    expect(surface.querySelector("blockquote img:not(.ProseMirror-separator)")?.getAttribute("src")).toBe("cid:a@inline.mailroom");
    assertQuoteClean(surface, "live editor");
    expect(obs.requests).toEqual([]);
  });
});
