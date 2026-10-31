// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// RESUMING A PLAIN-TEXT DRAFT (#871). The server lists a lone text/plain part in
// htmlBody as well as textBody (RFC 8621 4.1.4); compose read that as "this draft
// has HTML" and opened it in rich mode with its text treated as markup, which
// collapsed its line breaks. A plain draft must open in plain mode with the
// newlines in the textarea.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const identity = { id: "I1", accountId: "A1", email: "me@example.com", name: "Me", isWildcard: false };
const plainPart = { partId: "1", blobId: "B1", size: 20, name: null, type: "text/plain", charset: null, disposition: null, cid: null };
const htmlPart = { ...plainPart, partId: "2", blobId: "B2", type: "text/html" };
const values = {
  "1": { value: "line one\nline two", isEncodingProblem: false, isTruncated: false },
  "2": { value: "<p>rich draft</p>", isEncodingProblem: false, isTruncated: false },
};
const base = {
  id: "D1",
  threadId: "T1",
  mailboxIds: {},
  keywords: { $draft: true },
  from: [{ name: "Me", email: "me@example.com" }],
  to: [{ name: null, email: "friend@example.net" }],
  cc: [],
  bcc: [],
  subject: "notes",
  attachments: [],
  bodyValues: values,
};

const actions = {
  sendMessage: vi.fn(),
  discardDraft: vi.fn(async () => true),
  saveDraft: vi.fn(async () => null),
  cancelSubmission: vi.fn(async () => true),
  releaseAttachment: vi.fn(),
};
const mailState = {
  identities: { I1: identity },
  mailboxes: {},
  emails: {},
  composeSnippets: [],
  session: null,
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

import { Compose } from "./Compose";

let root: Root;
let host: HTMLElement;

async function mount(draft: unknown): Promise<void> {
  await act(async () => {
    root.render(<Compose intent={{ kind: "draft", draft: draft as never }} placement="full" scope={null} onClose={vi.fn()} />);
  });
}

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
});

const textareaValues = (): string[] => Array.from(host.querySelectorAll("textarea")).map((t) => t.value);

describe("resuming a plain-text draft", () => {
  it("opens in plain mode with the newlines in the textarea", async () => {
    await mount({ ...base, textBody: [plainPart], htmlBody: [plainPart] });
    expect(textareaValues()).toContain("line one\nline two");
  });

  it("control: a draft with a text/html part still opens in rich mode (no body textarea)", async () => {
    await mount({ ...base, textBody: [plainPart], htmlBody: [htmlPart] });
    expect(textareaValues()).not.toContain("line one\nline two");
  });
});
