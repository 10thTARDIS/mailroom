// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// A DESTROYED EDITOR HANDED TO THE COMPOSER. Tiptap can hand over an instance it
// has already destroyed while it rebuilds one (an identity change remounts the
// editor as a reply opens). The composer read it to normalise the quote seed, which
// threw "Cannot read properties of null (reading 'cached')" and took the reply
// composer down intermittently. Here a stand-in editor hands over a destroyed
// instance first and a live one after: the composer must survive and send.

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


// The stand-in: the real Tiptap Editor class, so "destroyed" is the real thing.
vi.mock("./editor/LazyRichEditor", async () => {
  const { useEffect } = await import("react");
  const { Editor } = await import("@tiptap/core");
  const { default: StarterKit } = await import("@tiptap/starter-kit");
  return {
    RichEditor: (props: { initialHTML: string; onReady?: (e: unknown) => void }) => {
      useEffect(() => {
        const dead = new Editor({ extensions: [StarterKit], content: props.initialHTML });
        dead.destroy();
        props.onReady?.(dead);
        const live = new Editor({ extensions: [StarterKit], content: props.initialHTML });
        props.onReady?.(live);
        return () => live.destroy();
        // eslint-disable-next-line react-hooks/exhaustive-deps
      }, []);
      return <div className="ProseMirror" contentEditable="true" />;
    },
  };
});

import { Compose } from "./Compose";

const part = (over: Record<string, unknown>) => ({
  partId: "1", blobId: "B1", size: 100, name: null, type: "text/html", charset: null, disposition: null, cid: null, ...over,
});
const parent = {
  id: "P1", threadId: "T1", mailboxIds: {}, keywords: {}, isSeen: true,
  messageId: ["p1@example.com"], references: null,
  from: [{ name: "Alice", email: "alice@example.net" }], to: [{ name: null, email: "me@example.com" }], cc: [],
  subject: "the plan", receivedAt: "2026-03-02T10:00:00Z", attachments: [],
  textBody: [part({ partId: "t", type: "text/plain" })], htmlBody: [part({ partId: "h" })],
  bodyValues: {
    h: { value: "<p>Hello <strong>Bob</strong></p>", isEncodingProblem: false, isTruncated: false },
    t: { value: "Hello Bob", isEncodingProblem: false, isTruncated: false },
  },
};

let root: Root;
let host: HTMLElement;
const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 15));
async function until(cond: () => boolean, what: string): Promise<void> {
  for (let i = 0; i < 400; i += 1) {
    if (cond()) return;
    await act(async () => settle());
  }
  throw new Error(`timed out waiting for ${what}`);
}

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  actions.sendMessage.mockClear();
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
});

describe("a destroyed editor handed to the reply composer", () => {
  it("is ignored: the composer survives, and sends once the live editor arrives", async () => {
    await act(async () => {
      root.render(<Compose intent={{ kind: "reply", parent: parent as never }} placement="full" scope={null} onClose={vi.fn()} />);
    });
    await until(() => host.querySelector(".ProseMirror") !== null, "the editor");
    const form = host.querySelector("form") as HTMLFormElement;
    await until(() => !(host.querySelector('button[type="submit"]') as HTMLButtonElement | null)?.disabled, "Send to be enabled");
    await act(async () => {
      form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    });
    expect(actions.sendMessage).toHaveBeenCalledTimes(1);
    const html = (actions.sendMessage.mock.calls[0] as unknown as [{ html?: string }])[0].html ?? "";
    expect(html).toContain("Bob");
  });
});
