// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE KEY-CHANGED PROMPT UNDER AXE, in every wording it has (ADR-054): the
// ordinary prompt, the gentler vouched-for one, the compromised-previous-key one
// and the refused one. The browser lane (scripts/a11y-check.cjs) has no surface
// for this prompt: its fixture stack holds no signed mail from a rotated key, and
// making one needs a second mailbox and a certified key, which the fixture does
// not have. This is the check that exists, and it says what it cannot see: jsdom
// has no layout, so colour contrast and target size are not measured here (the
// vouched prompt adds no colour of its own: it reuses `.button-primary` and the
// `.key-change` block the other states already pass in the browser lane's
// composer and reader surfaces).
//
// THE CONTROL: a prompt with an unnamed button must be reported by the rule that
// names it, or a green result could mean "axe saw nothing".

import axe from "axe-core";
import { afterEach, describe, expect, it } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import { KeyChangeDecision } from "./KeyChangeDecision";

const OLD = "0123456789ABCDEF0123456789ABCDEF01234567";
const NEW = "FEDCBA9876543210FEDCBA9876543210FEDCBA98";

let host: HTMLDivElement | null = null;
let root: Root | null = null;

afterEach(() => {
  if (root) flushSync(() => root!.unmount());
  host?.remove();
  host = root = null;
});

function mount(props: { vouched?: boolean; refused?: boolean; previousCompromised?: boolean }): void {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <KeyChangeDecision
        address="alice@example.com"
        fingerprint={NEW}
        previousFingerprint={OLD}
        heading="Signed with a new key for alice@example.com."
        onDecide={async () => true}
        {...props}
      />,
    ),
  );
}

async function violations(): Promise<string[]> {
  const result = await axe.run(host!, {
    // No layout in jsdom: these cannot be answered here.
    rules: { "color-contrast": { enabled: false }, "target-size": { enabled: false }, region: { enabled: false } },
  });
  return result.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.html).join(" | ")}`);
}

const STATES: Array<[string, Parameters<typeof mount>[0]]> = [
  ["the ordinary prompt", {}],
  ["the vouched-for prompt", { vouched: true }],
  ["the compromised-previous-key prompt", { previousCompromised: true }],
  ["the already-refused prompt", { refused: true }],
];

describe("the key-changed prompt has no axe violations (ADR-054)", () => {
  for (const [name, props] of STATES) {
    it(`${name}`, async () => {
      mount(props);
      expect(await violations()).toEqual([]);
      // The rules that matter RAN and passed (not merely "nothing reported").
      const result = await axe.run(host!, { runOnly: ["button-name", "aria-allowed-role", "list", "definition-list"] });
      const passed = result.passes.map((p) => p.id);
      expect(passed).toContain("button-name");
    });
  }

  it("CONTROL: an unnamed button in the same block is reported", async () => {
    mount({ vouched: true });
    host!.querySelector(".key-change-actions")!.append(document.createElement("button"));
    expect((await violations()).join("\n")).toContain("button-name");
  });
});
