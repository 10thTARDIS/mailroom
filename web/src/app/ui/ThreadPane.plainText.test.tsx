// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// A PLAIN-TEXT MESSAGE KEEPS ITS LINE BREAKS (#871), rendered for real.
//
// The server lists a lone text/plain part in textBody AND htmlBody (RFC 8621
// 4.1.4). The pane used to treat any non-empty htmlBody as HTML and hand the text
// to the sandboxed frame, where every line break collapsed.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const client = {
  fetchInlineImage: vi.fn(async () => new Blob([])),
  imageProxyEnabled: () => false,
  blobUrl: (blobId: string) => `/jmap/download/A/${blobId}`,
  fetchProxiedImage: vi.fn(async () => null),
};
const sync = { loadThread: vi.fn(async () => undefined) };
const actions = new Proxy({}, { get: () => vi.fn(async () => true) });

function part(partId: string, type: string) {
  return { partId, blobId: `B.${partId}`, size: 20, name: null, type, charset: null, disposition: null, cid: null };
}
const plain = part("1", "text/plain");
const html = part("2", "text/html");
let current: unknown;

function stateFor(email: unknown) {
  return {
    emails: { M1: email },
    threads: { T1: { id: "T1", emailIds: ["M1"] } },
    overlay: {},
    mailboxes: {},
    labels: {},
    areas: {},
    remoteAllows: {},
    settings: {},
    identities: {},
    session: null,
  };
}

vi.mock("../state", () => ({
  useApp: () => ({ client, actions, sync, pushToast: vi.fn() }),
  useMailState: () => stateFor(current),
}));
vi.mock("./shellContext", () => ({ useShell: () => ({ openViewer: vi.fn(), paneLayout: "right" }) }));

import { effectiveEmail } from "../../core/store";
import { ThreadPane } from "./ThreadPane";

function email(extra: Record<string, unknown>) {
  return {
    id: "M1",
    threadId: "T1",
    receivedAt: "2026-10-01T10:00:00Z",
    subject: "hello",
    from: [{ name: "Ann", email: "ann@example.net" }],
    to: [],
    cc: [],
    keywords: { $seen: true },
    mailboxIds: {},
    attachments: [],
    preview: "preview",
    ...extra,
  };
}

let host: HTMLElement;
let root: Root;
const noop = () => undefined;

async function mount(e: unknown): Promise<void> {
  current = e;
  const eff = effectiveEmail(stateFor(e) as never, "M1");
  await act(async () => {
    root.render(
      <ThreadPane
        email={eff as never}
        onClose={noop}
        onReply={noop}
        onReplyAll={noop}
        onForward={noop}
        onArchive={noop}
        onNotJunk={noop}
        onTrash={noop}
        onTray={noop}
        onPin={noop}
        onPrint={noop}
        onMute={noop}
        onSnooze={noop}
        onImportant={noop}
        onCreateRule={noop}
        onLabel={noop}
        onDismiss={noop}
      />,
    );
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

describe("reading a plain-text message", () => {
  it("renders pre.message-text with its newline and no HTML frame", async () => {
    await mount(
      email({
        textBody: [plain],
        htmlBody: [plain],
        bodyValues: { "1": { value: "line one\nline two", isEncodingProblem: false, isTruncated: false } },
      }),
    );
    const pre = host.querySelector("pre.message-text");
    expect(pre).not.toBeNull();
    expect(pre?.textContent).toBe("line one\nline two");
    expect(host.querySelector("iframe")).toBeNull();
  });

  it("control: an alternative message still renders the HTML frame", async () => {
    await mount(
      email({
        textBody: [plain],
        htmlBody: [html],
        bodyValues: {
          "1": { value: "line one\nline two", isEncodingProblem: false, isTruncated: false },
          "2": { value: "<p>rich</p>", isEncodingProblem: false, isTruncated: false },
        },
      }),
    );
    expect(host.querySelector("iframe")).not.toBeNull();
    expect(host.querySelector("pre.message-text")).toBeNull();
  });
});
