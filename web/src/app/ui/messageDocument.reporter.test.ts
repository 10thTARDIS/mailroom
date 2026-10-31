// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment node

// THE REPORTER IS PINNED BY HASH, AND IT NOW CARRIES THE WIDTH TOO (#801).
// The frame's only script is allowed by `script-src 'sha256-…'` in this
// document's policy and, by hand, in the app shell's (cmd/mailroom/main.go). One
// changed byte and the browser silently refuses to run it: no height, no width,
// no error anywhere. So the pin is recomputed here from the bytes that are
// actually shipped, and the same hash is required in the Go source that pins the
// shell's policy.

import { describe, expect, it } from "vitest";

import mainGo from "../../../../cmd/mailroom/main.go?raw";

import { messageCSP, messageDocument, REPORTER_SHA256 } from "./messageDocument";

function shippedScript(): string {
  const doc = messageDocument("<p>hi</p>", false);
  const m = /<script>([\s\S]*)<\/script><\/body>/.exec(doc);
  if (!m?.[1]) throw new Error("no script in the message document");
  return m[1];
}

describe("the frame's reporter", () => {
  it("hashes to the pin the policy carries", async () => {
    const bytes = new TextEncoder().encode(shippedScript());
    const sum = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
    expect(REPORTER_SHA256).toBe(`sha256-${btoa(String.fromCharCode(...sum))}`);
    expect(messageCSP(false)).toContain(`script-src '${REPORTER_SHA256}'`);
  });

  it("is the same pin the app shell's policy carries", () => {
    expect(mainGo).toContain(`"${REPORTER_SHA256}"`);
  });

  it("reports the document's natural width as well as its height", () => {
    const js = shippedScript();
    expect(js).toContain("height:document.body?document.body.scrollHeight:0");
    expect(js).toContain("width:document.documentElement.scrollWidth");
  });

  it("is still the only script in the document", () => {
    const doc = messageDocument("<p>hi</p>", false);
    expect(doc.match(/<script/g)).toHaveLength(1);
  });
});
