// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// A SEND THE SERVER REFUSES (#692).
//
// `sendMessage` answers a refused submission (noRecipients, forbiddenFrom, ...)
// with `{emailId, submissionId: null}` after toasting the server's words. The
// composer treated any non-null answer as a send: it toasted "sent" beside the
// error, discarded the autosave and closed — losing what the user wrote. These
// mount the real composer over a stubbed `useApp` and press Send.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { SendOutcome } from "../../core/actions";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const identity = {
  id: "I1",
  accountId: "A1",
  email: "me@example.com",
  name: "Me",
  isWildcard: false,
};
const draft = {
  id: "D1",
  threadId: "T1",
  mailboxIds: {},
  keywords: { $draft: true },
  from: [{ name: "Me", email: "me@example.com" }],
  to: [{ name: null, email: "friend@example.net" }],
  cc: [],
  bcc: [],
  subject: "the invoice",
  attachments: [],
};

const actions = {
  sendMessage: vi.fn<() => Promise<SendOutcome | null>>(),
  discardDraft: vi.fn(async () => true),
  saveDraft: vi.fn(async () => null),
  cancelSubmission: vi.fn(async () => true),
  releaseAttachment: vi.fn(),
};
// ONE object across renders: a fresh one each call re-runs every memo keyed on it.
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
const pushToast = vi.fn();
const offerUndo = vi.fn();
const requestLeave = vi.fn();

vi.mock("../state", () => ({
  useApp: () => ({ actions, client, pushToast, offerUndo }),
  useMailState: () => mailState,
}));
vi.mock("./useListVerbs", () => ({ requestPaneLeave: (...a: unknown[]) => requestLeave(...a) }));

import { Compose } from "./Compose";

let root: Root;
let host: HTMLElement;
const onClose = vi.fn();

async function mount(): Promise<void> {
  await act(async () => {
    root.render(
      <Compose
        intent={{ kind: "draft", draft: draft as never }}
        placement="full"
        scope={null}
        onClose={onClose}
      />,
    );
  });
}

async function pressSend(): Promise<void> {
  const form = host.querySelector("form") as HTMLFormElement;
  await act(async () => {
    form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
});

const toastMessages = (): string[] => pushToast.mock.calls.map((c) => (c[0] as { message: string }).message);

describe("a send the server refuses", () => {
  it("keeps the composer open, announces nothing and keeps the autosave", async () => {
    // What sendMessage returns for a refusal; it has already toasted the error.
    actions.sendMessage.mockResolvedValue({ emailId: "E9", submissionId: null, refused: true });
    await mount();
    await pressSend();

    expect(actions.sendMessage).toHaveBeenCalledTimes(1); // the control: Send really ran
    expect(onClose).not.toHaveBeenCalled();
    expect(pushToast).not.toHaveBeenCalled();
    expect(offerUndo).not.toHaveBeenCalled();
    expect(requestLeave).not.toHaveBeenCalled();
    // The autosaved draft D1 is the only saved copy: it must survive. The
    // send-created E9 is the redundant one, retired quietly.
    expect(actions.discardDraft).not.toHaveBeenCalledWith("D1", expect.anything());
    expect(actions.discardDraft).toHaveBeenCalledWith("E9", { quiet: true });
    // Everything written is still there to resend.
    expect(host.querySelector("form")).not.toBeNull();
    expect(host.textContent).toContain("friend@example.net");
  });
});

describe("controls", () => {
  it("a successful send still toasts, retires the autosave and closes", async () => {
    actions.sendMessage.mockResolvedValue({ emailId: "E9", submissionId: "S1", sendAt: null });
    await mount();
    await pressSend();

    expect(onClose).toHaveBeenCalledTimes(1);
    expect(toastMessages().length + offerUndo.mock.calls.length).toBe(1);
    expect(actions.discardDraft).toHaveBeenCalledWith("D1", { quiet: true });
  });

  it("a queued (offline) send is held: info toast, closes, no discard", async () => {
    actions.sendMessage.mockResolvedValue({ emailId: null, submissionId: null, queued: true });
    await mount();
    await pressSend();

    expect(onClose).toHaveBeenCalledTimes(1);
    expect(pushToast).toHaveBeenCalledTimes(1);
    expect(actions.discardDraft).not.toHaveBeenCalled();
  });
});
