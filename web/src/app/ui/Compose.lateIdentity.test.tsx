// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE COMPOSER'S SENDING IDENTITY WHEN THE IDENTITIES ARRIVE LATE (#150). A draft
// opened from a reload's own URL renders once the draft has loaded, and the
// identity list is fetched alongside it, so the composer can mount first. It read
// the identity once (useState) and stayed on "no sending address yet" for good,
// which also meant no key to sign or encrypt with: the browser lane's key-change
// composer surfaces hit it as a switch that never appeared.
//
// THE CONTROL is the first test: with no identities the line says so, which is what
// shows the second test is observing the From line and not an empty page.

import "./editor/jsdomShim";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const identity = { id: "I1", accountId: "A1", email: "me@example.com", name: "Me", isWildcard: false };
const actions = {
  sendMessage: vi.fn(),
  discardDraft: vi.fn(async () => true),
  saveDraft: vi.fn(async () => null),
  cancelSubmission: vi.fn(async () => true),
  releaseAttachment: vi.fn(),
};
const mailState = { identities: { I1: identity }, mailboxes: {}, emails: {}, composeSnippets: [], session: null };
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

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
  mailState.identities = { I1: identity };
});

const render = async (): Promise<void> => {
  await act(async () => {
    root.render(<Compose intent={{ kind: "new" }} placement="main" scope={null} onClose={vi.fn()} />);
  });
};
const fromText = (): string => (host.querySelector(".compose-from")?.textContent ?? host.textContent ?? "").replace(/\s+/g, " ");

describe("the composer's sending identity", () => {
  it("control: with no identities at all the From line says there is no sending address yet", async () => {
    mailState.identities = {} as typeof mailState.identities;
    await render();
    expect(host.textContent).toContain("no sending address yet");
  });

  it("is filled in when the identities arrive after the composer mounted", async () => {
    mailState.identities = {} as typeof mailState.identities;
    await render();
    expect(host.textContent).toContain("no sending address yet");
    // The list arrives (a new object, as the store produces one).
    mailState.identities = { I1: identity };
    await render();
    expect(host.textContent).not.toContain("no sending address yet");
    expect(fromText()).toContain("me@example.com");
  });
});
